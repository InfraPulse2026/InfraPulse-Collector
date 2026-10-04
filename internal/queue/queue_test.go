package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func telemetry(id string) model.Envelope {
	return model.Envelope{MessageID: id, Telemetry: &model.TelemetrySample{DeviceID: "device"}}
}

func mustQueue(t *testing.T, messages, bytes, messageBytes int) *Queue {
	t.Helper()
	q, err := New(messages, bytes, messageBytes)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestQueueLimitsAndImmutableSnapshot(t *testing.T) {
	q := mustQueue(t, 2, 4096, 2048)
	first := telemetry("first")
	data, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Push(first); err != nil {
		t.Fatal(err)
	}
	first.Telemetry.DeviceID = "mutated after enqueue"
	if q.State().Bytes != len(data) {
		t.Fatalf("wrong byte accounting: %+v", q.State())
	}
	got, err := q.Pop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Telemetry.DeviceID != "device" {
		t.Fatal("queue retained a mutable caller payload")
	}
	if q.State().Messages != 0 || q.State().Bytes != 0 {
		t.Fatalf("queue did not release data: %+v", q.State())
	}
	oversize := telemetry(strings.Repeat("x", 3000))
	if err := q.Push(oversize); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize accepted: %v", err)
	}
	if q.State().DroppedTelemetry != 1 {
		t.Fatalf("rejected telemetry not counted: %+v", q.State())
	}
}

func TestPriorityEvictionAndOrder(t *testing.T) {
	q := mustQueue(t, 3, 10000, 3000)
	for _, e := range []model.Envelope{telemetry("old"), {MessageID: "inventory", Inventory: &model.InventoryUpdate{}}, telemetry("new")} {
		if err := q.Push(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Push(model.Envelope{MessageID: "result", CommandResult: &model.CommandResult{}}); err != nil {
		t.Fatal(err)
	}
	if q.State().DroppedTelemetry != 1 {
		t.Fatalf("wrong dropped counter: %+v", q.State())
	}
	for _, want := range []string{"result", "inventory", "new"} {
		got, err := q.Pop(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.MessageID != want {
			t.Fatalf("got %q; want %q", got.MessageID, want)
		}
	}
}

func TestRejectedPushPreservesHigherPriorityMessages(t *testing.T) {
	q := mustQueue(t, 2, 4096, 2048)
	for _, id := range []string{"result1", "result2"} {
		if err := q.Push(model.Envelope{MessageID: id, CommandResult: &model.CommandResult{}}); err != nil {
			t.Fatal(err)
		}
	}
	before := q.State()
	if err := q.Push(telemetry("drop")); !errors.Is(err, ErrFull) {
		t.Fatalf("want full, got %v", err)
	}
	after := q.State()
	if after.Messages != before.Messages || after.Bytes != before.Bytes || after.DroppedTelemetry != 1 {
		t.Fatalf("unexpected mutation: before=%+v after=%+v", before, after)
	}
	for _, want := range []string{"result1", "result2"} {
		got, err := q.Pop(context.Background())
		if err != nil || got.MessageID != want {
			t.Fatalf("lost important message: got=%q err=%v", got.MessageID, err)
		}
	}
}

func TestCommandResultsCannotEvictAcceptedResults(t *testing.T) {
	q := mustQueue(t, 2, 4096, 2048)
	for _, id := range []string{"result1", "result2"} {
		if err := q.Push(model.Envelope{MessageID: id, CommandResult: &model.CommandResult{}}); err != nil {
			t.Fatal(err)
		}
	}
	before := q.State()
	if err := q.Push(model.Envelope{MessageID: "result3", CommandResult: &model.CommandResult{}}); !errors.Is(err, ErrFull) {
		t.Fatalf("want full, got %v", err)
	}
	after := q.State()
	if after.Messages != before.Messages || after.Bytes != before.Bytes || after.DroppedOther != 1 {
		t.Fatalf("unexpected mutation: before=%+v after=%+v", before, after)
	}
	for _, want := range []string{"result1", "result2"} {
		got, err := q.Pop(context.Background())
		if err != nil || got.MessageID != want {
			t.Fatalf("lost accepted result: got=%q err=%v", got.MessageID, err)
		}
	}
}

func TestResultEvictsTelemetryAndTelemetryStillDropsOldest(t *testing.T) {
	q := mustQueue(t, 2, 4096, 2048)
	for _, id := range []string{"old", "middle", "new"} {
		if err := q.Push(telemetry(id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Push(model.Envelope{MessageID: "result", CommandResult: &model.CommandResult{}}); err != nil {
		t.Fatal(err)
	}
	if q.State().DroppedTelemetry != 2 {
		t.Fatalf("wrong dropped telemetry count: %+v", q.State())
	}
	for _, want := range []string{"result", "new"} {
		got, err := q.Pop(context.Background())
		if err != nil || got.MessageID != want {
			t.Fatalf("wrong ordering: got=%q err=%v", got.MessageID, err)
		}
	}
}

func TestByteLimitEvictsMultipleMessages(t *testing.T) {
	item := telemetry("entry")
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQueue(t, 10, len(data)*2+20, len(data)*2+20)
	if err := q.Push(item); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(item); err != nil {
		t.Fatal(err)
	}
	larger := telemetry(strings.Repeat("L", len(data)+25))
	if err := q.Push(larger); err != nil {
		t.Fatal(err)
	}
	state := q.State()
	if state.Messages != 1 || state.DroppedTelemetry != 2 || state.Bytes > len(data)*2+20 {
		t.Fatalf("byte bound failed: %+v", state)
	}
}

func TestPopCancellationAndConcurrentConsumers(t *testing.T) {
	q := mustQueue(t, 4, 10000, 3000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Pop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	if err := q.Push(telemetry("keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Pop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pop consumed message: %v", err)
	}
	if q.State().Messages != 1 {
		t.Fatal("canceled pop changed queue")
	}
	if _, err := q.Pop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan string, 3)
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			e, err := q.Pop(ctx)
			if err == nil {
				results <- e.MessageID
			}
		})
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Push(telemetry(id)); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
	close(results)
	seen := map[string]bool{}
	for id := range results {
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Fatalf("consumers did not receive every message: %v", seen)
	}
}

func TestValidation(t *testing.T) {
	for _, limits := range [][3]int{{0, 100, 10}, {1, 0, 10}, {1, 10, 0}, {1, 10, 11}} {
		if _, err := New(limits[0], limits[1], limits[2]); err == nil {
			t.Fatalf("accepted invalid limits %v", limits)
		}
	}
	q := mustQueue(t, 2, 4096, 2048)
	if err := q.Push(model.Envelope{}); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("accepted missing payload: %v", err)
	}
	e := telemetry("ambiguous")
	e.Heartbeat = &model.Heartbeat{}
	if err := q.Push(e); !errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("accepted ambiguous payload: %v", err)
	}
}
