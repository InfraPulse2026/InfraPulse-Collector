package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testSource() SourceRef {
	return SourceRef{Type: SourceSNMP, CollectorID: "collector", TargetID: "polling-target", Protocol: "snmp", Provenance: ProvenanceReal}
}

func testObserved() time.Time {
	return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
}

func TestTargetValidationAndCredentialBoundary(t *testing.T) {
	valid := Target{ID: "polling-target", Type: SourceSNMP, Protocol: "snmp", Endpoint: "192.0.2.1:161",
		Enabled: true, CredentialRef: "snmp-secret-reference", Polling: TargetPolling{FastInterval: 3 * time.Second, Timeout: time.Second}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Target){
		"missing ID":          func(v *Target) { v.ID = "" },
		"oversized ID":        func(v *Target) { v.ID = strings.Repeat("x", 257) },
		"control ID":          func(v *Target) { v.ID = "two\ntargets" },
		"unknown source":      func(v *Target) { v.Type = "unknown-protocol" },
		"missing endpoint":    func(v *Target) { v.Endpoint = "" },
		"injected endpoint":   func(v *Target) { v.Endpoint = "192.0.2.1\nsecret" },
		"embedded credential": func(v *Target) { v.Endpoint = "https://user:secret@example.test" },
		"negative interval":   func(v *Target) { v.Polling.TopologyInterval = -time.Second },
		"negative timeout":    func(v *Target) { v.Polling.Timeout = -time.Second },
		"label bounds":        func(v *Target) { v.Labels = map[string]string{"site": strings.Repeat("x", 1025)} },
	} {
		t.Run(name, func(t *testing.T) {
			v := valid
			change(&v)
			if err := v.Validate(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("expected safe validation failure, got %v", err)
			}
		})
	}
	for _, source := range []SourceType{SourceRedfish, SourceIPMI, SourceSyslog} {
		v := valid
		v.Type = source
		if err := v.Validate(); err != nil {
			t.Fatalf("future domain vocabulary %s: %v", source, err)
		}
	}
	local := Target{ID: "local:collector", Type: SourceLocal, Enabled: true}
	if err := local.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.ID = strings.Repeat("x", 256)
	if err := valid.Validate(); err != nil {
		t.Fatal("valid prefixed target bound", err)
	}
	encoded, err := json.Marshal(valid)
	if err != nil || strings.Contains(string(encoded), "credential") || strings.Contains(string(encoded), valid.CredentialRef) {
		t.Fatalf("target leaked credential reference: %s %v", encoded, err)
	}
}

func TestExistingOpaqueIDsAndFilePathBoundsRemainCompatible(t *testing.T) {
	target := Target{ID: "remote:existing target ID", Type: SourceSyslog, Protocol: "file", Enabled: true,
		CredentialRef: "existing credential reference", Endpoint: "/" + strings.Repeat("x", 4095)}
	if err := target.Validate(); err != nil {
		t.Fatal("existing IDs/file path must remain valid", err)
	}
	ref := SourceRef{Type: SourceSyslog, CollectorID: "collector identity", TargetID: target.ID,
		InstanceID: "existing source label", Protocol: "file", Provenance: ProvenanceReal}
	if err := ref.Validate(); err != nil {
		t.Fatal("source IDs are opaque configured strings", err)
	}
	observation := NewObservation(ref, "linux-file", testObserved(), testObserved(), nil)
	if observation.Source.TargetID != target.ID || observation.Source.InstanceID != ref.InstanceID {
		t.Fatal("opaque IDs were rewritten", observation.Source)
	}
	// Spaces may be literal file-name characters; protocol-specific config still
	// controls network addresses. Generic domain validation must not trim paths.
	target.Endpoint = " /existing/file path "
	if err := target.Validate(); err != nil {
		t.Fatal("literal file path spaces were rejected", err)
	}
	target.Endpoint = strings.Repeat("x", 4097)
	if target.Validate() == nil {
		t.Fatal("file endpoint exceeds legacy bound")
	}
	ref.Protocol = "two protocols"
	if ref.Validate() == nil {
		t.Fatal("carrier protocol must remain a strict identifier")
	}
	value := IdentityEvidence{Type: IdentityMAC, Value: "00:11:22:33:44:55", SubjectID: "existing interface ID", Source: testSource(), Confidence: "medium"}
	if err := value.Validate(); err != nil {
		t.Fatal("observed subject ID must preserve existing spaces", err)
	}
}

func TestSourceRefDomainsAndProvenance(t *testing.T) {
	for _, source := range []SourceType{SourceLocal, SourceSNMP, SourceRouterOS, SourceLLDP, SourceCDP,
		SourceMNDP, SourceSIP, SourceSyslog, SourceAuditd, SourceRedfish, SourceIPMI} {
		ref := testSource()
		ref.Type = source
		if err := ref.Validate(); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
	}
	ref := testSource()
	ref.Type = SourceLLDP
	simulated := ref.ForLegacy("simulated")
	if simulated.Type != SourceLLDP || simulated.Protocol != "snmp" || simulated.Provenance != ProvenanceSimulated || simulated.TargetID != ref.TargetID {
		t.Fatal("simulated provenance changed domain/carrier", simulated)
	}
	ref.CollectorID = "" // deterministic generation before runtime binding
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []SourceRef{{Type: "Cisco"}, {Type: SourceSNMP, Provenance: "snmp"},
		{Type: SourceSNMP, TargetID: "injected\nID"}, {Type: SourceSNMP, InstanceID: strings.Repeat("x", 257)}} {
		if bad.Validate() == nil {
			t.Fatalf("invalid provenance accepted: %+v", bad)
		}
	}
}

func TestObservationTimeUnknownAndIndependentClocks(t *testing.T) {
	at := testObserved()
	o := NewObservation(testSource(), "snmp", at, at.Add(time.Second), nil)
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	if o.Time.ClockOffset != nil || o.Time.Uncertainty != nil || o.Time.SourceTimestamp != nil || o.Time.Quality != ClockUnknown {
		t.Fatal("unknown clock was fabricated", o)
	}
	encoded, err := json.Marshal(o)
	if err != nil || strings.Contains(string(encoded), "clock_offset") || strings.Contains(string(encoded), "source_timestamp") || strings.Contains(string(encoded), "uncertainty") {
		t.Fatalf("unknown optional estimates must be omitted: %s %v", encoded, err)
	}
	var decoded ObservationMetadata
	if err = json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(*o, decoded) {
		t.Fatalf("nil clock estimate did not survive JSON: %+v %v", decoded, err)
	}
	zero, uncertainty := time.Duration(0), 30*time.Second
	source := at.Add(24 * time.Hour)
	measured := ObservationTime{ObservedAt: at, CollectedAt: at.Add(-time.Second), SourceTimestamp: &source,
		ClockOffset: &zero, Uncertainty: &uncertainty, Quality: ClockDegraded}
	if err := measured.Validate(); err != nil {
		t.Fatal("independent/corrected clocks cannot require ordering", err)
	}
	encoded, _ = json.Marshal(measured)
	if !strings.Contains(string(encoded), "\"clock_offset\":0") {
		t.Fatal("measured zero lost presence", string(encoded))
	}
	withoutInference := NewObservation(testSource(), "snmp", at, at.Add(time.Second), &source)
	if !withoutInference.Time.ObservedAt.Equal(at) || withoutInference.Time.ClockOffset != nil {
		t.Fatal("source timestamp inferred observation/offset", withoutInference)
	}
	negative := -time.Nanosecond
	measured.Uncertainty = &negative
	if measured.Validate() == nil {
		t.Fatal("negative uncertainty accepted")
	}
	measured.Uncertainty = nil
	measured.Quality = "exact-without-measurement"
	if measured.Validate() == nil {
		t.Fatal("unknown clock quality accepted")
	}
	missing := NewObservation(testSource(), "snmp", time.Time{}, at, nil)
	if !missing.Time.ObservedAt.Equal(at) {
		t.Fatal("collection completion must be the fallback state time")
	}
}

func TestObserveEnvelopePreservesBoundMetadataAndOwnership(t *testing.T) {
	at := testObserved()
	sourceTime, offset, uncertainty := at.Add(time.Minute), -5*time.Second, 2*time.Second
	metadata := &ObservationMetadata{Source: SourceRef{Type: SourceRedfish, TargetID: "original-target", Protocol: "https", Provenance: ProvenanceExternal},
		Time: ObservationTime{SourceTimestamp: &sourceTime, ObservedAt: at.Add(-time.Minute), CollectedAt: at,
			ClockOffset: &offset, Uncertainty: &uncertainty, Quality: ClockAcceptable}}
	original := Envelope{Telemetry: &TelemetrySample{DeviceID: "observed-device", ObservedAt: at, CollectedAt: at.Add(time.Second), Source: "snmp", Observation: metadata}}
	before, _ := json.Marshal(original)
	adapted := ObserveEnvelope(original, testSource(), at.Add(time.Hour))
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatal("binding mutated the input envelope")
	}
	want := *CloneObservation(metadata)
	want.Source.CollectorID = "collector"
	if !reflect.DeepEqual(*adapted.Telemetry.Observation, want) || adapted.Telemetry.DeviceID != "observed-device" || adapted.Telemetry.Source != "snmp" || !adapted.Telemetry.CollectedAt.Equal(original.Telemetry.CollectedAt) {
		t.Fatalf("existing metadata/legacy fields overwritten: %+v", adapted.Telemetry)
	}
	*adapted.Telemetry.Observation.Time.ClockOffset = time.Hour
	*adapted.Telemetry.Observation.Time.SourceTimestamp = at
	if offset != -5*time.Second || !sourceTime.Equal(at.Add(time.Minute)) {
		t.Fatal("bound metadata aliases source pointer storage")
	}
}

func TestSimulatedLegacyMarkerOverridesInconsistentExistingMetadata(t *testing.T) {
	at := testObserved()
	metadata := NewObservation(testSource(), "snmp", at, at, nil)
	sample := TelemetrySample{Source: "simulated", Observation: metadata}
	got := ObserveEnvelope(Envelope{Telemetry: &sample}, testSource(), at).Telemetry
	if got.Observation.Source.Provenance != ProvenanceSimulated || got.Observation.Source.Type != SourceSNMP ||
		metadata.Source.Provenance != ProvenanceReal {
		t.Fatal("existing metadata erased the authoritative simulated marker or mutated input", got.Observation)
	}
}

func TestTargetIDDoesNotBecomeObservedDeviceID(t *testing.T) {
	at := testObserved()
	e := Envelope{Inventory: &InventoryUpdate{ObservedAt: at, Devices: []Device{
		{ID: "system-1", LastSeen: at.Add(-time.Hour)}, {ID: "chassis-2", LastSeen: at.Add(-2 * time.Hour)},
	}}}
	adapted := ObserveEnvelope(e, testSource(), at.Add(time.Second))
	for i, d := range adapted.Inventory.Devices {
		if d.ID != e.Inventory.Devices[i].ID || d.Observation.Source.TargetID != "polling-target" || !d.Observation.Time.ObservedAt.Equal(at) {
			t.Fatal("target/device or last-seen/observed boundary lost", d)
		}
		if e.Inventory.Devices[i].Observation != nil {
			t.Fatal("binding mutated the original device")
		}
	}
	if got := ObserveEnvelope(e, SourceRef{Type: SourceSNMP}, at); got.Inventory.Devices[0].Observation.Source.TargetID != "" {
		t.Fatal("missing configured target was inferred from device ID")
	}
}

func TestExistingSourcesObservationAdaptation(t *testing.T) {
	at, collected := testObserved(), testObserved().Add(time.Second)
	for _, tc := range []struct {
		name   string
		ref    SourceRef
		source string
	}{
		{"Linux", SourceRef{Type: SourceLocal, TargetID: "local-target", Protocol: "linux"}, "local"},
		{"OpenWrt", SourceRef{Type: SourceLocal, TargetID: "local-target", Protocol: "ubus"}, "local"},
		{"SNMP", SourceRef{Type: SourceSNMP, TargetID: "snmp-target", Protocol: "snmp"}, "snmp"},
		{"RouterOS", SourceRef{Type: SourceRouterOS, TargetID: "routeros-target", Protocol: "https"}, "routeros-rest"},
		{"Simulator", SourceRef{Type: SourceSNMP, TargetID: "simulated-target", Protocol: "simulated", Provenance: ProvenanceSimulated}, "simulated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := TelemetrySample{DeviceID: "device", Source: tc.source, ObservedAt: at, CollectedAt: collected,
				Interfaces: []Interface{{ID: "if-1", MAC: "00:11:22:33:44:55"}}}
			got := ObserveEnvelope(Envelope{Telemetry: &s}, tc.ref, at.Add(time.Hour)).Telemetry
			if got.Source != tc.source || !got.Observation.Time.ObservedAt.Equal(at) || !got.Observation.Time.CollectedAt.Equal(collected) || got.Observation.Source.Type != tc.ref.Type || got.Observation.Source.Protocol != tc.ref.Protocol || got.Observation.Source.TargetID != tc.ref.TargetID {
				t.Fatal(got)
			}
			if err := got.Observation.Validate(); err != nil {
				t.Fatal(err)
			}
			if len(got.IdentityEvidence) != 1 || got.IdentityEvidence[0].SubjectID != "if-1" || got.IdentityEvidence[0].Type != IdentityMAC {
				t.Fatal("interface-scoped MAC evidence missing", got.IdentityEvidence)
			}
			if tc.source == "simulated" && got.IdentityEvidence[0].Source.Provenance != ProvenanceSimulated {
				t.Fatal("simulated nested evidence lost provenance")
			}
		})
	}
	device := Device{ID: "openwrt-device", Source: "local", Services: []Service{{Name: "dnsmasq", Source: "ubus.service.list", ObservedAt: at}},
		Networks: []NetworkState{{Name: "lan", Source: "ubus.network.interface.dump", ObservedAt: at}},
		Firewall: &FirewallSnapshot{Source: "ubus.uci.firewall.configuration", ObservedAt: at}}
	localRef := SourceRef{Type: SourceLocal, TargetID: "local-target", Protocol: "ubus"}
	gotDevice := ObserveDevice(device, localRef, at, collected)
	for _, metadata := range []*ObservationMetadata{gotDevice.Observation, gotDevice.Services[0].Observation,
		gotDevice.Networks[0].Observation, gotDevice.Firewall.Observation} {
		if metadata == nil || metadata.Source.Type != SourceLocal || !metadata.Time.CollectedAt.Equal(collected) {
			t.Fatal("nested local inventory not adapted", metadata)
		}
	}
	h := TelephonyHealth{TargetID: "sip-config-ID", ObservedAt: at, Source: "sip.options"}
	sipRef := SourceRef{Type: SourceSIP, TargetID: "sip-target", Protocol: "sip"}
	u := ObserveEnvelope(Envelope{Telephony: &TelephonyUpdate{ObservedAt: at, Source: "sip.options", Targets: []TelephonyHealth{h}}}, sipRef, collected).Telephony
	if u.Targets[0].TargetID != h.TargetID || u.Targets[0].Observation.Source.TargetID != "sip-target" || u.Targets[0].Observation.Source.Type != SourceSIP {
		t.Fatal("SIP binding rewrote legacy target", u)
	}
}

func TestLogsKeepSourceTimestampOptionalAndAuditDomain(t *testing.T) {
	at, source := testObserved(), testObserved().Add(-24*time.Hour)
	original := LogBatch{ObservedAt: at, Source: "linux-file", Records: []LogRecord{
		{Source: "linux-file", SourceID: "system-log", ObservedAt: at},
		{Source: "linux-file", SourceID: "timestamped-log", ObservedAt: at, OriginalTimestamp: &source},
		{Source: "simulated", SourceID: "audit-log", ObservedAt: at, OriginalTimestamp: &source, Audit: &AuditRecord{EventID: "audit-ID"}},
	}}
	ref := SourceRef{Type: SourceSyslog, CollectorID: "collector", TargetID: "logs:host", Protocol: "file"}
	got := ObserveEnvelope(Envelope{Logs: &original}, ref, at.Add(time.Second)).Logs
	if got.Records[0].Observation.Time.SourceTimestamp != nil || got.Records[0].Observation.Time.ClockOffset != nil {
		t.Fatal("missing year/timezone became an invented timestamp/offset")
	}
	if !got.Records[1].Observation.Time.SourceTimestamp.Equal(source) || !got.Records[1].Observation.Time.ObservedAt.Equal(at) || got.Records[1].Observation.Source.InstanceID != "timestamped-log" {
		t.Fatal("log temporal/source semantics changed", got.Records[1])
	}
	if got.Records[2].Observation.Source.Type != SourceAuditd || got.Records[2].Observation.Source.Provenance != ProvenanceSimulated {
		t.Fatal("audit/simulated domain lost", got.Records[2])
	}
	*got.Records[1].Observation.Time.SourceTimestamp = at
	if !source.Equal(at.Add(-24*time.Hour)) || original.Records[1].Observation != nil {
		t.Fatal("source log pointer was mutated")
	}
}

func TestLegacyJSONOmitsNewMetadata(t *testing.T) {
	var s TelemetrySample
	if err := json.Unmarshal([]byte(`{"device_id":"device","source":"local","observed_at":"2026-10-07T12:00:00Z","collected_at":"2026-10-07T12:00:00Z"}`), &s); err != nil {
		t.Fatal(err)
	}
	if s.Observation != nil || s.IdentityEvidence != nil {
		t.Fatal("old durable JSON acquired fabricated metadata")
	}
	encoded, err := json.Marshal(s)
	if err != nil || strings.Contains(string(encoded), "observation") || strings.Contains(string(encoded), "identity_evidence") {
		t.Fatalf("legacy zero-value metadata changed serialized fields: %s %v", encoded, err)
	}
}
