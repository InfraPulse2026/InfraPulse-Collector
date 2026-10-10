package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	driveripmi "InfraPulseAgent/internal/driver/ipmi"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/transport"
)

func ipmiFixtureConfig(t *testing.T, c *config.Config, bmc *ipmitest.BMC) {
	t.Helper()
	credential := bmc.Credential()
	t.Setenv("IPMI_E2E_USER", credential.Username)
	t.Setenv("IPMI_E2E_PASSWORD", credential.Password)
	c.Local.Enabled = false
	c.Credentials = map[string]credentials.Reference{"ipmi-fixture": {UsernameEnv: "IPMI_E2E_USER", PasswordEnv: "IPMI_E2E_PASSWORD"}}
	port, suite := bmc.Port, 17
	c.Devices = []config.Device{{ID: "legacy-fixture", Driver: "ipmi", Address: bmc.Address, CredentialRef: "ipmi-fixture", Polling: config.Polling{MediumInterval: 150 * time.Millisecond, InventoryInterval: 300 * time.Millisecond, Timeout: 2 * time.Second}, IPMI: config.IPMI{Port: &port, CipherSuite: &suite, Provenance: "simulated", RequestTimeout: 200 * time.Millisecond, MaxSessionDuration: 2 * time.Second}}}
	c.Telemetry.HeartbeatInterval = time.Hour
	configureDurable(c)
	c.Queue.MaxMessages = 128
}

func TestIPMIRegisteredReadOnlyContracts(t *testing.T) {
	c, err := config.Parse([]byte(`security:
  mode: development
credentials:
  monitor:
    username_env: IPMI_TEST_USER
    password_env: IPMI_TEST_PASSWORD
devices:
  - id: bmc
    driver: ipmi
    address: 192.0.2.1
    credential_ref: monitor
`))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := RemoteAdapterFactory(c, credentials.NewEnvironment(c.Credentials))(c.Devices[0])
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if _, ok := adapter.(*driveripmi.Adapter); !ok {
		t.Fatal("IPMI registration missing")
	}
	if _, ok := adapter.(driver.InventoryBatchReader); !ok {
		t.Fatal("IPMI must reuse hardware inventory reader")
	}
	if _, ok := adapter.(driver.HardwareReader); !ok {
		t.Fatal("IPMI must reuse hardware reader")
	}
	if _, ok := adapter.(driver.InterfaceManager); ok {
		t.Fatal("read-only IPMI exposes mutation contract")
	}
	if _, ok := adapter.(driver.Driver); ok {
		t.Fatal("hardware adapter exposes fake network/topology contracts")
	}
}

// Uses real UDP LANplus/RAKP and the library reference BMC's native storage,
// with explicit deterministic Sensor Reading handlers absent upstream. This is
// a reference-BMC + protocol-fixture E2E, never a production hardware test.
func TestAgentIPMIReferenceFixtureDurableOfflineRestartACK(t *testing.T) {
	bmc := ipmitest.Start(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	c := testConfig(t, address)
	ipmiFixtureConfig(t, &c, bmc)
	c.Queue.MaxInflight = c.Queue.MaxMessages
	c.Queue.ACKTimeout = 30 * time.Second
	a := newTestAgent(t, c, nil)
	stopA := runTestAgent(t, a)
	awaitP2(t, func() bool {
		states := a.Snapshot().RemoteDevices
		return len(states) == 1 && states[0].ConnectivityState == "online" && len(states[0].Devices) >= 2 && a.queue.State().Messages >= 4
	})
	stopA()
	b := newTestAgent(t, c, nil)
	var pendingInventory, pendingTelemetry model.Envelope
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for pendingInventory.Inventory == nil || pendingTelemetry.Telemetry == nil {
		e, err := b.queue.Pop(ctx)
		if err != nil {
			t.Fatal("IPMI observations were not durably admitted", err, a.Snapshot().RecentErrors)
		}
		if e.Inventory != nil && len(e.Inventory.Devices) >= 2 {
			pendingInventory = e
		}
		if e.Telemetry != nil && len(e.Telemetry.Metrics) > 0 {
			pendingTelemetry = e
		}
	}
	b.queue.ResetInflight()
	observed := make(chan model.Envelope, 512)
	_ = startTCPController(t, address, mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	stopB := runTestAgent(t, b)
	defer stopB()
	replayed := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == pendingInventory.MessageID })
	assertIPMIInventory(t, replayed.Inventory, b.Snapshot().AgentID)
	metrics := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == pendingTelemetry.MessageID })
	assertIPMIMetrics(t, metrics.Telemetry)
	if replayed.AgentID != pendingInventory.AgentID || replayed.SessionID == pendingInventory.SessionID {
		t.Fatal("restart broke durable identity/session binding")
	}
	awaitP2(t, func() bool {
		return !b.queue.Contains(pendingInventory.MessageID) && !b.queue.Contains(pendingTelemetry.MessageID)
	})
	fresh := waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Inventory != nil && e.MessageID != pendingInventory.MessageID && len(e.Inventory.Devices) >= 2
	})
	ids := map[string]string{}
	for _, d := range replayed.Inventory.Devices {
		ids[d.Observation.Source.ResourceID] = d.ID
		for _, component := range d.Components {
			ids[component.Observation.Source.ResourceID] = component.ID
		}
	}
	for _, d := range fresh.Inventory.Devices {
		if ids[d.Observation.Source.ResourceID] != d.ID {
			t.Fatal("device identity changed across restart")
		}
		for _, component := range d.Components {
			if ids[component.Observation.Source.ResourceID] != component.ID {
				t.Fatal("component identity changed across restart")
			}
		}
	}
	data, _ := json.Marshal(b.Snapshot())
	if strings.Contains(string(data), bmc.Credential().Password) || strings.Contains(string(data), bmc.Credential().Username) {
		t.Fatal("credentials leaked into diagnostics")
	}
	stopB()
	awaitP2(t, func() bool { return bmc.ActiveSessions() == 0 })
	t.Logf("LANplus suite17 USER; devices=%d; metrics=%d; durable offline/restart/replay/ACK; active sessions=%d", len(replayed.Inventory.Devices), len(metrics.Telemetry.Metrics), bmc.ActiveSessions())
}

func assertIPMIInventory(t *testing.T, inventory *model.InventoryUpdate, collector string) {
	t.Helper()
	var manager, fru bool
	for _, d := range inventory.Devices {
		if d.Hardware == nil || d.Observation == nil || d.Observation.Source.Type != model.SourceIPMI || d.Observation.Source.Protocol != "ipmi" || d.Observation.Source.TargetID != "remote:legacy-fixture" || d.Observation.Source.CollectorID != collector || d.Observation.Source.Provenance != model.ProvenanceSimulated || d.Source != "simulated" || d.ID == "legacy-fixture" {
			t.Fatal("IPMI source/identity/wire contract lost", d)
		}
		if d.Hardware.ResourceType == "Manager" {
			manager = true
			if d.SerialNumber != "" || d.Hardware.BIOSVersion != "" || d.FirmwareVersion != "1.23" || len(d.VendorEvidence) == 0 {
				t.Fatal("BMC identity/version must stay scoped", d)
			}
		}
		for _, component := range d.Components {
			if component.SerialNumber != "" {
				fru = true
			}
			if component.Observation == nil || component.Observation.Source.Type != model.SourceIPMI {
				t.Fatal("component source lost")
			}
		}
	}
	if !manager || !fru {
		t.Fatal("real fixture DeviceID/FRU evidence missing")
	}
}

func assertIPMIMetrics(t *testing.T, sample *model.TelemetrySample) {
	t.Helper()
	if sample.Observation == nil || sample.Observation.Source.Type != model.SourceIPMI || sample.Observation.Time.SourceTimestamp != nil || len(sample.HardwareHealth) == 0 {
		t.Fatal("IPMI observation lifecycle lost")
	}
	want := map[string]struct {
		value float64
		unit  string
	}{"hardware.temperature.celsius": {42, "Cel"}, "hardware.fan.rpm": {1200, "RPM"}, "hardware.voltage.volts": {12, "V"}, "hardware.power.watts": {150, "W"}}
	for _, m := range sample.Metrics {
		if w, ok := want[m.Name]; ok {
			if m.Value != w.value || m.Unit != w.unit || m.Observation == nil || m.Observation.Source.Type != model.SourceIPMI || m.ComponentID == "" || m.ResourceID == "" {
				t.Fatal("fixture conversion or provenance changed", m)
			}
			delete(want, m.Name)
		}
	}
	if len(want) > 0 {
		t.Fatal("IPMI metrics missing", want)
	}
}

func TestRedfishIPMICompatibleSensorSemantics(t *testing.T) {
	bmc := ipmitest.Start(t)
	c := testConfig(t, "127.0.0.1:9999")
	ipmiFixtureConfig(t, &c, bmc)
	getSamples := func(cfg config.Config) []model.TelemetrySample {
		adapter, err := RemoteAdapterFactory(cfg, credentials.NewEnvironment(cfg.Credentials))(cfg.Devices[0])
		if err != nil {
			t.Fatal(err)
		}
		defer adapter.Close()
		reader, ok := adapter.(driver.HardwareReader)
		if !ok {
			t.Fatal("hardware contract missing")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		samples, err := reader.Hardware(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return samples
	}
	ipmiSamples := getSamples(c)
	fixture := newRedfishAgentFixture()
	fans := fixture.resources["/redfish/v1/Chassis/C/Thermal"].(map[string]any)["Fans"].([]any)
	fans[0].(map[string]any)["Reading"] = 1200
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	ca := filepath.Join(testTempDir(t), "redfish-comparison-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	rfConfig := testConfig(t, "127.0.0.1:9999")
	redfishFixtureConfig(t, &rfConfig, server.URL, ca, "session")
	rfSamples := getSamples(rfConfig)
	for _, entry := range []struct {
		name  string
		value float64
		unit  string
	}{{"hardware.temperature.celsius", 42, "Cel"}, {"hardware.fan.rpm", 1200, "RPM"}} {
		for _, source := range []struct {
			samples []model.TelemetrySample
			kind    model.SourceType
		}{{ipmiSamples, model.SourceIPMI}, {rfSamples, model.SourceRedfish}} {
			found := false
			for _, sample := range source.samples {
				for _, metric := range sample.Metrics {
					if metric.Name == entry.name && metric.Value == entry.value {
						found = true
						if metric.Unit != entry.unit || metric.Observation == nil || metric.Observation.Source.Type != source.kind || metric.Observation.Source.Protocol != string(source.kind) {
							t.Fatal("hardware semantics/source mismatch", metric)
						}
					}
				}
			}
			if !found {
				t.Fatal("equivalent sensor observation missing", source.kind, entry)
			}
		}
	}
}

func TestIPMIOfflineTargetIsolationAndBoundedShutdown(t *testing.T) {
	bmc := ipmitest.Start(t)
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	observed := make(chan model.Envelope, 512)
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	ipmiFixtureConfig(t, &c, bmc)
	c.Remote.Concurrency = 2
	offline := c.Devices[0]
	offline.ID = "offline-bmc"
	port := blackhole.LocalAddr().(*net.UDPAddr).Port
	offline.IPMI.Port = &port
	offline.IPMI.RequestTimeout = 30 * time.Millisecond
	offline.IPMI.MaxSessionDuration = 100 * time.Millisecond
	offline.Polling.Timeout = 100 * time.Millisecond
	c.Devices = append([]config.Device{offline}, c.Devices...)
	a := newTestAgent(t, c, nil)
	stop := runTestAgent(t, a)
	defer stop()
	envelope := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Telemetry != nil && len(e.Telemetry.Metrics) >= 4 })
	assertIPMIMetrics(t, envelope.Telemetry)
	awaitP2(t, func() bool {
		states := a.Snapshot().RemoteDevices
		return len(states) == 2 && states[0].ConnectivityState == "offline" && states[1].ConnectivityState == "online"
	})
	stop()
	awaitP2(t, func() bool { return bmc.ActiveSessions() == 0 })
}

type ipmiObservedTransport struct {
	transport.Transport
	observe func(model.Envelope)
}

func (t *ipmiObservedTransport) Connect(ctx context.Context) (transport.Session, error) {
	session, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &ipmiObservedSession{Session: session, observe: t.observe}, nil
}

type ipmiObservedSession struct {
	transport.Session
	observe func(model.Envelope)
}

func (s *ipmiObservedSession) Send(e model.Envelope) error {
	s.observe(e)
	return s.Session.Send(e)
}

func (s *ipmiObservedSession) CloseSend() error {
	if closer, ok := s.Session.(transport.SendCloser); ok {
		return closer.CloseSend()
	}
	return nil
}

// Both protocols poll concurrently in the real Agent registry on one management
// IP. Separate configured targets must retain their own asset IDs and sources.
func TestAgentMixedRedfishIPMITargetIsolationAndACK(t *testing.T) {
	bmc := ipmitest.Start(t)
	redfish := newRedfishAgentFixture()
	server := httptest.NewTLSServer(redfish)
	t.Cleanup(server.Close)
	if server.Listener.Addr().(*net.TCPAddr).IP.String() != bmc.Address {
		t.Fatal("mixed-protocol fixture must use the same management IP")
	}
	ca := filepath.Join(testTempDir(t), "mixed-bmc-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	observed := make(chan model.Envelope, 512)
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	ipmiFixtureConfig(t, &c, bmc)
	rfConfig := testConfig(t, controller.listener.Addr().String())
	redfishFixtureConfig(t, &rfConfig, server.URL, ca, "session")
	c.Credentials["redfish-fixture"] = rfConfig.Credentials["redfish-fixture"]
	offline := c.Devices[0]
	offline.ID = "offline-bmc"
	port := blackhole.LocalAddr().(*net.UDPAddr).Port
	offline.IPMI.Port = &port
	offline.IPMI.RequestTimeout = 30 * time.Millisecond
	offline.IPMI.MaxSessionDuration = 100 * time.Millisecond
	offline.Polling.Timeout = 100 * time.Millisecond
	c.Devices = append([]config.Device{offline}, append(c.Devices, rfConfig.Devices...)...)
	c.Remote.Concurrency = 2
	a := newTestAgent(t, c, nil)
	// Legacy Event protobuf has no Observation. Capture metadata at the typed
	// Send boundary and correlate by event ID with what the Controller receives.
	var eventMu sync.Mutex
	normalizedEvents := map[string]model.Event{}
	a.transport = &ipmiObservedTransport{Transport: a.transport, observe: func(e model.Envelope) {
		eventMu.Lock()
		defer eventMu.Unlock()
		for _, event := range e.Events {
			event.Observation = model.CloneObservation(event.Observation)
			normalizedEvents[event.ID] = event
		}
	}}
	stop := runTestAgent(t, a)
	defer stop()
	collector := a.Snapshot().AgentID
	inventories := map[model.SourceType]model.Envelope{}
	telemetries := map[model.SourceType]model.Envelope{}
	lifecycle := map[string]bool{}
	var acknowledged []string
	var offlineAt time.Time
	checkSource := func(o *model.ObservationMetadata, kind model.SourceType, target string) {
		t.Helper()
		if o == nil || o.Source.Type != kind || o.Source.Protocol != string(kind) || o.Source.TargetID != target || o.Source.CollectorID != collector || o.Source.Provenance != model.ProvenanceSimulated {
			t.Fatalf("mixed-target observation lost its configured source: %+v", o)
		}
	}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.Inventory != nil && len(e.Inventory.Devices) > 0 {
			if e.Inventory.Devices[0].Observation == nil {
				t.Fatal("hardware inventory lost source metadata")
			}
			kind := e.Inventory.Devices[0].Observation.Source.Type
			if _, exists := inventories[kind]; !exists {
				inventories[kind] = e
				acknowledged = append(acknowledged, e.MessageID)
			}
		}
		if e.Telemetry != nil && len(e.Telemetry.Metrics) > 0 {
			if e.Telemetry.Observation == nil {
				t.Fatal("hardware telemetry lost source metadata")
			}
			kind := e.Telemetry.Observation.Source.Type
			if _, exists := telemetries[kind]; !exists {
				telemetries[kind] = e
				acknowledged = append(acknowledged, e.MessageID)
			}
		}
		for _, event := range e.Events {
			if event.Type != "target.online" && event.Type != "target.offline" {
				continue
			}
			eventMu.Lock()
			normalized, exists := normalizedEvents[event.ID]
			eventMu.Unlock()
			if !exists || normalized.Observation == nil || event.Source != "simulated" {
				t.Fatal("target event lost normalized metadata or wire simulated marker", event)
			}
			ref := normalized.Observation.Source
			checkSource(normalized.Observation, ref.Type, ref.TargetID)
			if event.DeviceID != "" {
				t.Fatal("target connectivity event was bound to an equipment ID")
			}
			lifecycle[ref.TargetID+"/"+event.Type] = true
			if ref.TargetID == "remote:offline-bmc" && event.Type == "target.offline" {
				offlineAt = event.Timestamp
			}
			acknowledged = append(acknowledged, e.MessageID)
		}
		return len(inventories) == 2 && len(telemetries) == 2 && lifecycle["remote:legacy-fixture/target.online"] && lifecycle["remote:bmc-fixture/target.online"] && lifecycle["remote:offline-bmc/target.offline"]
	})
	assertIPMIInventory(t, inventories[model.SourceIPMI].Inventory, collector)
	assertRedfishInventory(t, inventories[model.SourceRedfish].Inventory, collector)
	assertIPMIMetrics(t, telemetries[model.SourceIPMI].Telemetry)
	seen := map[string]model.SourceType{}
	for _, entry := range []struct {
		kind   model.SourceType
		target string
	}{{model.SourceIPMI, "remote:legacy-fixture"}, {model.SourceRedfish, "remote:bmc-fixture"}} {
		checkSource(inventories[entry.kind].Inventory.Observation, entry.kind, entry.target)
		for _, device := range inventories[entry.kind].Inventory.Devices {
			checkSource(device.Observation, entry.kind, entry.target)
			if previous, exists := seen[device.ID]; exists {
				t.Fatalf("same management IP merged observed assets: ID=%s protocols=%s/%s", device.ID, previous, entry.kind)
			}
			seen[device.ID] = entry.kind
		}
		checkSource(telemetries[entry.kind].Telemetry.Observation, entry.kind, entry.target)
	}
	awaitP2(t, func() bool {
		for _, id := range acknowledged {
			if a.queue.Contains(id) {
				return false
			}
		}
		return true
	})
	awaitP2(t, func() bool {
		states := a.Snapshot().RemoteDevices
		return len(states) == 3 && states[0].ConnectivityState == "offline" && states[1].ConnectivityState == "online" && states[2].ConnectivityState == "online" && len(states[1].Devices) == 2 && len(states[2].Devices) == 4
	})
	// Demand a subsequent Redfish poll after the offline threshold has been
	// reached, rather than accepting only observations from the initial tick.
	fresh := waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Telemetry != nil && e.Telemetry.Observation != nil && len(e.Telemetry.Metrics) > 0 && e.Telemetry.Observation.Source.Type == model.SourceRedfish && e.Telemetry.CollectedAt.After(offlineAt)
	})
	checkSource(fresh.Telemetry.Observation, model.SourceRedfish, "remote:bmc-fixture")
	awaitP2(t, func() bool { return !a.queue.Contains(fresh.MessageID) })
	stop()
	awaitP2(t, func() bool { return bmc.ActiveSessions() == 0 })
	if redfish.logins.Load() == 0 || redfish.logins.Load() != redfish.logouts.Load() || redfish.writes.Load() != 0 {
		t.Fatalf("mixed Redfish lifecycle leaked or mutated equipment: login=%d logout=%d writes=%d", redfish.logins.Load(), redfish.logouts.Load(), redfish.writes.Load())
	}
}
