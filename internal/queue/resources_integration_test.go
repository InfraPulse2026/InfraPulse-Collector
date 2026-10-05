//go:build integration

package queue

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestDurableResourceBoundsAcrossReconnects(t *testing.T) {
	baseline := runtime.NumGoroutine()
	o := durableOptions(t)
	o.ACKTimeout = time.Second
	q := openDurable(t, o)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var largestFile int64
	for cycle := range 40 {
		for i := range 8 {
			if err := q.Push(telemetry(fmt.Sprintf("%d-%d", cycle, i))); err != nil {
				t.Fatal(err)
			}
		}
		for range 12 {
			for range o.MaxInflight {
				if _, err := q.Pop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if s := q.State(); s.Unacked > o.MaxInflight || s.Messages > o.MaxMessages || s.Bytes > o.MaxBytes {
				t.Fatal("resource bounds exceeded", s)
			}
			q.ResetInflight()
		}
		for range 8 {
			e, err := q.Pop(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if accepted, err := q.Acknowledge(e.MessageID); err != nil || !accepted {
				t.Fatalf("ACK %v %v", accepted, err)
			}
		}
		if s := q.State(); s.Messages != 0 || s.Bytes != 0 || s.Unacked != 0 {
			t.Fatal("queue retained completed records", s)
		}
		fi, err := os.Stat(o.Path)
		if err != nil || fi.Size() > int64(o.MaxStorageBytes) {
			t.Fatalf("physical disk bound: %v %v", fi, err)
		}
		largestFile = max(largestFile, fi.Size())
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	// Allow transient bbolt Batch timers to finish; one leaked goroutine per
	// reconnect would exceed this bound by two orders of magnitude.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	remaining := runtime.NumGoroutine()
	if remaining > baseline+4 {
		t.Fatalf("goroutines grew from %d to %d", baseline, remaining)
	}
	t.Logf("480 reconnect resets, 320 admissions/ACKs; max file=%d bytes; goroutines before=%d after=%d", largestFile, baseline, remaining)
}
