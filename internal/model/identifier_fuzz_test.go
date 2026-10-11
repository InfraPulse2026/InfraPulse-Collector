package model

import (
	"bytes"
	"testing"
)

func FuzzIdentityCanonicalization(f *testing.F) {
	for _, value := range []string{"00:11:22:33:44:55", "001122334455", " 000123 ", "Host.Example.", "::ffff:192.0.2.1", "00112233-4455-6677-8899-aabbccddeeff", "\x00bad", "\xff"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 8192 {
			t.Skip()
		}
		ref := SourceRef{Type: SourceSNMP, CollectorID: "collector-fixture", TargetID: "target-fixture", Provenance: ProvenanceSimulated}
		for _, kind := range []IdentityEvidenceType{IdentitySerial, IdentityHostname, IdentityMAC, IdentityHardwareUUID, IdentityIP} {
			fact, err := NewIdentityFact(kind, value, ref, "low", "subject-fixture", "network_device", "fixture", nil)
			if err != nil {
				continue
			}
			if fact.Validate() != nil || fact.Source.Provenance != ProvenanceSimulated {
				t.Fatal("normalization produced invalid evidence or lost provenance")
			}
			repeated, err := NewIdentityFact(kind, fact.Value, ref, "low", "subject-fixture", "network_device", "fixture", nil)
			if err != nil || repeated.Value != fact.Value || repeated.OriginalValue != "" {
				t.Fatal("normalization is not idempotent")
			}
		}
	})
}

func FuzzTopologyStableScopeAndOwnership(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5}, []byte("port-1"), uint8(4), uint8(7))
	f.Add([]byte{}, []byte{}, uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, chassis, port []byte, chassisSubtype, portSubtype uint8) {
		if len(chassis) > 1024 || len(port) > 1024 {
			t.Skip()
		}
		n := Neighbor{LocalDeviceID: "device-fixture", LocalInterfaceID: "if-fixture", Protocol: "lldp", Source: "simulated", RemoteChassisID: chassis, RemotePortID: port, RemoteChassisSubtype: uint32(chassisSubtype % 8), RemotePortSubtype: uint32(portSubtype % 8), Observation: &ObservationMetadata{Source: SourceRef{Type: SourceSNMP, CollectorID: "collector-a", TargetID: "target-fixture", Provenance: ProvenanceSimulated}}}
		a := EnrichNeighbor(n)
		b := EnrichNeighbor(a)
		if a.ObservationID != b.ObservationID || a.Observation.Source.Provenance != ProvenanceSimulated {
			t.Fatal("topology ID changed on repeated enrichment or lost provenance")
		}
		n.Observation.Source.CollectorID = "collector-b"
		if a.ObservationID == EnrichNeighbor(n).ObservationID {
			t.Fatal("different collectors shared a topology observation ID")
		}
		if len(chassis) > 0 {
			copy := bytes.Clone(chassis)
			chassis[0] ^= 255
			if !bytes.Equal(a.RemoteChassisID, copy) {
				t.Fatal("enriched observation aliases adapter bytes")
			}
		}
	})
}
