package simulator

import (
	"reflect"
	"testing"
	"time"

	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/model"
)

func TestSimulatorDomainMetadataIsDeterministicAndRuntimeBound(t *testing.T) {
	g, err := NewGenerator(Options{Scenario: "all", Duration: 10 * time.Second, Interval: time.Second, Devices: 2, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	input := g.Tick(3)
	if !reflect.DeepEqual(input, g.Tick(3)) {
		t.Fatal("metadata changed deterministic generator output")
	}
	check := func(o *model.ObservationMetadata, deviceID string) {
		t.Helper()
		if o == nil || o.Source.Provenance != model.ProvenanceSimulated || o.Source.CollectorID != "collector-test" || o.Source.TargetID == "" || deviceID != "" && o.Source.TargetID == deviceID {
			t.Fatalf("simulated source binding: %+v", o)
		}
		if o.Time.ClockOffset != nil || o.Time.Uncertainty != nil || o.Time.Quality != model.ClockUnknown {
			t.Fatal("simulator fabricated clock synchronization")
		}
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range input {
		bound := metadata(e, identity.Identity{AgentID: "collector-test", SessionID: "session-test"})
		if s := bound.Telemetry; s != nil {
			check(s.Observation, s.DeviceID)
			if !s.Observation.Time.ObservedAt.Equal(s.ObservedAt) || !s.Observation.Time.CollectedAt.Equal(s.CollectedAt) {
				t.Fatal("runtime restamped simulated telemetry")
			}
		}
		if u := bound.Inventory; u != nil {
			check(u.Observation, "")
			for _, d := range u.Devices {
				check(d.Observation, d.ID)
				for _, ev := range d.IdentityEvidence {
					if ev.Source.CollectorID != "collector-test" || ev.Source.Provenance != model.ProvenanceSimulated {
						t.Fatal("identity evidence lost simulated source")
					}
				}
			}
		}
		if u := bound.Topology; u != nil {
			check(u.Observation, u.DeviceID)
			for _, n := range u.Observations {
				check(n.Observation, n.LocalDeviceID)
			}
		}
		if b := bound.Logs; b != nil {
			check(b.Observation, "")
			for _, r := range b.Records {
				check(r.Observation, r.DeviceID)
			}
		}
		if u := bound.Telephony; u != nil {
			check(u.Observation, "")
			for _, h := range u.Targets {
				check(h.Observation, "")
			}
		}
		for _, v := range bound.Events {
			check(v.Observation, v.DeviceID)
		}
	}
	if !reflect.DeepEqual(input, g.Tick(3)) {
		t.Fatal("runtime binding mutated generator values")
	}
}
