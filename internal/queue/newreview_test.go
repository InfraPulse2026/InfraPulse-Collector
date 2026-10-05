package queue

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestEvictionClearsInflightLeaseWithoutACKTimeout(t *testing.T) {
	o := durableOptions(t)
	o.MaxMessages, o.MaxInflight = 1, 1
	o.ACKTimeout = 10 * time.Millisecond
	q := openDurable(t, o)
	if err := q.Push(telemetry("leased-telemetry")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := model.Envelope{MessageID: "protected-result", CommandResult: &model.CommandResult{CommandID: "command", Status: "success"}}
	if err := q.Push(result); err != nil {
		t.Fatal(err)
	}
	if q.Contains("leased-telemetry") || q.State().Unacked != 0 || q.State().PendingResults != 1 {
		t.Fatal(q.State())
	}
	// Wait beyond the former lease deadline: it must not reconnect an otherwise
	// healthy session or block admission of a higher priority result.
	time.Sleep(15 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := q.Pop(ctx)
	if err != nil || got.MessageID != result.MessageID {
		t.Fatal(got, err)
	}
	if found, err := q.Acknowledge("leased-telemetry"); err != nil || found {
		t.Fatal(found, err)
	}
	if found, err := q.Acknowledge(result.MessageID); err != nil || !found {
		t.Fatal(found, err)
	}
	if q.State().Messages != 0 || q.State().Unacked != 0 {
		t.Fatal(q.State())
	}
}

func TestRetentionClearsLeaseAndPreservesPendingResult(t *testing.T) {
	o := durableOptions(t)
	o.Retention = time.Millisecond
	o.MaxInflight = 1
	q := openDurable(t, o)
	if err := q.Push(telemetry("expired-lease")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond)
	if err := q.Push(model.Envelope{MessageID: "result", CommandResult: &model.CommandResult{CommandID: "c", Status: "success"}}); err != nil {
		t.Fatal(err)
	}
	if q.Contains("expired-lease") || q.State().Unacked != 0 {
		t.Fatal(q.State())
	}
	time.Sleep(3 * time.Millisecond)
	if err := q.Push(telemetry("fresh")); err != nil {
		t.Fatal(err)
	}
	if !q.Contains("result") || q.State().PendingResults != 1 {
		t.Fatal("retention removed pending result")
	}
	if got, err := q.Pop(context.Background()); err != nil || got.MessageID != "result" {
		t.Fatal(got, err)
	}
}

func TestReducedDurableConfigPreservesExistingRecords(t *testing.T) {
	o := durableOptions(t)
	q := openDurable(t, o)
	for _, id := range []string{"one", "two"} {
		e := telemetry(id)
		e.Telemetry.Metrics = []model.Metric{{Name: strings.Repeat("metric", 160), Value: 1}}
		if err := q.Push(e); err != nil {
			t.Fatal(err)
		}
	}
	size := q.State().Bytes
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(o.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DurableOptions){
		func(o *DurableOptions) { o.MaxMessages, o.MaxInflight = 1, 1 },
		func(o *DurableOptions) { o.MaxMessageBytes = 512 },
		func(o *DurableOptions) { o.MaxBytes, o.MaxMessageBytes = size-1, size/2+64 },
	} {
		smaller := o
		mutate(&smaller)
		opened, err := NewDurable(smaller)
		if err == nil {
			_ = opened.Close()
			t.Fatal("existing data exceeded reduced limit")
		}
		if !errors.Is(err, ErrFull) || errors.Is(err, ErrCorrupt) {
			t.Fatalf("configuration reduction classified as corruption: %v", err)
		}
		after, err := os.ReadFile(o.Path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("configuration reduction changed durable records", err)
		}
	}
	q = openDurable(t, o)
	if q.State().Messages != 2 {
		t.Fatal(q.State())
	}
	for _, id := range []string{"one", "two"} {
		if !q.Contains(id) {
			t.Fatal("restoring configured limits lost record", id)
		}
	}
}
