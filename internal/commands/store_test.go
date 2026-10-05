package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func safetyStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := commandTempDir(t)
	s, err := OpenStore(dir, DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}
func safetyTarget() model.Device {
	return model.Device{ID: "router", Driver: "routeros-rest", SoftwareVersion: "7.20"}
}
func safetyResolved(up bool) driver.ResolvedInterface {
	return driver.ResolvedInterface{Interface: model.Interface{ID: "*2", Name: "ether2", MAC: "02:00:00:00:00:02", AdminUp: up, Metadata: &model.InterfaceMetadata{AdminStatus: true}}, Aliases: []string{"*2", "ether2", "02:00:00:00:00:02"}}
}
func safetyComplete(t *testing.T, s *Store, c model.Command, result model.CommandResult) model.Envelope {
	t.Helper()
	result.CommandID = c.ID
	if result.StartedAt.IsZero() {
		result.StartedAt = time.Now().UTC()
	}
	result.FinishedAt = time.Now().UTC()
	e := model.Envelope{MessageID: "message-" + c.ID, AgentID: "agent", SessionID: "original-session", SentAt: time.Now().UTC(), CommandResult: &result}
	if err := s.Complete(c, e, time.Now()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestJournalRestartCrashBoundaries(t *testing.T) {
	for _, phase := range []string{"reserved", "executing", "verified", "result_persisted", "acknowledged"} {
		t.Run(phase, func(t *testing.T) {
			s, dir := safetyStore(t)
			c := command()
			if _, duplicate, err := s.Begin(c, time.Now()); err != nil || duplicate {
				t.Fatal(duplicate, err)
			}
			if phase != "reserved" {
				id, err := s.Executing(c, safetyTarget(), safetyResolved(true), false, time.Now())
				if err != nil || id == "" {
					t.Fatal(id, err)
				}
			}
			// Verification alone is not durable result completion: recovery must
			// conservatively mark the externally executed command uncertain.
			if phase == "result_persisted" || phase == "acknowledged" {
				safetyComplete(t, s, c, model.CommandResult{Status: "success", Changed: true, Verification: &model.InterfaceVerification{Verified: true}})
			}
			if phase == "acknowledged" {
				if err := s.Acknowledge("message-"+c.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := OpenStore(dir, DefaultStoreOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			r, duplicate, err := restarted.Begin(c, time.Now())
			if err != nil || !duplicate || r.Result == nil {
				t.Fatal(r, duplicate, err)
			}
			if phase == "reserved" && (r.Result.Uncertain || r.State != "rejected") {
				t.Fatal(r)
			}
			if (phase == "executing" || phase == "verified") && (!r.Result.Uncertain || r.State != "uncertain") {
				t.Fatal(r)
			}
			if phase == "result_persisted" && (r.Result.Status != "success" || r.Envelope.MessageID != "message-"+c.ID) {
				t.Fatal(r)
			}
			pending := restarted.Pending()
			if phase == "acknowledged" {
				if len(pending) != 0 {
					t.Fatal(pending)
				}
			} else if len(pending) != 1 {
				t.Fatal(pending)
			}
			if err := restarted.Close(); err != nil {
				t.Fatal(err)
			}
			again, err := OpenStore(dir, DefaultStoreOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = again.Close() })
			if phase != "acknowledged" && again.Pending()[0].MessageID != pending[0].MessageID {
				t.Fatal("restart replaced replay ID")
			}
		})
	}
}

func TestJournalACKDedupConflictRetentionAndBoundedReservation(t *testing.T) {
	opts := DefaultStoreOptions()
	opts.JournalMaxEntries = 2
	opts.JournalRetention = time.Minute
	s, err := OpenStore(commandTempDir(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c := command()
	c.IdempotencyKey = "once"
	if _, _, err = s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	safetyComplete(t, s, c, model.CommandResult{Status: "success"})
	copy := c
	copy.ID = "same-operation-new-id"
	if r, dup, e := s.Begin(copy, time.Now()); e != nil || !dup || r.Result == nil {
		t.Fatal(r, dup, e)
	}
	copy.Interface = &model.InterfaceAction{InterfaceID: "ether9", Operation: "disable"}
	if _, _, e := s.Begin(copy, time.Now()); e == nil {
		t.Fatal("conflicting duplicate admitted")
	}
	second := command()
	second.ID = "second"
	if _, _, e := s.Begin(second, time.Now()); e != nil {
		t.Fatal(e)
	}
	third := command()
	third.ID = "third"
	if _, _, e := s.Begin(third, time.Now()); !errors.Is(e, ErrStoreFull) {
		t.Fatal(e)
	}
	if err := s.Acknowledge("unknown", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge("message-"+c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge("message-"+c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, e := s.Begin(third, time.Now().Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if s.Health().JournalEntries != 2 {
		t.Fatal(s.Health())
	}
}

func TestJournalCorruptionNeverDeletesPersistedState(t *testing.T) {
	for _, mode := range []string{"truncate", "checksum", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			s, dir := safetyStore(t)
			if _, _, err := s.Begin(command(), time.Now()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "safety-state.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "truncate":
				data = data[:len(data)/2]
			case "checksum":
				data = []byte(strings.Replace(string(data), "reserved", "executing", 1))
			case "metadata":
				var disk diskStore
				if err := json.Unmarshal(data, &disk); err != nil {
					t.Fatal(err)
				}
				disk.Version = 99
				data, _ = json.Marshal(disk)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			broken, err := OpenStore(dir, DefaultStoreOptions())
			t.Cleanup(func() { _ = broken.Close() })
			if !errors.Is(err, ErrStoreCorrupt) || broken.Health().Available {
				t.Fatal(err, broken.Health())
			}
			if _, _, err = broken.Begin(command(), time.Now()); !errors.Is(err, ErrStorageUnavailable) {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Fatal("corruption silently reset", err)
			}
		})
	}
}

func TestStateStorageFailurePreventsExecution(t *testing.T) {
	s, dir := safetyStore(t)
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Cross-platform permission/disk failure fixture: an unavailable staging
	// path behaves like a denied/full atomic state publication.
	if err := os.Mkdir(filepath.Join(dir, ".safety-state.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	f := &persistentFixture{current: safetyResolved(true), supported: true}
	r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
	if r.Status != "rejected" || r.ErrorCode != "storage_unavailable" || f.writes != 0 {
		t.Fatal(r, f.writes)
	}
}

func TestFilesystemErrorFixturesPreventMutationAndPreserveRecoveryState(t *testing.T) {
	for _, filesystemErr := range []error{syscall.ENOSPC, syscall.EROFS, os.ErrPermission} {
		t.Run(filesystemErr.Error(), func(t *testing.T) {
			s, dir := safetyStore(t)
			c := command()
			if _, _, err := s.Begin(c, time.Now()); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "safety-state.json"))
			if err != nil {
				t.Fatal(err)
			}
			s.writeState = func([]byte) error { return &os.PathError{Op: "write", Path: "fixture", Err: filesystemErr} }
			f := &persistentFixture{current: safetyResolved(true), supported: true}
			r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
			if r.ErrorCode != "storage_unavailable" || f.writes != 0 || s.Health().Available {
				t.Fatal(r, f.writes, s.Health())
			}
			after, err := os.ReadFile(filepath.Join(dir, "safety-state.json"))
			if err != nil || string(after) != string(before) {
				t.Fatal("storage error replaced valid state", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(dir, DefaultStoreOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if pending := reopened.Pending(); len(pending) != 1 || pending[0].CommandResult.Uncertain {
				t.Fatal(pending)
			}
		})
	}
}

type persistentFixture struct {
	current   driver.ResolvedInterface
	supported bool
	writes    int
	err       error
	uncertain bool
}

func (f *persistentFixture) Probe(context.Context) (model.Device, error) { return safetyTarget(), nil }
func (f *persistentFixture) Inventory(context.Context) (model.Device, error) {
	return safetyTarget(), nil
}
func (f *persistentFixture) Collect(context.Context) (model.TelemetrySample, error) {
	return model.TelemetrySample{}, nil
}
func (f *persistentFixture) Topology(context.Context) (model.TopologyUpdate, error) {
	return model.TopologyUpdate{}, nil
}
func (f *persistentFixture) Close() error { return nil }
func (f *persistentFixture) Capabilities() []model.Capability {
	return []model.Capability{{Name: "interfaces.disable", Supported: f.supported}, {Name: "interfaces.enable", Supported: f.supported}, {Name: "resource.rollback", Supported: f.supported}}
}
func (f *persistentFixture) ResolveInterface(context.Context, string) (driver.ResolvedInterface, error) {
	return f.current, nil
}
func (f *persistentFixture) SetInterface(_ context.Context, _ driver.ResolvedInterface, expected bool) (model.InterfaceVerification, error) {
	f.writes++
	v := model.InterfaceVerification{InterfaceID: f.current.Interface.ID, Name: f.current.Interface.Name, ExpectedAdminUp: expected, Verified: f.err == nil && !f.uncertain, Changed: f.current.Interface.AdminUp != expected, Uncertain: f.uncertain}
	if v.Verified {
		f.current.Interface.AdminUp = expected
		v.ObservedAdminUp = &expected
	}
	return v, f.err
}

func TestSnapshotRollbackTypedProtectionConflictAndRestart(t *testing.T) {
	s, dir := safetyStore(t)
	f := &persistentFixture{current: safetyResolved(true), supported: true}
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
	if r.Status != "success" || r.SnapshotID == "" {
		t.Fatal(r)
	}
	safetyComplete(t, s, c, r)
	var err error
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dir, DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	snap, err := s.Snapshot(r.SnapshotID, time.Now())
	if err != nil || !snap.PreviousAdminUp || snap.ExpectedAdminUp {
		t.Fatal(snap, err)
	}
	rollback := command()
	rollback.ID = "rollback"
	rollback.Interface = nil
	rollback.Rollback = &model.RollbackAction{SnapshotID: r.SnapshotID}
	if _, _, err = s.Begin(rollback, time.Now()); err != nil {
		t.Fatal(err)
	}
	protected := ExecuteDurable(context.Background(), rollback, safetyTarget(), f, Policy{Enabled: true, ProtectedInterfaces: []string{"ether2"}}, time.Now(), s)
	if protected.ErrorCode != "protected_resource" || f.writes != 1 {
		t.Fatal(protected, f.writes)
	}
	// The rejected command did not cross the execution boundary, so this unit
	// fixture can exercise the same reserved transaction with a changed policy.
	restored := ExecuteDurable(context.Background(), rollback, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
	if restored.Status != "success" || !f.current.Interface.AdminUp || f.writes != 2 {
		t.Fatal(restored, f.writes)
	}
	safetyComplete(t, s, rollback, restored)
	if _, err = s.Snapshot(r.SnapshotID, time.Now()); !errors.Is(err, ErrSnapshotConflict) {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(restored.SnapshotID, time.Now().Add(2*24*time.Hour)); !errors.Is(err, ErrSnapshotExpired) {
		t.Fatal(err)
	}
	if record, dup, e := s.Begin(rollback, time.Now()); e != nil || !dup || record.Result.Status != "success" || f.writes != 2 {
		t.Fatal(record, dup, e)
	}
}

func TestSnapshotOwnershipCompatibilityABAAndReservationFailure(t *testing.T) {
	for _, mode := range []string{"target", "identity", "state", "version", "newer", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := safetyStore(t)
			f := &persistentFixture{current: safetyResolved(true), supported: true}
			c := command()
			if _, _, e := s.Begin(c, time.Now()); e != nil {
				t.Fatal(e)
			}
			r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
			safetyComplete(t, s, c, r)
			target := safetyTarget()
			if mode == "target" {
				target.ID = "other"
			}
			if mode == "identity" {
				f.current.Interface.MAC = "02:00:00:00:00:99"
			}
			if mode == "state" {
				f.current.Interface.AdminUp = true
			}
			if mode == "version" {
				target.SoftwareVersion = "7.21"
			}
			if mode == "newer" {
				n := command()
				n.ID = "newer"
				if _, _, e := s.Begin(n, time.Now()); e != nil {
					t.Fatal(e)
				}
				if _, e := s.Executing(n, safetyTarget(), f.current, false, time.Now()); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "capacity" {
				s.opts.SnapshotMaxEntries = 1
			}
			rollback := command()
			rollback.ID = "rollback"
			rollback.TargetDeviceID = target.ID
			rollback.Interface = nil
			rollback.Rollback = &model.RollbackAction{SnapshotID: r.SnapshotID}
			if _, _, e := s.Begin(rollback, time.Now()); e != nil {
				t.Fatal(e)
			}
			before := f.writes
			result := ExecuteDurable(context.Background(), rollback, target, f, Policy{Enabled: true}, time.Now(), s)
			if result.Status != "rejected" || f.writes != before {
				t.Fatal(result, f.writes)
			}
		})
	}
}

func TestFailedAndUncertainSnapshotCannotRollback(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		s, _ := safetyStore(t)
		c := command()
		if _, _, e := s.Begin(c, time.Now()); e != nil {
			t.Fatal(e)
		}
		f := &persistentFixture{current: safetyResolved(true), supported: true, err: driver.ErrUnavailable, uncertain: uncertain}
		r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
		safetyComplete(t, s, c, r)
		if _, e := s.Snapshot(r.SnapshotID, time.Now()); !errors.Is(e, ErrSnapshotConflict) {
			t.Fatal(r, e)
		}
	}
}

func TestCommandStoreSingleWriterLockAndPartialStagingRecovery(t *testing.T) {
	s, dir := safetyStore(t)
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := OpenStore(dir, DefaultStoreOptions())
	if !errors.Is(err, ErrStorageUnavailable) || second.Health().Available {
		t.Fatal(err, second.Health())
	}
	_ = second.Close()
	if err := os.WriteFile(filepath.Join(dir, ".safety-state.tmp"), []byte(`{"version":1,"incomplete":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenStore(dir, DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	r, duplicate, err := restarted.Begin(c, time.Now())
	if err != nil || !duplicate || r.Result == nil || r.Result.ErrorCode != "execution_interrupted" {
		t.Fatal(r, duplicate, err)
	}
	info, err := os.Stat(filepath.Join(dir, "safety-state.json"))
	if err != nil || info.Size() > int64(DefaultStoreOptions().JournalMaxBytes+DefaultStoreOptions().SnapshotMaxBytes+64<<10) {
		t.Fatal(info, err)
	}
}

func TestResultCapacityReservedBeforeMutation(t *testing.T) {
	s, _ := safetyStore(t)
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Executing(c, safetyTarget(), safetyResolved(true), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(s.data.Journal)
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the logical bound at the already-persisted execution reservation.
	// A near-maximum result including both Journal.result and envelope payload
	// must fit without growing the admitted journal allocation.
	s.opts.JournalMaxBytes = len(before)
	safetyComplete(t, s, c, model.CommandResult{Status: "failed", ErrorCode: "fixture_error", ErrorMessage: strings.Repeat("x", 2900)})
	after, err := json.Marshal(s.data.Journal)
	if err != nil || len(after) > len(before) {
		t.Fatal(len(before), len(after), err)
	}
}

func TestOlderSnapshotCannotReviveAfterNewSnapshotRetentionChanges(t *testing.T) {
	s, _ := safetyStore(t)
	f := &persistentFixture{current: safetyResolved(true), supported: true}
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	r := ExecuteDurable(context.Background(), c, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
	safetyComplete(t, s, c, r)
	s.opts.SnapshotRetention = time.Minute
	c2 := command()
	c2.ID = "newer-short-retention"
	if _, _, err := s.Begin(c2, time.Now()); err != nil {
		t.Fatal(err)
	}
	r2 := ExecuteDurable(context.Background(), c2, safetyTarget(), f, Policy{Enabled: true}, time.Now(), s)
	safetyComplete(t, s, c2, r2)
	if err := s.Acknowledge("message-"+c2.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	c3 := command()
	c3.ID = "prune-newer"
	if _, _, err := s.Begin(c3, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(r.SnapshotID, time.Now().Add(2*time.Minute)); !errors.Is(err, ErrSnapshotConflict) {
		t.Fatal("older snapshot revived", err)
	}
}
