package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"github.com/gosnmp/gosnmp"
)

// The four real adapters read isolated protocol fixtures. Explicit simulated
// markers are applied by this test; a production SNMP/RouterOS connection never
// becomes simulated just because it uses the same adapter or loopback address.
func TestPhase6AdaptersIdentityTopologyDurableReplayACK(t *testing.T) {
	blackhole := startACKBlackhole(t)
	originals := make(chan model.Envelope, 512)
	pb.RegisterAgentServiceServer(blackhole.server, &noACKController{observed: originals})
	go func() { blackhole.finished <- blackhole.server.Serve(blackhole.listener) }()
	c := testConfig(t, blackhole.listener.Addr().String())
	c.Local.Enabled, c.Commands.Enabled = false, false
	c.Devices = nil // Reads below are independent; there are no remote poll jobs.
	c.Telemetry.HeartbeatInterval = time.Hour
	configureDurable(&c)
	c.Queue.MaxMessages, c.Queue.MaxInflight, c.Queue.ACKTimeout = 256, 256, 30*time.Second
	a := newTestAgent(t, c, nil)

	snmpAddress, snmpPort := phase6SNMPFixture(t)
	snmpConfig := config.Defaults()
	t.Setenv("PHASE6_SNMP_COMMUNITY", "phase6-private-community")
	snmpConfig.Credentials = map[string]credentials.Reference{"snmp": {CommunityEnv: "PHASE6_SNMP_COMMUNITY"}}
	snmpConfig.Devices = []config.Device{{ID: "phase6-switch", Driver: "snmp", Address: snmpAddress, CredentialRef: "snmp", SNMP: config.SNMP{Port: snmpPort}, Polling: config.Polling{Timeout: time.Second}}}
	routerConfig := config.Defaults()
	routerFixture := &remoteRouterFixture{}
	routerServer := configureRouterFixture(t, &routerConfig, routerFixture)
	// The wrapper adds only neighbor facts; authenticated HTTPS requests still
	// execute the production fixed-path adapter and the original interface fixture.
	routerHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/ip/neighbor" {
			routerFixture.ServeHTTP(w, r)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "monitor" || password != "private-fixture-password" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{{"interface": "ether1", "identity": "unresolved-fixture-neighbor", "mac-address": "02:10:00:00:00:01", "address": "192.0.2.99", "discovered-by": "lldp,cdp,mndp", "age": "1s", "platform": "fixture-platform", "board": "fixture-board", "version": "7.20", "interface-name": "ether9", "system-caps": "bridge,router", "system-caps-enabled": "bridge"}})
	})
	routerServer.Close()
	wrappedRouter := httptest.NewUnstartedServer(routerHandler)
	wrappedRouter.TLS = routerServer.TLS.Clone()
	wrappedRouter.StartTLS()
	t.Cleanup(wrappedRouter.Close)
	routerConfig.Devices[0].Address = wrappedRouter.URL
	routerConfig.Commands.Enabled, routerConfig.Devices[0].EnableMutations = false, false
	routerConfig.Devices[0].ID = "phase6-router"
	redfishFixture := newRedfishAgentFixture()
	redfishFixture.resources["/redfish/v1/Systems/A"].(map[string]any)["SerialNumber"] = "SYSTEM-SERIAL"
	redfishServer := httptest.NewTLSServer(redfishFixture)
	t.Cleanup(redfishServer.Close)
	ca := filepath.Join(testTempDir(t), "phase6-bmc-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redfishServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	redfishConfig := config.Defaults()
	redfishFixtureConfig(t, &redfishConfig, redfishServer.URL, ca, "session")
	redfishConfig.Devices[0].ID = "phase6-redfish"
	bmc := ipmitest.Start(t)
	ipmiConfig := config.Defaults()
	ipmiFixtureConfig(t, &ipmiConfig, bmc)
	ipmiConfig.Devices[0].ID = "phase6-ipmi"
	if redfishServer.Listener.Addr().(*net.TCPAddr).IP.String() != bmc.Address {
		t.Fatal("fixture endpoints must share management IP to exercise independent source scopes")
	}

	type pendingObservation struct {
		target   model.Target
		envelope model.Envelope
	}
	var pending []pendingObservation
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, cfg := range []config.Config{snmpConfig, routerConfig, redfishConfig, ipmiConfig} {
		dc := cfg.Devices[0]
		target := dc.Target(config.Polling{})
		adapter, err := RemoteAdapterFactory(cfg, credentials.NewEnvironment(cfg.Credentials))(dc)
		if err != nil {
			t.Fatal(dc.Driver, err)
		}
		func() {
			defer adapter.Close()
			appendObservation := func(e model.Envelope) {
				e = phase6MarkFixture(e, target)
				e.MessageID = fmt.Sprintf("phase6-%s-%d", dc.Driver, len(pending))
				pending = append(pending, pendingObservation{target, e})
			}
			if reader, ok := adapter.(driver.InventoryBatchReader); ok {
				inventory, err := reader.InventoryBatch(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "inventory", err)
				}
				if len(inventory.Devices) < 2 {
					t.Fatal(dc.Driver, "expected separately scoped hardware resources")
				}
				appendObservation(model.Envelope{Inventory: &inventory})
			} else if reader, ok := adapter.(driver.InventoryReader); ok {
				device, err := reader.Inventory(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "inventory", err)
				}
				appendObservation(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: time.Now().UTC(), Devices: []model.Device{device}}})
			} else {
				t.Fatal(dc.Driver, "inventory reader missing")
			}
			if reader, ok := adapter.(driver.TopologyReader); ok {
				first, err := reader.Topology(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "topology", err)
				}
				second, err := reader.Topology(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "repeat topology", err)
				}
				ref := model.SourceRef{Type: target.Type, CollectorID: a.id.AgentID, TargetID: target.ID, Protocol: target.Protocol, Provenance: model.ProvenanceSimulated}
				one := model.ObserveEnvelope(phase6MarkFixture(model.Envelope{Topology: &first}, target), ref, time.Now().UTC())
				two := model.ObserveEnvelope(phase6MarkFixture(model.Envelope{Topology: &second}, target), ref, time.Now().UTC())
				if len(one.Topology.Observations) == 0 || len(one.Topology.Observations) != len(two.Topology.Observations) {
					t.Fatal(dc.Driver, "topology fixture missing")
				}
				for i, n := range one.Topology.Observations {
					if n.ObservationID != two.Topology.Observations[i].ObservationID {
						t.Fatal(dc.Driver, "repeated poll changed observation ID")
					}
				}
				appendObservation(model.Envelope{Topology: &first})
			}
			if reader, ok := adapter.(driver.HardwareReader); ok {
				samples, err := reader.Hardware(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "hardware", err)
				}
				if len(samples) == 0 {
					t.Fatal(dc.Driver, "hardware fixture missing")
				}
				for _, sample := range samples {
					appendObservation(model.Envelope{Telemetry: &sample})
				}
			} else if reader, ok := adapter.(driver.TelemetryReader); ok {
				sample, err := reader.Collect(ctx)
				if err != nil {
					t.Fatal(dc.Driver, "telemetry", err)
				}
				appendObservation(model.Envelope{Telemetry: &sample})
			}
		}()
	}
	wanted := make(map[string]model.Target, len(pending))
	for _, item := range pending {
		if err := a.emitTarget(item.target, item.envelope); err != nil {
			t.Fatal("durable admission", item.envelope.MessageID, err)
		}
		wanted[item.envelope.MessageID] = item.target
	}
	stopA := runTestAgent(t, a)
	firstMessages := make(map[string]model.Envelope, len(pending))
	waitEnvelope(t, originals, func(e model.Envelope) bool {
		if target, ok := wanted[e.MessageID]; ok {
			phase6AssertSource(t, e, target, a.id.AgentID)
			firstMessages[e.MessageID] = e
		}
		return len(firstMessages) == len(wanted)
	})
	for id := range wanted {
		if !a.queue.Contains(id) {
			t.Fatal("Send removed record without application ACK", id)
		}
	}
	phase6AssertEvidenceAndTopology(t, firstMessages)
	stopA()
	address := blackhole.listener.Addr().String()
	blackhole.stop()
	replayed := make(chan model.Envelope, 512)
	_ = startTCPController(t, address, mockcontroller.New(io.Discard, nil, observeChannel(replayed)))
	b := newTestAgent(t, c, nil)
	stopB := runTestAgent(t, b)
	defer stopB()
	seen := make(map[string]bool, len(wanted))
	waitEnvelope(t, replayed, func(e model.Envelope) bool {
		if target, ok := wanted[e.MessageID]; ok {
			phase6AssertSource(t, e, target, b.id.AgentID)
			old := firstMessages[e.MessageID]
			if old.AgentID != e.AgentID || old.SessionID == e.SessionID {
				t.Fatal("restart source/session identity mismatch")
			}
			old.SessionID, old.SentAt, old.Sequence = e.SessionID, e.SentAt, e.Sequence
			if !reflect.DeepEqual(old, e) {
				t.Fatalf("replay changed evidence/time/subjects: %s", e.MessageID)
			}
			seen[e.MessageID] = true
		}
		return len(seen) == len(wanted)
	})
	awaitP2(t, func() bool {
		for id := range wanted {
			if b.queue.Contains(id) {
				return false
			}
		}
		return true
	})
	if routerFixture.writes.Load() != 0 || redfishFixture.writes.Load() != 0 {
		t.Fatal("identity enrichment executed mutation")
	}
	t.Logf("SNMP UDP + verified RouterOS/Redfish HTTPS + IPMI LANplus reference BMC; %d source observations retained, restarted, replayed and ACKed", len(wanted))
}

func phase6MarkFixture(e model.Envelope, target model.Target) model.Envelope {
	if e.Inventory != nil {
		for i := range e.Inventory.Devices {
			e.Inventory.Devices[i].Source = "simulated"
		}
	}
	if e.Telemetry != nil {
		e.Telemetry.Source = "simulated"
	}
	if e.Topology != nil {
		e.Topology.Source = "simulated"
		for i := range e.Topology.Observations {
			e.Topology.Observations[i].Source = "simulated"
		}
	}
	ref := model.SourceRef{Type: target.Type, TargetID: target.ID, Protocol: target.Protocol, Provenance: model.ProvenanceSimulated}
	return model.ObserveEnvelope(e, ref, time.Now().UTC())
}

func phase6AssertSource(t *testing.T, e model.Envelope, target model.Target, collector string) {
	t.Helper()
	check := func(o *model.ObservationMetadata) {
		t.Helper()
		if o == nil || o.Source.TargetID != target.ID || o.Source.CollectorID != collector || o.Source.Provenance != model.ProvenanceSimulated || o.Source.Protocol == "" {
			t.Fatalf("source metadata lost: %s %+v", e.MessageID, o)
		}
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
		if o.Time.SourceTimestamp != nil || o.Time.ClockOffset != nil || o.Time.Uncertainty != nil || o.Time.Quality != model.ClockUnknown {
			t.Fatal("fixture fabricated source clock measurement")
		}
	}
	facts := func(values []model.IdentityEvidence, owner string) {
		t.Helper()
		for _, fact := range values {
			// Empty top-level SubjectID keeps the legacy owner-container meaning.
			// Neighbor and nested interface/component facts need their own scope.
			if fact.Source.TargetID != target.ID || fact.Source.CollectorID != collector || fact.Source.Provenance != model.ProvenanceSimulated || fact.SubjectID == "" && owner == "" || fact.SubjectKind == "" || fact.Time == nil {
				t.Fatalf("identity fact scope/provenance/time lost: %+v", fact)
			}
			if err := fact.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if u := e.Inventory; u != nil {
		check(u.Observation)
		for _, d := range u.Devices {
			check(d.Observation)
			if (d.SerialNumber != "" || d.Hostname != "" || d.Model != "" || d.Vendor != "" || d.Hardware != nil && d.Hardware.HardwareUUID != "") && len(d.IdentityEvidence) == 0 {
				t.Fatal("available inventory identity evidence lost", e.MessageID)
			}
			facts(d.IdentityEvidence, d.ID)
			for _, component := range d.Components {
				check(component.Observation)
			}
		}
	}
	if s := e.Telemetry; s != nil {
		check(s.Observation)
		if target.Type == model.SourceSNMP || target.Type == model.SourceRouterOS {
			if len(s.Interfaces) > 0 && len(s.IdentityEvidence) == 0 {
				t.Fatal("interface identity evidence lost", e.MessageID)
			}
			facts(s.IdentityEvidence, s.DeviceID)
		}
		for _, metric := range s.Metrics {
			if metric.ResourceID != "" {
				check(metric.Observation)
			}
		}
		for _, health := range s.HardwareHealth {
			check(health.Observation)
		}
	}
	if u := e.Topology; u != nil {
		check(u.Observation)
		for _, n := range u.Observations {
			check(n.Observation)
			if len(n.IdentityEvidence) == 0 {
				t.Fatal("neighbor identity evidence lost", e.MessageID)
			}
			facts(n.IdentityEvidence, "")
			if n.Kind != model.TopologyObservedL2Neighbor || n.ObservationID == "" {
				t.Fatal("directed observation semantics lost")
			}
		}
	}
}

func phase6AssertEvidenceAndTopology(t *testing.T, messages map[string]model.Envelope) {
	t.Helper()
	sources := make(map[model.SourceType]bool)
	devices := make(map[string]model.SourceType)
	serialSources := make(map[model.SourceType]bool)
	protocols := make(map[string]string)
	for _, e := range messages {
		if u := e.Inventory; u != nil {
			for _, d := range u.Devices {
				kind := d.Observation.Source.Type
				sources[kind] = true
				if previous, ok := devices[d.ID]; ok && previous != kind {
					t.Fatal("different protocols were merged into a single device ID")
				}
				devices[d.ID] = kind
				for _, f := range d.IdentityEvidence {
					if f.Type == model.IdentitySerial && f.Value == "SYSTEM-SERIAL" {
						serialSources[kind] = true
					}
				}
			}
		}
		if u := e.Topology; u != nil {
			for _, n := range u.Observations {
				if n.LocalInterfaceID == "" || n.LocalInterfaceName == "" || n.LocalInterfaceMAC == "" || len(n.RemoteChassisID) == 0 || n.RemoteSystemName == "" || n.ExpiresAt.IsZero() {
					t.Fatal("topology endpoint evidence missing", n)
				}
				if u.DeviceID == "phase6-switch" {
					if n.Protocol != "lldp" || n.LocalPortNumber != 7 || n.LocalInterfaceIndex == nil || *n.LocalInterfaceIndex != 42 || n.RemoteChassisSubtype != 4 || n.RemotePortSubtype != 5 || string(n.RemotePortID) != "uplink" {
						t.Fatal("SNMP LLDP raw/subtype/local interface evidence changed", n)
					}
				}
				if u.DeviceID == "phase6-router" {
					if n.RemoteChassisSubtype != 0 || len(n.RemoteManagementAddresses) != 1 || n.AgeSeconds == nil || *n.AgeSeconds != 1 {
						t.Fatal("RouterOS source facts were invented or dropped", n)
					}
					if n.Protocol != "cdp" && len(n.RemotePortID) != 0 {
						t.Fatal("unknown remote port was fabricated")
					}
					if previous, ok := protocols[n.Protocol]; ok && previous != n.ObservationID {
						t.Fatal("unexpected duplicated router protocol")
					}
					protocols[n.Protocol] = n.ObservationID
				}
			}
		}
	}
	for _, kind := range []model.SourceType{model.SourceSNMP, model.SourceRouterOS, model.SourceRedfish, model.SourceIPMI} {
		if !sources[kind] {
			t.Fatal("adapter inventory missing", kind)
		}
	}
	if !serialSources[model.SourceRedfish] || !serialSources[model.SourceIPMI] {
		t.Fatal("overlapping cross-protocol serial evidence missing")
	}
	if len(protocols) != 3 || protocols["lldp"] == protocols["cdp"] || protocols["lldp"] == protocols["mndp"] || protocols["cdp"] == protocols["mndp"] {
		t.Fatal("multi-protocol neighbor facts were merged")
	}
}

// Bounded UDP serving loop shares the existing SYSTEM/IF/ENTITY/LLDP fixture
// values. Unsupported OIDs remain absent; no synthetic production driver exists.
func phase6SNMPFixture(t *testing.T) (string, uint16) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		decoder := &gosnmp.GoSNMP{Version: gosnmp.Version2c}
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			request, err := decoder.SnmpDecodePacket(buf[:n])
			if err != nil || request.Community != "phase6-private-community" {
				continue
			}
			values := snmpE2EValues(started)
			response := &gosnmp.SnmpPacket{Version: request.Version, Community: request.Community, RequestID: request.RequestID, PDUType: gosnmp.GetResponse}
			if request.PDUType == gosnmp.GetRequest {
				for _, v := range request.Variables {
					found := false
					for _, candidate := range values {
						if candidate.Name == v.Name {
							response.Variables = append(response.Variables, candidate)
							found = true
							break
						}
					}
					if !found {
						response.Variables = append(response.Variables, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.NoSuchObject})
					}
				}
			} else if request.PDUType == gosnmp.GetBulkRequest || request.PDUType == gosnmp.GetNextRequest {
				if len(request.Variables) == 0 {
					continue
				}
				maximum := min(max(int(request.MaxRepetitions), 1), 100)
				cursor := request.Variables[0].Name
				for _, v := range values {
					if snmpE2EOIDCompare(v.Name, cursor) > 0 {
						response.Variables = append(response.Variables, v)
						if len(response.Variables) == maximum {
							break
						}
					}
				}
				if len(response.Variables) == 0 {
					response.Variables = []gosnmp.SnmpPDU{{Name: cursor, Type: gosnmp.EndOfMibView}}
				}
			} else {
				continue
			}
			wire, err := response.MarshalMsg()
			if err == nil {
				_, _ = conn.WriteTo(wire, peer)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	host, port, _ := net.SplitHostPort(conn.LocalAddr().String())
	number, _ := strconv.ParseUint(port, 10, 16)
	return host, uint16(number)
}
