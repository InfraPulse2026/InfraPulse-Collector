package model

import (
	"testing"
	"time"
)

func TestHardwareMetadataOwnership(t *testing.T) {
	at := time.Now().UTC()
	ref := SourceRef{Type: SourceRedfish, TargetID: "remote:bmc", ResourceID: "/redfish/v1/Systems/A", Protocol: "redfish", Provenance: ProvenanceReal}
	capacity := uint64(0)
	d := Device{ID: "system", Type: "server", Source: "redfish", Hardware: &HardwareInventory{ResourceType: "computer_system", HardwareUUID: "a-uuid", CapacityBytes: &capacity, Relations: []ResourceRelationship{{Type: "system_chassis", TargetResourceID: "/redfish/v1/Chassis/C"}}}, Observation: NewObservation(ref, "redfish", at, at, nil), Components: []InventoryComponent{{ID: "cpu", Hardware: &HardwareInventory{ResourceType: "processor"}, Observation: NewObservation(SourceRef{Type: SourceRedfish, TargetID: ref.TargetID, ResourceID: "/redfish/v1/Systems/A/Processors/P"}, "redfish", at, at, nil)}}}
	bound := ObserveDevice(d, SourceRef{CollectorID: "collector"}, at, at)
	if bound.Observation.Source.CollectorID != "collector" || bound.Components[0].Observation.Source.CollectorID != "collector" {
		t.Fatal("missing collector binding")
	}
	found := false
	for _, e := range bound.IdentityEvidence {
		if e.Type == IdentityHardwareUUID && e.Value == "a-uuid" {
			found = true
		}
	}
	if !found {
		t.Fatal("UUID evidence missing")
	}
	bound.Hardware.Relations[0].Type = "modified"
	*bound.Hardware.CapacityBytes = 2
	bound.Components[0].Hardware.ResourceType = "modified"
	if d.Hardware.Relations[0].Type != "system_chassis" || capacity != 0 || d.Components[0].Hardware.ResourceType != "processor" {
		t.Fatal("hardware aliasing")
	}
}

func TestHardwareTelemetryBinding(t *testing.T) {
	at := time.Now().UTC()
	sourceAt := at.Add(-time.Minute)
	ref := SourceRef{Type: SourceRedfish, TargetID: "remote:bmc", ResourceID: "/redfish/v1/Chassis/C", Provenance: ProvenanceSimulated}
	sensor := ref
	sensor.ResourceID += "/Sensors/T"
	e := Envelope{Telemetry: &TelemetrySample{DeviceID: "chassis", Source: "simulated", ObservedAt: at, CollectedAt: at, Observation: NewObservation(ref, "simulated", at, at, nil), Metrics: []Metric{{Name: "temperature", Value: 0, Unit: "celsius", ResourceID: sensor.ResourceID, Observation: NewObservation(sensor, "simulated", at, at, &sourceAt)}}, HardwareHealth: []ComponentHealth{{ID: "sensor", ResourceID: sensor.ResourceID, Status: HardwareStatus{Health: "unknown"}}}}}
	bound := ObserveEnvelope(e, SourceRef{CollectorID: "collector"}, at)
	m := bound.Telemetry.Metrics[0]
	if m.Observation.Source.CollectorID != "collector" || m.Observation.Source.Provenance != ProvenanceSimulated || !m.Observation.Time.SourceTimestamp.Equal(sourceAt) || m.Observation.Time.ClockOffset != nil {
		t.Fatal("invalid clock/provenance binding")
	}
	if bound.Telemetry.HardwareHealth[0].Observation.Source.ResourceID != sensor.ResourceID {
		t.Fatal("health resource missing")
	}
	m.Observation.Source.TargetID = "changed"
	if e.Telemetry.Metrics[0].Observation.Source.TargetID != ref.TargetID {
		t.Fatal("metric observation aliasing")
	}
}

func TestResourceSourceValidation(t *testing.T) {
	for _, path := range []string{"https://evil/redfish/v1/Systems/A", "//evil/x", "bad", "/x\n"} {
		if (SourceRef{Type: SourceRedfish, ResourceID: path}).Validate() == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}
