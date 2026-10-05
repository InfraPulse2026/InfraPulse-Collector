package telemetry

import (
	"InfraPulseAgent/internal/model"
	"math"
	"testing"
	"time"
)

func remoteSample(at time.Time, count uint64, bits uint8) model.TelemetrySample {
	s := sample(at, count)
	tick := uint32(0)
	speed := uint64(1_000_000_000)
	s.Interfaces[0].SpeedBitsPerSecond = &speed
	s.Interfaces[0].CounterMetadata = &model.CounterMetadata{Available: model.CounterAvailability{RXBytes: true, TXBytes: true}, Widths: model.CounterWidths{RXBytes: bits, TXBytes: bits}, DiscontinuityTicks: &tick}
	return s
}
func TestRemotePresenceAndDiscontinuity(t *testing.T) {
	at := time.Unix(1000, 0)
	r := New()
	a := remoteSample(at, 100, 64)
	r.Apply(&a)
	b := remoteSample(at.Add(2*time.Second), 140, 64)
	r.Apply(&b)
	rate := b.Interfaces[0].Rates
	if rate == nil || rate.RXBytesPerSecond != 20 || rate.Available.RXErrors {
		t.Fatal("presence/rates", rate)
	}
	c := remoteSample(at.Add(3*time.Second), 180, 64)
	*c.Interfaces[0].CounterMetadata.DiscontinuityTicks = 10
	r.Apply(&c)
	if c.Interfaces[0].Rates != nil {
		t.Fatal("discontinuity ignored")
	}
}
func TestCounter32WrapRequiresBoundAndMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.TelemetrySample)
		valid  bool
	}{
		{"safe wrap", func(*model.TelemetrySample) {}, true},
		{"no marker", func(s *model.TelemetrySample) { s.Interfaces[0].CounterMetadata.DiscontinuityTicks = nil }, false},
		{"no speed", func(s *model.TelemetrySample) { s.Interfaces[0].SpeedBitsPerSecond = nil }, false},
		{"multiple wraps", func(s *model.TelemetrySample) { v := uint64(100_000_000_000); s.Interfaces[0].SpeedBitsPerSecond = &v }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			at := time.Unix(1000, 0)
			a := remoteSample(at, math.MaxUint32-4, 32)
			a.Interfaces[0].Counters.TXBytes = 0
			tc.mutate(&a)
			r.Apply(&a)
			b := remoteSample(at.Add(time.Second), 3, 32)
			b.Interfaces[0].Counters.TXBytes = 0
			tc.mutate(&b)
			r.Apply(&b)
			rate := b.Interfaces[0].Rates
			if tc.valid {
				if rate == nil || !rate.Available.RXBytes || rate.RXBytesPerSecond != 8 {
					t.Fatal(rate)
				}
			} else if rate != nil && rate.Available.RXBytes {
				t.Fatal("ambiguous wrap produced rate")
			}
		})
	}
}
func TestRemoteRebootResetAndUnknownUptime(t *testing.T) {
	for _, mutate := range []func(*model.TelemetrySample){func(s *model.TelemetrySample) { s.UptimeSeconds = 1 }, func(s *model.TelemetrySample) { v := false; s.UptimeAvailable = &v }, func(s *model.TelemetrySample) { s.Interfaces[0].Counters.RXBytes = 1 }} {
		r := New()
		at := time.Unix(1000, 0)
		a := remoteSample(at, 100, 64)
		r.Apply(&a)
		b := remoteSample(at.Add(time.Second), 150, 64)
		mutate(&b)
		r.Apply(&b)
		if b.Interfaces[0].Rates != nil && b.Interfaces[0].Rates.Available.RXBytes {
			t.Fatal("reset/reboot generated spike")
		}
	}
}
