package model

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestIdentityEvidenceUsesAvailableFactsAndComponentScopes(t *testing.T) {
	ref := testSource()
	d := Device{ID: "device", Hostname: "router", ManagementAddress: "192.0.2.1:161", VendorHint: "Cisco",
		SysObjectID: "1.3.6.1.4.1.9.1.1", Components: []InventoryComponent{{ID: "entity:5", SerialNumber: "observed-component-serial", Model: "observed-component-model"}}}
	values := DeviceIdentityEvidence(d, ref)
	seen := map[IdentityEvidenceType][]IdentityEvidence{}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			t.Fatal(value, err)
		}
		seen[value.Type] = append(seen[value.Type], value)
		if value.Type == IdentityManufacturer || value.Type == IdentityHardwareUUID ||
			value.Type == IdentitySerial && value.SubjectID == "" {
			t.Fatal("missing identity/confirmed vendor fabricated", value)
		}
		if (value.Type == IdentityHostname || value.Type == IdentityManagementIP || value.Type == IdentitySysName) && value.Confidence != "low" {
			t.Fatal("mutable logical identity must not claim hardware confidence", value)
		}
	}
	if len(seen[IdentitySerial]) != 1 || seen[IdentitySerial][0].SubjectID != "entity:5" || len(seen[IdentitySysName]) != 1 ||
		len(seen[IdentityManagementIP]) != 1 || seen[IdentityManagementIP][0].Value != "192.0.2.1" {
		t.Fatal("component identity scope or known SNMP/IP evidence lost", values)
	}
	if missing := DeviceIdentityEvidence(Device{ID: "missing", Name: "display-only", VendorHint: "Cisco"}, ref); len(missing) != 0 {
		t.Fatal("missing evidence must remain missing", missing)
	}
	if value := managementIP("https://[2001:db8::1]:443"); value != "2001:db8::1" {
		t.Fatal("known literal IPv6 lost", value)
	}
	if value := managementIP("https://dns-name.example"); value != "" {
		t.Fatal("DNS name became invented management IP", value)
	}
}

func TestTopologyEvidencePreservesRawChassisAndUnknownSubtype(t *testing.T) {
	at := testObserved()
	raw := []byte{0, 255, 128, 0, 42}
	n := Neighbor{LocalDeviceID: "local-device", Protocol: "lldp", Source: "snmp.lldp", ObservedAt: at,
		RemoteChassisID: raw, RemoteChassisSubtype: 4, RemoteSystemName: "neighbor-host"}
	topology := TopologyUpdate{DeviceID: "local-device", Source: "snmp.lldp", ObservedAt: at, CollectedAt: at, Observations: []Neighbor{n}}
	got := ObserveEnvelope(Envelope{Topology: &topology}, testSource(), at.Add(time.Second)).Topology
	if got.Observation.Source.Type != SourceLLDP || got.Observation.Source.Protocol != "snmp" ||
		got.Observations[0].Observation.Source.Type != SourceLLDP || got.DeviceID != "local-device" {
		t.Fatal("LLDP domain/carrier or local device changed", got)
	}
	value := got.Observations[0].IdentityEvidence[0]
	if value.Type != IdentityLLDPChassisID || !bytes.Equal(value.RawValue, raw) || value.Value != base64.StdEncoding.EncodeToString(raw) ||
		value.Subtype == nil || *value.Subtype != 4 || value.SubjectID != "neighbor:"+got.Observations[0].ObservationID || value.SubjectKind != "remote_neighbor" {
		t.Fatal("neighbor evidence fabricated local ownership or erased raw identity", value)
	}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.RawValue[0] = 99
	if raw[0] != 0 {
		t.Fatal("identity evidence shares source neighbor bytes")
	}
	n.Source, n.Protocol, n.RemoteChassisSubtype = "routeros-neighbor", "mndp", 0
	topology.Source, topology.Observations = "routeros-neighbor", []Neighbor{n}
	ref := SourceRef{Type: SourceRouterOS, TargetID: "routeros-target", Protocol: "https"}
	got = ObserveEnvelope(Envelope{Topology: &topology}, ref, at).Topology
	value = got.Observations[0].IdentityEvidence[0]
	if got.Observation.Source.Type != SourceRouterOS || got.Observations[0].Observation.Source.Type != SourceMNDP ||
		value.Type != IdentityChassisID || value.Subtype == nil || *value.Subtype != 0 || value.Confidence != "low" {
		t.Fatal("unknown discovery subtype became a proved LLDP identity", got)
	}
	encoded, _ := json.Marshal(value)
	if !bytes.Contains(encoded, []byte(`"subtype":0`)) {
		t.Fatal("unknown advertised subtype lost presence", string(encoded))
	}
	var restored IdentityEvidence
	if err := json.Unmarshal(encoded, &restored); err != nil || !reflect.DeepEqual(value, restored) {
		t.Fatal("raw identity did not survive persistence", restored, err)
	}
}

func TestIdentityEvidenceValidationRejectsUnknownFacts(t *testing.T) {
	valid := IdentityEvidence{Type: IdentitySerial, Value: "observed-serial", Source: testSource(), Confidence: "high"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*IdentityEvidence){
		"missing value":      func(e *IdentityEvidence) { e.Value = "" },
		"unknown type":       func(e *IdentityEvidence) { e.Type = "fake-hardware-id" },
		"unknown confidence": func(e *IdentityEvidence) { e.Confidence = "certain-global-asset" },
		"raw mismatch":       func(e *IdentityEvidence) { e.RawValue = []byte("other") },
		"invented subtype":   func(e *IdentityEvidence) { v := uint32(4); e.Subtype = &v },
	} {
		t.Run(name, func(t *testing.T) {
			e := valid
			change(&e)
			if e.Validate() == nil {
				t.Fatal("invalid identity evidence accepted", e)
			}
		})
	}
}

func TestCloneDeviceOwnsObservationAndIdentityStorage(t *testing.T) {
	at, offset, uncertainty, subtype := testObserved(), time.Second, 30*time.Second, uint32(0)
	observation := NewObservation(testSource(), "snmp", at, at, &at)
	observation.Time.ClockOffset, observation.Time.Uncertainty = &offset, &uncertainty
	d := Device{Observation: observation, IdentityEvidence: []IdentityEvidence{{Type: IdentityChassisID, Value: "AAE=", RawValue: []byte{0, 1}, Subtype: &subtype}},
		Services: []Service{{Observation: observation}}, Networks: []NetworkState{{Observation: observation}}, Firewall: &FirewallSnapshot{Observation: observation}}
	clone := CloneDevice(d)
	*clone.Observation.Time.ClockOffset = time.Hour
	*clone.Observation.Time.Uncertainty = time.Minute
	*clone.Observation.Time.SourceTimestamp = at.Add(time.Hour)
	clone.IdentityEvidence[0].RawValue[0] = 99
	*clone.IdentityEvidence[0].Subtype = 4
	clone.Services[0].Observation.Source.TargetID = "other-target"
	clone.Networks[0].Observation.Time.Quality = ClockSynced
	clone.Firewall.Observation.Source.Protocol = "changed"
	if offset != time.Second || uncertainty != 30*time.Second || d.IdentityEvidence[0].RawValue[0] != 0 || subtype != 0 ||
		d.Services[0].Observation.Source.TargetID != "polling-target" || d.Networks[0].Observation.Time.Quality != ClockUnknown ||
		d.Firewall.Observation.Source.Protocol != "snmp" || !at.Equal(testObserved()) {
		t.Fatal("cloned observation shares mutable state", d)
	}
}

func TestSimulatedDeviceIdentityEvidenceKeepsProvenance(t *testing.T) {
	at := testObserved()
	d := Device{ID: "simulated-device", Source: "simulated", SerialNumber: "explicit-simulator-serial", IdentityEvidence: []IdentityEvidence{
		{Type: IdentitySerial, Value: "explicit-simulator-serial", Source: testSource(), Confidence: "high"},
	}}
	got := ObserveDevice(d, testSource(), at, at)
	if got.Observation.Source.Provenance != ProvenanceSimulated || got.IdentityEvidence[0].Source.Provenance != ProvenanceSimulated ||
		d.IdentityEvidence[0].Source.Provenance != ProvenanceReal {
		t.Fatal("simulated nested evidence mistaken for real or mutated input", got)
	}
}

func BenchmarkObserveTelemetry(b *testing.B) {
	at := testObserved()
	s := TelemetrySample{DeviceID: "device", Source: "snmp", ObservedAt: at, CollectedAt: at,
		Interfaces: []Interface{{ID: "if-1", MAC: "00:11:22:33:44:55"}, {ID: "if-2", MAC: "00:11:22:33:44:66"}}}
	ref := testSource()
	b.ReportAllocs()
	for b.Loop() {
		_ = ObserveEnvelope(Envelope{Telemetry: &s}, ref, at)
	}
}
