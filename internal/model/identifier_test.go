package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIdentifierNormalizationRetainsMeaning(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		normalize func(string) (string, error)
	}{
		{"AA-BB-CC-DD-EE-FF", "aa:bb:cc:dd:ee:ff", NormalizeMAC},
		{"aabb.ccdd.eeff", "aa:bb:cc:dd:ee:ff", NormalizeMAC},
		{"00:11:22:33:44:55:66:77", "00:11:22:33:44:55:66:77", NormalizeMAC},
		{"38947555-7742-3448-3784-82334782383A", "38947555-7742-3448-3784-82334782383a", NormalizeUUID},
		{" 0000-ABC/001 ", "0000-ABC/001", NormalizeSerial},
		{" Switch.Example. ", "Switch.Example.", NormalizeHostname},
	} {
		got, err := tc.normalize(tc.raw)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.raw, got, err)
		}
	}
	for _, tc := range []struct {
		value     string
		normalize func(string) (string, error)
	}{
		{"", NormalizeUUID}, {"fake-uuid", NormalizeUUID}, {"00000000-0000-0000-0000-000000000000", NormalizeUUID},
		{"ff:ff:not:a:mac", NormalizeMAC}, {"host\ncommand", NormalizeHostname}, {strings.Repeat("a", 256), NormalizeHostname},
		{string([]byte{255}), NormalizeSerial},
	} {
		if v, err := tc.normalize(tc.value); err == nil || v != "" {
			t.Fatal("malformed identifier accepted", tc.value, v)
		}
	}
	for _, tc := range []struct{ raw, want, family string }{{"192.0.2.1", "192.0.2.1", "ipv4"}, {"2001:0DB8:0:0::1", "2001:db8::1", "ipv6"}, {"::ffff:192.0.2.1", "::ffff:192.0.2.1", "ipv6"}} {
		v, family, err := NormalizeIP(tc.raw)
		if err != nil || v != tc.want || family != tc.family {
			t.Fatal(v, family, err)
		}
	}
	for _, invalid := range []string{"hostname", "010.0.0.1", "fe80::1%eth0", "192.0.2.1/24", "https://192.0.2.1"} {
		if _, _, err := NormalizeIP(invalid); err == nil {
			t.Fatal("invalid IP accepted", invalid)
		}
	}
}

func TestIdentityFactDiagnosticsAndOriginal(t *testing.T) {
	at := testObserved()
	clock := ObservationTime{ObservedAt: at, CollectedAt: at.Add(time.Second), Quality: ClockUnknown}
	v, err := NewIdentityFact(IdentityMAC, "AA-BB-CC-DD-EE-FF", testSource(), "medium", "ifindex:2", "network_interface", "snmp.ifPhysAddress", &clock)
	if err != nil || v.Value != "aa:bb:cc:dd:ee:ff" || v.OriginalValue != "AA-BB-CC-DD-EE-FF" || v.Validate() != nil {
		t.Fatal(v, err)
	}
	clock.ObservedAt = at.Add(time.Hour)
	if !v.Time.ObservedAt.Equal(at) {
		t.Fatal("fact shares clock storage")
	}
	for _, raw := range []string{"fake-uuid", strings.Repeat("a", 4097), "uuid\ncontrol"} {
		v, err = NewIdentityFact(IdentityHardwareUUID, raw, testSource(), "low", "system", "computer_system", "redfish.UUID", nil)
		if err == nil || v.Normalization == "canonical" || v.Validate() != nil {
			t.Fatal("invalid input lacks bounded diagnostic", v, err)
		}
		if v.Normalization == "rejected" && (v.OriginalValue != "" || len(v.Value) > 64) {
			t.Fatal("rejected hostile input retained", v)
		}
	}
}

func TestIdentityCrossSourceFactsDoNotMergeAssets(t *testing.T) {
	at := testObserved()
	var observations []Device
	for _, source := range []SourceType{SourceSNMP, SourceRouterOS, SourceRedfish, SourceIPMI} {
		ref := SourceRef{Type: source, CollectorID: "collector", TargetID: "remote:" + string(source), Protocol: string(source), Provenance: ProvenanceSimulated}
		for _, serial := range []string{"0001", "different-serial"} {
			d := Device{ID: string(source) + ":" + serial, Source: "simulated", Hostname: "sw-core", ManagementAddress: "192.0.2.1", SerialNumber: serial}
			if source == SourceRedfish || source == SourceIPMI {
				d.Hardware = &HardwareInventory{ResourceType: "Manager"}
			}
			observations = append(observations, ObserveDevice(d, ref, at, at))
		}
	}
	if len(observations) != 8 {
		t.Fatal("sources merged")
	}
	for _, d := range observations {
		if d.ID == "" || d.Observation.Source.TargetID == d.ID {
			t.Fatal("target and subject conflated", d)
		}
		for _, v := range d.IdentityEvidence {
			if v.Source.Provenance != ProvenanceSimulated || v.Time == nil || !v.Time.ObservedAt.Equal(at) || v.Time.SourceTimestamp != nil {
				t.Fatal("fact source/time lost", v)
			}
		}
		data, _ := json.Marshal(d)
		if strings.Contains(string(data), "asset_id") || strings.Contains(string(data), "global_asset") {
			t.Fatal("fake Asset ID")
		}
	}
	ref := testSource()
	v, _ := NewIdentityFact(IdentitySerial, "0001", ref, "high", "component", "component", "fru.product.serial", nil)
	conflict := v
	conflict.Value = "0002"
	otherSource := v
	otherSource.Source.Type = SourceRedfish
	otherOrigin := v
	otherOrigin.Origin = "fru.board.serial"
	merged := MergeIdentityEvidence([]IdentityEvidence{v, otherOrigin}, []IdentityEvidence{v, conflict, otherSource})
	if len(merged) != 4 {
		t.Fatal("conflict/origin/source removed", merged)
	}
}

func TestInterfaceFactsRemainScopedAndMissingRemainAbsent(t *testing.T) {
	ref := testSource()
	interfaces := []Interface{{ID: "ifindex:1", Name: "eth0", MAC: "02:00:00:00:00:01", Addresses: []string{"192.0.2.1/24"}}, {ID: "ifindex:2", Name: "eth0", MAC: "02:00:00:00:00:01"}}
	values := InterfaceIdentityEvidence(interfaces, ref, nil)
	if len(values) != 5 {
		t.Fatal(values)
	}
	counts := map[string]int{}
	for _, v := range values {
		if v.SubjectKind != "network_interface" || v.SubjectID == "" || v.Validate() != nil {
			t.Fatal(v)
		}
		counts[v.SubjectID]++
	}
	if len(counts) != 2 {
		t.Fatal("duplicate MAC merged interfaces")
	}
	missing := ObserveDevice(Device{ID: "missing"}, ref, testObserved(), testObserved())
	if len(missing.IdentityEvidence) != 0 {
		t.Fatal("missing identifiers fabricated", missing)
	}
}

func TestGeneratedIdentityBoundsHaveDeterministicDiagnostics(t *testing.T) {
	interfaces := make([]Interface, 2050)
	for i := range interfaces {
		interfaces[i] = Interface{ID: "ifindex:" + strconv.Itoa(i+1), Name: "eth" + strconv.Itoa(i+1), MAC: "02:00:00:00:00:01"}
	}
	values := InterfaceIdentityEvidence(interfaces, testSource(), nil)
	if len(values) != MaxIdentityEvidence {
		t.Fatal("derived evidence grew past budget", len(values))
	}
	last := values[len(values)-1]
	if last.Normalization != "rejected" || last.Origin != "collector.identity_evidence_limit" || last.Validate() != nil {
		t.Fatal("overflow silently lost", last)
	}
	slicesReverse := make([]Interface, len(interfaces))
	for i := range interfaces {
		slicesReverse[len(interfaces)-1-i] = interfaces[i]
	}
	a, _ := json.Marshal(values)
	b, _ := json.Marshal(InterfaceIdentityEvidence(slicesReverse, testSource(), nil))
	if string(a) != string(b) {
		t.Fatal("interface evidence depends on array order")
	}
	merged := MergeIdentityEvidence(values, []IdentityEvidence{{Type: IdentityHostname, Value: "device-name", Source: testSource(), Confidence: "low"}})
	if len(merged) > MaxIdentityEvidence {
		t.Fatal("already-diagnosed bounded evidence expanded", len(merged))
	}
	interfaces = []Interface{{ID: "iface", Name: "eth0", MAC: "02:00:00:00:00:01"}}
	for i := 0; i < 100; i++ {
		interfaces[0].Addresses = append(interfaces[0].Addresses, "192.0.2.1/24")
	}
	values = InterfaceIdentityEvidence(interfaces, testSource(), nil)
	if len(values) != MaxIdentityPerSubject || values[len(values)-1].Origin != "collector.identity_subject_limit" {
		t.Fatal("per-interface budget exceeded", len(values))
	}
}
