package simulator

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
)

func assertMarkers(t *testing.T, e model.Envelope) {
	t.Helper()
	if e.Hello != nil && (e.Hello.Device.Source != "simulated" || e.Hello.Device.Driver != "simulated") {
		t.Fatal("unmarked hello")
	}
	if e.Inventory != nil {
		for _, d := range e.Inventory.Devices {
			if d.Source != "simulated" || d.Driver != "simulated" {
				t.Fatal("unmarked inventory")
			}
		}
	}
	if e.Telemetry != nil && e.Telemetry.Source != "simulated" {
		t.Fatal("unmarked telemetry")
	}
	if e.Topology != nil {
		if e.Topology.Source != "simulated" {
			t.Fatal("unmarked topology")
		}
		for _, n := range e.Topology.Observations {
			if n.Source != "simulated" {
				t.Fatal("unmarked neighbor")
			}
			for _, v := range n.Evidence {
				if v.Source != "simulated" {
					t.Fatal("unmarked topology evidence")
				}
			}
		}
	}
	if e.Logs != nil {
		if e.Logs.Source != "simulated" {
			t.Fatal("unmarked batch")
		}
		for _, r := range e.Logs.Records {
			if r.Source != "simulated" {
				t.Fatal("unmarked record")
			}
		}
	}
	if e.Telephony != nil {
		if e.Telephony.Source != "simulated" {
			t.Fatal("unmarked telephony batch")
		}
		for _, h := range e.Telephony.Targets {
			if h.Source != "simulated" {
				t.Fatal("unmarked telephony target")
			}
		}
	}
	for _, event := range e.Events {
		if event.Source != "simulated" {
			t.Fatal("unmarked event")
		}
	}
}

func TestAllScenariosDeterministicMarkedBoundedAndWireCompatible(t *testing.T) {
	for _, scenario := range Scenarios() {
		t.Run(scenario, func(t *testing.T) {
			options := Options{Scenario: scenario, Duration: 8 * time.Second, Interval: time.Second, Seed: 42, Devices: 3}
			first, err := NewGenerator(options)
			if err != nil {
				t.Fatal(err)
			}
			second, _ := NewGenerator(options)
			for step := uint64(0); step < first.Steps(); step++ {
				tick := first.Tick(step)
				if !reflect.DeepEqual(tick, second.Tick(step)) || !reflect.DeepEqual(tick, first.Tick(step)) {
					t.Fatal("non deterministic payloads")
				}
				if len(tick) > 2*options.Devices+4 {
					t.Fatal("unbounded envelopes")
				}
				for _, e := range tick {
					assertMarkers(t, e)
					if e.Logs != nil && len(e.Logs.Records) > 129 {
						t.Fatal("unbounded log batch")
					}
					e.AgentID = "simulated-agent"
					e.SessionID = "simulated-session"
					e.MessageID = "message"
					e.Sequence = step + 1
					e.SentAt = first.options.Start
					wire, err := transportgrpc.Encode(e)
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := transportgrpc.DecodeEnvelope(wire)
					if err != nil {
						t.Fatal(err)
					}
					assertMarkers(t, decoded)
				}
			}
			if first.Tick(first.Steps()) != nil {
				t.Fatal("out-of-duration data")
			}
		})
	}
}

func firstTelemetry(envelopes []model.Envelope) *model.TelemetrySample {
	for _, e := range envelopes {
		if e.Telemetry != nil {
			return e.Telemetry
		}
	}
	return nil
}
func TestTrendsAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		scenario string
		cpu      []float64
		errors   []uint64
	}{{"cpu-increase", cpuTrend, nil}, {"interface-errors", nil, errorTrend}} {
		g, _ := NewGenerator(Options{Scenario: tc.scenario, Duration: 8 * time.Second, Devices: 1})
		for i := 0; i < 6; i++ {
			sample := firstTelemetry(g.Tick(uint64(i)))
			if len(tc.cpu) > 0 && *sample.CPU.UsagePercent != tc.cpu[level(uint64(i), len(tc.cpu))] {
				t.Fatal(sample.CPU)
			}
			if len(tc.errors) > 0 && sample.Interfaces[0].Counters.RXErrors != tc.errors[level(uint64(i), len(tc.errors))] {
				t.Fatal(sample.Interfaces)
			}
		}
	}
	g, _ := NewGenerator(Options{Scenario: "sip-latency", Duration: 8 * time.Second, Devices: 1})
	for step := uint64(0); step < 7; step++ {
		for _, e := range g.Tick(step) {
			if e.Telephony == nil {
				continue
			}
			h := e.Telephony.Targets[0]
			if step < 4 && (*h.LatencyMillis != sipTrend[step] || !h.Reachable) {
				t.Fatal(h)
			}
			if step >= 4 && (h.Reachable || h.ErrorCategory != "timeout" || h.ConsecutiveFailures != uint32(step-3)) {
				t.Fatal(h)
			}
		}
	}
	g, _ = NewGenerator(Options{Scenario: "device-recovery", Duration: 8 * time.Second, Devices: 2})
	for step := uint64(0); step < 7; step++ {
		tick := g.Tick(step)
		if tick[0].Inventory.Devices[0].Online != (step < 2 || step >= 5) {
			t.Fatal("device recovery", step)
		}
	}
	g, _ = NewGenerator(Options{Scenario: "topology-change", Duration: 8 * time.Second, Devices: 2})
	for step := uint64(0); step < 7; step++ {
		for _, e := range g.Tick(step) {
			if e.Topology != nil && (len(e.Topology.Observations) > 0) != (step >= 2 && step < 5) {
				t.Fatal("topology change", step, e.Topology)
			}
		}
	}
	g, _ = NewGenerator(Options{Scenario: "all", Duration: 8 * time.Second, Devices: 2})
	for _, e := range g.Tick(7) {
		if e.Telephony != nil && e.Telephony.Targets[0].State != "up" {
			t.Fatal("composite SIP did not recover", e.Telephony)
		}
	}
}

func TestSeedAndLimits(t *testing.T) {
	first, _ := NewGenerator(Options{Seed: 1})
	second, _ := NewGenerator(Options{Seed: 2})
	a, _ := json.Marshal(first.Tick(0))
	b, _ := json.Marshal(second.Tick(0))
	if string(a) == string(b) {
		t.Fatal("seed has no effect")
	}
	for _, o := range []Options{{Scenario: "unknown"}, {Devices: 65}, {Devices: -1}, {Duration: 25 * time.Hour}, {Interval: time.Millisecond}, {Duration: -time.Second}, {Interval: -time.Second}, {Start: time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)}} {
		if _, err := NewGenerator(o); err == nil {
			t.Fatal("accepted", o)
		}
	}
	max, _ := NewGenerator(Options{Scenario: "all", Devices: 64, Duration: time.Second, Interval: 10 * time.Millisecond})
	for _, e := range max.Tick(3) {
		data, _ := json.Marshal(e)
		if len(data) > 1<<20 {
			t.Fatal("default envelope limit", len(data))
		}
	}
}

func TestCompositeTopologyEventsReflectObservedLinks(t *testing.T) {
	for _, devices := range []int{1, 2, 4} {
		g, err := NewGenerator(Options{Scenario: "all", Duration: 10 * time.Second, Interval: time.Second, Devices: devices})
		if err != nil {
			t.Fatal(err)
		}
		previousLink := false
		for step := uint64(0); step < g.Steps(); step++ {
			link := false
			appeared, disappeared := false, false
			for _, e := range g.Tick(step) {
				if e.Topology != nil && e.Topology.DeviceID == deviceID(0) {
					link = len(e.Topology.Observations) > 0
				}
				for _, event := range e.Events {
					if event.Type == "topology.link.appeared" {
						appeared = true
					}
					if event.Type == "topology.link.disappeared" {
						disappeared = true
					}
				}
			}
			if link != (devices > 1 && step >= 5 && step < 8) {
				t.Fatalf("devices=%d step=%d topology=%v", devices, step, link)
			}
			if step > 0 && (appeared != (link && !previousLink) || disappeared != (!link && previousLink)) {
				t.Fatalf("false transition devices=%d step=%d prev=%v current=%v appeared=%v disappeared=%v", devices, step, previousLink, link, appeared, disappeared)
			}
			previousLink = link
		}
	}
}

func TestOfflineLastSeenPreservesLastObservation(t *testing.T) {
	for _, scenario := range []string{"device-recovery", "all"} {
		g, _ := NewGenerator(Options{Scenario: scenario, Duration: 8 * time.Second, Interval: time.Second, Devices: 2})
		lastOnline := g.Tick(1)[0].Inventory.Devices[0].LastSeen
		for step := uint64(2); step < 5; step++ {
			d := g.Tick(step)[0].Inventory.Devices[0]
			if d.Online || !d.LastSeen.Equal(lastOnline) {
				t.Fatal("offline observation changed LastSeen", scenario, step, d)
			}
		}
		d := g.Tick(5)[0].Inventory.Devices[0]
		if !d.Online || !d.LastSeen.Equal(g.options.Start.Add(5*time.Second)) {
			t.Fatal("recovery observation", scenario, d)
		}
	}
}
