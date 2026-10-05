package routeros

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/commands"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/routeros"
)

// These files reproduce officially documented API shapes. The HTTPS server is
// a local fixture, not evidence of live hardware or any particular RouterOS model.
type fixture struct {
	mu          sync.Mutex
	bodies      map[string]string
	status      map[string]int
	interfaces  []protocol.Record
	patches     int
	mode        string
	delayPath   string
	patched     bool
	patchSignal chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{bodies: map[string]string{}, status: map[string]int{}}
	for file, path := range map[string]string{"identity": "system/identity", "resource": "system/resource", "routerboard": "system/routerboard", "interfaces": "interface", "neighbors": "ip/neighbor"} {
		data, err := os.ReadFile("testdata/" + file + ".json")
		if err != nil {
			t.Fatal(err)
		}
		f.bodies[path] = string(data)
	}
	if err := json.Unmarshal([]byte(f.bodies["interface"]), &f.interfaces); err != nil {
		t.Fatal(err)
	}
	f.bodies["ip/address"] = `[{"address":"192.0.2.1/24","interface":"uplink","disabled":"false"}]`
	f.bodies["ipv6/address"] = `[{"address":"2001:db8::1/64","interface":"uplink","disabled":"false"}]`
	f.bodies["interface/bridge/port"] = `[{"interface":"uplink","bridge":"bridge1"}]`
	f.bodies["interface/bonding"] = `[{"name":"bond1","slaves":"bridge1"}]`
	f.bodies["interface/vlan"] = `[{"name":"management-vlan","interface":"bond1"}]`
	f.bodies["user"] = `[{"name":"monitor","group":"infrapulse","disabled":"false"}]`
	f.bodies["user/group"] = `[{"name":"infrapulse","policy":"read,write,api,rest-api,!sensitive"}]`
	return f
}
func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != "monitor" || p != "secret-fixture-password" {
		w.WriteHeader(401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/rest/")
	f.mu.Lock()
	if path == f.delayPath {
		f.mu.Unlock()
		<-r.Context().Done()
		return
	}
	if code := f.status[path]; code != 0 {
		f.mu.Unlock()
		w.WriteHeader(code)
		fmt.Fprint(w, `{"password":"secret-fixture-password"}`)
		return
	}
	if strings.HasPrefix(path, "interface/*") {
		id := strings.TrimPrefix(path, "interface/")
		var record protocol.Record
		for _, row := range f.interfaces {
			if row[".id"] == id {
				record = row
			}
		}
		if record == nil {
			f.mu.Unlock()
			w.WriteHeader(404)
			return
		}
		if r.Method == http.MethodPatch {
			var body struct {
				Disabled bool `json:"disabled"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				f.mu.Unlock()
				w.WriteHeader(400)
				return
			}
			f.patches++
			f.patched = true
			if f.patchSignal != nil {
				close(f.patchSignal)
				f.patchSignal = nil
			}
			mode := f.mode
			if mode != "mismatch" && mode != "timeout-old" && mode != "denied-write" && mode != "server-error-old" && mode != "malformed-old" {
				record["disabled"] = fmt.Sprint(body.Disabled)
			}
			data, _ := json.Marshal(record)
			f.mu.Unlock()
			if mode == "timeout-known" || mode == "timeout-old" {
				<-r.Context().Done()
				return
			}
			if mode == "malformed-write" || mode == "malformed-old" {
				fmt.Fprint(w, "{")
				return
			}
			if mode == "oversized-write" {
				fmt.Fprint(w, strings.Repeat("x", 8193))
				return
			}
			if mode == "server-error-write" || mode == "server-error-old" {
				w.WriteHeader(500)
				return
			}
			if mode == "denied-write" {
				w.WriteHeader(403)
				return
			}
			w.Write(data)
			return
		}
		if f.patched && f.mode == "uncertain" {
			f.mu.Unlock()
			w.WriteHeader(503)
			return
		}
		data, _ := json.Marshal(record)
		f.mu.Unlock()
		w.Write(data)
		return
	}
	if path == "interface" && f.bodies[path] == "dynamic" {
		data, _ := json.Marshal(f.interfaces)
		f.mu.Unlock()
		w.Write(data)
		return
	}
	body, ok := f.bodies[path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(404)
		return
	}
	fmt.Fprint(w, body)
}
func setup(t *testing.T, f *fixture, mutations bool, opts driver.Options) *Driver {
	t.Helper()
	server := httptest.NewTLSServer(f)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	opts.ID = "router-stable"
	opts.Address = server.URL
	opts.EnableMutations = mutations
	opts.Location = model.PhysicalLocation{Rack: "configured-rack", RackUnit: 8}
	t.Setenv("ROS_USER", "monitor")
	t.Setenv("ROS_PASSWORD", "secret-fixture-password")
	provider := credentials.NewEnvironment(map[string]credentials.Reference{"router": {UsernameEnv: "ROS_USER", PasswordEnv: "ROS_PASSWORD"}})
	d, err := New(opts, provider, "router", protocol.Options{TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
func TestInventoryTelemetryAndNoCredentialSerialization(t *testing.T) {
	f := newFixture(t)
	d := setup(t, f, true, driver.Options{Name: "configured-name", VendorHint: "unconfirmed"})
	dev, err := d.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dev.ID != "router-stable" || dev.Name != "configured-name" || dev.Hostname != "edge-router" || dev.Vendor != "MikroTik" || dev.Model != "fixture-product" || dev.SerialNumber != "FIXTURE-001" || dev.UptimeSeconds != 93784 || dev.Location.Rack != "configured-rack" || dev.FirmwareVersion != "7.20.1" {
		t.Fatalf("wrong inventory %+v", dev)
	}
	if !commands.Supported(dev.Capabilities, "interfaces.disable") {
		t.Fatalf("permission evidence missing %+v", dev.Capabilities)
	}
	sample, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	iface := sample.Interfaces[0]
	if iface.Counters.RXBytes != 4294967300 || !iface.CounterMetadata.Available.RXBytes || iface.CounterMetadata.Widths.RXBytes != 64 || iface.Index != 0 || iface.Metadata.Index || !iface.Metadata.AdminStatus || len(iface.Addresses) != 2 || sample.Source != "routeros-rest" || sample.CPU != nil || sample.Memory != nil {
		t.Fatalf("wrong sample %+v %+v", sample, iface)
	}
	data, _ := json.Marshal(struct {
		Device model.Device
		Sample model.TelemetrySample
	}{dev, sample})
	if strings.Contains(string(data), "secret-fixture-password") || strings.Contains(string(data), "ROS_PASSWORD") {
		t.Fatal("credential serialized")
	}
}
func TestInterfaceUnavailableAndMalformedSemantics(t *testing.T) {
	rows := []protocol.Record{{".id": "*1", "name": "bare", "rx-byte": "0"}}
	interfaces, err := mapInterfaces(rows)
	if err != nil {
		t.Fatal(err)
	}
	iface := interfaces[0]
	if !iface.CounterMetadata.Available.RXBytes || iface.Counters.RXBytes != 0 || iface.CounterMetadata.Available.TXBytes || iface.Metadata.AdminStatus || iface.Metadata.MTU || iface.OperState != "unknown" {
		t.Fatalf("unavailable conflated with zero %+v", iface)
	}
	for _, field := range []string{"rx-byte", "mtu", "disabled", "running", "mac-address"} {
		t.Run(field, func(t *testing.T) {
			r := protocol.Record{".id": "*1", "name": "malformed", field: "bad-value"}
			if _, err := mapInterfaces([]protocol.Record{r}); !errors.Is(err, driver.ErrMalformed) {
				t.Fatalf("malformed %s accepted", field)
			}
		})
	}
	if _, err := mapInterfaces([]protocol.Record{{".id": "*1", "name": "a"}, {".id": "*1", "name": "b"}}); !errors.Is(err, driver.ErrMalformed) {
		t.Fatal("duplicate ID accepted")
	}
}
func TestPermissionsRequireEvidenceAndCanBeRevoked(t *testing.T) {
	for _, policy := range []string{"read,api,rest-api,!write", "read,write,api", "read,write,api,rest-api,!write", "!write,read,write,api,rest-api"} {
		t.Run(policy, func(t *testing.T) {
			f := newFixture(t)
			f.bodies["user/group"] = fmt.Sprintf(`[{"name":"infrapulse","policy":%q}]`, policy)
			d := setup(t, f, true, driver.Options{})
			dev, err := d.Inventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if commands.Supported(dev.Capabilities, "interfaces.disable") {
				t.Fatal("write capability fabricated")
			}
			resolved, err := d.ResolveInterface(context.Background(), "test-port")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.SetInterface(context.Background(), resolved, false); !errors.Is(err, driver.ErrUnavailable) {
				t.Fatalf("permission bypass %v", err)
			}
			f.mu.Lock()
			patches := f.patches
			f.mu.Unlock()
			if patches != 0 {
				t.Fatal("PATCH despite missing rights")
			}
		})
	}
	f := newFixture(t)
	d := setup(t, f, true, driver.Options{})
	if _, err := d.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	resolved, err := d.ResolveInterface(context.Background(), "test-port")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.bodies["user/group"] = `[{"name":"infrapulse","policy":"read,api,rest-api,!write"}]`
	f.mu.Unlock()
	if _, err = d.SetInterface(context.Background(), resolved, false); !errors.Is(err, driver.ErrUnavailable) || commands.Supported(d.Capabilities(), "interfaces.disable") {
		t.Fatalf("cached permission persisted %v", err)
	}
}
func TestMutationOptInAndUnavailablePermissionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
		status int
	}{{"default disabled", false, 0}, {"hidden metadata", true, 403}, {"remote RADIUS account", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.status != 0 {
				f.status["user"] = tc.status
			}
			if tc.name == "remote RADIUS account" {
				f.bodies["user"] = `[]`
			}
			d := setup(t, f, tc.enable, driver.Options{})
			dev, err := d.Inventory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if commands.Supported(dev.Capabilities, "interfaces.enable") {
				t.Fatal("write advertised without evidence")
			}
		})
	}
}
func TestCanonicalAliasesDependencyProtectionAndFailClosed(t *testing.T) {
	f := newFixture(t)
	d := setup(t, f, true, driver.Options{})
	for _, ref := range []string{"*1", "uplink", "ether1", "02:00:00:00:00:01"} {
		resolved, err := d.ResolveInterface(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.Interface.ID != "*1" {
			t.Fatal("wrong canonical record")
		}
		for _, protected := range []string{"*1", "uplink", "ether1", "02:00:00:00:00:01", "bridge1", "bond1", "management-vlan"} {
			cmd := model.Command{TargetDeviceID: "router-stable", Interface: &model.InterfaceAction{InterfaceID: ref, Operation: "disable"}}
			if !errors.Is((commands.Policy{Enabled: true, ProtectedInterfaces: []string{protected}}).Check(cmd, resolved.Aliases...), commands.ErrProtected) {
				t.Fatalf("protection alias %q bypassed via %q", protected, ref)
			}
		}
	}
	f.mu.Lock()
	f.status["interface/vlan"] = 403
	f.mu.Unlock()
	if _, err := d.ResolveInterface(context.Background(), "test-port"); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("dependency failure did not block resolution")
	}
}
func TestInterfaceMutationEnableDisableVerification(t *testing.T) {
	f := newFixture(t)
	f.bodies["interface"] = "dynamic"
	d := setup(t, f, true, driver.Options{})
	for _, desired := range []bool{false, true, true} {
		resolved, err := d.ResolveInterface(context.Background(), "test-port")
		if err != nil {
			t.Fatal(err)
		}
		v, err := d.SetInterface(context.Background(), resolved, desired)
		if err != nil || !v.Verified || v.ObservedAdminUp == nil || *v.ObservedAdminUp != desired || v.Uncertain {
			t.Fatalf("verification %+v %v", v, err)
		}
	}
	f.mu.Lock()
	patches := f.patches
	f.mu.Unlock()
	if patches != 2 {
		t.Fatalf("idempotent state repeated PATCH count %d", patches)
	}
}
func TestMutationUncertainAndTimeoutReadback(t *testing.T) {
	for _, mode := range []string{"timeout-known", "timeout-old", "uncertain", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.mode = mode
			d := setup(t, f, true, driver.Options{})
			resolved, err := d.ResolveInterface(context.Background(), "test-port")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			v, err := d.SetInterface(ctx, resolved, false)
			switch mode {
			case "timeout-known":
				if err != nil || !v.Verified || !v.Changed {
					t.Fatalf("known timeout state %+v %v", v, err)
				}
			case "timeout-old", "uncertain":
				if !errors.Is(err, driver.ErrUncertain) || !v.Uncertain || v.Verified {
					t.Fatalf("unknown timeout %+v %v", v, err)
				}
			case "mismatch":
				if err == nil || v.Verified || v.Uncertain {
					t.Fatalf("mismatch %+v %v", v, err)
				}
			}
		})
	}
}
func TestAppliedMutationBadResponseStillReadBack(t *testing.T) {
	for _, mode := range []string{"malformed-write", "oversized-write", "server-error-write"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.mode = mode
			d := setup(t, f, true, driver.Options{MaxResponseBytes: 4096})
			resolved, err := d.ResolveInterface(context.Background(), "test-port")
			if err != nil {
				t.Fatal(err)
			}
			v, err := d.SetInterface(context.Background(), resolved, false)
			if err != nil || !v.Verified || !v.Changed || v.Uncertain || v.ObservedAdminUp == nil || *v.ObservedAdminUp {
				t.Fatalf("applied mutation falsely failed %+v %v", v, err)
			}
			f.mu.Lock()
			patches := f.patches
			f.mu.Unlock()
			if patches != 1 {
				t.Fatal("mutation retried")
			}
		})
	}
}
func TestBadMutationResponseMismatchAndAPIRejection(t *testing.T) {
	for _, mode := range []string{"denied-write", "server-error-old", "malformed-old"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.mode = mode
			d := setup(t, f, true, driver.Options{})
			resolved, err := d.ResolveInterface(context.Background(), "test-port")
			if err != nil {
				t.Fatal(err)
			}
			v, err := d.SetInterface(context.Background(), resolved, false)
			if v.Verified || v.Changed || v.ObservedAdminUp == nil || !*v.ObservedAdminUp {
				t.Fatalf("bad mismatch state %+v %v", v, err)
			}
			if mode == "denied-write" {
				if !errors.Is(err, driver.ErrUnavailable) || v.Uncertain {
					t.Fatalf("definite rejection %+v %v", v, err)
				}
			} else {
				if !errors.Is(err, driver.ErrUncertain) || !v.Uncertain {
					t.Fatalf("ambiguous response %+v %v", v, err)
				}
			}
		})
	}
}
func TestCanceledMutationCannotClaimVerifiedOutcome(t *testing.T) {
	f := newFixture(t)
	f.mode = "timeout-known"
	signal := make(chan struct{})
	f.patchSignal = signal
	d := setup(t, f, true, driver.Options{})
	resolved, err := d.ResolveInterface(context.Background(), "test-port")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		v   model.InterfaceVerification
		err error
	}
	done := make(chan outcome, 1)
	go func() { v, err := d.SetInterface(ctx, resolved, false); done <- outcome{v, err} }()
	select {
	case <-signal:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("PATCH did not start")
	}
	select {
	case result := <-done:
		if !result.v.Uncertain || result.v.Verified || !errors.Is(result.err, driver.ErrUncertain) {
			t.Fatalf("canceled mutation %+v %v", result.v, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock")
	}
}
func TestTopologyEvidenceAndCompleteness(t *testing.T) {
	f := newFixture(t)
	d := setup(t, f, false, driver.Options{TopologyTTLSeconds: 120})
	u, err := d.Topology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Completeness != "complete" || len(u.Observations) != 4 || u.TTLSeconds != 120 {
		t.Fatalf("topology %+v", u)
	}
	for _, n := range u.Observations {
		if n.RemoteChassisSubtype != 0 {
			t.Fatal("neighbor MAC fabricated as LLDP chassis subtype")
		}
		if n.ExpiresAt.Sub(n.ObservedAt) != 120*time.Second {
			t.Fatal("TTL assumption lost")
		}
		if n.Protocol == "mndp" && n.LocalInterfaceID != "" {
			t.Fatal("ambiguous slave/master fabricated")
		}
		if n.Protocol == "unknown" && n.LocalInterfaceID != "" {
			t.Fatal("unresolved port fabricated")
		}
		if n.Protocol == "lldp" && (n.LocalInterfaceID != "*1" || n.RemotePortID != nil) {
			t.Fatal("CDP port leaked to LLDP")
		}
	}
	f.mu.Lock()
	f.bodies["ip/neighbor"] = `[]`
	f.mu.Unlock()
	u, err = d.Topology(context.Background())
	if err != nil || u.Completeness != "complete" || len(u.Observations) != 0 {
		t.Fatalf("empty complete %+v %v", u, err)
	}
	f.mu.Lock()
	f.bodies["ip/neighbor"] = `[{"interface":"uplink","discovered-by":"lldp","age":"bad"}]`
	f.mu.Unlock()
	u, err = d.Topology(context.Background())
	if !errors.Is(err, driver.ErrMalformed) || u.Completeness != "partial" || len(u.Observations) != 1 {
		t.Fatalf("partial %+v %v", u, err)
	}
	f.mu.Lock()
	f.status["ip/neighbor"] = 404
	f.mu.Unlock()
	u, err = d.Topology(context.Background())
	if !errors.Is(err, driver.ErrUnavailable) || u.Completeness != "failed" {
		t.Fatalf("failed %+v %v", u, err)
	}
}
func TestDriverResponseLimitsAndCancellation(t *testing.T) {
	f := newFixture(t)
	d := setup(t, f, false, driver.Options{MaxRows: 2})
	if _, err := d.Collect(context.Background()); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("row limit %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
}
func TestTopologyTimeoutRetainsFailedSemantics(t *testing.T) {
	f := newFixture(t)
	f.delayPath = "ip/neighbor"
	d := setup(t, f, false, driver.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	update, err := d.Topology(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, driver.ErrConnectivity) || update.Completeness != "failed" {
		t.Fatalf("timeout snapshot %+v %v", update, err)
	}
}
func TestTopologyExpandedObservationsBounded(t *testing.T) {
	f := newFixture(t)
	d := setup(t, f, false, driver.Options{MaxRows: 3})
	update, err := d.Topology(context.Background())
	if !errors.Is(err, driver.ErrLimit) || update.Completeness != "partial" || len(update.Observations) != 3 {
		t.Fatalf("unbounded protocol expansion %+v %v", update, err)
	}
}
func TestProbeDoesNotReadSlowInventory(t *testing.T) {
	f := newFixture(t)
	f.status["system/routerboard"] = 503
	f.status["interface"] = 503
	f.status["ip/address"] = 503
	d := setup(t, f, false, driver.Options{})
	dev, err := d.Probe(context.Background())
	if err != nil || dev.Hostname != "edge-router" || !commands.Supported(dev.Capabilities, "system.read") {
		t.Fatalf("medium probe depended on slow inventory %+v %v", dev, err)
	}
}
func TestResolveRejectsDynamicAmbiguousAndChangedIdentity(t *testing.T) {
	f := newFixture(t)
	f.bodies["interface"] = "dynamic"
	d := setup(t, f, true, driver.Options{})
	f.interfaces[1]["dynamic"] = "true"
	if _, err := d.ResolveInterface(context.Background(), "test-port"); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("dynamic interface accepted")
	}
	f.mu.Lock()
	f.interfaces[1]["dynamic"] = "false"
	f.interfaces[1]["default-name"] = "ether1"
	f.mu.Unlock()
	if _, err := d.ResolveInterface(context.Background(), "ether1"); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("ambiguous alias accepted")
	}
	resolved, err := d.ResolveInterface(context.Background(), "test-port")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.interfaces[1]["mac-address"] = "02:00:00:00:aa:aa"
	f.mu.Unlock()
	if _, err := d.SetInterface(context.Background(), resolved, false); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("recreated interface mutated")
	}
	f.mu.Lock()
	patches := f.patches
	f.mu.Unlock()
	if patches != 0 {
		t.Fatal("identity mismatch reached PATCH")
	}
}
func TestOversizedCanonicalNameCannotReachMutation(t *testing.T) {
	f := newFixture(t)
	f.bodies["interface"] = "dynamic"
	d := setup(t, f, true, driver.Options{})
	resolved, err := d.ResolveInterface(context.Background(), "test-port")
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.interfaces[1]["name"] = strings.Repeat("x", 129)
	f.mu.Unlock()
	if _, err := d.ResolveInterface(context.Background(), "*2"); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("oversized canonical resolution %v", err)
	}
	if _, err := d.SetInterface(context.Background(), resolved, false); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("oversized canonical apply %v", err)
	}
	f.mu.Lock()
	patches := f.patches
	f.mu.Unlock()
	if patches != 0 {
		t.Fatal("oversized name reached PATCH")
	}
}
func TestUptimeParser(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
		ok    bool
	}{{"0s", 0, true}, {"1w2d3h4m5s", 788645, true}, {"03:04:05", 11045, true}, {"", 0, false}, {"-1s", 0, false}, {"banana", 0, false}, {"1ssecret", 0, false}, {"03:61:01", 0, false}} {
		got, ok := parseUptime(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parse %q=%v,%v", tc.value, got, ok)
		}
	}
}
