package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/protobuf/proto"
)

// This checks the stress fixture's marker through the actual admission/outbox
// boundary and the same protobuf decoding used by Mock Controller. It is a
// wire-mapping fixture, not a claim of production Backend or hardware execution.
func TestPhase8SimulatedPayloadsSurviveDurableWireRoundTrip(t *testing.T) {
	c := testConfig(t, "127.0.0.1:19443")
	c.Local.Enabled = false
	configureDurable(&c)
	a := newTestAgent(t, c, nil)
	t.Cleanup(func() { _ = a.remote.Close() })
	at := time.Date(2026, 10, 11, 12, 0, 0, 123456789, time.UTC)
	ref := model.SourceRef{Type: model.SourceSNMP, CollectorID: a.id.AgentID, TargetID: "remote:phase8-fixture", Protocol: "snmp", Provenance: model.ProvenanceReal}
	cases := []struct {
		name string
		ref  model.SourceRef
		e    model.Envelope
	}{
		{"inventory", ref, model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: at, Devices: []model.Device{{ID: "fixture-switch", Name: "fixture", Type: "switch", Source: "snmp", Hostname: "fixture.example", SerialNumber: "FIXTURE-01", Components: []model.InventoryComponent{{ID: "fixture-component", ParentID: "fixture-switch", Class: "chassis", SerialNumber: "FIXTURE-CHASSIS"}}}}}}},
		{"telemetry", ref, model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "fixture-switch", Source: "snmp", ObservedAt: at, CollectedAt: at, Interfaces: []model.Interface{{ID: "ifindex:1", Index: 1, Name: "eth1", MAC: "02:00:00:00:00:01"}}}}},
		{"redfish", phase8AdmissionRef(ref, model.SourceRedfish, "redfish"), model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "fixture-system", Source: "redfish", ObservedAt: at, CollectedAt: at, Metrics: []model.Metric{{Name: "hardware.temperature", Unit: "celsius", Value: 24, ResourceID: "/redfish/v1/Chassis/1/Thermal"}}, HardwareHealth: []model.ComponentHealth{{ID: "fixture-health", ResourceID: "/redfish/v1/Systems/1", Status: model.HardwareStatus{Health: "ok", HealthRollup: "ok", State: "Enabled"}}}}}},
		{"ipmi", phase8AdmissionRef(ref, model.SourceIPMI, "ipmi"), model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "fixture-ipmi", Source: "ipmi", ObservedAt: at, CollectedAt: at, Metrics: []model.Metric{{Name: "hardware.temperature", Unit: "celsius", Value: 25, ResourceID: "/ipmi/sensors/1"}}, HardwareHealth: []model.ComponentHealth{{ID: "fixture-ipmi-health", ResourceID: "/ipmi/sensors/1", Status: model.HardwareStatus{Health: "ok", HealthRollup: "ok", State: "enabled"}}}}}},
		{"routeros", phase8AdmissionRef(ref, model.SourceRouterOS, "routeros"), model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "fixture-router", Source: "routeros", ObservedAt: at, CollectedAt: at, Interfaces: []model.Interface{{ID: "ether1", Index: 1, Name: "ether1", MAC: "02:00:00:00:00:02"}}}}},
		{"topology", phase8AdmissionRef(ref, model.SourceLLDP, "snmp"), phase8TopologyFixture(at, 1)},
		{"sip", phase8AdmissionRef(ref, model.SourceSIP, "sip"), model.Envelope{Telephony: &model.TelephonyUpdate{Source: "sip.options", ObservedAt: at, Targets: []model.TelephonyHealth{{TargetID: "sip-fixture", Address: "127.0.0.1:5060", Transport: "udp", State: "up", Source: "sip.options", ObservedAt: at}}}}},
		{"lifecycle", ref, model.Envelope{Events: []model.Event{{ID: "fixture-event", Type: "device.online", DeviceID: "fixture-switch", Source: "snmp", Timestamp: at, Severity: "info", Message: "fixture target online"}}}},
		// Syslog in the workload is marked by its configured SourceRef at input;
		// it does not rely on phase8Simulated to convert real log observations.
		{"syslog", model.SourceRef{Type: model.SourceSyslog, CollectorID: a.id.AgentID, TargetID: ref.TargetID, Protocol: "tcp", Provenance: model.ProvenanceSimulated}, model.Envelope{Logs: &model.LogBatch{Source: "syslog", ObservedAt: at, Records: []model.LogRecord{{ID: "fixture-log", DeviceID: "fixture-sender", Source: "syslog", SourceID: "fixture-syslog", ObservedAt: at, OriginalTimestamp: &at, Message: "synthetic message"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := model.ObserveEnvelope(tc.e, tc.ref, at)
			if tc.name != "syslog" {
				e = phase8Simulated(e)
			}
			if err := a.push(e); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			retained, err := a.queue.Pop(ctx)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := transportgrpc.Encode(retained)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := proto.Marshal(encoded)
			if err != nil {
				t.Fatal(err)
			}
			decodedProto := new(pb.AgentEnvelope)
			if err := proto.Unmarshal(wire, decodedProto); err != nil {
				t.Fatal(err)
			}
			decoded, err := transportgrpc.DecodeEnvelope(decodedProto)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.MessageID != retained.MessageID || decoded.AgentID != a.id.AgentID || decoded.SessionID != a.id.SessionID {
				t.Fatal("durable/wire identity changed")
			}
			data, err := json.Marshal(decoded)
			if err != nil {
				t.Fatal(err)
			}
			var tree any
			if err := json.Unmarshal(data, &tree); err != nil {
				t.Fatal(err)
			}
			refs := phase8CheckSimulatedSources(t, tree, a.id.AgentID, tc.ref.TargetID)
			if refs == 0 {
				t.Fatal("fixture contains no observation source references")
			}
			if !a.queue.Contains(retained.MessageID) {
				t.Fatal("wire decode prematurely acknowledged durable message")
			}
			if acked, err := a.queue.Acknowledge(retained.MessageID); err != nil || !acked || a.queue.State().Messages != 0 {
				t.Fatal("application ACK failed", acked, err, a.queue.State())
			}
			t.Logf("payload=%s simulated_source_refs=%d proto_bytes=%d durable_replay_identity=preserved", tc.name, refs, len(wire))
		})
	}
}

func phase8AdmissionRef(ref model.SourceRef, source model.SourceType, protocol string) model.SourceRef {
	ref.Type, ref.Protocol = source, protocol
	return ref
}

func phase8CheckSimulatedSources(t *testing.T, value any, collector, target string) int {
	t.Helper()
	count := 0
	switch node := value.(type) {
	case map[string]any:
		if provenance, ok := node["provenance"]; ok {
			if provenance != string(model.ProvenanceSimulated) || node["collector_id"] != collector || node["target_id"] != target {
				t.Fatalf("synthetic source binding changed: %#v", node)
			}
			count++
		}
		for _, child := range node {
			count += phase8CheckSimulatedSources(t, child, collector, target)
		}
	case []any:
		for _, child := range node {
			count += phase8CheckSimulatedSources(t, child, collector, target)
		}
	}
	return count
}

// Both the JSON durable envelope and protobuf message must fit. The fixture
// retains typical LLDP raw IDs, advertised address, descriptions and identity
// evidence, rather than estimating the budget from neighbor count alone.
func TestPhase8TypicalTopologyAdmissionBudget(t *testing.T) {
	for _, tc := range []struct {
		count int
		limit int
	}{{48, 1 << 20}, {256, 1 << 20}, {512, 1 << 20}, {512, 2 << 20}} {
		t.Run(fmt.Sprintf("neighbors-%d-limit-%d", tc.count, tc.limit), func(t *testing.T) {
			count := tc.count
			c := testConfig(t, "127.0.0.1:19443")
			c.Local.Enabled = false
			configureDurable(&c)
			if c.Queue.MaxMessageBytes != config.Defaults().Queue.MaxMessageBytes || c.Queue.MaxMessageBytes != 1<<20 {
				t.Fatal("test no longer checks the default message budget")
			}
			c.Queue.MaxMessageBytes = tc.limit
			a := newTestAgent(t, c, nil)
			t.Cleanup(func() { _ = a.remote.Close() })
			at := time.Date(2026, 10, 11, 12, 0, 0, 123456789, time.UTC)
			ref := model.SourceRef{Type: model.SourceLLDP, CollectorID: a.id.AgentID, TargetID: "remote:phase8-fixture", Protocol: "snmp", Provenance: model.ProvenanceSimulated}
			e := model.ObserveEnvelope(phase8TopologyFixture(at, count), ref, at)
			e, err := a.envelope(e)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := transportgrpc.Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			protoBytes := proto.Size(encoded)
			started := time.Now()
			err = a.push(e)
			elapsed := time.Since(started)
			if err != nil {
				if count <= 256 || tc.limit > 1<<20 {
					t.Fatalf("typical %d-neighbor fixture exceeds default budget: json=%d proto=%d error=%v", count, len(data), protoBytes, err)
				}
				if !errors.Is(err, queue.ErrMessageTooLarge) || a.queue.State().Messages != 0 {
					t.Fatal("oversized fixture must be explicitly rejected before retention", err, a.queue.State())
				}
				t.Logf("neighbors=%d json_bytes=%d protobuf_bytes=%d limit=%d admission=REJECTED_TOO_LARGE duration=%s retained=0", count, len(data), protoBytes, c.Queue.MaxMessageBytes, elapsed)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			retained, err := a.queue.Pop(ctx)
			if err != nil || retained.Topology == nil || len(retained.Topology.Observations) != count {
				t.Fatal("topology was silently truncated", err)
			}
			data, err = json.Marshal(retained)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) != a.queue.State().Bytes {
				t.Fatal("JSON budget differs from retained bytes")
			}
			retainedWire, err := transportgrpc.Encode(retained)
			if err != nil {
				t.Fatal(err)
			}
			protoBytes = proto.Size(retainedWire)
			if len(data) > c.Queue.MaxMessageBytes || protoBytes > c.Queue.MaxMessageBytes {
				t.Fatal("oversized topology bypassed admission", len(data), protoBytes, c.Queue.MaxMessageBytes)
			}
			if acked, err := a.queue.Acknowledge(retained.MessageID); !acked || err != nil {
				t.Fatal("topology ACK failed", acked, err)
			}
			t.Logf("neighbors=%d json_bytes=%d protobuf_bytes=%d limit=%d admission=PASS duration=%s retained_neighbors=%d", count, len(data), protoBytes, c.Queue.MaxMessageBytes, elapsed, len(retained.Topology.Observations))
		})
	}
}

func phase8TopologyFixture(at time.Time, count int) model.Envelope {
	neighbors := make([]model.Neighbor, count)
	for i := range neighbors {
		index := uint32(i + 1)
		name := fmt.Sprintf("Ethernet1/%d", index)
		neighbors[i] = model.Neighbor{
			LocalDeviceID: "fixture-switch", LocalInterfaceID: fmt.Sprintf("ifindex:%d", index), LocalInterfaceName: name,
			LocalInterfaceIndex: &index, LocalInterfaceMAC: "02:00:00:00:00:01", LocalPortNumber: index,
			LocalPortID: []byte(name), LocalPortSubtype: 5,
			RemoteChassisID: []byte{2, 0, 0, 0, byte(i >> 8), byte(i)}, RemoteChassisSubtype: 4,
			RemotePortID: []byte("Ethernet1/1"), RemotePortSubtype: 5, RemoteSystemName: fmt.Sprintf("neighbor-%03d.example", index),
			RemotePortDescription: "uplink to fixture-switch", RemoteSystemDescription: "Fixture managed Ethernet switch, firmware 1.2.3",
			RemoteManagementAddresses:   []model.ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}},
			RemoteCapabilitiesSupported: []string{"bridge", "router"}, RemoteCapabilitiesEnabled: []string{"bridge"},
			Protocol: "lldp", Source: "snmp.lldp", ObservedAt: at, ExpiresAt: at.Add(time.Minute), Confidence: "unresolved",
			Evidence: []model.Evidence{{Source: "lldpRemChassisId", Value: "MAC subtype; unverified neighbor", Confidence: "medium"}},
		}
	}
	return model.Envelope{Topology: &model.TopologyUpdate{DeviceID: "fixture-switch", Source: "snmp.lldp", ObservedAt: at, CollectedAt: at, Completeness: "complete", TTLSeconds: 60, ExpiryAssumption: "collector poll interval; not advertised LLDP TTL", Observations: neighbors}}
}
