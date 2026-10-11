//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

func TestPhase8IdleDurableStartupShutdownResources(t *testing.T) {
	c := testConfig(t, syslogTestAddress(t))
	c.Local.Enabled, c.Commands.Enabled = false, false
	configureDurable(&c)
	c.Controller.ReconnectInitial, c.Controller.ReconnectMax = 20*time.Millisecond, 80*time.Millisecond
	c.Telemetry.HeartbeatInterval = time.Hour
	runtime.GC()
	var before, idle, peak, after runtime.MemStats
	runtime.ReadMemStats(&before)
	initialGoroutines, initialFD := runtime.NumGoroutine(), phase8FDCount()
	var startup, shutdown time.Duration
	var identity string
	sessions := map[string]bool{}
	const cycles = 20
	for cycle := range cycles {
		start := time.Now()
		a := newTestAgent(t, c, nil)
		startup = max(startup, time.Since(start))
		if cycle == 0 {
			identity = a.id.AgentID
		}
		if a.id.AgentID != identity || sessions[a.id.SessionID] {
			t.Fatal("restart identity/session invariant")
		}
		sessions[a.id.SessionID] = true
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		time.Sleep(100 * time.Millisecond)
		runtime.ReadMemStats(&idle)
		peak.HeapAlloc = max(peak.HeapAlloc, idle.HeapAlloc)
		peak.HeapInuse = max(peak.HeapInuse, idle.HeapInuse)
		start = time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown exceeded deadline")
		}
		shutdown = max(shutdown, time.Since(start))
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > initialGoroutines+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	finalFD := phase8FDCount()
	if runtime.NumGoroutine() > initialGoroutines+1 || initialFD >= 0 && finalFD > initialFD+1 {
		t.Fatal("resources did not stabilize", runtime.NumGoroutine(), initialGoroutines, finalFD, initialFD)
	}
	data, _ := json.Marshal(map[string]any{"cycles": cycles, "before_heap": before.HeapAlloc, "last_idle_heap": idle.HeapAlloc, "peak_heap": peak.HeapAlloc, "peak_heap_inuse": peak.HeapInuse, "after_gc_heap": after.HeapAlloc, "total_alloc": after.TotalAlloc - before.TotalAlloc, "allocations": after.Mallocs - before.Mallocs, "goroutines_before": initialGoroutines, "goroutines_after": runtime.NumGoroutine(), "fd_before": initialFD, "fd_after": finalFD, "max_new_ns": startup.Nanoseconds(), "max_shutdown_ns": shutdown.Nanoseconds()})
	t.Logf("PHASE8_IDLE %s", data)
}
