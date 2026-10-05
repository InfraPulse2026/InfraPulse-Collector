package agent

import (
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This local HTTPS fixture emulates only the documented fixed operations. It
// does not claim hardware validation or produce synthetic production data.
type remoteRouterFixture struct {
	disabled, unreachable atomic.Bool
	writes                atomic.Int32
	counter               atomic.Uint64
}

func (f *remoteRouterFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != "monitor" || p != "private-fixture-password" {
		w.WriteHeader(401)
		return
	}
	if f.unreachable.Load() {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	record := func(id, name string) map[string]string {
		disabled := false
		if id == "*2" {
			disabled = f.disabled.Load()
		}
		n := f.counter.Add(100)
		return map[string]string{".id": id, "name": name, "default-name": name, "type": "ether", "mac-address": "02:00:00:00:00:0" + id[1:], "mtu": "1500", "disabled": strconv.FormatBool(disabled), "running": strconv.FormatBool(!disabled), "dynamic": "false", "rx-byte": strconv.FormatUint(n, 10), "tx-byte": strconv.FormatUint(n*2, 10), "rx-packet": "10", "tx-packet": "20", "rx-error": "0", "tx-error": "0", "rx-drop": "0", "tx-drop": "0"}
	}
	var data any
	switch r.URL.Path {
	case "/rest/system/identity":
		data = map[string]string{"name": "fixture-router"}
	case "/rest/system/resource":
		data = map[string]string{"uptime": "1h2m3s", "version": "7.20", "architecture-name": "arm64", "board-name": "fixture-board", "cpu-load": "5", "free-memory": "4096", "total-memory": "8192"}
	case "/rest/system/routerboard":
		data = map[string]string{"model": "fixture-board", "serial-number": "fixture-serial", "current-firmware": "7.20"}
	case "/rest/interface":
		data = []map[string]string{record("*1", "ether1"), record("*2", "ether2")}
	case "/rest/interface/*1":
		data = record("*1", "ether1")
	case "/rest/interface/*2":
		if r.Method == http.MethodPatch {
			var body struct {
				Disabled bool `json:"disabled"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				w.WriteHeader(400)
				return
			}
			f.disabled.Store(body.Disabled)
			f.writes.Add(1)
		}
		data = record("*2", "ether2")
	case "/rest/ip/address":
		data = []map[string]string{{"address": "192.0.2.1/24", "interface": "ether1", "disabled": "false"}}
	case "/rest/ipv6/address", "/rest/interface/bridge/port", "/rest/interface/bonding", "/rest/interface/vlan":
		data = []map[string]string{}
	case "/rest/ip/neighbor":
		data = []map[string]string{{"interface": "ether1", "identity": "unknown-switch", "mac-address": "02:10:00:00:00:01", "discovered-by": "lldp", "age": "1s"}}
	case "/rest/user":
		data = []map[string]string{{"name": "monitor", "group": "fixture-write", "disabled": "false"}}
	case "/rest/user/group":
		data = []map[string]string{{"name": "fixture-write", "policy": "read,write,api,rest-api"}}
	default:
		w.WriteHeader(404)
		return
	}
	_ = json.NewEncoder(w).Encode(data)
}
func configureRouterFixture(t *testing.T, c *config.Config, f *remoteRouterFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(f)
	t.Cleanup(server.Close)
	ca := filepath.Join(testTempDir(t), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_E2E_USER", "monitor")
	t.Setenv("ROUTER_E2E_PASSWORD", "private-fixture-password")
	c.Credentials = map[string]credentials.Reference{"router": {UsernameEnv: "ROUTER_E2E_USER", PasswordEnv: "ROUTER_E2E_PASSWORD"}}
	c.Devices = []config.Device{{ID: "remote-router", Name: "test-router", Driver: "routeros", Address: server.URL, CredentialRef: "router", TLS: config.RemoteTLS{CAFile: ca}, EnableMutations: true}}
	c.Commands.Enabled = true
	c.Commands.ProtectedInterfaces = []string{"ether1"}
	c.Telemetry.MediumInterval = 40 * time.Millisecond
	c.Telemetry.TopologyInterval = 40 * time.Millisecond
	c.Telemetry.CollectTimeout = 150 * time.Millisecond
	c.Queue.MaxMessages = 128
	return server
}
func TestAgentRouterOSRemoteStreamingMutationsAndProtection(t *testing.T) {
	observed := make(chan model.Envelope, 1024)
	command := &model.Command{ID: "fixture-disable", TargetDeviceID: "remote-router", Timeout: time.Second, Initiator: "ai_suggested", IdempotencyKey: "disable-once", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(io.Discard, command, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	fixture := &remoteRouterFixture{}
	configureRouterFixture(t, &c, fixture)
	a := newTestAgent(t, c, newFixturePlatform())
	// Observe permissions before Controller submits a mutation. Initial unknown
	// capabilities deliberately remain unsupported until a successful poll.
	warm, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := a.remote.Run(warm); err != nil {
		t.Fatal(err)
	}
	cancel()
	stop := runTestAgent(t, a)
	result := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.CommandResult != nil && e.CommandResult.CommandID == command.ID })
	if r := result.CommandResult; r.Status != "success" || !r.Changed || r.Verification == nil || !r.Verification.Verified || !fixture.disabled.Load() {
		t.Fatal("disable not verified", r)
	}
	topology := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Topology != nil && len(e.Topology.Observations) > 0 })
	if topology.Topology.DeviceID != "remote-router" || topology.Topology.Observations[0].LocalInterfaceID != "*1" || topology.Topology.Observations[0].Protocol != "lldp" {
		t.Fatal("topology mapping", topology.Topology)
	}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Telemetry != nil && e.Telemetry.DeviceID == "remote-router" && len(e.Telemetry.Interfaces) > 0 && e.Telemetry.Interfaces[0].Rates != nil
	})
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Telemetry != nil && strings.HasPrefix(e.Telemetry.DeviceID, "local:")
	})
	// Reconnect repeats the same idempotency key; the result is replayed and no
	// second PATCH is executed. Then enable uses a separate typed identity.
	address := controller.listener.Addr().String()
	controller.stop()
	controller = startTCPController(t, address, mockcontroller.New(nil, command, observeChannel(observed)))
	result = waitEnvelope(t, observed, func(e model.Envelope) bool { return e.CommandResult != nil && e.CommandResult.CommandID == command.ID })
	if result.CommandResult.Status != "success" || fixture.writes.Load() != 1 {
		t.Fatal("duplicate executed mutation")
	}
	controller.stop()
	enable := *command
	enable.ID = "fixture-enable"
	enable.IdempotencyKey = "enable-once"
	enable.Interface = &model.InterfaceAction{InterfaceID: "*2", Operation: "enable"}
	controller = startTCPController(t, address, mockcontroller.New(nil, &enable, observeChannel(observed)))
	result = waitEnvelope(t, observed, func(e model.Envelope) bool { return e.CommandResult != nil && e.CommandResult.CommandID == enable.ID })
	if result.CommandResult.Status != "success" || fixture.disabled.Load() || fixture.writes.Load() != 2 {
		t.Fatal("enable failed", result.CommandResult)
	}
	controller.stop()
	protected := *command
	protected.ID = "fixture-protected"
	protected.IdempotencyKey = "protected"
	protected.Interface = &model.InterfaceAction{InterfaceID: "*1", Operation: "disable"}
	controller = startTCPController(t, address, mockcontroller.New(nil, &protected, observeChannel(observed)))
	result = waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.CommandResult != nil && e.CommandResult.CommandID == protected.ID
	})
	if result.CommandResult.Status != "rejected" || result.CommandResult.ErrorCode != "protected_resource" || fixture.writes.Load() != 2 {
		t.Fatal("management interface not protected", result.CommandResult)
	}
	data, _ := json.Marshal(a.Snapshot())
	if strings.Contains(string(data), "private-fixture-password") {
		t.Fatal("password in diagnostics")
	}
	stop()
}
func TestRemoteTimeoutDoesNotBlockLocalCollectionAndRecovers(t *testing.T) {
	observed := make(chan model.Envelope, 1024)
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, nil, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	fixture := &remoteRouterFixture{}
	configureRouterFixture(t, &c, fixture)
	c.Telemetry.CollectTimeout = 30 * time.Millisecond
	c.Remote.Concurrency = 1
	a := newTestAgent(t, c, newFixturePlatform())
	runTestAgent(t, a)
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		for _, event := range e.Events {
			if event.Type == "device.online" {
				return true
			}
		}
		return false
	})
	fixture.unreachable.Store(true)
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		for _, event := range e.Events {
			if event.Type == "device.offline" {
				return true
			}
		}
		return false
	})
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Telemetry != nil && strings.HasPrefix(e.Telemetry.DeviceID, "local:")
	})
	fixture.unreachable.Store(false)
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		for _, event := range e.Events {
			if event.Type == "device.online" {
				return true
			}
		}
		return false
	})
}
func TestFullOutboxRejectsCommandBeforeWrite(t *testing.T) {
	c := testConfig(t, "127.0.0.1:9443")
	fixture := &remoteRouterFixture{}
	configureRouterFixture(t, &c, fixture)
	a := newTestAgent(t, c, newFixturePlatform())
	defer a.remote.Close()
	for i := 0; i < c.Queue.MaxMessages; i++ {
		if err := a.push(model.Envelope{CommandResult: &model.CommandResult{CommandID: strconv.Itoa(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	command := model.Command{ID: "blocked", TargetDeviceID: "remote-router", RequestedAt: time.Now().UTC(), Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	if err := a.handleCommand(context.Background(), command); err == nil || fixture.writes.Load() != 0 {
		t.Fatal("full queue admitted mutation")
	}
}
