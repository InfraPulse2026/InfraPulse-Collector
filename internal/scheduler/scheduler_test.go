package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true")
		case <-ticker.C:
		}
	}
}

func runScheduler(t *testing.T, s *Scheduler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- s.Run(ctx) }()
	return cancel, finished
}

func stopScheduler(t *testing.T, cancel context.CancelFunc, finished <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler shutdown did not finish")
	}
}

func TestNoOverlapAndSkippedRuns(t *testing.T) {
	var running, overlap atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	s := New(4, nil)
	if err := s.Add(Job{Name: "blocked", Interval: 10 * time.Millisecond, Timeout: time.Second, Collect: func(ctx context.Context) error {
		if running.Add(1) != 1 {
			overlap.Add(1)
		}
		defer running.Add(-1)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}}); err != nil {
		t.Fatal(err)
	}
	cancel, finished := runScheduler(t, s)
	defer stopScheduler(t, cancel, finished)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("collector not started")
	}
	waitFor(t, func() bool { return s.Stats()[0].Skipped >= 3 })
	if overlap.Load() != 0 {
		t.Fatal("collector overlapped itself")
	}
	close(release)
	waitFor(t, func() bool { return s.Stats()[0].Runs >= 1 })
}

func TestTimeoutAndErrorIsolation(t *testing.T) {
	errorSeen := make(chan error, 10)
	s := New(2, func(_ string, err error) {
		select {
		case errorSeen <- err:
		default:
		}
	})
	if err := s.Add(Job{Name: "timeout", Interval: 100 * time.Millisecond, Timeout: 15 * time.Millisecond, Collect: func(ctx context.Context) error { <-ctx.Done(); return nil }}); err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Int32
	if err := s.Add(Job{Name: "healthy", Interval: 10 * time.Millisecond, Timeout: time.Second, Collect: func(context.Context) error { healthy.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	cancel, finished := runScheduler(t, s)
	defer stopScheduler(t, cancel, finished)
	select {
	case err := <-errorSeen:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wrong timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout was not reported")
	}
	waitFor(t, func() bool { return healthy.Load() >= 3 })
	if s.Stats()[0].LastError == "" {
		t.Fatal("collector error missing from stats")
	}
}

func TestFixedWorkerLimit(t *testing.T) {
	s := New(2, nil)
	var running, maximum atomic.Int32
	for i := range 10 {
		if err := s.Add(Job{Name: fmt.Sprintf("job%d", i), Interval: 5 * time.Millisecond, Timeout: time.Second, Collect: func(ctx context.Context) error {
			current := running.Add(1)
			defer running.Add(-1)
			for old := maximum.Load(); current > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, current) {
					break
				}
			}
			<-ctx.Done()
			return ctx.Err()
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cancel, finished := runScheduler(t, s)
	waitFor(t, func() bool { return maximum.Load() == 2 })
	stopScheduler(t, cancel, finished)
	if maximum.Load() > 2 || running.Load() != 0 {
		t.Fatalf("workers exceeded bound or leaked: max=%d running=%d", maximum.Load(), running.Load())
	}
}

func TestJobValidation(t *testing.T) {
	s := New(1, nil)
	if err := s.Add(Job{}); err == nil {
		t.Fatal("accepted invalid job")
	}
	job := Job{Name: "valid", Interval: time.Second, Timeout: time.Second, Collect: func(context.Context) error { return nil }}
	if err := s.Add(job); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(job); err == nil {
		t.Fatal("accepted duplicate name")
	}
	cancel, finished := runScheduler(t, s)
	defer stopScheduler(t, cancel, finished)
	waitFor(t, func() bool { return s.Stats()[0].Runs > 0 })
	job.Name = "late"
	if err := s.Add(job); err == nil {
		t.Fatal("accepted job after start")
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("accepted concurrent Run")
	}
}

func TestErrorSummaryHasRetainedSizeLimit(t *testing.T) {
	s := New(1, nil)
	if err := s.Add(Job{Name: "large-error", Interval: time.Second, Timeout: time.Second, Collect: func(context.Context) error {
		return errors.New("response-error:" + strings.Repeat("x", 2<<20))
	}}); err != nil {
		t.Fatal(err)
	}
	cancel, finished := runScheduler(t, s)
	defer stopScheduler(t, cancel, finished)
	waitFor(t, func() bool { return s.Stats()[0].Runs > 0 })
	message := s.Stats()[0].LastError
	if len(message) > 1024 || !strings.HasPrefix(message, "response-error:") {
		t.Fatalf("invalid bounded error summary: length=%d", len(message))
	}
}
