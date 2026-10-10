package agent

import (
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestIdentityAdmissionRejectsUnsendableEvidenceWithoutPoisoningOutbox(t *testing.T) {
	c := testConfig(t, "127.0.0.1:19443")
	c.Local.Enabled = false
	configureDurable(&c)
	a := newTestAgent(t, c, nil)
	at := time.Now().UTC()
	ref := model.SourceRef{Type: model.SourceSNMP, TargetID: "remote:fixture", Protocol: "snmp", Provenance: model.ProvenanceSimulated}
	d := model.Device{ID: "fixture", Type: "switch", Source: "simulated", Hostname: "fixture", LastSeen: at}
	valid := model.ObserveEnvelope(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: at, Devices: []model.Device{d}}}, ref, at)
	for _, tc := range []struct {
		name   string
		change func(*model.Envelope)
	}{
		{"missing source", func(e *model.Envelope) { e.Inventory.Devices[0].IdentityEvidence[0].Source.Type = "" }},
		{"malformed UTF8", func(e *model.Envelope) { e.Inventory.Devices[0].Name = string([]byte{255}) }},
		{"oversized fact", func(e *model.Envelope) { e.Inventory.Devices[0].IdentityEvidence[0].Value = strings.Repeat("x", 4097) }},
		{"outbox bytes", func(e *model.Envelope) {
			e.Inventory.Devices[0].SystemDescription = strings.Repeat("x", c.Queue.MaxMessageBytes+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := valid
			inventory := *valid.Inventory
			inventory.Devices = []model.Device{model.CloneDevice(d)}
			inventory.Devices[0] = model.CloneDevice(valid.Inventory.Devices[0])
			e.Inventory = &inventory
			tc.change(&e)
			if err := a.push(e); err == nil {
				t.Fatal("unsendable observation admitted")
			}
			if a.queue.State().Messages != 0 {
				t.Fatal("rejected observation blocked durable outbox")
			}
		})
	}
	if err := a.push(valid); err != nil || a.queue.State().Messages != 1 {
		t.Fatal("valid source observations cannot follow rejection", err, a.queue.State())
	}
}
