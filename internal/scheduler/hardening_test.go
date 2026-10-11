package scheduler

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// All targets are due at startup, including slow hardware jobs. Saturation must
// defer their first poll until a worker is available, rather than skip a whole
// inventory interval. One OS thread makes the original queue-full case reliable.
func TestSaturatedStartupServicesEveryTarget(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, count := range []int{10, 50, 100} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := New(4, nil)
			calls := make([]atomic.Int32, count)
			for i := range count {
				if err := s.Add(Job{Name: fmt.Sprintf("target-%03d", i), Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error {
					calls[i].Add(1)
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			cancel, done := runScheduler(t, s)
			defer stopScheduler(t, cancel, done)
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				serviced := 0
				for i := range calls {
					if calls[i].Load() != 0 {
						serviced++
					}
				}
				if serviced == count {
					return
				}
				select {
				case <-deadline.C:
					t.Fatalf("only %d/%d targets received their first poll; saturated startup skipped long-period jobs", serviced, count)
				case <-ticker.C:
				}
			}
		})
	}
}

func TestSaturatedPeriodsRemainBoundedAndFair(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	s := New(2, nil)
	const count = 50
	var active, peak atomic.Int32
	calls := make([]atomic.Int32, count)
	for i := range count {
		if err := s.Add(Job{Name: fmt.Sprintf("target-%03d", i), Interval: 5 * time.Millisecond, Timeout: time.Second, Collect: func(ctx context.Context) error {
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old; old = peak.Load() {
				if peak.CompareAndSwap(old, current) {
					break
				}
			}
			calls[i].Add(1)
			select {
			case <-time.After(time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := runScheduler(t, s)
	waitFor(t, func() bool {
		for i := range calls {
			if calls[i].Load() < 2 {
				return false
			}
		}
		return true
	})
	stopScheduler(t, cancel, done)
	if peak.Load() > 2 || active.Load() != 0 {
		t.Fatalf("worker bound/cleanup violated: peak=%d active=%d", peak.Load(), active.Load())
	}
	for _, state := range s.Stats() {
		if state.Running {
			t.Fatal("shutdown retained running job")
		}
	}
}

func TestPendingTargetsRemovedDuringSaturationNeverRun(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	s := New(1, nil)
	started := make(chan struct{})
	if err := s.Add(Job{Name: "occupied", Interval: time.Hour, Timeout: time.Hour, Collect: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	names := []string{"occupied"}
	for i := range 50 {
		name := fmt.Sprintf("pending-%02d", i)
		names = append(names, name)
		if err := s.Add(Job{Name: name, Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error {
			calls.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	<-started
	s.mu.Lock()
	pending := 0
	for _, job := range s.jobs {
		if job.pending {
			pending++
		}
	}
	s.mu.Unlock()
	if pending == 0 {
		t.Fatal("fixture did not saturate the scheduler")
	}
	change, err := s.PrepareChanges(names, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer change.Close()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err = change.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err = change.Commit(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(s.Stats()) != 0 {
		t.Fatalf("removed pending targets executed: calls=%d states=%d", calls.Load(), len(s.Stats()))
	}
}

func TestSlowHardwareDoesNotStarveLongPeriodHealthyTargets(t *testing.T) {
	s := New(2, nil)
	started := make(chan struct{})
	if err := s.Add(Job{Name: "slow-bmc", Interval: time.Hour, Timeout: time.Hour, Collect: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	var serviced atomic.Int32
	for i := range 50 {
		if err := s.Add(Job{Name: fmt.Sprintf("healthy-%02d", i), Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error {
			serviced.Add(1)
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	<-started
	waitFor(t, func() bool { return serviced.Load() == 50 })
}
