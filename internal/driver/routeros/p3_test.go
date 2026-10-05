package routeros

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"InfraPulseAgent/internal/commands"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func p3Command(id string) model.Command {
	return model.Command{ID: id, TargetDeviceID: "router-stable", RequestedAt: time.Now().UTC(), Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "test-port", Operation: "disable"}}
}
func p3Store(t *testing.T) (*commands.Store, string) {
	t.Helper()
	dir := snapshotTempDir(t)
	s, err := commands.OpenStore(dir, commands.DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}
func p3Execute(t *testing.T, s *commands.Store, d *Driver, c model.Command, policy commands.Policy) model.CommandResult {
	t.Helper()
	if _, duplicate, err := s.Begin(c, time.Now()); err != nil || duplicate {
		t.Fatal(duplicate, err)
	}
	target, err := d.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := commands.ExecuteDurable(context.Background(), c, target, d, policy, time.Now(), s)
	e := model.Envelope{MessageID: "result-" + c.ID, AgentID: "agent", SessionID: "session", SentAt: time.Now().UTC(), CommandResult: &r}
	if err := s.Complete(c, e, time.Now()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHTTPSConfigurationSnapshotRestartAndVerifiedRollback(t *testing.T) {
	f := newFixture(t)
	f.bodies["interface"] = "dynamic"
	d := setup(t, f, true, driver.Options{})
	s, dir := p3Store(t)
	first := p3Execute(t, s, d, p3Command("disable"), commands.Policy{Enabled: true})
	if first.Status != "success" || first.SnapshotID == "" || !first.Verification.Verified {
		t.Fatal(first)
	}
	snap, err := s.Snapshot(first.SnapshotID, time.Now())
	if err != nil || snap.CanonicalID != "*2" || !snap.PreviousAdminUp || snap.ExpectedAdminUp || snap.Source != "routeros.https.interface" {
		t.Fatal(snap, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = commands.OpenStore(dir, commands.DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rollback := p3Command("rollback")
	rollback.Interface = nil
	rollback.Rollback = &model.RollbackAction{SnapshotID: first.SnapshotID}
	restored := p3Execute(t, s, d, rollback, commands.Policy{Enabled: true})
	if restored.Status != "success" || restored.Verification == nil || !restored.Verification.Verified || !restored.Verification.ExpectedAdminUp {
		t.Fatal(restored)
	}
	f.mu.Lock()
	writes := f.patches
	up := f.interfaces[1]["disabled"] == "false"
	f.mu.Unlock()
	if writes != 2 || !up {
		t.Fatal(writes, up)
	}
	if _, err := s.Snapshot(first.SnapshotID, time.Now()); !errors.Is(err, commands.ErrSnapshotConflict) {
		t.Fatal(err)
	}
	if cached, duplicate, err := s.Begin(rollback, time.Now()); err != nil || !duplicate || cached.Result.Status != "success" {
		t.Fatal(cached, duplicate, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches != 2 {
		t.Fatal("duplicate rollback executed")
	}
}

func TestHTTPSRollbackProtectionOwnershipStaleResourceAndExpiry(t *testing.T) {
	for _, mode := range []string{"protected", "wrong_target", "wrong_resource", "stale_state", "version", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.bodies["interface"] = "dynamic"
			d := setup(t, f, true, driver.Options{})
			s, _ := p3Store(t)
			first := p3Execute(t, s, d, p3Command("disable"), commands.Policy{Enabled: true})
			rollback := p3Command("rollback")
			rollback.Interface = nil
			rollback.Rollback = &model.RollbackAction{SnapshotID: first.SnapshotID}
			policy := commands.Policy{Enabled: true}
			if mode == "protected" {
				policy.ProtectedInterfaces = []string{"02:00:00:00:00:02"}
			}
			if mode == "wrong_target" {
				rollback.TargetDeviceID = "other"
			}
			if mode == "expired" {
				rollback.Rollback.SnapshotID = "expired-or-unknown"
			}
			f.mu.Lock()
			if mode == "wrong_resource" {
				f.interfaces[1]["name"] = "different-port"
			}
			if mode == "stale_state" {
				f.interfaces[1]["disabled"] = "false"
			}
			if mode == "version" {
				f.bodies["system/resource"] = `{"uptime":"1h","version":"changed-version","architecture-name":"arm","board-name":"schema"}`
			}
			f.mu.Unlock()
			result := p3Execute(t, s, d, rollback, policy)
			if result.Status != "rejected" {
				t.Fatal(result)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.patches != 1 {
				t.Fatal("unsafe rollback write", f.patches)
			}
		})
	}
}

func TestHTTPSRollbackRejectedUncertainWriteAndVerification(t *testing.T) {
	for _, mode := range []string{"denied-write", "uncertain", "malformed-write"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.bodies["interface"] = "dynamic"
			d := setup(t, f, true, driver.Options{})
			s, _ := p3Store(t)
			first := p3Execute(t, s, d, p3Command("disable"), commands.Policy{Enabled: true})
			f.mu.Lock()
			f.mode = mode
			f.patched = false
			f.mu.Unlock()
			rollback := p3Command("rollback")
			rollback.Interface = nil
			rollback.Rollback = &model.RollbackAction{SnapshotID: first.SnapshotID}
			result := p3Execute(t, s, d, rollback, commands.Policy{Enabled: true})
			if mode == "uncertain" && (!result.Uncertain || result.ErrorCode != "mutation_uncertain") {
				t.Fatal(result)
			}
			if mode == "denied-write" && result.Status == "success" {
				t.Fatal(result)
			}
			if mode == "malformed-write" && (result.Status != "success" || result.Verification == nil || !result.Verification.Verified) {
				t.Fatal(result)
			}
		})
	}
}

func TestHTTPSSnapshotStoreFailureAndStateChangePreventPATCH(t *testing.T) {
	f := newFixture(t)
	f.bodies["interface"] = "dynamic"
	d := setup(t, f, true, driver.Options{})
	s, dir := p3Store(t)
	c := p3Command("snapshot-failure")
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".safety-state.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	target, err := d.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := commands.ExecuteDurable(context.Background(), c, target, d, commands.Policy{Enabled: true}, time.Now(), s)
	if result.ErrorCode != "storage_unavailable" {
		t.Fatal(result)
	}
	f.mu.Lock()
	writes := f.patches
	f.mu.Unlock()
	if writes != 0 {
		t.Fatal(writes)
	}
	resolved, err := d.ResolveInterface(context.Background(), "test-port")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.interfaces[1]["disabled"] = "true"
	f.mu.Unlock()
	if _, err := d.SetInterface(context.Background(), resolved, false); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches != 0 {
		t.Fatal(f.patches)
	}
}
