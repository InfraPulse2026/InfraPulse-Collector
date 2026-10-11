package managedconfig

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func TestRapidRevisionsReportSerializedTruthfulStatuses(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var statuses []model.ConfigApplyResult
	report := func(r model.ConfigApplyResult) {
		mu.Lock()
		statuses = append(statuses, r)
		mu.Unlock()
	}
	var callers sync.WaitGroup
	for revision := uint64(10); revision <= 13; revision++ {
		s := snapshot(t, revision, target("snmp", "managed"))
		callers.Go(func() { h.manager.Apply(context.Background(), s, true, "", report) })
	}
	callers.Wait()
	if h.manager.Applied().Revision != 13 || len(h.registry.Targets()) != 1 || len(h.created) != 1 {
		t.Fatalf("rapid revisions retained conflicting state/workers: applied=%+v targets=%d adapters=%d", h.manager.Applied(), len(h.registry.Targets()), len(h.created))
	}
	var active uint64
	for index := 0; index < len(statuses); index += 3 {
		if index+2 >= len(statuses) {
			t.Fatal("partial status sequence")
		}
		received, validating, terminal := statuses[index], statuses[index+1], statuses[index+2]
		if received.Status != model.ConfigStatusReceived || validating.Status != model.ConfigStatusValidating || received.Revision != validating.Revision || received.Revision != terminal.Revision {
			t.Fatalf("parallel/conflicting status sequence: %+v", statuses)
		}
		if received.AppliedRevision != active || validating.AppliedRevision != active {
			t.Fatal("intermediate status claims an inactive applied revision")
		}
		if terminal.Status == model.ConfigStatusApplied {
			if terminal.Revision < active || terminal.AppliedRevision != terminal.Revision {
				t.Fatal("APPLIED status rolled back active state")
			}
			active = terminal.Revision
		} else if terminal.Status != model.ConfigStatusRejected || terminal.ErrorCode != "stale_revision" || terminal.AppliedRevision != active {
			t.Fatalf("wrong stale result: %+v", terminal)
		}
	}
	if active != 13 {
		t.Fatal("final status does not describe final active state")
	}
}

func TestManagedDiskFullPreservesActiveAndLastKnownGood(t *testing.T) {
	for _, mode := range []string{"before_publication", "after_publication", "persistent_failure"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			apply(t, h, snapshot(t, 1, target("snmp", "original")), model.ConfigStatusApplied)
			write := h.manager.store.write
			attempt := 0
			h.manager.store.write = func(raw []byte) error {
				attempt++
				if attempt == 1 || mode == "persistent_failure" {
					if mode == "after_publication" {
						if err := write(raw); err != nil {
							return err
						}
					}
					return syscall.ENOSPC
				}
				return write(raw)
			}
			r := apply(t, h, snapshot(t, 2, target("snmp", "replacement")), model.ConfigStatusFailed)
			if r.AppliedRevision != 1 || h.manager.Applied().Revision != 1 || len(h.registry.Targets()) != 1 || h.registry.Targets()[0].ID != "remote:original" || h.created[0].closed.Load() || !h.created[1].closed.Load() {
				t.Fatalf("disk full changed active targets or reported success: %+v", r)
			}
			state, err := h.manager.store.load(collectorID)
			if err != nil || state.Applied == nil || state.Applied.Revision != 1 {
				t.Fatalf("disk full destroyed previous last-known-good: %+v %v", state, err)
			}
			if mode == "persistent_failure" {
				if !h.manager.Snapshot().StorageDegraded || r.ErrorCode != "persistence_recovery_failed" {
					t.Fatal("persistent failure hidden")
				}
				if next := apply(t, h, snapshot(t, 3), model.ConfigStatusFailed); next.ErrorCode != "state_unavailable" {
					t.Fatal("degraded state accepted another revision")
				}
			} else if state.HighestRevision != 2 || h.manager.Snapshot().StorageDegraded {
				t.Fatal("recovered failure lost revision watermark or remained degraded")
			}
		})
	}
}

func TestCancellationDuringManagedPollDrainPreservesOriginal(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.manager.factory = func(d config.Device) (driver.Adapter, error) {
		adapter, err := h.factory(d)
		if err == nil && d.ID == "original" {
			return &cancelOnDrainAdapter{fakeAdapter: adapter.(*fakeAdapter), cancelApply: cancel}, nil
		}
		return adapter, err
	}
	apply(t, h, snapshot(t, 1, target("snmp", "original")), model.ConfigStatusApplied)
	old := h.created[0]
	old.entered = make(chan struct{}, 1)
	runCtx, stopRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.registry.Run(runCtx) }()
	defer func() {
		stopRun()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("shutdown leaked registry worker")
		}
	}()
	select {
	case <-old.entered:
	case <-time.After(time.Second):
		t.Fatal("original poll did not start")
	}
	replacement := snapshot(t, 2, target("snmp", "replacement"))
	// The original poll cancels the apply only after its own context is canceled
	// by the transition, so this exercises cancellation while draining a real job.
	r := h.manager.Apply(ctx, replacement, true, "", nil)
	if r.Status == model.ConfigStatusApplied || r.AppliedRevision != 1 || h.manager.Applied().Revision != 1 || len(h.registry.Targets()) != 1 || h.registry.Targets()[0].ID != "remote:original" || old.closed.Load() {
		t.Fatalf("cancelled apply replaced original runtime: %+v", r)
	}
	if r.ErrorCode != "apply_cancelled" && r.ErrorCode != "runtime_apply_failed" {
		t.Fatalf("unexpected cancellation category: %+v", r)
	}
	if errors.Is(ctx.Err(), context.Canceled) == false {
		t.Fatal("cancellation fixture not triggered")
	}
}

type cancelOnDrainAdapter struct {
	*fakeAdapter
	cancelApply context.CancelFunc
}

func (a *cancelOnDrainAdapter) Collect(ctx context.Context) (model.TelemetrySample, error) {
	select {
	case a.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	a.cancelApply()
	return model.TelemetrySample{}, ctx.Err()
}
