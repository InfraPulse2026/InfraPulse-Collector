package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDynamicEmptySchedulerApplyWaitsForCommit(t *testing.T) {
	s := New(2, nil)
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	var calls atomic.Int32
	tx, err := s.PrepareChanges(nil, []Job{{Name: "added", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error { calls.Add(1); return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	if err = tx.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(s.Stats()) != 1 || s.Stats()[0].Running {
		t.Fatal("prepared job ran before persistence commit")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	remove, err := s.PrepareChanges([]string{"added"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = remove.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = remove.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(s.Stats()) != 0 {
		t.Fatal("removed job remains scheduled")
	}
}

func TestDynamicReplacementCancelsRunningJobWithoutOverlap(t *testing.T) {
	s := New(2, nil)
	started := make(chan struct{})
	var active, newCalls, overlap atomic.Int32
	if err := s.Add(Job{Name: "target", Interval: time.Hour, Timeout: time.Hour, Collect: func(ctx context.Context) error {
		active.Add(1)
		defer active.Add(-1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	<-started
	tx, err := s.PrepareChanges([]string{"target"}, []Job{{Name: "target", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error {
		if active.Load() != 0 {
			overlap.Add(1)
		}
		newCalls.Add(1)
		return nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 || newCalls.Load() != 0 {
		t.Fatal("old operation not drained or replacement ran early")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return newCalls.Load() == 1 })
	if overlap.Load() != 0 {
		t.Fatal("replacement overlapped the old job")
	}
}

func TestDynamicRollbackPreservesUnchangedJobAndOriginalJob(t *testing.T) {
	s := New(2, nil)
	var oldCalls, untouched atomic.Int32
	for _, job := range []Job{{Name: "original", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error { oldCalls.Add(1); return nil }}, {Name: "static", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error { untouched.Add(1); return nil }}} {
		if err := s.Add(job); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	waitFor(t, func() bool { return oldCalls.Load() == 1 && untouched.Load() == 1 })
	tx, err := s.PrepareChanges([]string{"original"}, []Job{{Name: "replacement", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error { t.Error("uncommitted replacement ran"); return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return oldCalls.Load() == 2 })
	if untouched.Load() != 1 || s.Stats()[1].Runs != 1 {
		t.Fatal("unchanged job was restarted")
	}
}

func TestDynamicCancelledDrainResumesOnlyAfterOperationEnds(t *testing.T) {
	s := New(2, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls, active, overlap atomic.Int32
	if err := s.Add(Job{Name: "slow-cancel", Interval: time.Millisecond, Timeout: time.Second, Collect: func(ctx context.Context) error {
		if active.Add(1) != 1 {
			overlap.Add(1)
		}
		defer active.Add(-1)
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			<-release
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	<-started
	tx, err := s.PrepareChanges([]string{"slow-cancel"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err = tx.Apply(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrong drain failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("old job restarted before canceled operation drained")
	}
	close(release)
	waitFor(t, func() bool { return calls.Load() > 1 })
	if overlap.Load() != 0 {
		t.Fatal("rollback scheduled duplicate operation")
	}
}

func TestDynamicQueuedJobCancellation(t *testing.T) {
	s := New(1, nil)
	started := make(chan struct{})
	if err := s.Add(Job{Name: "occupied", Interval: time.Hour, Timeout: time.Hour, Collect: func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() }}); err != nil {
		t.Fatal(err)
	}
	cancel, done := runScheduler(t, s)
	defer stopScheduler(t, cancel, done)
	<-started
	var calls atomic.Int32
	add, err := s.PrepareChanges(nil, []Job{{Name: "queued", Interval: time.Hour, Timeout: time.Second, Collect: func(context.Context) error { calls.Add(1); return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if err = add.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = add.Commit(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.Stats()[1].Running })
	remove, err := s.PrepareChanges([]string{"occupied", "queued"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = remove.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = remove.Commit(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("canceled queued operation was executed")
	}
}
