package agent

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestCollectorLocalJobsBindTargetWithoutChangingLegacyIdentity(t *testing.T) {
	c := testConfig(t, "127.0.0.1:19443")
	a := newTestAgent(t, c, newFixturePlatform())
	var collector *Collector = a
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- collector.scheduler.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("scheduler: %v", err)
		}
	}()
	var inventory, telemetry bool
	for !inventory || !telemetry {
		e, err := a.queue.Pop(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var observation *model.ObservationMetadata
		if s := e.Telemetry; s != nil {
			telemetry = true
			observation = s.Observation
			if s.DeviceID != "local:"+a.id.AgentID || s.Source != "simulated" || observation.Source.Provenance != model.ProvenanceSimulated {
				t.Fatalf("legacy telemetry/provenance changed: %+v", s)
			}
			if !observation.Time.ObservedAt.Equal(s.ObservedAt) || !observation.Time.CollectedAt.Equal(s.CollectedAt) {
				t.Fatal("local sample times were restamped")
			}
		}
		if u := e.Inventory; u != nil {
			inventory = true
			observation = u.Observation
			if len(u.Devices) != 1 || u.Devices[0].ID != "local:"+a.id.AgentID || u.Devices[0].Observation.Source.TargetID != a.localTarget.ID {
				t.Fatalf("inventory target/device binding lost: %+v", u)
			}
		}
		if observation != nil {
			if observation.Source.CollectorID != a.id.AgentID || observation.Source.TargetID != a.localTarget.ID || observation.Source.Type != model.SourceLocal || observation.Source.TargetID == "local:"+a.id.AgentID {
				t.Fatalf("collector source binding incorrect: %+v", observation)
			}
			if observation.Time.ClockOffset != nil || observation.Time.Uncertainty != nil || observation.Time.SourceTimestamp != nil || observation.Time.Quality != model.ClockUnknown {
				t.Fatal("unknown clock metadata was fabricated")
			}
		}
	}
}

func TestCollectorTargetsAreDiagnosticProjections(t *testing.T) {
	a := newTestAgent(t, testConfig(t, "127.0.0.1:19443"), newFixturePlatform())
	snapshot := a.Snapshot()
	if len(snapshot.Targets) != 1 || snapshot.Targets[0].ID != a.localTarget.ID || snapshot.Targets[0].Polling.FastInterval != a.cfg.Telemetry.FastInterval {
		t.Fatalf("missing configured local target: %+v", snapshot.Targets)
	}
	if err := snapshot.Targets[0].Validate(); err != nil {
		t.Fatal(err)
	}
	snapshot.Targets[0].ID = "caller-mutated"
	if a.Snapshot().Targets[0].ID == "caller-mutated" {
		t.Fatal("diagnostic target mutation reached Collector")
	}
}
