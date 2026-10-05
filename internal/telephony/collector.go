package telephony

import (
	"context"
	"math"
	"sync"
	"time"

	"InfraPulseAgent/internal/model"
	"github.com/google/uuid"
)

type Collector struct {
	endpoint Endpoint
	pollGate chan struct{}
	mu       sync.RWMutex
	state    model.TelephonyHealth
}

func New(endpoint Endpoint) (*Collector, error) {
	endpoint = endpoint.WithDefaults()
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	return &Collector{endpoint: endpoint, pollGate: make(chan struct{}, 1), state: model.TelephonyHealth{TargetID: endpoint.ID, Transport: endpoint.Transport, State: "unknown", Source: "sip.options"}}, nil
}

func (c *Collector) State() model.TelephonyHealth {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return clone(c.state)
}

func clone(h model.TelephonyHealth) model.TelephonyHealth {
	if h.ResponseCode != nil {
		v := *h.ResponseCode
		h.ResponseCode = &v
	}
	if h.LatencyMillis != nil {
		v := *h.LatencyMillis
		h.LatencyMillis = &v
	}
	return h
}

// Collect preserves the previous health state when its scheduling context is
// canceled. Failures concern this SIP target only, never remote device online.
func (c *Collector) Collect(ctx context.Context) (model.TelephonyHealth, []model.Event, error) {
	select {
	case <-ctx.Done():
		return c.State(), nil, ctx.Err()
	case c.pollGate <- struct{}{}:
	}
	defer func() { <-c.pollGate }()
	if err := ctx.Err(); err != nil {
		return c.State(), nil, err
	}
	h := Probe(ctx, c.endpoint)
	if err := ctx.Err(); err != nil {
		return c.State(), nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.state
	if h.State != "up" {
		h.ConsecutiveFailures = previous.ConsecutiveFailures
		if h.ConsecutiveFailures < math.MaxUint32 {
			h.ConsecutiveFailures++
		}
		if !h.Reachable && h.ConsecutiveFailures >= c.endpoint.FailureThreshold {
			h.State = "down"
		}
	} else if h.LatencyMillis != nil && *h.LatencyMillis > float64(c.endpoint.LatencyThreshold)/float64(time.Millisecond) {
		h.State = "degraded"
		h.ErrorCategory = "high_latency"
	}
	var events []model.Event
	if previous.State != "unknown" && h.State != previous.State {
		severity := "info"
		if h.State == "degraded" {
			severity = "warning"
		}
		if h.State == "down" {
			severity = "error"
		}
		events = append(events, model.Event{ID: uuid.NewString(), Type: "telephony." + h.State, Timestamp: h.ObservedAt, Severity: severity, Message: "SIP OPTIONS target " + c.endpoint.ID + " changed state to " + h.State, Source: "sip.options"})
	}
	c.state = clone(h)
	return h, events, nil
}
