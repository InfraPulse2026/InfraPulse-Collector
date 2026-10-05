// Package remote owns bounded remote polling, device state and observations.
package remote

import (
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/scheduler"
	"InfraPulseAgent/internal/telemetry"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"sync"
	"time"
)

type Factory func(config.Device) (driver.Driver, error)
type State struct {
	Device                          model.Device `json:"device"`
	ConnectivityState               string       `json:"connectivity_state"`
	ConsecutiveConnectivityFailures int          `json:"consecutive_connectivity_failures"`
	LastError                       string       `json:"last_error,omitempty"`
}
type entry struct {
	cfg                                               config.Device
	driver                                            driver.Driver
	gate                                              chan struct{}
	state                                             State
	interfaces                                        map[string]string
	links                                             map[string]model.Neighbor
	maxLinks                                          int
	linkOrder                                         *list.List
	linkPositions                                     map[string]*list.Element
	nextFast, nextMedium, nextInventory, nextTopology time.Time
}
type Registry struct {
	mu        sync.Mutex
	entries   map[string]*entry
	order     []string
	scheduler *scheduler.Scheduler
	rates     *telemetry.RateTracker
	emit      func(model.Envelope) error
}

func New(cfg config.Config, factory Factory, emit func(model.Envelope) error) (*Registry, error) {
	r := &Registry{entries: make(map[string]*entry), rates: telemetry.NewRateTracker(), emit: emit}
	r.scheduler = scheduler.New(cfg.Remote.Concurrency, nil)
	for _, dc := range cfg.Devices {
		d, err := factory(dc)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		poll := dc.Polling
		defaults := config.Polling{FastInterval: cfg.Telemetry.FastInterval, MediumInterval: cfg.Telemetry.MediumInterval, InventoryInterval: cfg.Telemetry.InventoryInterval, TopologyInterval: cfg.Telemetry.TopologyInterval, Timeout: cfg.Telemetry.CollectTimeout}
		if poll.FastInterval == 0 {
			poll.FastInterval = defaults.FastInterval
		}
		if poll.MediumInterval == 0 {
			poll.MediumInterval = defaults.MediumInterval
		}
		if poll.InventoryInterval == 0 {
			poll.InventoryInterval = defaults.InventoryInterval
		}
		if poll.TopologyInterval == 0 {
			poll.TopologyInterval = defaults.TopologyInterval
		}
		if poll.Timeout == 0 {
			poll.Timeout = defaults.Timeout
		}
		dc.Polling = poll
		e := &entry{cfg: dc, driver: d, gate: make(chan struct{}, 1), interfaces: make(map[string]string), links: make(map[string]model.Neighbor), maxLinks: cfg.Remote.MaxRows, state: State{Device: model.Device{ID: dc.ID, Name: dc.Name, ManagementAddress: dc.Address, Driver: dc.Driver, Type: "unknown", Location: dc.Location, VendorHint: dc.VendorHint}}}
		r.entries[dc.ID] = e
		e.linkOrder = list.New()
		e.linkPositions = make(map[string]*list.Element)
		e.state.ConnectivityState = "unknown"
		r.order = append(r.order, dc.ID)
		interval := min(poll.FastInterval, poll.MediumInterval, poll.InventoryInterval, poll.TopologyInterval)
		if err = r.scheduler.Add(scheduler.Job{Name: "remote:" + dc.ID, Interval: interval, Timeout: 4 * poll.Timeout, Collect: func(ctx context.Context) error { return r.poll(ctx, e, time.Now()) }}); err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	return r, nil
}
func (r *Registry) Run(ctx context.Context) error { return r.scheduler.Run(ctx) }
func (r *Registry) Close() error {
	var errs []error
	for _, id := range r.order {
		errs = append(errs, r.entries[id].driver.Close())
	}
	return errors.Join(errs...)
}
func (r *Registry) Stats() []scheduler.State { return r.scheduler.Stats() }
func (r *Registry) Snapshot() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make([]State, 0, len(r.order))
	for _, id := range r.order {
		// A JSON clone keeps diagnostics callers from sharing driver-owned slices.
		data, _ := json.Marshal(r.entries[id].state)
		var state State
		_ = json.Unmarshal(data, &state)
		states = append(states, state)
	}
	return states
}
func (r *Registry) Devices() []model.Device {
	states := r.Snapshot()
	devices := make([]model.Device, 0, len(states))
	for _, s := range states {
		devices = append(devices, s.Device)
	}
	return devices
}

// WithDevice serializes writes with all polls of this device. The driver's
// protocol operations still have their own cancellation and response bounds.
func (r *Registry) WithDevice(ctx context.Context, id string, fn func(driver.Driver) model.CommandResult) (model.CommandResult, error) {
	e, ok := r.entries[id]
	if !ok {
		return model.CommandResult{}, errors.New("unknown remote target")
	}
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return model.CommandResult{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return model.CommandResult{}, err
	}
	return fn(e.driver), nil
}
func (r *Registry) poll(ctx context.Context, e *entry, now time.Time) error {
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	default:
		return nil
	}
	var errs []error
	connectivityObserved := false
	connectivitySuccessful := false
	var connectivityErr error
	run := func(fn func(context.Context) error) error {
		c, cancel := context.WithTimeout(ctx, e.cfg.Polling.Timeout)
		defer cancel()
		return fn(c)
	}
	if !now.Before(e.nextFast) {
		e.nextFast = now.Add(e.cfg.Polling.FastInterval)
		err := run(func(c context.Context) error {
			s, err := e.driver.Collect(c)
			connectivityObserved = true
			connectivityErr = err
			connectivitySuccessful = err == nil
			if err != nil {
				return err
			}
			s.DeviceID = e.cfg.ID
			r.rates.Apply(&s)
			r.interfaceEvents(e, s)
			return r.emit(model.Envelope{Telemetry: &s})
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !now.Before(e.nextMedium) {
		e.nextMedium = now.Add(e.cfg.Polling.MediumInterval)
		err := run(func(c context.Context) error {
			d, err := e.driver.Probe(c)
			connectivityObserved = true
			if err == nil {
				connectivitySuccessful = true
			} else if connectivityErr == nil || errors.Is(err, driver.ErrConnectivity) || errors.Is(err, context.DeadlineExceeded) {
				connectivityErr = err
			}
			if err != nil {
				return err
			}
			r.device(e, d, now, false)
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if connectivityObserved {
		if connectivitySuccessful {
			r.connectivity(e, nil, now)
		} else {
			r.connectivity(e, connectivityErr, now)
		}
	}
	if !now.Before(e.nextInventory) {
		e.nextInventory = now.Add(e.cfg.Polling.InventoryInterval)
		err := run(func(c context.Context) error {
			d, err := e.driver.Inventory(c)
			if err != nil {
				return err
			}
			r.device(e, d, now, true)
			return r.emit(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: now.UTC(), Devices: []model.Device{r.deviceSnapshot(e)}}})
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !now.Before(e.nextTopology) {
		e.nextTopology = now.Add(e.cfg.Polling.TopologyInterval)
		err := run(func(c context.Context) error {
			t, err := e.driver.Topology(c)
			t.DeviceID = e.cfg.ID
			if t.ObservedAt.IsZero() {
				t.ObservedAt = now.UTC()
			}
			if t.CollectedAt.IsZero() {
				t.CollectedAt = time.Now().UTC()
			}
			if t.Source == "" {
				t.Source = e.cfg.Driver
			}
			if t.Completeness == "" {
				t.Completeness = "failed"
			}
			r.topologyEvents(e, t)
			return errors.Join(err, r.emit(model.Envelope{Topology: &t}))
		})
		if err != nil && !errors.Is(err, driver.ErrUnavailable) {
			errs = append(errs, err)
		}
	}
	result := errors.Join(errs...)
	message := ""
	if result != nil {
		switch {
		case errors.Is(result, driver.ErrConnectivity) || errors.Is(result, context.DeadlineExceeded):
			message = "remote connectivity failure"
		case errors.Is(result, driver.ErrLimit):
			message = "remote response exceeds limit"
		case errors.Is(result, driver.ErrMalformed):
			message = "malformed remote response"
		case errors.Is(result, driver.ErrUnavailable):
			message = "remote operation unavailable"
		case errors.Is(result, queue.ErrFull) || errors.Is(result, queue.ErrMessageTooLarge):
			message = "remote delivery backlog"
		default:
			message = "remote collection incomplete"
		}
	}
	r.mu.Lock()
	previous := e.state.LastError
	e.state.LastError = message
	r.mu.Unlock()
	if message != "" && message != previous {
		r.event(e, "collector.error", "warn", message, now)
	}
	// Safe aggregate prevents library errors and credentials entering scheduler stats.
	if result != nil {
		return errors.New(message)
	}
	return nil
}
func (r *Registry) device(e *entry, d model.Device, now time.Time, inventory bool) {
	d.ID = e.cfg.ID
	d.Location = e.cfg.Location
	d.ManagementAddress = e.cfg.Address
	d.VendorHint = e.cfg.VendorHint
	if d.Name == "" {
		d.Name = e.cfg.Name
	}
	r.mu.Lock()
	if !inventory {
		old := e.state.Device
		// Medium status reads do not invalidate slow hardware inventory.
		if old.Model != "" {
			d.Model = old.Model
		}
		d.SerialNumber = old.SerialNumber
		d.HardwareRevision = old.HardwareRevision
		d.FirmwareVersion = old.FirmwareVersion
		d.Components = old.Components
		if d.SoftwareVersion == "" {
			d.SoftwareVersion = old.SoftwareVersion
		}
	}
	d.Online = e.state.Device.Online
	d.LastSeen = e.state.Device.LastSeen
	e.state.Device = d
	r.mu.Unlock()
}
func (r *Registry) deviceSnapshot(e *entry) model.Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	return e.state.Device
}
func (r *Registry) connectivity(e *entry, err error, at time.Time) {
	r.mu.Lock()
	wasOnline := e.state.Device.Online
	transition := ""
	if err == nil {
		e.state.ConsecutiveConnectivityFailures = 0
		e.state.Device.LastSeen = at.UTC()
		e.state.Device.Online = true
		e.state.ConnectivityState = "online"
		if !wasOnline {
			transition = "device.online"
		}
	} else if errors.Is(err, driver.ErrConnectivity) || errors.Is(err, context.DeadlineExceeded) {
		e.state.ConsecutiveConnectivityFailures++
		if e.state.ConsecutiveConnectivityFailures >= 3 && e.state.ConnectivityState != "offline" {
			e.state.Device.Online = false
			e.state.ConnectivityState = "offline"
			transition = "device.offline"
		}
	} else if !errors.Is(err, context.Canceled) {
		e.state.ConsecutiveConnectivityFailures = 0
	}
	r.mu.Unlock()
	if transition != "" {
		r.event(e, transition, "info", transition, at)
		d := r.deviceSnapshot(e)
		_ = r.emit(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: at.UTC(), Devices: []model.Device{d}}})
	}
}
func (r *Registry) interfaceEvents(e *entry, s model.TelemetrySample) {
	current := make(map[string]string, len(s.Interfaces))
	for _, i := range s.Interfaces {
		current[i.ID] = i.OperState
		if old, ok := e.interfaces[i.ID]; ok && old != i.OperState && (i.OperState == "up" || i.OperState == "down") {
			r.event(e, "interface."+i.OperState, "info", fmt.Sprintf("interface %s %s", i.ID, i.OperState), s.ObservedAt)
		}
	}
	e.interfaces = current
}
func linkKey(n model.Neighbor) string {
	data, _ := json.Marshal([]any{n.LocalPortNumber, n.LocalPortID, n.LocalInterfaceID, n.RemoteChassisSubtype, n.RemoteChassisID, n.RemotePortSubtype, n.RemotePortID, n.Protocol, n.Source})
	return string(data)
}
func (r *Registry) topologyEvents(e *entry, t model.TopologyUpdate) {
	if t.Completeness == "failed" {
		return
	}
	seen := make(map[string]bool, len(t.Observations))
	for _, n := range t.Observations {
		key := linkKey(n)
		seen[key] = true
		if _, ok := e.links[key]; !ok {
			// Memory eviction bounds dedup history; it does not claim link removal.
			if len(e.links) >= e.maxLinks {
				oldest := e.linkOrder.Front()
				oldKey := oldest.Value.(string)
				delete(e.links, oldKey)
				delete(e.linkPositions, oldKey)
				e.linkOrder.Remove(oldest)
			}
			e.linkPositions[key] = e.linkOrder.PushBack(key)
			r.event(e, "topology.link.observed", "info", "neighbor observation added", t.ObservedAt)
		} else {
			e.linkOrder.MoveToBack(e.linkPositions[key])
		}
		e.links[key] = n
	}
	// Partial/failed collections never remove or expire previous observations.
	if t.Completeness == "complete" {
		for key, n := range e.links {
			if !seen[key] && !n.ExpiresAt.IsZero() && !t.ObservedAt.Before(n.ExpiresAt) {
				delete(e.links, key)
				e.linkOrder.Remove(e.linkPositions[key])
				delete(e.linkPositions, key)
				r.event(e, "topology.link.expired", "info", "neighbor observation expired after complete snapshot", t.ObservedAt)
			}
		}
	}
}
func (r *Registry) event(e *entry, kind, severity, message string, at time.Time) {
	_ = r.emit(model.Envelope{Events: []model.Event{{ID: uuid.NewString(), DeviceID: e.cfg.ID, Type: kind, Severity: severity, Message: message, Source: "remote", Timestamp: at.UTC()}}})
}
