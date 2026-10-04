package telemetry

import (
	"math"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestCounterDelta(t *testing.T) {
	cases := []struct {
		name     string
		old, now uint64
		bits     uint8
		delta    uint64
		valid    bool
	}{
		{"increasing", 100, 150, 64, 50, true},
		{"unchanged", 10, 10, 64, 0, true},
		{"64 reset", 100, 1, 64, 0, false},
		{"32 wrap", math.MaxUint32 - 4, 3, 32, 8, true},
		{"32 invalid value", math.MaxUint32 + 1, 5, 32, 0, false},
		{"unknown width", 1, 2, 16, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := CounterDelta(tc.old, tc.now, tc.bits)
			if got != tc.delta || valid != tc.valid {
				t.Fatalf("got (%d,%v), want (%d,%v)", got, valid, tc.delta, tc.valid)
			}
		})
	}
}

func sample(observed time.Time, count uint64) model.TelemetrySample {
	return model.TelemetrySample{DeviceID: "router", BootID: "boot1", UptimeSeconds: 100, ObservedAt: observed,
		Interfaces: []model.Interface{{ID: "1", Index: 1, Name: "ether1", MAC: "00:11:22:33:44:55", Counters: model.Counters{
			RXBytes: count, TXBytes: count * 2, RXPackets: count, TXPackets: count, RXErrors: count, TXErrors: count, RXDropped: count, TXDropped: count,
		}}}}
}

func TestRatesUseActualObservationIntervalAndEveryCounter(t *testing.T) {
	tracker := NewRateTracker()
	first := sample(time.Unix(1000, 0), 100)
	tracker.Apply(&first)
	if first.Interfaces[0].Rates != nil {
		t.Fatal("initial rates must be absent")
	}
	next := sample(first.ObservedAt.Add(2500*time.Millisecond), 125)
	tracker.Apply(&next)
	rates := next.Interfaces[0].Rates
	if rates == nil {
		t.Fatal("missing valid rates")
	}
	if rates.RXBytesPerSecond != 10 || rates.TXBytesPerSecond != 20 || rates.RXPacketsPerSecond != 10 || rates.TXPacketsPerSecond != 10 || rates.RXErrorsPerSecond != 10 || rates.TXErrorsPerSecond != 10 || rates.RXDroppedPerSecond != 10 || rates.TXDroppedPerSecond != 10 {
		t.Fatalf("wrong rates: %+v", rates)
	}
}

func TestRateDiscontinuitiesAndIdentityChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.TelemetrySample)
	}{
		{"reboot ID", func(s *model.TelemetrySample) { s.BootID = "boot2" }},
		{"uptime reset", func(s *model.TelemetrySample) { s.UptimeSeconds = 1 }},
		{"counter reset", func(s *model.TelemetrySample) { s.Interfaces[0].Counters.RXErrors = 0 }},
		{"interface ID", func(s *model.TelemetrySample) { s.Interfaces[0].ID = "2" }},
		{"interface name", func(s *model.TelemetrySample) { s.Interfaces[0].Name = "replacement" }},
		{"interface MAC", func(s *model.TelemetrySample) { s.Interfaces[0].MAC = "new" }},
		{"device ID", func(s *model.TelemetrySample) { s.DeviceID = "other" }},
		{"out of order", func(s *model.TelemetrySample) { s.ObservedAt = s.ObservedAt.Add(-time.Minute) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := New()
			first := sample(time.Unix(1000, 0), 100)
			tracker.Apply(&first)
			next := sample(first.ObservedAt.Add(3*time.Second), 125)
			next.Interfaces[0].Rates = &model.Rates{RXBytesPerSecond: 123}
			tc.mutate(&next)
			tracker.Apply(&next)
			if next.Interfaces[0].Rates != nil {
				t.Fatalf("stale rates survived %s", tc.name)
			}
		})
	}
}

func TestVanishedInterfacesArePruned(t *testing.T) {
	tracker := New()
	first := sample(time.Unix(1000, 0), 100)
	tracker.Apply(&first)
	empty := sample(first.ObservedAt.Add(time.Second), 100)
	empty.Interfaces = nil
	tracker.Apply(&empty)
	returned := sample(first.ObservedAt.Add(2*time.Second), 200)
	tracker.Apply(&returned)
	if returned.Interfaces[0].Rates != nil {
		t.Fatal("recreated interface used removed baseline")
	}
}

func TestLargeCountersKeepSmallDeltasPrecise(t *testing.T) {
	tracker := New()
	first := sample(time.Unix(1000, 0), 0)
	first.Interfaces[0].Counters.RXBytes = uint64(1) << 60
	tracker.Apply(&first)
	next := sample(first.ObservedAt.Add(time.Second), 0)
	next.Interfaces[0].Counters.RXBytes = (uint64(1) << 60) + 7
	tracker.Apply(&next)
	if next.Interfaces[0].Rates == nil || next.Interfaces[0].Rates.RXBytesPerSecond != 7 {
		t.Fatalf("lost integer precision: %+v", next.Interfaces[0].Rates)
	}
}

func TestLateSampleDoesNotCorruptNewerBaseline(t *testing.T) {
	tracker := New()
	first := sample(time.Unix(1000, 0), 100)
	tracker.Apply(&first)
	late := sample(first.ObservedAt.Add(-time.Second), 50)
	tracker.Apply(&late)
	if late.Interfaces[0].Rates != nil {
		t.Fatal("late sample has rates")
	}
	next := sample(first.ObservedAt.Add(time.Second), 110)
	tracker.Apply(&next)
	if next.Interfaces[0].Rates == nil || next.Interfaces[0].Rates.RXBytesPerSecond != 10 {
		t.Fatalf("late sample corrupted baseline: %+v", next.Interfaces[0].Rates)
	}
}
