package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/linux"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
)

type runnerFunc func(context.Context, Operation) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, op Operation) ([]byte, error) { return f(ctx, op) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureRunner(t *testing.T) Runner {
	t.Helper()
	responses := map[Operation][]byte{
		Board: fixture(t, "board.json"), Networks: fixture(t, "networks.json"),
		Services: fixture(t, "services.json"), Firewall: fixture(t, "firewall.json"),
	}
	return runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return responses[op], nil
	})
}

func fixturePlatform(t *testing.T, runner Runner) *Platform {
	t.Helper()
	etc := t.TempDir()
	if err := os.WriteFile(filepath.Join(etc, "openwrt_release"), fixture(t, "openwrt_release"), 0600); err != nil {
		t.Fatal(err)
	}
	return NewWithOptions(Options{ProcRoot: "../linux/testdata/proc", SysRoot: "../linux/testdata/sys", EtcRoot: etc, Runner: runner})
}

func capability(d model.Device, name string) bool {
	for _, c := range d.Capabilities {
		if c.Name == name {
			return c.Supported
		}
	}
	return false
}

func TestNativeIdentityAndLinuxReuse(t *testing.T) {
	p := fixturePlatform(t, fixtureRunner(t))
	d, err := p.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Driver != "local-openwrt" || d.Source != "local" || d.Hostname != "openwrt-schema-fixture" ||
		d.Model != "Schema fixture board" || d.BoardName != "fixture,schema-board" || d.SoftwareVersion != "24.10.0" ||
		d.FirmwareVersion != "r-schema-fixture" || d.Distribution != "OpenWrt 24.10.0 schema fixture" ||
		d.Kernel != "6.6.73" || d.UptimeSeconds != 1234.5 {
		t.Fatalf("OpenWrt identity = %+v", d)
	}
	if len(d.PlatformEvidence) < 5 || len(d.Services) != 4 || len(d.Networks) != 2 || d.Firewall == nil ||
		!capability(d, "services.read") || !capability(d, "firewall.read") {
		t.Fatalf("OpenWrt inventory = %+v", d)
	}
	for _, c := range d.Capabilities {
		if strings.Contains(c.Name, ".start") || strings.Contains(c.Name, ".stop") || strings.Contains(c.Name, ".write") || c.Name == "health.read" {
			t.Fatalf("unimplemented capability: %+v", c)
		}
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), "do-not-forward") {
		t.Fatal("raw OpenWrt secret-bearing fields escaped into inventory")
	}
	l := linux.New("../linux/testdata/proc", "../linux/testdata/sys")
	expected, err := l.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual.CPU, expected.CPU) || !reflect.DeepEqual(actual.Memory, expected.Memory) ||
		!reflect.DeepEqual(actual.Interfaces, expected.Interfaces) || actual.BootID != expected.BootID || actual.Source != "local" {
		t.Fatal("OpenWrt altered Linux measurements")
	}
}

func TestUnavailableOptionalSourcesKeepLinuxAndRelease(t *testing.T) {
	p := fixturePlatform(t, runnerFunc(func(context.Context, Operation) ([]byte, error) {
		return nil, errors.New("password=do-not-forward-error-secret")
	}))
	d, err := p.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Driver != "local-openwrt" || d.SoftwareVersion != "24.10.0" || d.FirmwareVersion != "r-release-fixture" ||
		d.UptimeSeconds != 1234.5 || len(d.Services) != 0 || d.Firewall.Completeness != "failed" ||
		capability(d, "services.read") || capability(d, "firewall.read") || !capability(d, "interfaces.read") {
		t.Fatalf("fallback inventory = %+v", d)
	}
	data, _ := json.Marshal(d)
	if strings.Contains(string(data), "do-not-forward") {
		t.Fatal("runner error leaked into diagnostics evidence")
	}
	if _, err := p.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPartialSourcesAndCapabilityRecovery(t *testing.T) {
	good := fixtureRunner(t)
	broken := true
	p := fixturePlatform(t, runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		if broken && (op == Services || op == Board) {
			return []byte(`{"hostname":3,"instances":false}`), nil
		}
		return good.Run(ctx, op)
	}))
	d, err := p.Identity(context.Background())
	if err != nil || capability(d, "services.read") || !capability(d, "firewall.read") || len(d.Networks) != 2 || d.Hostname != "fixture-node" {
		t.Fatalf("partial = %+v, %v", d, err)
	}
	broken = false
	d, err = p.Identity(context.Background())
	if err != nil || !capability(d, "services.read") || d.Hostname != "openwrt-schema-fixture" {
		t.Fatalf("recovered = %+v, %v", d, err)
	}
}

func TestOptionalTimeoutDoesNotConsumeParentDeadline(t *testing.T) {
	p := fixturePlatform(t, runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	d, err := p.Identity(ctx)
	if err != nil || ctx.Err() != nil || d.Firewall.Completeness != "failed" {
		t.Fatalf("optional timeout destroyed Linux fallback: %+v %v %v", d, err, ctx.Err())
	}
	found := false
	for _, e := range d.PlatformEvidence {
		found = found || e.Value == "read timeout"
	}
	if !found {
		t.Fatal("timeouts omitted from source evidence")
	}
}

func TestCancellation(t *testing.T) {
	started := make(chan struct{})
	p := fixturePlatform(t, runnerFunc(func(ctx context.Context, _ Operation) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if _, err := p.Identity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Identity cancellation = %v", err)
	}
	if _, err := p.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Collect cancellation = %v", err)
	}
}

func TestOversizedRunnerResponse(t *testing.T) {
	p := fixturePlatform(t, runnerFunc(func(context.Context, Operation) ([]byte, error) {
		return make([]byte, maxResponseBytes+1), nil
	}))
	d, err := p.Identity(context.Background())
	if err != nil || capability(d, "services.read") || capability(d, "firewall.read") {
		t.Fatalf("oversized response accepted: %+v %v", d, err)
	}
}

func TestBootEnablementOnlyWithValidatedPositiveEvidence(t *testing.T) {
	p := fixturePlatform(t, fixtureRunner(t))
	for _, dir := range []string{"init.d", "rc.d"} {
		if err := os.Mkdir(filepath.Join(p.etcRoot, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(p.etcRoot, "init.d", "dnsmasq"), []byte("schema fixture; not executed"), 0600); err != nil {
		t.Fatal(err)
	}
	// Windows may not permit symlink creation for an unprivileged process.
	if err := os.Symlink(filepath.Join("..", "init.d", "dnsmasq"), filepath.Join(p.etcRoot, "rc.d", "S19dnsmasq")); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "init.d", "dnsmasq"), filepath.Join(p.etcRoot, "rc.d", "S50dropbear")); err != nil {
		t.Fatal(err)
	}
	d, err := p.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Services {
		if s.Name == "dnsmasq" {
			if s.Enabled == nil || !*s.Enabled {
				t.Fatal("positive boot evidence not mapped")
			}
		} else if s.Enabled != nil {
			t.Fatalf("boot enablement guessed for %s", s.Name)
		}
	}
}

func TestNativeInventoryAndUnavailableSourcesUseExistingWireContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner Runner
	}{
		{"available", fixtureRunner(t)},
		{"unavailable", runnerFunc(func(context.Context, Operation) ([]byte, error) {
			return nil, errors.New("ubus unavailable")
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixturePlatform(t, tc.runner)
			d, err := p.Identity(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			d.ID = "local:openwrt-fixture"
			at := time.Now().UTC()
			for _, payload := range []string{"hello", "inventory"} {
				e := model.Envelope{MessageID: "openwrt-wire", AgentID: "agent-fixture", SessionID: "session-fixture", SentAt: at}
				if payload == "hello" {
					e.Hello = &model.AgentHello{AgentID: e.AgentID, SessionID: e.SessionID, StartedAt: at, Device: d, ProtocolVersion: 1}
				} else {
					e.Inventory = &model.InventoryUpdate{ObservedAt: at, Devices: []model.Device{d}}
				}
				wire, err := transportgrpc.Encode(e)
				if err != nil {
					t.Fatalf("%s encode rejected actual OpenWrt snapshot: %v", payload, err)
				}
				decoded, err := transportgrpc.DecodeEnvelope(wire)
				if err != nil {
					t.Fatal(err)
				}
				got := d
				if decoded.Hello != nil {
					got = decoded.Hello.Device
				} else {
					got = decoded.Inventory.Devices[0]
				}
				if !reflect.DeepEqual(d, got) {
					t.Fatalf("%s OpenWrt wire round trip lost normalized fields\ninput: %+v\noutput: %+v", payload, d, got)
				}
			}
		})
	}
}
