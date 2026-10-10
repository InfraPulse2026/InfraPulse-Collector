package managedconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestMalformedConfigLimitsShutdownAndConflictAfterFailure(t *testing.T) {
	h := newHarness(t)
	s := snapshot(t, 1, target("snmp", "one"))
	s.Targets[0].IPMI = &model.ManagedIPMIOptions{}
	if _, err := Digest(s); err == nil {
		t.Fatal("mismatched options accepted")
	}
	if apply(t, h, s, model.ConfigStatusRejected).ErrorCode != "invalid_protocol_options" {
		t.Fatal("wrong mismatch error")
	}
	s = snapshot(t, 2, target("sip", "one"))
	s.Targets[0].IPMI = &model.ManagedIPMIOptions{}
	if _, err := Digest(s); err == nil {
		t.Fatal("SIP mismatch accepted")
	}
	s = snapshot(t, 3, target("snmp", "one"))
	h.manager.cfg.RemoteManagement.MaxConfigBytes = 1024
	s.Targets[0].Name = strings.Repeat("x", 2048)
	s.ConfigurationDigest, _ = Digest(s)
	if apply(t, h, s, model.ConfigStatusRejected).ErrorCode != "payload_limit" {
		t.Fatal("oversized config accepted")
	}
	s = snapshot(t, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := h.manager.Apply(ctx, s, true, "", nil); r.Status != model.ConfigStatusFailed || r.ErrorCode != "apply_cancelled" {
		t.Fatal(r)
	}
	if len(h.registry.Targets()) != 0 {
		t.Fatal("shutdown applied snapshot")
	}
	h.manager.cfg.RemoteManagement.MaxConfigBytes = 0
	s = snapshot(t, 5, target("snmp", "one"))
	apply(t, h, s, model.ConfigStatusApplied)
	original := h.manager.store.write
	attempts := 0
	h.manager.store.write = func(data []byte) error {
		attempts++
		if attempts == 1 {
			return errors.New("failure")
		}
		return original(data)
	}
	failed := snapshot(t, 6, target("snmp", "two"))
	apply(t, h, failed, model.ConfigStatusFailed)
	apply(t, h, failed, model.ConfigStatusFailed)
	if apply(t, h, snapshot(t, 6, target("snmp", "different")), model.ConfigStatusRejected).ErrorCode != "revision_conflict" {
		t.Fatal("failed revision silently overwritten")
	}
	if r := h.manager.Apply(context.Background(), snapshot(t, 7), true, "invalid_wire", nil); r.Status != model.ConfigStatusRejected || r.ErrorCode != "invalid_snapshot" {
		t.Fatal(r)
	}
}

func TestRestoredRuntimeStatusIsTruthful(t *testing.T) {
	h := newHarness(t)
	apply(t, h, snapshot(t, 1, target("snmp", "one")), model.ConfigStatusApplied)
	c := h.cfg
	c.RemoteManagement.Enabled = false
	m := New(c, collectorID, sessionID, h.registry, h.factory, fixtureProvider{})
	if m.Restore(context.Background()) != nil {
		t.Fatal("disabled restore failed")
	}
	if r := m.Result(); r.Status == model.ConfigStatusApplied || r.AppliedRevision != 0 {
		t.Fatal("inactive restored state reported applied", r)
	}
	c = h.cfg
	c.RemoteManagement.AllowedProtocols = []string{"sip"}
	m = New(c, collectorID, sessionID, h.registry, h.factory, fixtureProvider{})
	if m.Restore(context.Background()) == nil {
		t.Fatal("policy bypass")
	}
	if r := m.Result(); r.Status != model.ConfigStatusFailed || r.AppliedRevision != 0 {
		t.Fatal("denied restoration reported applied", r)
	}
	if r := m.Apply(context.Background(), snapshot(t, 1, target("snmp", "one")), true, "", nil); r.Status != model.ConfigStatusFailed || r.AppliedRevision != 0 {
		t.Fatal("replayed stored success reported inactive targets applied", r)
	}
}

func TestStateChecksumSchemaAndSize(t *testing.T) {
	h := newHarness(t)
	apply(t, h, snapshot(t, 1), model.ConfigStatusApplied)
	raw, e := os.ReadFile(h.manager.store.path)
	if e != nil {
		t.Fatal(e)
	}
	var env diskEnvelope
	if json.Unmarshal(raw, &env) != nil {
		t.Fatal("invalid generated disk envelope")
	}
	env.Data = json.RawMessage(`{"collector_id":"tampered"}`)
	bad, _ := json.Marshal(env)
	if os.WriteFile(h.manager.store.path, bad, 0600) != nil {
		t.Fatal("write fixture")
	}
	if _, e = h.manager.store.load(collectorID); e == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if os.WriteFile(h.manager.store.path, append(raw, []byte("{}")...), 0600) != nil {
		t.Fatal("write fixture")
	}
	if _, e = h.manager.store.load(collectorID); e == nil {
		t.Fatal("extra document accepted")
	}
	h.manager.store.limit = 10
	if _, e = h.manager.store.load(collectorID); e == nil {
		t.Fatal("oversized state accepted")
	}
}

func TestStateRejectsSymlinkAndInsecurePermissions(t *testing.T) {
	h := newHarness(t)
	apply(t, h, snapshot(t, 1), model.ConfigStatusApplied)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(h.manager.store.path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := h.manager.store.load(collectorID); err == nil {
			t.Fatal("world-readable state accepted")
		}
		if err := os.Chmod(h.manager.store.path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	targetPath := filepath.Join(h.manager.store.dir, "symlink-target.json")
	if err := os.Rename(h.manager.store.path, targetPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, h.manager.store.path); err != nil {
		if runtime.GOOS == "windows" && (os.IsPermission(err) || errors.Is(err, syscall.Errno(1314))) {
			t.Skip("Windows account lacks symlink privilege; Linux suite covers this check")
		}
		t.Fatal(err)
	}
	if _, err := h.manager.store.load(collectorID); err == nil {
		t.Fatal("symlink state accepted")
	}
	if err := h.manager.store.save(h.manager.state); err == nil {
		t.Fatal("symlink state overwritten")
	}
}
