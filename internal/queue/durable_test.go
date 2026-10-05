package queue

import (
	"InfraPulseAgent/internal/model"
	"context"
	"encoding/json"
	"errors"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func durableOptions(t *testing.T) DurableOptions {
	t.Helper()
	return DurableOptions{Path: filepath.Join(t.TempDir(), "outbox.db"), MaxMessages: 16, MaxBytes: 128 << 10, MaxMessageBytes: 8192, MaxStorageBytes: 2 << 20, Retention: time.Hour, BatchDelay: time.Millisecond, BatchSize: 16, MaxInflight: 4, ACKTimeout: 100 * time.Millisecond}
}
func openDurable(t *testing.T, o DurableOptions) *Queue {
	t.Helper()
	q, e := NewDurable(o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}
func TestDurableAdmissionRestartReplayAndACK(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	e := telemetry("stable")
	e.Telemetry.Source = "simulated"
	if err := q.Push(e); err != nil {
		t.Fatal(err)
	}
	e.Telemetry.Source = "mutated"
	got, err := q.Pop(context.Background())
	if err != nil || got.Telemetry.Source != "simulated" {
		t.Fatalf("immutable: %+v %v", got, err)
	}
	if q.State().Messages != 1 || q.State().Unacked != 1 {
		t.Fatal(q.State())
	}
	// Send has no deleting effect. Closing simulates process loss before ACK.
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	q = openDurable(t, o)
	got, err = q.Pop(context.Background())
	if err != nil || got.MessageID != "stable" {
		t.Fatalf("replay: %+v %v", got, err)
	}
	if found, err := q.Acknowledge("unknown"); err != nil || found {
		t.Fatalf("unknown ACK: %v %v", found, err)
	}
	if found, err := q.Acknowledge("stable"); err != nil || !found {
		t.Fatalf("ACK: %v %v", found, err)
	}
	if found, err := q.Acknowledge("stable"); err != nil || found {
		t.Fatalf("duplicate ACK: %v %v", found, err)
	}
	_ = q.Close()
	q = openDurable(t, o)
	if q.State().Messages != 0 {
		t.Fatal("ACK deletion not persisted")
	}
}
func TestDurableBeforeSendCrashAndResultReservation(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	if err := q.Push(telemetry("unsent")); err != nil {
		t.Fatal(err)
	}
	r, err := q.ReserveResult(4096)
	if err != nil {
		t.Fatal(err)
	}
	result := model.Envelope{MessageID: "result", CommandResult: &model.CommandResult{CommandID: "command", Status: "success"}}
	if err = r.Commit(result); err != nil {
		t.Fatal(err)
	}
	// An uncommitted reservation is recovered by the command journal instead.
	if _, err = q.ReserveResult(4096); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
	q = openDurable(t, o)
	if q.State().Messages != 2 || q.State().PendingResults != 1 {
		t.Fatal(q.State())
	}
	for _, id := range []string{"result", "unsent"} {
		got, err := q.Pop(context.Background())
		if err != nil || got.MessageID != id {
			t.Fatalf("order: %+v %v", got, err)
		}
	}
}
func TestDurableBoundsEvictionAndRetention(t *testing.T) {
	o := durableOptions(t)
	o.MaxMessages = 2
	o.MaxInflight = 2
	q := openDurable(t, o)
	for _, e := range []model.Envelope{telemetry("old"), {MessageID: "result", CommandResult: &model.CommandResult{}}, telemetry("new")} {
		if err := q.Push(e); err != nil {
			t.Fatal(err)
		}
	}
	if q.Contains("old") || !q.Contains("result") || q.State().DroppedTelemetry != 1 {
		t.Fatal(q.State())
	}
	if err := q.Push(model.Envelope{MessageID: "result2", CommandResult: &model.CommandResult{}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(telemetry("blocked")); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	if q.State().PendingResults != 2 {
		t.Fatal("result evicted")
	}
	// Age telemetry while preserving checksum: retention never deletes result.
	o2 := durableOptions(t)
	q2 := openDurable(t, o2)
	_ = q2.Push(telemetry("expired"))
	if err := q2.disk.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(recordsBucket)
		k, v := b.Cursor().First()
		r, e := decodeRecord(v)
		if e != nil {
			return e
		}
		r.CreatedAt = time.Now().Add(-2 * time.Hour)
		r.Checksum = recordChecksum(r)
		data, _ := json.Marshal(r)
		return b.Put(k, data)
	}); err != nil {
		t.Fatal(err)
	}
	if err := q2.Push(telemetry("fresh")); err != nil {
		t.Fatal(err)
	}
	if q2.Contains("expired") {
		t.Fatal("expired retained")
	}
}
func TestDurableDuplicateCancellationAndACKTimeout(t *testing.T) {
	o := durableOptions(t)
	o.MaxInflight = 1
	q := openDurable(t, o)
	e := telemetry("one")
	if err := q.Push(e); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(e); err != nil {
		t.Fatal(err)
	}
	if q.State().Messages != 1 {
		t.Fatal("duplicate admitted")
	}
	e.Telemetry.DeviceID = "different"
	if err := q.Push(e); !errors.Is(err, ErrDuplicateID) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Pop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, _ = q.Pop(context.Background())
	if _, err := q.Pop(context.Background()); !errors.Is(err, ErrACKTimeout) {
		t.Fatal(err)
	}
	q.ResetInflight()
	if got, err := q.Pop(context.Background()); err != nil || got.MessageID != "one" {
		t.Fatalf("reconnect %v %v", got, err)
	}
}
func TestDurableConcurrentBatchBounded(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if err := q.Push(model.Envelope{MessageID: string(rune('a' + i)), Inventory: &model.InventoryUpdate{}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s := q.State()
	if s.Messages != 16 || s.Bytes > o.MaxBytes || s.StorageBytes > int64(o.MaxStorageBytes) {
		t.Fatal(s)
	}
}
func TestDurableCorruptionPreservesState(t *testing.T) {
	for _, damage := range []string{"checksum", "metadata", "truncate", "partial", "schema"} {
		t.Run(damage, func(t *testing.T) {
			o := durableOptions(t)
			q := openDurable(t, o)
			_ = q.Push(telemetry("keep"))
			if damage == "checksum" || damage == "metadata" || damage == "schema" {
				if err := q.disk.db.Update(func(tx *bolt.Tx) error {
					if damage == "schema" {
						return tx.DeleteBucket(idsBucket)
					}
					b := tx.Bucket(recordsBucket)
					k, v := b.Cursor().First()
					var r diskRecord
					_ = json.Unmarshal(v, &r)
					if damage == "checksum" {
						r.Data = []byte("broken")
					} else {
						r.CreatedAt = time.Time{}
					}
					data, _ := json.Marshal(r)
					return b.Put(k, data)
				}); err != nil {
					t.Fatal(err)
				}
			}
			_ = q.Close()
			if damage == "truncate" {
				if err := os.Truncate(o.Path, 2048); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "partial" {
				if err := os.WriteFile(o.Path, []byte("partial-write"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(o.Path)
			bad, err := NewDurable(o)
			if err == nil {
				_ = bad.Close()
				t.Fatal("corruption accepted")
			}
			after, _ := os.ReadFile(o.Path)
			if len(before) != len(after) {
				t.Fatal("corrupt state erased")
			}
		})
	}
}
func TestDurableStorageFailuresAndHardFileLimit(t *testing.T) {
	o := durableOptions(t)
	o.Path = filepath.Join(o.Path, "child")
	if err := os.WriteFile(filepath.Dir(o.Path), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurable(o); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	o = durableOptions(t)
	o.MaxStorageBytes = 1 << 20
	o.MaxBytes = 128 << 10
	o.MaxMessages = 128
	q := openDurable(t, o)
	for i := range 500 {
		e := telemetry(string(rune(i + 1)))
		e.Telemetry.Metrics = make([]model.Metric, 50)
		err := q.Push(e)
		if err != nil && !errors.Is(err, ErrStorage) {
			t.Fatal(err)
		}
	}
	if q.State().StorageBytes > int64(o.MaxStorageBytes) {
		t.Fatal("file cap exceeded")
	}
	_ = q.Close()
	if err := q.Push(telemetry("closed")); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
}
