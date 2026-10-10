package grpc

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"
	"unicode"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func phase6CompatibleIdentityFixture() []model.IdentityEvidence {
	o := evidenceObservation(model.SourceRedfish, "redfish")
	var facts []model.IdentityEvidence
	for _, kind := range []model.IdentityEvidenceType{model.IdentitySerial, model.IdentityHardwareUUID, model.IdentityMAC, model.IdentityChassisID, model.IdentityLLDPChassisID, model.IdentityManagementIP, model.IdentityHostname, model.IdentitySysName, model.IdentitySysObjectID, model.IdentityManufacturer, model.IdentityModel} {
		facts = append(facts, model.IdentityEvidence{Type: kind, Value: "legacy fact", Source: o.Source, Confidence: "low", SubjectID: "subject", SubjectKind: "computer_system", Time: &o.Time})
	}
	for _, fact := range []struct {
		kind                 model.IdentityEvidenceType
		value, normalization string
	}{
		{model.IdentityIP, "192.0.2.1", "canonical"},
		{model.IdentityInterfaceName, "ether1", "canonical"},
		{model.IdentityHardwareUUID, "malformed-uuid", "invalid"},
		{model.IdentitySerial, "[rejected identifier]", "rejected"},
	} {
		facts = append(facts, model.IdentityEvidence{Type: fact.kind, Value: fact.value, Source: o.Source, Confidence: "unresolved", SubjectID: "subject", SubjectKind: "computer_system", Time: &o.Time, Normalization: fact.normalization})
	}
	return facts
}

// The relevant identity rules are frozen from model.IdentityEvidence.Validate at 14abc4e. They
// deliberately do not call the current vocabulary helper: an old peer applies
// these rules after protobuf decoding, even when new fields are unknown to it.
func validatePhase5IdentitySemantics(p protoreflect.Message) error {
	get := func(number protoreflect.FieldNumber) protoreflect.Value {
		return p.Get(p.Descriptor().Fields().ByNumber(number))
	}
	switch get(1).String() {
	case "serial", "hardware_uuid", "mac", "chassis_id", "lldp_chassis_id", "management_ip", "hostname", "sys_name", "sys_object_id", "manufacturer", "model":
	default:
		return fmt.Errorf("Phase5 rejects identity type %q", get(1).String())
	}
	value, raw := get(2).String(), get(3).Bytes()
	if value == "" || len(value) > 4096 || len(raw) > 1024 || len(get(7).String()) > 256 {
		return fmt.Errorf("Phase5 rejects identity bounds")
	}
	for _, text := range []string{value, get(7).String()} {
		for _, ch := range text {
			if unicode.IsControl(ch) {
				return fmt.Errorf("Phase5 rejects identity controls")
			}
		}
	}
	if len(raw) > 0 && value != base64.StdEncoding.EncodeToString(raw) {
		return fmt.Errorf("Phase5 rejects raw identity mismatch")
	}
	if p.Has(p.Descriptor().Fields().ByNumber(4)) && (len(raw) == 0 || get(4).Uint() > 7 || get(1).String() != "chassis_id" && get(1).String() != "lldp_chassis_id") {
		return fmt.Errorf("Phase5 rejects subtype")
	}
	switch get(6).String() {
	case "high", "medium", "low", "unresolved":
	default:
		return fmt.Errorf("Phase5 rejects confidence")
	}
	return nil
}

func TestPhase5IdentityPeerReceivesOnlyUsableKnownFacts(t *testing.T) {
	facts := phase6CompatibleIdentityFixture()
	o := evidenceObservation(model.SourceRedfish, "redfish")
	for _, inventory := range []bool{true, false} {
		t.Run(fmt.Sprintf("inventory=%t", inventory), func(t *testing.T) {
			e := testEnvelope()
			var message proto.Message
			var name protoreflect.FullName
			var legacyNumber protoreflect.FieldNumber
			if inventory {
				d := testDevice()
				d.IdentityEvidence, d.Observation = facts, o
				e.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{d}}
			} else {
				e.Telemetry = &model.TelemetrySample{DeviceID: "device", Source: "simulated", ObservedAt: testTime, CollectedAt: testTime, Observation: o, IdentityEvidence: facts}
			}
			encoded, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			if inventory {
				message, name, legacyNumber = encoded.GetInventory().Devices[0], "infrapulse.v1.Device", 41
			} else {
				message, name, legacyNumber = encoded.GetTelemetry().Samples[0], "infrapulse.v1.TelemetrySample", 15
			}
			data, err := proto.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			desc, err := phase5Descriptors(t).FindDescriptorByName(name)
			if err != nil {
				t.Fatal(err)
			}
			old := dynamicpb.NewMessage(desc.(protoreflect.MessageDescriptor))
			if err := proto.Unmarshal(data, old); err != nil {
				t.Fatal(err)
			}
			legacy := old.Get(old.Descriptor().Fields().ByNumber(legacyNumber)).List()
			if legacy.Len() != 11 || len(old.GetUnknown()) == 0 {
				t.Fatal("legacy usable facts or optional extension missing")
			}
			for i := 0; i < legacy.Len(); i++ {
				fact := legacy.Get(i).Message()
				if err := validatePhase5IdentitySemantics(fact); err != nil {
					t.Fatal(err)
				}
				if value := fact.Get(fact.Descriptor().Fields().ByNumber(2)).String(); value == "malformed-uuid" || value == "[rejected identifier]" {
					t.Fatal("old peer can mistake a diagnostic for a usable identifier")
				}
			}
			back, err := DecodeEnvelope(encoded)
			if err != nil {
				t.Fatal(err)
			}
			got := back.Telemetry
			var decodedFacts []model.IdentityEvidence
			if inventory {
				decodedFacts = back.Inventory.Devices[0].IdentityEvidence
			} else {
				decodedFacts = got.IdentityEvidence
			}
			if !reflect.DeepEqual(decodedFacts, facts) {
				t.Fatal("new peer lost unified evidence metadata")
			}
		})
	}
}

func TestIdentityLimitsApplyAcrossBothWireLists(t *testing.T) {
	o := evidenceObservation(model.SourceSNMP, "snmp")
	makeFacts := func(count int, kind model.IdentityEvidenceType) []model.IdentityEvidence {
		facts := make([]model.IdentityEvidence, count)
		for i := range facts {
			facts[i] = model.IdentityEvidence{Type: kind, Value: "fact", Source: o.Source, Confidence: "low", SubjectID: "subject"}
		}
		return facts
	}
	e := testEnvelope()
	e.Telemetry = &model.TelemetrySample{DeviceID: "device", Source: "simulated", ObservedAt: testTime, CollectedAt: testTime, IdentityEvidence: append(makeFacts(32, model.IdentitySerial), makeFacts(32, model.IdentityInterfaceName)...)}
	p, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.GetTelemetry().Samples[0].IdentityEvidence) != 32 || len(p.GetTelemetry().Samples[0].ExtendedIdentityEvidence) != 32 {
		t.Fatal("partition missing")
	}
	p.GetTelemetry().Samples[0].ExtendedIdentityEvidence = append(p.GetTelemetry().Samples[0].ExtendedIdentityEvidence, identityToProto(makeFacts(1, model.IdentityIP))[0])
	if _, err := DecodeEnvelope(p); err == nil {
		t.Fatal("two lists bypassed per-subject bound")
	}
	e.Telemetry.IdentityEvidence = append(e.Telemetry.IdentityEvidence, makeFacts(1, model.IdentityIP)...)
	if _, err := Encode(e); err == nil {
		t.Fatal("encoder partition bypassed per-subject bound")
	}
	e.Telemetry.IdentityEvidence = nil
	for i := 0; i < 4096; i++ {
		fact := makeFacts(1, model.IdentitySerial)[0]
		fact.SubjectID = fmt.Sprintf("subject:%d", i/64)
		if i%2 != 0 {
			fact.Type = model.IdentityInterfaceName
		}
		e.Telemetry.IdentityEvidence = append(e.Telemetry.IdentityEvidence, fact)
	}
	p, err = Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	p.GetTelemetry().Samples[0].ExtendedIdentityEvidence = append(p.GetTelemetry().Samples[0].ExtendedIdentityEvidence, identityToProto(makeFacts(1, model.IdentityIP))[0])
	if _, err := DecodeEnvelope(p); err == nil {
		t.Fatal("two lists bypassed overall bound")
	}
}

func TestIdentitySubjectLimitKeepsFullSourceScope(t *testing.T) {
	o := evidenceObservation(model.SourceLLDP, "snmp")
	var facts []model.IdentityEvidence
	for _, protocol := range []string{"snmp", "https"} {
		for i := 0; i < 64; i++ {
			ref := o.Source
			ref.Protocol = protocol
			facts = append(facts, model.IdentityEvidence{Type: model.IdentitySerial, Value: "00042", Source: ref, Confidence: "low", SubjectID: "subject"})
		}
	}
	if err := validateIdentityEvidence(facts); err != nil {
		t.Fatal("different protocol origins share a limit", err)
	}
}

func TestIdentityPartitionKeepsLegacyFieldNumbers(t *testing.T) {
	if (&pb.Device{}).ProtoReflect().Descriptor().Fields().ByName("extended_identity_evidence").Number() != 42 || (&pb.TelemetrySample{}).ProtoReflect().Descriptor().Fields().ByName("extended_identity_evidence").Number() != 16 {
		t.Fatal("optional field numbers changed")
	}
}
