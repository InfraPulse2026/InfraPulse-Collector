package sanitize

import (
	"encoding/json"
	"strings"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestIdentitySanitizerPreservesScopeWithRejectionDiagnostic(t *testing.T) {
	const secret = "private-fixture-token"
	source := model.SourceRef{Type: model.SourceRouterOS, TargetID: "remote:router", Protocol: "https", Provenance: model.ProvenanceSimulated}
	d := model.Device{ID: "router", IdentityEvidence: []model.IdentityEvidence{{Type: model.IdentitySerial, Value: "canonical", OriginalValue: secret, SubjectID: "*1", SubjectKind: "network_interface", Source: source}}}
	envelope := model.Envelope{Inventory: &model.InventoryUpdate{Devices: []model.Device{d}}}
	k := KnownSecrets{Values: []string{secret}}
	clean := k.Sanitize(envelope)
	fact := clean.Inventory.Devices[0].IdentityEvidence[0]
	if fact.Normalization != "rejected" || fact.Source != source || fact.SubjectID != "*1" || fact.OriginalValue != "" {
		t.Fatal("source fact silently dropped or altered", fact)
	}
	data, _ := json.Marshal(clean)
	if strings.Contains(string(data), secret) || envelope.Inventory.Devices[0].IdentityEvidence[0].OriginalValue != secret {
		t.Fatal("original leak or mutation")
	}
	if err := k.ValidateIdentitySources(clean); err != nil {
		t.Fatal(err)
	}
	clean.Inventory.Devices[0].IdentityEvidence[0].Source.ResourceID = "/source/" + secret
	if err := k.ValidateIdentitySources(clean); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("secret-bearing source identity not rejected safely", err)
	}
	n := model.Neighbor{RemoteChassisID: []byte(secret)}
	if err := k.ValidateIdentitySources(model.Envelope{Topology: &model.TopologyUpdate{Observations: []model.Neighbor{n}}}); err == nil {
		t.Fatal("raw chassis secret allowed")
	}
	n = model.Neighbor{RemoteManagementAddresses: []model.ManagementAddress{{AddressFamily: 99, RawAddress: []byte(secret)}}}
	if n.RemoteManagementAddresses[0].Validate() != nil {
		t.Fatal("fixture unknown family must be allowed")
	}
	if err := k.ValidateIdentitySources(model.Envelope{Topology: &model.TopologyUpdate{Observations: []model.Neighbor{n}}}); err == nil {
		t.Fatal("new raw management address leaked credential")
	}
	d = model.Device{ID: "hardware", Hardware: &model.HardwareInventory{Relations: []model.ResourceRelationship{{TargetResourceID: "/resource/" + secret}}}}
	if err := k.ValidateIdentitySources(model.Envelope{Inventory: &model.InventoryUpdate{Devices: []model.Device{d}}}); err == nil {
		t.Fatal("relationship URI leaked credential")
	}
}

func TestIdentitySourceURIRejectsPercentEncodedKnownSecret(t *testing.T) {
	k := KnownSecrets{Values: []string{"SECRET"}}
	for _, relationship := range []bool{false, true} {
		d := model.Device{ID: "hardware", Observation: &model.ObservationMetadata{Source: model.SourceRef{Type: model.SourceRedfish, Protocol: "https", ResourceID: "/redfish/v1/Chassis/S%45CRET"}}}
		if relationship {
			d.Observation.Source.ResourceID = "/redfish/v1/Managers/1"
			d.Hardware = &model.HardwareInventory{Relations: []model.ResourceRelationship{{TargetResourceID: "/redfish/v1/Chassis/S%45CRET"}}}
		}
		if err := d.Observation.Source.Validate(); err != nil {
			t.Fatal("fixture URI must otherwise be valid", err)
		}
		err := k.ValidateIdentitySources(model.Envelope{Inventory: &model.InventoryUpdate{Devices: []model.Device{d}}})
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("encoded source credential not rejected safely", err)
		}
	}
}
