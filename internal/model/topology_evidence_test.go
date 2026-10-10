package model

import (
	"bytes"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func phase6Neighbor() Neighbor {
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	ref := SourceRef{Type: SourceLLDP, CollectorID: "collector-a", TargetID: "target-a", Protocol: "snmp", Provenance: ProvenanceReal}
	return Neighbor{LocalDeviceID: "switch", LocalInterfaceID: "ifindex:31", LocalPortNumber: 7,
		LocalPortID: []byte("ether31"), LocalPortSubtype: 5, RemoteChassisID: []byte{0, 1, 2, 3, 4, 5}, RemoteChassisSubtype: 4,
		RemotePortID: []byte("remote-port"), RemotePortSubtype: 5, RemoteSystemName: "advertised-name",
		Protocol: "lldp", Source: "snmp.lldp", ObservedAt: at, ExpiresAt: at.Add(time.Minute),
		Observation: NewObservation(ref, "snmp.lldp", at, at.Add(time.Second), nil)}
}

func TestTopologyObservationIdentityIsScopedStableAndDirected(t *testing.T) {
	base := phase6Neighbor()
	first := EnrichNeighbor(base)
	if len(first.ObservationID) != 64 || first.Kind != TopologyObservedL2Neighbor ||
		first.Observation.Source.ResourceID != "/topology/"+first.ObservationID {
		t.Fatal("missing source-bound directed observation identity", first)
	}
	if err := ValidateNeighborEnrichment(first); err != nil {
		t.Fatal(err)
	}
	if base.Observation.Source.ResourceID != "" || base.ObservationID != "" {
		t.Fatal("enrichment mutated input")
	}
	if repeat := EnrichNeighbor(first); repeat.ObservationID != first.ObservationID {
		t.Fatal("repeated enrichment changed ID")
	}
	for name, change := range map[string]func(*Neighbor){
		"poll time": func(n *Neighbor) {
			n.ObservedAt = n.ObservedAt.Add(time.Hour)
			n.ExpiresAt = n.ExpiresAt.Add(time.Hour)
		},
		"table index": func(n *Neighbor) {
			n.Evidence = []Evidence{{Source: "snmp.lldpRemTable.index", Value: "999.7.22", Confidence: "high"}}
		},
		"display name": func(n *Neighbor) { n.RemoteSystemName = "renamed" },
		"provenance":   func(n *Neighbor) { n.Observation.Source.Provenance = ProvenanceSimulated },
	} {
		t.Run(name, func(t *testing.T) {
			n := CloneNeighbor(base)
			change(&n)
			if EnrichNeighbor(n).ObservationID != first.ObservationID {
				t.Fatal("volatile fact changed identity")
			}
		})
	}
	for name, change := range map[string]func(*Neighbor){
		"collector":       func(n *Neighbor) { n.Observation.Source.CollectorID = "collector-b" },
		"target":          func(n *Neighbor) { n.Observation.Source.TargetID = "target-b" },
		"protocol":        func(n *Neighbor) { n.Protocol = "cdp"; n.Observation.Source.Type = SourceCDP },
		"carrier":         func(n *Neighbor) { n.Observation.Source.Protocol = "routeros" },
		"local interface": func(n *Neighbor) { n.LocalInterfaceID = "ifindex:32" },
		"chassis subtype": func(n *Neighbor) { n.RemoteChassisSubtype = 7 },
		"port subtype":    func(n *Neighbor) { n.RemotePortSubtype = 7 },
		"remote endpoint": func(n *Neighbor) { n.RemoteChassisID[0] = 9 },
		"direction": func(n *Neighbor) {
			n.LocalDeviceID = "remote-switch"
			n.LocalInterfaceID = "remote-port"
			n.RemotePortID = []byte("ether31")
		},
	} {
		t.Run(name, func(t *testing.T) {
			n := CloneNeighbor(base)
			change(&n)
			if EnrichNeighbor(n).ObservationID == first.ObservationID {
				t.Fatal("independent observation merged")
			}
		})
	}
}

func TestTopologyUnknownEndpointAndStableSerialization(t *testing.T) {
	n := phase6Neighbor()
	n.RemoteChassisID, n.RemotePortID = nil, nil
	n.RemoteChassisSubtype, n.RemotePortSubtype = 0, 0
	n.RemoteManagementAddresses = []ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}, {AddressFamily: 1, Address: "192.0.2.2", RawAddress: []byte{192, 0, 2, 2}}}
	n.RemoteCapabilitiesSupported = []string{"router", "bridge", "router"}
	first := EnrichNeighbor(n)
	if len(first.RemoteChassisID) != 0 || len(first.RemotePortID) != 0 || !reflect.DeepEqual(first.RemoteCapabilitiesSupported, []string{"bridge", "router"}) {
		t.Fatal("missing endpoint invented or facts reordered incorrectly", first)
	}
	reversed := CloneNeighbor(n)
	reversed.RemoteManagementAddresses[0], reversed.RemoteManagementAddresses[1] = reversed.RemoteManagementAddresses[1], reversed.RemoteManagementAddresses[0]
	if second := EnrichNeighbor(reversed); !reflect.DeepEqual(first, second) {
		t.Fatal("input ordering changed normalized observation")
	}
	different := CloneNeighbor(n)
	different.RemoteManagementAddresses = different.RemoteManagementAddresses[1:]
	if EnrichNeighbor(different).ObservationID == first.ObservationID {
		t.Fatal("unknown remote endpoints with distinct address evidence merged")
	}
}

func TestTopologyObservationIDRebindsCollectorAndRetainsExplicitResource(t *testing.T) {
	n := phase6Neighbor()
	n.Observation.Source.CollectorID = ""
	before := EnrichNeighbor(n)
	bound := CloneNeighbor(before)
	bound.Observation.Source.CollectorID = "runtime-collector"
	after := EnrichNeighbor(bound)
	if before.ObservationID == after.ObservationID || after.Observation.Source.ResourceID != "/topology/"+after.ObservationID {
		t.Fatal("runtime binding retained another collector's topology identity", before, after)
	}
	n.Observation.Source.ResourceID = "/explicit/protocol/resource"
	explicit := EnrichNeighbor(n)
	explicit.Observation.Source.CollectorID = "runtime-collector"
	explicit = EnrichNeighbor(explicit)
	if explicit.Observation.Source.ResourceID != "/explicit/protocol/resource" {
		t.Fatal("explicit resource origin overwritten")
	}
}

func TestNeighborDeepCloneOwnsRawEvidenceAndMetadata(t *testing.T) {
	n := phase6Neighbor()
	index, age, subtype := uint32(31), uint32(2), uint32(2)
	n.LocalInterfaceIndex, n.AgeSeconds = &index, &age
	n.RemoteManagementAddresses = []ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}, InterfaceSubtype: &subtype, InterfaceID: &index}}
	n.RemoteCapabilitiesSupported = []string{"bridge"}
	n.IdentityEvidence = []IdentityEvidence{{RawValue: []byte{1}, Subtype: &subtype}}
	n.Evidence = []Evidence{{Value: "raw source"}}
	clone := CloneNeighbor(n)
	clone.LocalPortID[0], clone.RemoteChassisID[0], clone.RemotePortID[0] = 9, 9, 9
	clone.RemoteManagementAddresses[0].RawAddress[0] = 9
	*clone.RemoteManagementAddresses[0].InterfaceSubtype = 3
	*clone.LocalInterfaceIndex, *clone.AgeSeconds = 99, 99
	clone.RemoteCapabilitiesSupported[0] = "router"
	clone.IdentityEvidence[0].RawValue[0] = 9
	clone.Evidence[0].Value = "other"
	clone.Observation.Source.TargetID = "other"
	if index != 31 || age != 2 || subtype != 2 || n.LocalPortID[0] != 'e' || n.RemoteChassisID[0] != 0 || n.RemotePortID[0] != 'r' || n.RemoteManagementAddresses[0].RawAddress[0] != 192 || n.IdentityEvidence[0].RawValue[0] != 1 || n.Evidence[0].Value != "raw source" || n.Observation.Source.TargetID != "target-a" {
		t.Fatal("clone retained mutable aliases")
	}
}

func TestTopologyEnrichmentValidationAndAddressFamilies(t *testing.T) {
	legacy := phase6Neighbor()
	if err := ValidateNeighborEnrichment(legacy); err != nil {
		t.Fatal("legacy optional fields rejected", err)
	}
	ipv4 := ManagementAddress{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}
	unknown := ManagementAddress{AddressFamily: 99, RawAddress: []byte{0, 255}}
	mapped := ManagementAddress{AddressFamily: 2, Address: "::ffff:192.0.2.1", RawAddress: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 192, 0, 2, 1}}
	for _, a := range []ManagementAddress{ipv4, unknown, mapped} {
		if err := a.Validate(); err != nil {
			t.Fatal("valid family rejected", err)
		}
	}
	for name, change := range map[string]func(*Neighbor){
		"malformed hash":   func(n *Neighbor) { n.ObservationID = strings.Repeat("z", 64) },
		"invented cable":   func(n *Neighbor) { n.Kind = "verified_physical_cable" },
		"invalid MAC":      func(n *Neighbor) { n.LocalInterfaceMAC = "not-a-mac" },
		"text control":     func(n *Neighbor) { n.RemoteSystemDescription = "host\ncommand" },
		"invalid UTF8":     func(n *Neighbor) { n.RemoteModel = string([]byte{0xff}) },
		"excess addresses": func(n *Neighbor) { n.RemoteManagementAddresses = make([]ManagementAddress, 17) },
		"bad family": func(n *Neighbor) {
			a := ipv4
			a.AddressFamily = 2
			n.RemoteManagementAddresses = []ManagementAddress{a}
		},
		"changed raw": func(n *Neighbor) {
			a := ipv4
			a.RawAddress = bytes.Repeat([]byte{1}, 4)
			n.RemoteManagementAddresses = []ManagementAddress{a}
		},
		"invented unknown IP": func(n *Neighbor) {
			a := unknown
			a.Address = "192.0.2.1"
			n.RemoteManagementAddresses = []ManagementAddress{a}
		},
		"oversized raw": func(n *Neighbor) {
			n.RemoteManagementAddresses = []ManagementAddress{{AddressFamily: 99, RawAddress: make([]byte, 32)}}
		},
		"excess capabilities": func(n *Neighbor) { n.RemoteCapabilitiesEnabled = make([]string, 17) },
	} {
		t.Run(name, func(t *testing.T) {
			n := CloneNeighbor(legacy)
			change(&n)
			if ValidateNeighborEnrichment(n) == nil {
				t.Fatal("malformed enrichment accepted")
			}
		})
	}
}

func BenchmarkTopologyEnrichment(b *testing.B) {
	for _, count := range []int{1, 128, 1024} {
		b.Run(strconv.Itoa(count)+"_neighbors", func(b *testing.B) {
			neighbors := make([]Neighbor, count)
			for i := range neighbors {
				neighbors[i] = phase6Neighbor()
				neighbors[i].LocalPortNumber = uint32(i + 1)
				neighbors[i].RemoteManagementAddresses = []ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, neighbor := range neighbors {
					_ = EnrichNeighbor(neighbor)
				}
			}
		})
	}
}
