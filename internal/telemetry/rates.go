// Package telemetry derives rates from normalized cumulative counters.
package telemetry

import (
	"math"
	"sync"
	"time"

	"InfraPulseAgent/internal/model"
)

// CounterDelta computes a modular difference for explicitly known 32-bit
// counters. Callers must separately rule out reboot/discontinuity. A decreasing
// 64-bit counter is conservatively treated as a reset, not a wrap.
func CounterDelta(previous, current uint64, bits uint8) (uint64, bool) {
	switch bits {
	case 32:
		if previous > math.MaxUint32 || current > math.MaxUint32 {
			return 0, false
		}
		if current >= previous {
			return current - previous, true
		}
		return (uint64(1) << 32) - previous + current, true
	case 64:
		if current < previous {
			return 0, false
		}
		return current - previous, true
	default:
		return 0, false
	}
}

type interfaceIdentity struct {
	id        string
	index     int
	name, mac string
}
type previousSample struct {
	observed        time.Time
	bootID          string
	uptime          float64
	interfaces      map[interfaceIdentity]model.Interface
	uptimeAvailable bool
}

type RateTracker struct {
	mu      sync.Mutex
	devices map[string]previousSample
}

func NewRateTracker() *RateTracker { return &RateTracker{devices: make(map[string]previousSample)} }
func New() *RateTracker            { return NewRateTracker() }

// Apply replaces Rates using the actual interval between observations. Missing
// baselines, interface changes, reboot, out-of-order time or any counter reset
// leave all interface rates absent. Each snapshot prunes vanished interfaces.
// The caller owns sample and must not mutate it concurrently with Apply.
func (r *RateTracker) Apply(sample *model.TelemetrySample) {
	if sample == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.devices == nil {
		r.devices = make(map[string]previousSample)
	}
	previous, found := r.devices[sample.DeviceID]
	valid := sample.DeviceID != "" && !sample.ObservedAt.IsZero() && sample.UptimeSeconds >= 0 && !math.IsNaN(sample.UptimeSeconds) && !math.IsInf(sample.UptimeSeconds, 0)
	elapsed := sample.ObservedAt.Sub(previous.observed).Seconds()
	uptimeAvailable := sample.UptimeAvailable == nil || *sample.UptimeAvailable
	continuous := valid && found && elapsed > 0 && sample.BootID == previous.bootID && sample.UptimeSeconds >= previous.uptime && uptimeAvailable && previous.uptimeAvailable
	current := previousSample{observed: sample.ObservedAt, bootID: sample.BootID, uptime: sample.UptimeSeconds, uptimeAvailable: uptimeAvailable, interfaces: make(map[interfaceIdentity]model.Interface, len(sample.Interfaces))}
	for i := range sample.Interfaces {
		iface := &sample.Interfaces[i]
		iface.Rates = nil
		key := interfaceIdentity{id: iface.ID, index: iface.Index, name: iface.Name, mac: iface.MAC}
		if old, ok := previous.interfaces[key]; continuous && ok {
			if old.CounterMetadata == nil && iface.CounterMetadata == nil {
				iface.Rates = calculate(old.Counters, iface.Counters, elapsed)
			} else {
				iface.Rates = calculateRemote(old, *iface, elapsed)
			}
		}
		baseline := *iface
		baseline.Rates = nil
		if iface.CounterMetadata != nil {
			metadata := *iface.CounterMetadata
			if metadata.DiscontinuityTicks != nil {
				ticks := *metadata.DiscontinuityTicks
				metadata.DiscontinuityTicks = &ticks
			}
			baseline.CounterMetadata = &metadata
		}
		if iface.SpeedBitsPerSecond != nil {
			speed := *iface.SpeedBitsPerSecond
			baseline.SpeedBitsPerSecond = &speed
		}
		current.interfaces[key] = baseline
	}
	// A late or duplicate sample cannot replace a newer baseline: doing so would
	// distort the next rate, even when the late sample itself has absent rates.
	if valid && found && elapsed <= 0 {
		return
	}
	if valid {
		r.devices[sample.DeviceID] = current
	} else {
		delete(r.devices, sample.DeviceID)
	}
}

func calculate(previous, current model.Counters, elapsed float64) *model.Rates {
	old := [...]uint64{previous.RXBytes, previous.TXBytes, previous.RXPackets, previous.TXPackets, previous.RXErrors, previous.TXErrors, previous.RXDropped, previous.TXDropped}
	now := [...]uint64{current.RXBytes, current.TXBytes, current.RXPackets, current.TXPackets, current.RXErrors, current.TXErrors, current.RXDropped, current.TXDropped}
	var rates [8]float64
	for i := range old {
		delta, ok := CounterDelta(old[i], now[i], 64)
		if !ok {
			return nil
		}
		rates[i] = float64(delta) / elapsed
	}
	return &model.Rates{RXBytesPerSecond: rates[0], TXBytesPerSecond: rates[1], RXPacketsPerSecond: rates[2], TXPacketsPerSecond: rates[3], RXErrorsPerSecond: rates[4], TXErrorsPerSecond: rates[5], RXDroppedPerSecond: rates[6], TXDroppedPerSecond: rates[7]}
}
