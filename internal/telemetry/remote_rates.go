package telemetry

import (
	"InfraPulseAgent/internal/model"
	"math"
)

func counterValues(c model.Counters) [8]uint64 {
	return [8]uint64{c.RXBytes, c.TXBytes, c.RXPackets, c.TXPackets, c.RXErrors, c.TXErrors, c.RXDropped, c.TXDropped}
}
func availableValues(c model.CounterAvailability) [8]bool {
	return [8]bool{c.RXBytes, c.TXBytes, c.RXPackets, c.TXPackets, c.RXErrors, c.TXErrors, c.RXDropped, c.TXDropped}
}
func widthValues(c model.CounterWidths) [8]uint8 {
	return [8]uint8{c.RXBytes, c.TXBytes, c.RXPackets, c.TXPackets, c.RXErrors, c.TXErrors, c.RXDropped, c.TXDropped}
}
func availability(v [8]bool) *model.CounterAvailability {
	return &model.CounterAvailability{RXBytes: v[0], TXBytes: v[1], RXPackets: v[2], TXPackets: v[3], RXErrors: v[4], TXErrors: v[5], RXDropped: v[6], TXDropped: v[7]}
}
func calculateRemote(old, now model.Interface, elapsed float64) *model.Rates {
	// A protocol/width transition cannot reuse a baseline from another source.
	if old.CounterMetadata == nil || now.CounterMetadata == nil {
		return nil
	}
	a, b := old.CounterMetadata, now.CounterMetadata
	if (a.DiscontinuityTicks == nil) != (b.DiscontinuityTicks == nil) {
		return nil
	}
	markerKnown := a.DiscontinuityTicks != nil && b.DiscontinuityTicks != nil
	if markerKnown && *a.DiscontinuityTicks != *b.DiscontinuityTicks {
		return nil
	}
	ov, nv := counterValues(old.Counters), counterValues(now.Counters)
	oa, na := availableValues(a.Available), availableValues(b.Available)
	ow, nw := widthValues(a.Widths), widthValues(b.Widths)
	var values [8]float64
	var present [8]bool
	any := false
	for i := range values {
		if !oa[i] || !na[i] || ow[i] != nw[i] {
			continue
		}
		var delta uint64
		var ok bool
		if nw[i] == 64 {
			delta, ok = CounterDelta(ov[i], nv[i], 64)
		} else if nw[i] == 32 {
			if ov[i] > math.MaxUint32 || nv[i] > math.MaxUint32 {
				continue
			}
			// Even an increasing Counter32 can hide multiple wraps. Without a
			// speed bound, preserve the counters and omit its derived rate.
			if i > 1 || old.SpeedBitsPerSecond == nil || now.SpeedBitsPerSecond == nil || *old.SpeedBitsPerSecond != *now.SpeedBitsPerSecond {
				continue
			}
			bound := float64(*now.SpeedBitsPerSecond) * elapsed / 8
			if bound <= 0 || bound >= float64(uint64(1)<<32) {
				continue
			}
			if nv[i] < ov[i] && !markerKnown {
				continue
			}
			delta, ok = CounterDelta(ov[i], nv[i], 32)
			if float64(delta) > bound {
				ok = false
			}
		}
		if ok {
			values[i] = float64(delta) / elapsed
			present[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}
	return &model.Rates{RXBytesPerSecond: values[0], TXBytesPerSecond: values[1], RXPacketsPerSecond: values[2], TXPacketsPerSecond: values[3], RXErrorsPerSecond: values[4], TXErrorsPerSecond: values[5], RXDroppedPerSecond: values[6], TXDroppedPerSecond: values[7], Available: availability(present)}
}
