package routeros

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func TestRouterOSIdentityUsesBoardSerialAndExistingInterfaces(t *testing.T) {
	f := newFixture(t)
	f.bodies["system/routerboard"] = `{"model":"fixture-board","serial-number":"00001","software-id":"NOT-HARDWARE-SERIAL"}`
	d := setup(t, f, false, driver.Options{})
	dev, err := d.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	dev.Source = "simulated"
	dev = model.ObserveDevice(dev, model.SourceRef{Type: model.SourceRouterOS, CollectorID: "collector", TargetID: "remote:" + dev.ID, Protocol: "https", Provenance: model.ProvenanceSimulated}, at, at)
	var serial, mac, address bool
	for _, v := range dev.IdentityEvidence {
		if v.Validate() != nil || v.Source.Type != model.SourceRouterOS || v.Source.Provenance != model.ProvenanceSimulated || v.Time == nil {
			t.Fatal(v)
		}
		switch v.Type {
		case model.IdentitySerial:
			serial = true
			if v.Value != "00001" || v.Origin != "routeros.system.routerboard.serial-number" {
				t.Fatal("software-id or fake serial used", v)
			}
		case model.IdentityMAC:
			mac = true
			if v.SubjectKind != "network_interface" || v.SubjectID == "" {
				t.Fatal("MAC subject lost", v)
			}
		case model.IdentityIP:
			address = true
			if v.SubjectKind != "network_interface" {
				t.Fatal(v)
			}
		}
	}
	if !serial || !mac || !address {
		t.Fatal("already-read RouterOS inventory facts lost", dev.IdentityEvidence)
	}
	if f.patches != 0 {
		t.Fatal("enrichment performed mutation")
	}
}
