package grpc

import (
	"bytes"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
)

// Phase 3 adds observation fields only to logs. Other payloads stay identical;
// clearing the additive log fields reproduces the historical v1 wire payload.
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
			if name == "logs" {
				decoded, err := DecodeEnvelope(after)
				if err != nil || decoded.Logs.Observation == nil || decoded.Logs.Records[0].Observation == nil {
					t.Fatal("additive log source/time metadata lost", err)
				}
				after.GetLogs().Observation = nil
				for _, r := range after.GetLogs().Records {
					r.Observation = nil
				}
			}
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
