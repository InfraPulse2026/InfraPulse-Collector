package grpc

import (
	"reflect"
	"testing"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
)

func TestHardwareWireRoundTrip(t *testing.T) {
	ref := model.SourceRef{Type: model.SourceRedfish, CollectorID: "collector", TargetID: "remote:bmc", Protocol: "redfish", ResourceID: "/redfish/v1/Systems/A", Provenance: model.ProvenanceReal}
	capacity := uint64(0)
	status := model.HardwareStatus{Health: "unknown", HealthRollup: "warning", State: "Enabled"}
	d := model.Device{ID: "resource", Type: "server", Driver: "redfish", Source: "redfish", Vendor: "Vendor", SerialNumber: "serial", Hardware: &model.HardwareInventory{ResourceType: "computer_system", HardwareUUID: "uuid", BIOSVersion: "bios", PowerState: "Off", CapacityBytes: &capacity, Status: status, Relations: []model.ResourceRelationship{{Type: "system_chassis", TargetResourceID: "/redfish/v1/Chassis/C"}}}, Components: []model.InventoryComponent{{ID: "cpu", Class: "processor", Hardware: &model.HardwareInventory{ResourceType: "processor", Status: status}}}}
	d = model.ObserveDevice(d, ref, testTime, testTime)
	envelopes := []model.Envelope{
		{Inventory: &model.InventoryUpdate{ObservedAt: testTime, Completeness: "partial", Observation: model.NewObservation(ref, "redfish", testTime, testTime, nil), Devices: []model.Device{d}}},
		{Telemetry: &model.TelemetrySample{DeviceID: d.ID, Source: "redfish", ObservedAt: testTime, CollectedAt: testTime, Observation: d.Observation, IdentityEvidence: d.IdentityEvidence, Metrics: []model.Metric{{Name: "temperature_celsius", Value: 0, Unit: "celsius", ResourceID: ref.ResourceID, ComponentID: "sensor", Status: &status, Observation: d.Observation}}, HardwareHealth: []model.ComponentHealth{{ID: "cpu", ResourceID: ref.ResourceID, Name: "CPU", Status: status, Observation: d.Observation}}}},
	}
	for _, e := range envelopes {
		e.MessageID = "message"
		e.AgentID = "collector"
		e.SessionID = "session"
		e.Sequence = 1
		e.SentAt = testTime
		p, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		data, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var wire pb.AgentEnvelope
		if err = proto.Unmarshal(data, &wire); err != nil {
			t.Fatal(err)
		}
		back, err := DecodeEnvelope(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if e.Inventory != nil {
			got := back.Inventory.Devices[0]
			if !reflect.DeepEqual(got.Hardware, d.Hardware) || !reflect.DeepEqual(got.IdentityEvidence, d.IdentityEvidence) || !reflect.DeepEqual(got.Components, d.Components) || back.Inventory.Completeness != "partial" {
				t.Fatalf("inventory loss: %#v", got)
			}
		}
		if e.Telemetry != nil && !reflect.DeepEqual(back.Telemetry, e.Telemetry) {
			t.Fatalf("telemetry loss: %#v", back.Telemetry)
		}
	}
}

func TestHardwareWireValidation(t *testing.T) {
	status := model.HardwareStatus{Health: "ok", HealthRollup: "unknown", State: "Enabled"}
	s := model.TelemetrySample{DeviceID: "system", Source: "redfish", ObservedAt: testTime, CollectedAt: testTime, HardwareHealth: []model.ComponentHealth{{ID: "cpu", ResourceID: "/redfish/v1/Systems/A/Processors/P", Status: status}}}
	for _, mutate := range []func(*model.TelemetrySample){func(s *model.TelemetrySample) { s.HardwareHealth[0].Status.Health = "fake" }, func(s *model.TelemetrySample) { s.HardwareHealth[0].ResourceID = "https://evil/resource" }, func(s *model.TelemetrySample) { s.HardwareHealth[0].ID = "" }} {
		copy := s
		copy.HardwareHealth = append([]model.ComponentHealth(nil), s.HardwareHealth...)
		mutate(&copy)
		if err := validateHardwareSample(copy); err == nil {
			t.Fatal("accepted invalid hardware health")
		}
	}
}
