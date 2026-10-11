package queue

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	bolt "go.etcd.io/bbolt"
)

// Pin consecutive committed page versions and deny further allocation. This
// injects a real bbolt transaction-commit error without filling the host disk,
// changing filesystem permissions, or replacing the durable implementation.
func exhaustCommitPages(t *testing.T, q *Queue) func() {
	t.Helper()
	db := q.disk.db
	limit := db.MaxSize
	var readers []*bolt.Tx
	restore := func() {
		for _, reader := range readers {
			if err := reader.Rollback(); err != nil {
				t.Errorf("release fault-injection snapshot: %v", err)
			}
		}
		readers = nil
		db.MaxSize = limit
	}
	t.Cleanup(restore)
	db.MaxSize = 1
	for i := range 64 {
		reader, err := db.Begin(false)
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, reader)
		err = db.Update(func(tx *bolt.Tx) error {
			value := make([]byte, 8)
			binary.BigEndian.PutUint64(value, uint64(i))
			return tx.Bucket(metaBucket).Put([]byte("other"), value)
		})
		if err != nil {
			t.Logf("injected page-allocation failure after %d successful pinned commits: %v", i, err)
			return restore
		}
	}
	t.Fatal("fault injection did not exhaust allocatable pages")
	return restore
}

func TestPhase8DurableACKCommitFailureRetainsRecordAndLease(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	want := model.Envelope{MessageID: "must-replay", CommandResult: &model.CommandResult{CommandID: "executed", Status: "success"}}
	if err := q.Push(want); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	restore := exhaustCommitPages(t, q)
	if _, err := q.Acknowledge(want.MessageID); !errors.Is(err, ErrStorage) {
		t.Fatalf("failed commit was reported as successful ACK: %v", err)
	}
	if !q.Contains(want.MessageID) || q.State().Unacked != 1 || q.State().StorageError == "" {
		t.Fatal("failed ACK deleted retained record or lease", q.State())
	}
	if err := q.Push(telemetry("not-admitted")); !errors.Is(err, ErrStorage) {
		t.Fatal("storage allocation failure was not propagated", err)
	}
	if q.Contains("not-admitted") || !q.Contains(want.MessageID) {
		t.Fatal("failed admission changed committed records")
	}
	restore()
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q = openDurable(t, o)
	got, err := q.Pop(context.Background())
	if err != nil || got.MessageID != want.MessageID || got.CommandResult.CommandID != "executed" {
		t.Fatal("failed ACK did not recover with stable ID", got, err)
	}
	if accepted, err := q.Acknowledge(got.MessageID); err != nil || !accepted {
		t.Fatal("restored storage cannot commit ACK", accepted, err)
	}
	if q.State().Messages != 0 {
		t.Fatal("successful ACK did not delete replayed record")
	}
}

func TestPhase8DurableOutOfOrderDelayedAndDuplicateACK(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	for _, id := range []string{"first", "second", "third"} {
		if err := q.Push(telemetry(id)); err != nil {
			t.Fatal(err)
		}
	}
	if accepted, err := q.Acknowledge("third"); err != nil || accepted || !q.Contains("third") {
		t.Fatal("unsent ACK removed retained record", accepted, err)
	}
	for range 3 {
		if _, err := q.Pop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"third", "first"} {
		if accepted, err := q.Acknowledge(id); err != nil || !accepted {
			t.Fatal("out-of-order ACK rejected", id, accepted, err)
		}
	}
	q.ResetInflight()
	if accepted, err := q.Acknowledge("second"); err != nil || accepted || !q.Contains("second") {
		t.Fatal("old session ACK changed unleased replay", accepted, err)
	}
	got, err := q.Pop(context.Background())
	if err != nil || got.MessageID != "second" {
		t.Fatal("lost ACK cannot replay stable record", got, err)
	}
	if accepted, err := q.Acknowledge("second"); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if accepted, err := q.Acknowledge("second"); err != nil || accepted {
		t.Fatal("duplicate ACK was not harmless", accepted, err)
	}
	if q.State().Messages != 0 || q.State().Unacked != 0 {
		t.Fatal(q.State())
	}
}

func TestPhase8DurableConcurrentOfflinePressureConservesRecords(t *testing.T) {
	o := durableOptions(t)
	o.MaxMessages, o.MaxInflight, o.BatchSize = 64, 8, 32
	q := openDurable(t, o)
	for i := range 8 {
		if err := q.Push(model.Envelope{MessageID: fmt.Sprintf("protected-%d", i), CommandResult: &model.CommandResult{CommandID: fmt.Sprintf("command-%d", i), Status: "success"}}); err != nil {
			t.Fatal(err)
		}
	}
	const producers, perProducer = 8, 128
	started := time.Now()
	var workers sync.WaitGroup
	for producer := range producers {
		workers.Go(func() {
			for i := range perProducer {
				e := telemetry(fmt.Sprintf("ingest-%d-%d", producer, i))
				switch i % 4 {
				case 1:
					e.Telemetry = nil
					e.Logs = &model.LogBatch{Source: "simulated", Records: []model.LogRecord{{Message: "bounded syslog fixture", Source: "simulated"}}}
				case 2:
					e.Telemetry = nil
					e.Events = []model.Event{{ID: e.MessageID, Source: "simulated", Type: "fixture.pressure"}}
				case 3:
					e.Telemetry = nil
					e.Topology = &model.TopologyUpdate{Source: "simulated"}
				}
				if err := q.Push(e); err != nil && !errors.Is(err, ErrFull) {
					t.Error(err)
				}
				if s := q.State(); s.Messages > o.MaxMessages || s.Bytes > o.MaxBytes || s.StorageBytes > int64(o.MaxStorageBytes) {
					t.Error("offline ingestion exceeded resource limits", s)
				}
			}
		})
	}
	workers.Wait()
	s := q.State()
	if s.PendingResults != 8 || s.Messages != o.MaxMessages || s.DroppedTelemetry+s.DroppedOther != producers*perProducer+8-uint64(s.Messages) {
		t.Fatal("admitted/retained/dropped accounting does not conserve records", s)
	}
	for i := range 8 {
		if !q.Contains(fmt.Sprintf("protected-%d", i)) {
			t.Fatal("pressure evicted accepted command result")
		}
	}
	t.Logf("offline mixed ingestion: %d producers x %d envelopes in %s; retained=%d bytes=%d file=%d dropped=%d logs-dropped=%d", producers, perProducer, time.Since(started), s.Messages, s.Bytes, s.StorageBytes, s.DroppedTelemetry+s.DroppedOther, s.DroppedLogs)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q = openDurable(t, o)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seen := make(map[string]bool)
	for range s.Messages {
		e, err := q.Pop(ctx)
		if err != nil || seen[e.MessageID] {
			t.Fatal("restart replay lost record or repeated active lease", e.MessageID, err)
		}
		seen[e.MessageID] = true
		if accepted, err := q.Acknowledge(e.MessageID); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
	}
	if q.State().Messages != 0 || q.State().Unacked != 0 {
		t.Fatal("drain did not release retained capacity", q.State())
	}
}

func TestPhase8DurableCloseUnblocksWaitingPop(t *testing.T) {
	for _, fullInflight := range []bool{false, true} {
		t.Run(fmt.Sprintf("inflight=%v", fullInflight), func(t *testing.T) {
			o := durableOptions(t)
			o.MaxInflight, o.ACKTimeout = 1, time.Minute
			q := openDurable(t, o)
			if fullInflight {
				if err := q.Push(telemetry("retained")); err != nil {
					t.Fatal(err)
				}
				if _, err := q.Pop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := q.Pop(context.Background()); done <- err }()
			select {
			case err := <-done:
				t.Fatal("Pop completed before Close", err)
			case <-time.After(5 * time.Millisecond):
			}
			started := time.Now()
			if err := q.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrStorage) {
					t.Fatal("closed queue did not unblock Pop", err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close left blocked Pop")
			}
			t.Logf("Close + blocked Pop completed in %s", time.Since(started))
			q = openDurable(t, o)
			if fullInflight && !q.Contains("retained") {
				t.Fatal("shutdown discarded retained record")
			}
		})
	}
}
