package telephony

import (
	"context"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestSIPTargetSourceAndUnknownClock(t *testing.T) {
	ep := fixtureServer(t, "udp", func(int) replySpec { return replySpec{code: 200} })
	target := ep.Target()
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	ref := model.SourceRef{Type: model.SourceSIP, CollectorID: "collector-test", TargetID: target.ID, Protocol: "sip", Provenance: model.ProvenanceReal}
	c, err := New(ep, ref)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := h.Observation
	if h.TargetID != ep.ID || o == nil || o.Source.TargetID != "sip:"+ep.ID || o.Source.CollectorID != ref.CollectorID || o.Source.Type != model.SourceSIP || o.Source.Protocol != "sip" || h.Source != "sip.options" {
		t.Fatalf("SIP adaptation: %+v", h)
	}
	if o.Time.SourceTimestamp != nil || o.Time.ClockOffset != nil || o.Time.Uncertainty != nil || o.Time.Quality != model.ClockUnknown || !o.Time.ObservedAt.Equal(h.ObservedAt) {
		t.Fatalf("SIP clock metadata fabricated: %+v", o.Time)
	}
	o.Time.Quality = model.ClockSynced
	if c.State().Observation.Time.Quality != model.ClockUnknown {
		t.Fatal("health metadata shares caller storage")
	}
}
