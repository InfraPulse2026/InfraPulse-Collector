package grpc

import (
	"bytes"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
)

// Removing optional observation/evidence fields reproduces the legacy v1
// payload. Metadata never changes the numbers or meaning of historical fields.
func TestCollectorMetadataPreservesV1WireBytes(t *testing.T) {
	sample := testSample()
	inputs := map[string]model.Envelope{
		"hello":     {Hello: &model.AgentHello{AgentID: "agent-1", SessionID: "session-1", StartedAt: testTime, Device: testDevice(), ProtocolVersion: 1}},
		"heartbeat": {Heartbeat: &model.Heartbeat{Timestamp: testTime, Health: "healthy"}},
		"telemetry": {Telemetry: &sample},
		"inventory": {Inventory: &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{testDevice()}}},
		"topology":  {Topology: &model.TopologyUpdate{DeviceID: "device-1", ObservedAt: testTime, CollectedAt: testTime.Add(time.Millisecond), Source: "snmp.lldp", Completeness: "complete", Observations: []model.Neighbor{{LocalDeviceID: "device-1", Protocol: "lldp", Source: "snmp.lldp", ObservedAt: testTime, Confidence: "unresolved", RemoteChassisID: []byte{0, 1, 2}, RemoteChassisSubtype: 7}}}},
		"logs":      {Logs: &model.LogBatch{ObservedAt: testTime, Source: "auditd", Records: []model.LogRecord{{ID: "log-1", DeviceID: "device-1", SourceID: "audit", Source: "auditd", ObservedAt: testTime, OriginalTimestamp: &testTime, Message: "record"}}}},
		"telephony": {Telephony: &model.TelephonyUpdate{ObservedAt: testTime, Source: "sip.options", Targets: []model.TelephonyHealth{{TargetID: "pbx", Address: "192.0.2.1:5060", Transport: "udp", ObservedAt: testTime, Source: "sip.options", State: "up", Reachable: true}}}},
		"event":     {Events: []model.Event{{ID: "event-1", DeviceID: "device-1", Type: "device.online", Timestamp: testTime, Source: "remote", Severity: "info", Message: "online"}}},
		"result":    {CommandResult: &model.CommandResult{CommandID: "command-1", Status: "rejected", StartedAt: testTime, FinishedAt: testTime.Add(time.Millisecond)}},
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			base := testEnvelope()
			input.AgentID, input.SessionID, input.MessageID, input.Sequence, input.SentAt = base.AgentID, base.SessionID, base.MessageID, base.Sequence, base.SentAt
			ref := model.SourceRef{Type: model.SourceSNMP, CollectorID: input.AgentID, TargetID: "remote:configured-target", Protocol: "snmp", Provenance: model.ProvenanceReal}
			annotated := model.ObserveEnvelope(input, ref, testTime.Add(time.Second))
			before, err := Encode(input)
			if err != nil {
				t.Fatal(err)
			}
			after, err := Encode(annotated)
			if err != nil {
				t.Fatal(err)
			}
			clearOptionalMetadata(after)
			marshal := proto.MarshalOptions{Deterministic: true}
			beforeBytes, err := marshal.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			afterBytes, err := marshal.Marshal(after)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("existing v1 wire payload changed")
			}
			decoded, err := DecodeEnvelope(after)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Telemetry != nil && (decoded.Telemetry.Observation != nil || len(decoded.Telemetry.IdentityEvidence) > 0) {
				t.Fatal("decoder fabricated unavailable source/clock/identity metadata")
			}
		})
	}
}

func clearOptionalMetadata(p *pb.AgentEnvelope) {
	clearDevice := func(d *pb.Device) {
		d.Observation, d.IdentityEvidence = nil, nil
		d.ExtendedIdentityEvidence = nil
		for _, c := range d.Components {
			c.Observation = nil
		}
		for _, s := range d.Services {
			s.Observation = nil
		}
		for _, n := range d.Networks {
			n.Observation = nil
		}
		if d.Firewall != nil {
			d.Firewall.Observation = nil
		}
	}
	if h := p.GetHello(); h != nil {
		clearDevice(h.Device)
	}
	if u := p.GetInventory(); u != nil {
		u.Observation = nil
		for _, d := range u.Devices {
			clearDevice(d)
		}
	}
	if b := p.GetTelemetry(); b != nil {
		for _, s := range b.Samples {
			s.Observation, s.IdentityEvidence = nil, nil
			s.ExtendedIdentityEvidence = nil
		}
	}
	if u := p.GetTopology(); u != nil {
		u.Observation = nil
		for _, n := range u.Observations {
			n.Observation, n.IdentityEvidence = nil, nil
			n.ObservationId, n.Kind = "", ""
			n.LocalInterfaceIndex = nil
			n.LocalInterfaceMac, n.LocalInterfaceDescription, n.LocalInterfaceAlias = "", "", ""
			n.RemoteManagementAddresses = nil
			n.RemoteSystemDescription, n.RemotePlatform, n.RemoteModel, n.RemoteSoftwareVersion = "", "", "", ""
			n.RemoteCapabilitiesSupported, n.RemoteCapabilitiesEnabled = nil, nil
			n.AgeSeconds = nil
		}
	}
	if b := p.GetLogs(); b != nil {
		b.Observation = nil
		for _, r := range b.Records {
			r.Observation = nil
		}
	}
	if u := p.GetTelephony(); u != nil {
		u.Observation = nil
		for _, h := range u.Targets {
			h.Observation = nil
		}
	}
	if b := p.GetEvents(); b != nil {
		for _, e := range b.Events {
			e.Observation = nil
		}
	}
}
