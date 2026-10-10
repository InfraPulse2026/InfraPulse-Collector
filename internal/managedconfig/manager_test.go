package managedconfig

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/remote"
	"InfraPulseAgent/internal/telephony"
	"InfraPulseAgent/internal/testutil"
)

const collectorID = "9801c72e-6308-40db-a5d8-584b5f636a5f"
const sessionID = "d0a36d21-211c-4777-9698-2c5bb25026dc"

type fixtureProvider struct{}

func (fixtureProvider) Get(ctx context.Context, ref string) (credentials.Credential, error) {
	if ctx.Err() != nil {
		return credentials.Credential{}, ctx.Err()
	}
	if ref == "missing" {
		return credentials.Credential{}, errors.New("password=do-not-reflect")
	}
	return credentials.Credential{Community: "fixture-secret", Username: "fixture", Password: "fixture-secret", AuthPassword: "fixture-auth-secret", PrivacyPassword: "fixture-privacy-secret"}, nil
}

type fakeAdapter struct {
	id      string
	closed  atomic.Bool
	calls   atomic.Int64
	active  *atomic.Int64
	block   atomic.Bool
	entered chan struct{}
}

type hardwareFake struct{ f *fakeAdapter }

func (a *hardwareFake) Capabilities() []model.Capability { return nil }
func (a *hardwareFake) Close() error                     { return a.f.Close() }
func (a *hardwareFake) Hardware(ctx context.Context) ([]model.TelemetrySample, error) {
	s, e := a.f.Collect(ctx)
	return []model.TelemetrySample{s}, e
}

type sipFake struct{ f *fakeAdapter }

func (a *sipFake) Capabilities() []model.Capability { return nil }
func (a *sipFake) Close() error                     { return a.f.Close() }
func (a *sipFake) CollectTelephony(ctx context.Context) (model.TelephonyUpdate, []model.Event, error) {
	_, e := a.f.Collect(ctx)
	return model.TelephonyUpdate{ObservedAt: time.Now().UTC(), Source: "simulated"}, nil, e
}

func (f *fakeAdapter) Capabilities() []model.Capability { return nil }
func (f *fakeAdapter) Close() error                     { f.closed.Store(true); return nil }
func (f *fakeAdapter) Collect(ctx context.Context) (model.TelemetrySample, error) {
	if f.closed.Load() {
		return model.TelemetrySample{}, errors.New("closed adapter polled")
	}
	if f.active != nil {
		if f.active.Add(1) > 1 {
			panic("overlapping target polls")
		}
		defer f.active.Add(-1)
	}
	f.calls.Add(1)
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block.Load() {
		<-ctx.Done()
		return model.TelemetrySample{}, ctx.Err()
	}
	return model.TelemetrySample{DeviceID: f.id, Source: "simulated", ObservedAt: time.Now().UTC()}, nil
}

type harness struct {
	cfg      config.Config
	registry *remote.Registry
	manager  *Manager
	created  []*fakeAdapter
	lock     sync.Mutex
	fail     atomic.Bool
	emitted  atomic.Int64
	active   atomic.Int64
}

func managementConfig(dir string) config.Config {
	c := config.Defaults()
	c.Agent.StateDir = dir
	c.Local.Enabled = false
	c.Security.Mode = "production"
	c.Security.CAFile = "ca"
	c.Security.CertFile = "cert"
	c.Security.KeyFile = "key"
	c.RemoteManagement = config.RemoteManagement{Enabled: true, AllowedCredentialRefs: []string{"read", "basic", "v3", "missing"}, MinPollingInterval: 10 * time.Millisecond, MinHardwareInterval: 10 * time.Millisecond, ApplyTimeout: time.Second}
	c.Credentials = map[string]credentials.Reference{"read": {CommunityEnv: "FIXTURE_READ"}, "missing": {CommunityEnv: "FIXTURE_MISSING"}, "basic": {UsernameEnv: "FIXTURE_USER", PasswordEnv: "FIXTURE_PASSWORD"}, "v3": {UsernameEnv: "FIXTURE_USER", AuthPasswordEnv: "FIXTURE_AUTH", PrivacyPasswordEnv: "FIXTURE_PRIVACY"}}
	return c
}
func newHarness(t testing.TB) *harness {
	t.Helper()
	h := &harness{cfg: managementConfig(testutil.TempDir(t))}
	var err error
	h.registry, err = remote.NewAdapters(h.cfg, h.factory, func(model.Envelope) error { h.emitted.Add(1); return nil }, remote.WithCollectorID(collectorID))
	if err != nil {
		t.Fatal(err)
	}
	h.manager = New(h.cfg, collectorID, sessionID, h.registry, h.factory, fixtureProvider{})
	t.Cleanup(func() { _ = h.registry.Close() })
	return h
}
func (h *harness) factory(d config.Device) (driver.Adapter, error) {
	if h.fail.Load() {
		return nil, errors.New("password=do-not-reflect constructor error")
	}
	f := &fakeAdapter{id: d.ID, active: &h.active}
	h.lock.Lock()
	h.created = append(h.created, f)
	h.lock.Unlock()
	if d.Driver == "redfish" || d.Driver == "ipmi" {
		return &hardwareFake{f}, nil
	}
	if d.Driver == "sip" {
		return &sipFake{f}, nil
	}
	return f, nil
}
func target(protocol, id string) model.ManagedTargetSpec {
	t := model.ManagedTargetSpec{ID: id, Protocol: protocol, Enabled: true, Endpoint: "127.0.0.1", CredentialRef: "read", Polling: model.TargetPolling{FastInterval: 20 * time.Millisecond, MediumInterval: time.Second, InventoryInterval: time.Second, TopologyInterval: time.Second, Timeout: 100 * time.Millisecond}}
	switch protocol {
	case "routeros":
		t.Endpoint = "https://127.0.0.1"
		t.CredentialRef = "basic"
	case "redfish", "ipmi":
		t.CredentialRef = "basic"
		t.Polling.FastInterval, t.Polling.TopologyInterval = 0, 0
		if protocol == "redfish" {
			t.Endpoint = "https://127.0.0.1"
		}
	case "sip":
		t.CredentialRef = ""
		t.Polling.MediumInterval, t.Polling.InventoryInterval, t.Polling.TopologyInterval = 0, 0, 0
	}
	return t
}
func snapshot(t testing.TB, revision uint64, targets ...model.ManagedTargetSpec) model.DesiredCollectorConfig {
	t.Helper()
	s := model.DesiredCollectorConfig{CollectorID: collectorID, SessionID: sessionID, Revision: revision, Targets: targets}
	var e error
	s.ConfigurationDigest, e = Digest(s)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func apply(t testing.TB, h *harness, s model.DesiredCollectorConfig, want string) model.ConfigApplyResult {
	t.Helper()
	r := h.manager.Apply(context.Background(), s, true, "", nil)
	if r.Status != want {
		t.Fatalf("revision %d: got %+v want %s", s.Revision, r, want)
	}
	return r
}

func TestDigestDeterminismDefaultsAndOwnership(t *testing.T) {
	a := snapshot(t, 1, target("snmp", "a"), target("sip", "b"))
	b := model.CloneDesiredCollectorConfig(a)
	b.Targets[0], b.Targets[1] = b.Targets[1], b.Targets[0]
	b.Revision = 99
	b.SessionID = "another"
	b.IssuedAt = time.Now().UTC()
	da, _ := Digest(a)
	db, _ := Digest(b)
	if da != db {
		t.Fatal("ordering/delivery affected digest")
	}
	n, err := Normalize(a)
	if err != nil {
		t.Fatal(err)
	}
	dn, _ := Digest(n)
	if dn != da {
		t.Fatal("defaults affected digest")
	}
	n.Targets[0].SNMP.Port = 162
	if a.Targets[0].SNMP != nil {
		t.Fatal("normalize aliases input")
	}
	dc, _ := Digest(n)
	if dc == da {
		t.Fatal("changed content not detected")
	}
	a.CollectorID = "another"
	dc, _ = Digest(a)
	if dc == da {
		t.Fatal("collector binding omitted")
	}
	zero := 0
	bad := target("ipmi", "bmc")
	bad.IPMI = &model.ManagedIPMIOptions{CipherSuite: &zero}
	s := snapshot(t, 1, bad)
	normalized, _ := Normalize(s)
	if *normalized.Targets[0].IPMI.CipherSuite != 0 {
		t.Fatal("explicit unsafe cipher defaulted")
	}
}

func TestRevisionIdempotencyConflictStaleAndOverflow(t *testing.T) {
	h := newHarness(t)
	s := snapshot(t, 10, target("snmp", "one"))
	apply(t, h, s, model.ConfigStatusApplied)
	apply(t, h, s, model.ConfigStatusApplied)
	if len(h.created) != 1 {
		t.Fatal("duplicate reinitialized target")
	}
	conflict := snapshot(t, 10, target("snmp", "two"))
	r := apply(t, h, conflict, model.ConfigStatusRejected)
	if r.ErrorCode != "revision_conflict" {
		t.Fatal(r)
	}
	apply(t, h, snapshot(t, 9), model.ConfigStatusRejected)
	bad := snapshot(t, 12, target("snmp", "x"))
	bad.Targets[0].CredentialRef = "forbidden"
	bad.ConfigurationDigest, _ = Digest(bad)
	apply(t, h, bad, model.ConfigStatusRejected)
	if apply(t, h, snapshot(t, 11), model.ConfigStatusRejected).ErrorCode != "stale_revision" {
		t.Fatal("out-of-order accepted")
	}
	apply(t, h, s, model.ConfigStatusApplied)
	apply(t, h, snapshot(t, 13, target("snmp", "two")), model.ConfigStatusApplied)
	if apply(t, h, s, model.ConfigStatusRejected).ErrorCode != "stale_revision" {
		t.Fatal("old applied revision rolled back")
	}
	apply(t, h, snapshot(t, math.MaxUint64), model.ConfigStatusApplied)
	if apply(t, h, snapshot(t, 0), model.ConfigStatusRejected).ErrorCode != "invalid_revision" {
		t.Fatal("overflow rollover accepted")
	}
}

func TestSecurityAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		edit       func(*harness, *model.DesiredCollectorConfig)
	}{
		{"disabled", "management_disabled", func(h *harness, s *model.DesiredCollectorConfig) { h.manager.cfg.RemoteManagement.Enabled = false }},
		{"identity", "identity_mismatch", func(h *harness, s *model.DesiredCollectorConfig) { s.CollectorID = "other" }},
		{"session", "identity_mismatch", func(h *harness, s *model.DesiredCollectorConfig) { s.SessionID = "other" }},
		{"protocol", "protocol_forbidden", func(h *harness, s *model.DesiredCollectorConfig) {
			h.manager.cfg.RemoteManagement.AllowedProtocols = []string{"sip"}
		}},
		{"credential", "credential_forbidden", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].CredentialRef = "unknown" }},
		{"missing credential", "credential_unavailable", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].CredentialRef = "missing" }},
		{"interval", "polling_interval_forbidden", func(h *harness, s *model.DesiredCollectorConfig) {
			s.Targets[0].Polling.FastInterval = time.Millisecond
		}},
		{"timeout", "invalid_polling_timeout", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].Polling.Timeout = -time.Second }},
		{"limit", "target_limit", func(h *harness, s *model.DesiredCollectorConfig) {
			h.manager.cfg.RemoteManagement.MaxManagedTargets = 1
			s.Targets = append(s.Targets, target("snmp", "two"))
		}},
		{"duplicates", "invalid_target_id", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets = append(s.Targets, s.Targets[0]) }},
		{"local conflict", "local_target_conflict", func(h *harness, s *model.DesiredCollectorConfig) {
			h.manager.cfg.Devices = []config.Device{{ID: "one"}}
		}},
		{"sip local conflict", "local_target_conflict", func(h *harness, s *model.DesiredCollectorConfig) {
			h.manager.cfg.Telephony.Endpoints = append(h.manager.cfg.Telephony.Endpoints, *telephonyFixture("one"))
		}},
		{"endpoint", "invalid_target", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].Endpoint = "https://user:secret@evil" }},
		{"port options", "invalid_target_settings", func(h *harness, s *model.DesiredCollectorConfig) {
			s.Targets[0].SNMP = &model.ManagedSNMPOptions{Retries: 4}
		}},
		{"other options", "invalid_protocol_options", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].IPMI = &model.ManagedIPMIOptions{} }},
		{"ca path", "ca_reference_forbidden", func(h *harness, s *model.DesiredCollectorConfig) { s.Targets[0].CARef = "/root/pki/key" }},
		{"destination", "destination_forbidden", func(h *harness, s *model.DesiredCollectorConfig) {
			h.manager.cfg.RemoteManagement.AllowedDestinations = []string{"192.0.2.0/24"}
		}},
		{"cipher zero", "invalid_target_settings", func(h *harness, s *model.DesiredCollectorConfig) {
			v := 0
			s.Targets[0] = target("ipmi", "bmc")
			s.Targets[0].IPMI = &model.ManagedIPMIOptions{CipherSuite: &v}
		}},
		{"ipmi admin", "invalid_target_settings", func(h *harness, s *model.DesiredCollectorConfig) {
			s.Targets[0] = target("ipmi", "bmc")
			s.Targets[0].IPMI = &model.ManagedIPMIOptions{Privilege: "administrator"}
		}},
		{"sip auth", "sip_auth_unsupported", func(h *harness, s *model.DesiredCollectorConfig) {
			s.Targets[0] = target("sip", "pbx")
			s.Targets[0].CredentialRef = "basic"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			s := snapshot(t, 1, target("snmp", "one"))
			tc.edit(h, &s)
			s.ConfigurationDigest, _ = Digest(s)
			r := apply(t, h, s, model.ConfigStatusRejected)
			if r.ErrorCode != tc.code {
				t.Fatalf("got %s want %s", r.ErrorCode, tc.code)
			}
			if len(h.registry.Targets()) != 0 {
				t.Fatal("rejected snapshot changed runtime")
			}
			raw, _ := json.Marshal(h.manager.Snapshot())
			if strings.Contains(string(raw), "do-not-reflect") || strings.Contains(string(raw), "fixture-secret") {
				t.Fatal("secret in diagnostics")
			}
		})
	}
	h := newHarness(t)
	s := snapshot(t, 1, target("snmp", "one"))
	r := h.manager.Apply(context.Background(), s, false, "", nil)
	if r.ErrorCode != "unauthenticated_delivery" || h.manager.state.HighestRevision != 0 {
		t.Fatal(r)
	}
	s.ConfigurationDigest = "invalid"
	if apply(t, h, s, model.ConfigStatusRejected).ErrorCode != "digest_mismatch" {
		t.Fatal("digest mismatch")
	}
}

func telephonyFixture(id string) *telephony.Endpoint {
	return &telephony.Endpoint{ID: id, Address: "127.0.0.1"}
}

type resolverFixture struct {
	ips   []netip.Addr
	calls int
}

func (r *resolverFixture) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return r.ips, nil
}
func TestDNSPolicyPinsOnlyApprovedAddresses(t *testing.T) {
	h := newHarness(t)
	h.cfg.RemoteManagement.AllowedDestinations = []string{"192.0.2.0/24"}
	r := &resolverFixture{ips: []netip.Addr{netip.MustParseAddr("192.0.2.10")}}
	s := snapshot(t, 1, target("snmp", "one"))
	s.Targets[0].Endpoint = "device.example"
	d, e := Validate(context.Background(), h.cfg, s, fixtureProvider{}, r)
	if e != nil {
		t.Fatal(e)
	}
	if r.calls != 1 || d[0].DialAddress != "192.0.2.10" || d[0].Address != "device.example" {
		t.Fatal("unbound DNS policy")
	}
	r.ips = append(r.ips, netip.MustParseAddr("127.0.0.1"))
	if _, e = Validate(context.Background(), h.cfg, s, fixtureProvider{}, r); e == nil || e.Error() != "destination_forbidden" {
		t.Fatal("mixed allowed/forbidden DNS accepted")
	}
	r.ips = make([]netip.Addr, 17)
	if _, e = Validate(context.Background(), h.cfg, s, fixtureProvider{}, r); e == nil || e.Error() != "destination_limit" {
		t.Fatal("unbounded resolver output")
	}
}

func TestFullSnapshotLifecycleAndRecovery(t *testing.T) {
	for _, protocol := range []string{"snmp", "routeros", "sip", "redfish", "ipmi"} {
		t.Run(protocol, func(t *testing.T) {
			h := newHarness(t)
			s := snapshot(t, 1, target(protocol, "one"))
			var stages []string
			r := h.manager.Apply(context.Background(), s, true, "", func(r model.ConfigApplyResult) { stages = append(stages, r.Status) })
			if r.Status != model.ConfigStatusApplied || !reflect.DeepEqual(stages, []string{"received", "validating", "applied"}) {
				t.Fatal(r, stages)
			}
			s.Targets[0].Polling.Timeout += time.Millisecond
			s.Revision = 2
			s.ConfigurationDigest, _ = Digest(s)
			apply(t, h, s, model.ConfigStatusApplied)
			if len(h.created) != 2 || !h.created[0].closed.Load() {
				t.Fatal("update did not replace and close adapter")
			}
			s.Targets[0].Enabled = false
			s.Revision = 3
			s.ConfigurationDigest, _ = Digest(s)
			apply(t, h, s, model.ConfigStatusApplied)
			if h.registry.Targets()[0].Enabled || !h.created[1].closed.Load() {
				t.Fatal("disable did not stop target")
			}
			s.Targets[0].Enabled = true
			s.Revision = 4
			s.ConfigurationDigest, _ = Digest(s)
			apply(t, h, s, model.ConfigStatusApplied)
			h.fail.Store(true)
			s.Targets[0].Name = "changed"
			s.Revision = 5
			s.ConfigurationDigest, _ = Digest(s)
			apply(t, h, s, model.ConfigStatusFailed)
			if h.manager.Applied().Revision != 4 || !h.registry.Targets()[0].Enabled {
				t.Fatal("prepare failure changed working set")
			}
			h.fail.Store(false)
			apply(t, h, snapshot(t, 6), model.ConfigStatusApplied)
			if len(h.registry.Targets()) != 0 {
				t.Fatal("removed target active")
			}
		})
	}
}

func TestPersistenceFailureRollbackAndCorruption(t *testing.T) {
	h := newHarness(t)
	apply(t, h, snapshot(t, 1, target("snmp", "one")), model.ConfigStatusApplied)
	original := h.manager.store.write
	var attempts int
	h.manager.store.write = func(data []byte) error {
		attempts++
		if attempts == 1 {
			return errors.New("write failed")
		}
		return original(data)
	}
	r := apply(t, h, snapshot(t, 2, target("snmp", "two")), model.ConfigStatusFailed)
	if r.ErrorCode != "state_write_failed" || h.registry.Targets()[0].ID != "remote:one" || h.created[0].closed.Load() || !h.created[1].closed.Load() {
		t.Fatal("failed persistence did not restore old runtime", r)
	}
	state, e := h.manager.store.load(collectorID)
	if e != nil || state.Applied.Revision != 1 {
		t.Fatal("old persisted state lost", e)
	}
	raw, e := os.ReadFile(h.manager.store.path)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), "fixture-secret") || strings.Contains(string(raw), sessionID) {
		t.Fatal("secret/session persisted")
	}
	if e = os.WriteFile(h.manager.store.path, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	m := New(h.cfg, collectorID, sessionID, h.registry, h.factory, fixtureProvider{})
	if m.Restore(context.Background()) == nil || !m.Snapshot().StorageDegraded {
		t.Fatal("corrupted state accepted")
	}
	if m.Apply(context.Background(), snapshot(t, 3), true, "", nil).Status != model.ConfigStatusFailed {
		t.Fatal("corrupt state silently overwritten")
	}
}

func TestOfflineRestoreRevalidationDisabledAndIdentity(t *testing.T) {
	h := newHarness(t)
	s := snapshot(t, 1, target("snmp", "one"))
	apply(t, h, s, model.ConfigStatusApplied)
	_ = h.registry.Close()
	r, e := remote.NewAdapters(h.cfg, h.factory, func(model.Envelope) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	m := New(h.cfg, collectorID, "new-session", r, h.factory, fixtureProvider{})
	if e = m.Restore(context.Background()); e != nil || m.Applied().Revision != 1 || len(r.Targets()) != 1 {
		t.Fatal("offline restore", e)
	}
	c := h.cfg
	c.RemoteManagement.Enabled = false
	r2, e := remote.NewAdapters(c, h.factory, func(model.Envelope) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer r2.Close()
	m2 := New(c, collectorID, sessionID, r2, h.factory, fixtureProvider{})
	if e = m2.Restore(context.Background()); e != nil || len(r2.Targets()) != 0 {
		t.Fatal("disabled restore ran target")
	}
	c = h.cfg
	c.RemoteManagement.AllowedProtocols = []string{"sip"}
	m3 := New(c, collectorID, sessionID, r2, h.factory, fixtureProvider{})
	if m3.Restore(context.Background()) == nil || m3.Applied().Revision != 0 {
		t.Fatal("changed policy bypassed")
	}
	if _, e = m.store.load("different-collector"); e == nil {
		t.Fatal("state identity not bound")
	}
	// Highest rejected revision and its content conflict protection survive restart.
	bad := snapshot(t, 2, target("snmp", "one"))
	bad.Targets[0].CredentialRef = "unknown"
	bad.ConfigurationDigest, _ = Digest(bad)
	if m.Apply(context.Background(), withSession(bad, "new-session"), true, "", nil).Status != model.ConfigStatusRejected {
		t.Fatal("expected reject")
	}
	m4 := New(h.cfg, collectorID, "new-session", r, h.factory, fixtureProvider{})
	if m4.state.HighestRevision != 2 {
		t.Fatal("revision watermark lost")
	}
}
func withSession(s model.DesiredCollectorConfig, id string) model.DesiredCollectorConfig {
	s.SessionID = id
	return s
}

func TestUpdateDuringActivePollAndSequentialBurst(t *testing.T) {
	h := newHarness(t)
	apply(t, h, snapshot(t, 1, target("snmp", "one")), model.ConfigStatusApplied)
	old := h.created[0]
	old.block.Store(true)
	old.entered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.registry.Run(ctx) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}()
	select {
	case <-old.entered:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	s := snapshot(t, 2, target("snmp", "one"))
	s.Targets[0].Polling.FastInterval = 30 * time.Millisecond
	s.ConfigurationDigest, _ = Digest(s)
	apply(t, h, s, model.ConfigStatusApplied)
	if old.active.Load() != 0 || !old.closed.Load() {
		t.Fatal("old poll not drained")
	}
	var wg sync.WaitGroup
	for n := uint64(3); n < 20; n++ {
		wg.Go(func() { h.manager.Apply(context.Background(), snapshot(t, n, target("snmp", "one")), true, "", nil) })
	}
	wg.Wait()
	if h.manager.Applied().Revision != 19 || len(h.registry.Targets()) != 1 {
		t.Fatal("burst mixed revisions", h.manager.Applied())
	}
}

func BenchmarkManagedValidation(b *testing.B) {
	h := newHarness(b)
	s := snapshot(b, 1, target("snmp", "one"))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, e := Validate(context.Background(), h.cfg, s, fixtureProvider{}, nil); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkManagedApply(b *testing.B) {
	h := newHarness(b)
	s := snapshot(b, 1, target("snmp", "one"))
	b.ReportAllocs()
	b.ResetTimer()
	for n := range b.N {
		s.Revision = uint64(n + 1)
		if r := h.manager.Apply(context.Background(), s, true, "", nil); r.Status != model.ConfigStatusApplied {
			b.Fatal(r)
		}
	}
}
func TestResourceUsageManagedTransitions(t *testing.T) {
	h := newHarness(t)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	g := runtime.NumGoroutine()
	start := time.Now()
	for n := uint64(1); n <= 50; n++ {
		apply(t, h, snapshot(t, n, target("snmp", "one")), model.ConfigStatusApplied)
	}
	elapsed := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if len(h.created) != 1 || runtime.NumGoroutine() > g+1 {
		t.Fatal("unchanged revisions leaked instances/goroutines")
	}
	t.Logf("50 revisions elapsed=%s total_alloc=%d heap_before=%d heap_after=%d goroutines=%d->%d", elapsed, after.TotalAlloc-before.TotalAlloc, before.HeapAlloc, after.HeapAlloc, g, runtime.NumGoroutine())
	start = time.Now()
	r, e := remote.NewAdapters(h.cfg, h.factory, func(model.Envelope) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	m := New(h.cfg, collectorID, sessionID, r, h.factory, fixtureProvider{})
	if e = m.Restore(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Logf("restore_latency=%s persistent_bytes=%d", time.Since(start), fileSize(t, h.manager.store.path))
}
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, e := os.Stat(filepath.Clean(path))
	if e != nil {
		t.Fatal(e)
	}
	return info.Size()
}
