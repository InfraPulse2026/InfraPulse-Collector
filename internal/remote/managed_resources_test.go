package remote

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
)

func TestManagedLifecycleResourceProbe(t *testing.T) {
	runtime.GC()
	var before, idle, active, after runtime.MemStats
	runtime.ReadMemStats(&before)
	gBefore := runtime.NumGoroutine()
	cfg := config.Defaults()
	q, err := queue.New(32, 1<<20, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var calls, opened, closed atomic.Int64
	factory := func(config.Device) (driver.Adapter, error) {
		opened.Add(1)
		return &resourceAdapter{calls: &calls, closed: &closed}, nil
	}
	r, err := NewAdapters(cfg, factory, q.Push, WithCollectorID("resource-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	stop := managedRun(t, r)
	runtime.GC()
	runtime.ReadMemStats(&idle)
	gIdle := runtime.NumGoroutine()
	devices := make([]config.Device, 32)
	for i := range devices {
		devices[i] = managedDevice("snmp")
		devices[i].ID = fmt.Sprintf("managed-%02d", i)
		devices[i].Polling.FastInterval = 10 * time.Millisecond
	}
	var addDuration, removeDuration time.Duration
	var maxGoroutines int
	started := time.Now()
	for range 20 {
		at := time.Now()
		managedApply(t, r, devices, factory)
		addDuration += time.Since(at)
		previous := calls.Load()
		managedWait(t, func() bool { return calls.Load() > previous })
		if n := runtime.NumGoroutine(); n > maxGoroutines {
			maxGoroutines = n
		}
		if len(r.Targets()) != 32 {
			t.Fatal("resource probe lost active targets")
		}
		at = time.Now()
		managedApply(t, r, nil, factory)
		removeDuration += time.Since(at)
		if opened.Load() != closed.Load() {
			t.Fatal("removed adapter resources retained")
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&active)
	stop()
	runtime.GC()
	runtime.ReadMemStats(&after)
	gAfter := runtime.NumGoroutine()
	if gAfter > gBefore {
		t.Fatalf("managed lifecycle leaked goroutines: before=%d after=%d", gBefore, gAfter)
	}
	s := q.State()
	t.Logf("20 cycles x32 targets: elapsed=%s add_mean=%s remove_mean=%s adapters=%d/%d polls=%d goroutines=%d->%d peak=%d ->%d idle_heap_delta=%d retained_queue_heap_delta=%d after_close_heap_delta=%d total_alloc=%d queue_messages=%d queue_bytes=%d dropped_telemetry=%d", time.Since(started), addDuration/20, removeDuration/20, opened.Load(), closed.Load(), calls.Load(), gBefore, gIdle, maxGoroutines, gAfter, int64(idle.HeapAlloc)-int64(before.HeapAlloc), int64(active.HeapAlloc)-int64(before.HeapAlloc), int64(after.HeapAlloc)-int64(before.HeapAlloc), after.TotalAlloc-before.TotalAlloc, s.Messages, s.Bytes, s.DroppedTelemetry)
	runtime.KeepAlive(r)
	runtime.KeepAlive(q)
}

type resourceAdapter struct{ calls, closed *atomic.Int64 }

func (*resourceAdapter) Capabilities() []model.Capability { return nil }
func (a *resourceAdapter) Close() error                   { a.closed.Add(1); return nil }
func (a *resourceAdapter) Collect(context.Context) (model.TelemetrySample, error) {
	a.calls.Add(1)
	at := time.Now().UTC()
	return model.TelemetrySample{Source: "simulated", ObservedAt: at, CollectedAt: at}, nil
}

func BenchmarkManagedPrepare32Targets(b *testing.B) {
	cfg := config.Defaults()
	factory := func(config.Device) (driver.Adapter, error) { return &managedNetwork{&managedCounts{}}, nil }
	r, err := NewAdapters(cfg, factory, func(model.Envelope) error { return nil })
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	devices := make([]config.Device, 32)
	for i := range devices {
		devices[i] = managedDevice("snmp")
		devices[i].ID = fmt.Sprintf("benchmark-%02d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		p, err := r.PrepareManaged(devices, factory)
		if err != nil {
			b.Fatal(err)
		}
		if err = p.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func TestHundredManagedLifecycleChangesReleaseResources(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()
	var opened, closed, calls atomic.Int64
	factory := func(config.Device) (driver.Adapter, error) {
		opened.Add(1)
		return &resourceAdapter{calls: &calls, closed: &closed}, nil
	}
	r, err := NewAdapters(config.Defaults(), factory, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	stop := managedRun(t, r)
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	at := time.Now()
	for cycle := range 100 {
		d := managedDevice("snmp")
		d.ID = fmt.Sprintf("lifecycle-%03d", cycle)
		d.Polling.FastInterval = time.Hour
		previous := calls.Load()
		managedApply(t, r, []config.Device{d}, factory)
		managedWait(t, func() bool { return calls.Load() > previous })
		managedApply(t, r, nil, factory)
		if len(r.Targets()) != 0 || len(r.Stats()) != 0 || opened.Load() != closed.Load() {
			t.Fatalf("cycle %d retained targets/jobs/adapters: opened=%d closed=%d", cycle, opened.Load(), closed.Load())
		}
	}
	stop()
	stop = nil
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before {
		t.Fatalf("goroutines did not stabilize after 100 lifecycle changes: %d -> %d", before, after)
	}
	t.Logf("100 add/poll/remove cycles elapsed=%s opened=%d closed=%d polls=%d goroutines=%d->%d", time.Since(at), opened.Load(), closed.Load(), calls.Load(), before, after)
}

func BenchmarkManagedLiveAddRemove32Targets(b *testing.B) {
	cfg := config.Defaults()
	factory := func(config.Device) (driver.Adapter, error) { return &managedNetwork{&managedCounts{}}, nil }
	r, err := NewAdapters(cfg, factory, func(model.Envelope) error { return nil })
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() { cancel(); <-done; _ = r.Close() }()
	devices := make([]config.Device, 32)
	for i := range devices {
		devices[i] = managedDevice("snmp")
		devices[i].ID = fmt.Sprintf("benchmark-%02d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for _, desired := range [][]config.Device{devices, nil} {
			p, err := r.PrepareManaged(desired, factory)
			if err != nil {
				b.Fatal(err)
			}
			if err = p.Apply(ctx); err != nil {
				p.Close()
				b.Fatal(err)
			}
			if err = p.Commit(); err != nil {
				p.Close()
				b.Fatal(err)
			}
		}
	}
}
