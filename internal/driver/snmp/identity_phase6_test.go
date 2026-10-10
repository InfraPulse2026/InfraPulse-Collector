package snmp

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/snmp"
)

func TestSNMPIdentityKeepsExistingInventoryFactsWithoutAdditionalPolling(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	f.walk[entityRoot] = []protocol.Value{cell(entityRoot, 5, 1, protocol.Integer, 3), cell(entityRoot, 11, 1, protocol.OctetString, []byte("0000123")), cell(entityRoot, 13, 1, protocol.OctetString, []byte("fixture-model"))}
	d := newFixtureDriver(f)
	dev, err := d.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	get, walk := f.getCalls, f.walkCalls
	at := time.Now().UTC()
	ref := model.SourceRef{Type: model.SourceSNMP, CollectorID: "fixture-collector", TargetID: "remote:" + dev.ID, Protocol: "snmp", Provenance: model.ProvenanceSimulated}
	dev.Source = "simulated"
	dev = model.ObserveDevice(dev, ref, at, at)
	facts := map[model.IdentityEvidenceType]bool{}
	for _, v := range dev.IdentityEvidence {
		facts[v.Type] = true
		if v.Validate() != nil || v.Source.Provenance != model.ProvenanceSimulated || v.Source.TargetID != ref.TargetID || v.Time == nil {
			t.Fatal(v)
		}
		if v.Type == model.IdentitySerial && v.Value != "0000123" {
			t.Fatal("leading serial zeros lost", v)
		}
		if v.Type == model.IdentitySerial && (v.SubjectID != "entity:1" || v.SubjectKind != "chassis") {
			t.Fatal("ENTITY chassis serial assigned to network-device subject", v)
		}
		if v.Type == model.IdentityMAC && (v.SubjectID != "ifindex:31" || v.SubjectKind != "network_interface" || v.Origin != "snmp.ifPhysAddress") {
			t.Fatal("interface subject lost", v)
		}
	}
	for _, kind := range []model.IdentityEvidenceType{model.IdentitySysName, model.IdentitySysObjectID, model.IdentitySerial, model.IdentityMAC} {
		if !facts[kind] {
			t.Fatal("existing SNMP fact lost", kind)
		}
	}
	if f.getCalls != get || f.walkCalls != walk {
		t.Fatal("enrichment duplicated SNMP polling")
	}
}
