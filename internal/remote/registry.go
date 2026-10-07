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
type AdapterFactory func(config.Device) (driver.Adapter, error)
type Option func(*Registry)

func WithCollectorID(id string) Option { return func(r *Registry) { r.collectorID = id } }

type State struct {
	Target                          model.Target `json:"target"`
	Device                          model.Device `json:"device"`
	ConnectivityState               string       `json:"connectivity_state"`
	ConsecutiveConnectivityFailures int          `json:"consecutive_connectivity_failures"`
	LastError                       string       `json:"last_error,omitempty"`
}
type entry struct {
	cfg                                               config.Device
	target                                            model.Target
	driver                                            driver.Adapter
	gate                                              chan struct{}
	state                                             State
	rates                                             *telemetry.RateTracker
	lastTelemetryDeviceID                             string
	interfaces                                        map[string]string
	links                                             map[string]model.Neighbor
	maxLinks                                          int
	linkOrder                                         *list.List
	linkPositions                                     map[string]*list.Element
	nextFast, nextMedium, nextInventory, nextTopology time.Time
}
type Registry struct {
	mu          sync.Mutex
	entries     map[string]*entry
	order       []string
	scheduler   *scheduler.Scheduler
	emit        func(model.Envelope) error
	collectorID string
}

// New is the compatibility bridge for existing full SNMP/RouterOS drivers.
func New(cfg config.Config, factory Factory, emit func(model.Envelope) error, options ...Option) (*Registry, error) {
	if factory == nil {
		return nil, errors.New("remote adapter factory is required")
	}
	return NewAdapters(cfg, func(dc config.Device) (driver.Adapter, error) { return factory(dc) }, emit, options...)
}

// NewAdapters uses the same bounded registry and scheduler for adapters with
// any supported subset of readers. Missing readers do not fabricate snapshots.
func NewAdapters(cfg config.Config, factory AdapterFactory, emit func(model.Envelope) error, options ...Option) (*Registry, error) {
	if factory == nil || emit == nil {
		return nil, errors.New("remote adapter factory and observation sink are required")
	}
	r := &Registry{entries: make(map[string]*entry), emit: emit}
	for _, option := range options {
		if option != nil {
			option(r)
		}
	}
	r.scheduler = scheduler.New(cfg.Remote.Concurrency, nil)
	for _, dc := range cfg.Devices {
		if _, exists := r.entries[dc.ID]; exists {
			_ = r.Close()
			return nil, errors.New("duplicate remote target ID")
		}
		d, err := factory(dc)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		if d == nil {
			_ = r.Close()
			return nil, errors.New("remote adapter factory returned no adapter")
		}
		defaults := config.Polling{FastInterval: cfg.Telemetry.FastInterval, MediumInterval: cfg.Telemetry.MediumInterval, InventoryInterval: cfg.Telemetry.InventoryInterval, TopologyInterval: cfg.Telemetry.TopologyInterval, Timeout: cfg.Telemetry.CollectTimeout}
		target := dc.Target(defaults)
		poll := config.Polling{FastInterval: target.Polling.FastInterval, MediumInterval: target.Polling.MediumInterval, InventoryInterval: target.Polling.InventoryInterval, TopologyInterval: target.Polling.TopologyInterval, Timeout: target.Polling.Timeout}
		dc.Polling = poll
		e := &entry{cfg: dc, target: target, driver: d, gate: make(chan struct{}, 1), rates: telemetry.NewRateTracker(), interfaces: make(map[string]string), links: make(map[string]model.Neighbor), maxLinks: cfg.Remote.MaxRows, state: State{Target: target, Device: model.Device{ID: dc.ID, Name: dc.Name, ManagementAddress: dc.Address, Driver: dc.Driver, Type: "unknown", Location: dc.Location, VendorHint: dc.VendorHint}}}
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

func (r *Registry) Targets() []model.Target {
	targets := make([]model.Target, 0, len(r.order))
	for _, id := range r.order {
		targets = append(targets, r.entries[id].target)
	}
	return targets
}

func (r *Registry) source(e *entry) model.SourceRef {
	return model.SourceRef{Type: e.target.Type, CollectorID: r.collectorID, TargetID: e.target.ID, Protocol: e.target.Protocol}
}

func (r *Registry) observation(e *entry, envelope model.Envelope) error {
	return r.emit(model.ObserveEnvelope(envelope, r.source(e), time.Now().UTC()))
}

// Commands keep the legacy exact route. A configured Target ID is not an alias
// for an unrelated observed Device ID, and ambiguous identities cannot mutate.
func (r *Registry) commandEntry(id string) (*entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, exists := r.entries[id]
	if !exists || e.state.Device.ID != id {
		return nil, errors.New("remote command target identity does not match")
	}
	for _, other := range r.entries {
		if other != e && other.state.Device.ID == id {
			return nil, errors.New("remote command target identity is ambiguous")
		}
	}
	return e, nil
}

// WithDevice serializes writes with all polls of this device. The driver's
// protocol operations still have their own cancellation and response bounds.
func (r *Registry) WithDevice(ctx context.Context, id string, fn func(driver.Adapter) model.CommandResult) (model.CommandResult, error) {
	e, err := r.commandEntry(id)
	if err != nil {
		return model.CommandResult{}, err
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
	if _, err := r.commandEntry(id); err != nil {
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
	if reader, supported := e.driver.(driver.TelemetryReader); supported && !now.Before(e.nextFast) {
		e.nextFast = now.Add(e.cfg.Polling.FastInterval)
		err := run(func(c context.Context) error {
			s, err := reader.Collect(c)
			connectivityObserved = true
			connectivityErr = err
			connectivitySuccessful = err == nil
			if err != nil {
				return err
			}
			if s.DeviceID == "" {
				s.DeviceID = e.cfg.ID
			}
			r.telemetry(e, &s)
			r.interfaceEvents(e, s)
			return r.observation(e, model.Envelope{Telemetry: &s})
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if reader, supported := e.driver.(driver.ProbeReader); supported && !now.Before(e.nextMedium) {
		e.nextMedium = now.Add(e.cfg.Polling.MediumInterval)
		err := run(func(c context.Context) error {
			d, err := reader.Probe(c)
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
	if reader, supported := e.driver.(driver.InventoryReader); supported && !now.Before(e.nextInventory) {
		e.nextInventory = now.Add(e.cfg.Polling.InventoryInterval)
		_, probeReader := e.driver.(driver.ProbeReader)
		_, telemetryReader := e.driver.(driver.TelemetryReader)
		inventoryConnectivity := !probeReader && !telemetryReader
		err := run(func(c context.Context) error {
			d, err := reader.Inventory(c)
			if err != nil {
				if inventoryConnectivity {
					r.connectivity(e, err, now)
				}
				return err
			}
			if changed := r.device(e, d, now, true); changed || inventoryConnectivity {
				// This successful inventory established a new primary entity;
				// or is the adapter's only device-reading capability.
				// Neither inherits the previous entity's connectivity history.
				r.connectivity(e, nil, now)
			}
			return r.observation(e, model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: now.UTC(), Devices: []model.Device{r.deviceSnapshot(e)}}})
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if reader, supported := e.driver.(driver.TopologyReader); supported && !now.Before(e.nextTopology) {
		e.nextTopology = now.Add(e.cfg.Polling.TopologyInterval)
		err := run(func(c context.Context) error {
			t, err := reader.Topology(c)
			if t.DeviceID == "" {
				t.DeviceID = e.cfg.ID
			}
			for i := range t.Observations {
				if t.Observations[i].LocalDeviceID == "" {
					t.Observations[i].LocalDeviceID = t.DeviceID
				}
			}
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
			return errors.Join(err, r.observation(e, model.Envelope{Topology: &t}))
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
func (r *Registry) device(e *entry, d model.Device, now time.Time, inventory bool) bool {
	if d.ID == "" {
		d.ID = e.cfg.ID
	}
	d.Location = e.cfg.Location
	d.ManagementAddress = e.cfg.Address
	d.VendorHint = e.cfg.VendorHint
	if d.Name == "" {
		d.Name = e.cfg.Name
	}
	r.mu.Lock()
	changed := d.ID != e.state.Device.ID
	if !inventory && !changed {
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
	if changed {
		d.Online, d.LastSeen = false, time.Time{}
		e.state.ConnectivityState = "unknown"
		e.state.ConsecutiveConnectivityFailures = 0
		if e.lastTelemetryDeviceID != d.ID {
			e.rates = telemetry.NewRateTracker()
			e.lastTelemetryDeviceID = ""
			e.interfaces = make(map[string]string)
		}
		// The single primary entity cache is replaced without claiming link
		// expiry on the previously observed entity.
		e.links = make(map[string]model.Neighbor)
		e.linkOrder.Init()
		e.linkPositions = make(map[string]*list.Element)
	} else {
		d.Online = e.state.Device.Online
		d.LastSeen = e.state.Device.LastSeen
	}
	d = model.ObserveEnvelope(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: now.UTC(), Devices: []model.Device{d}}}, r.source(e), time.Now().UTC()).Inventory.Devices[0]
	e.state.Device = d
	r.mu.Unlock()
	return changed
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
		_, probe := e.driver.(driver.ProbeReader)
		_, inventory := e.driver.(driver.InventoryReader)
		if probe || inventory {
			d := r.deviceSnapshot(e)
			_ = r.observation(e, model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: at.UTC(), Devices: []model.Device{d}}})
		}
	}
}

// A target currently emits one primary telemetry sample per call. Keep one
// baseline per target, rather than a global unbounded cache by observed ID.
func (r *Registry) telemetry(e *entry, s *model.TelemetrySample) {
	if e.lastTelemetryDeviceID != s.DeviceID {
		e.rates = telemetry.NewRateTracker()
		e.lastTelemetryDeviceID = s.DeviceID
		e.interfaces = make(map[string]string)
	}
	e.rates.Apply(s)
}

func (r *Registry) interfaceEvents(e *entry, s model.TelemetrySample) {
	current := make(map[string]string, len(s.Interfaces))
	for _, i := range s.Interfaces {
		current[i.ID] = i.OperState
		if old, ok := e.interfaces[i.ID]; ok && old != i.OperState && (i.OperState == "up" || i.OperState == "down") {
			r.eventForDevice(e, s.DeviceID, "interface."+i.OperState, "info", fmt.Sprintf("interface %s %s", i.ID, i.OperState), s.ObservedAt)
		}
	}
	e.interfaces = current
}
func linkKey(n model.Neighbor) string {
	data, _ := json.Marshal([]any{n.LocalDeviceID, n.LocalPortNumber, n.LocalPortID, n.LocalInterfaceID, n.RemoteChassisSubtype, n.RemoteChassisID, n.RemotePortSubtype, n.RemotePortID, n.Protocol, n.Source})
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
			r.eventForDevice(e, n.LocalDeviceID, "topology.link.observed", "info", "neighbor observation added", t.ObservedAt)
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
				r.eventForDevice(e, n.LocalDeviceID, "topology.link.expired", "info", "neighbor observation expired after complete snapshot", t.ObservedAt)
			}
		}
	}
}
func (r *Registry) event(e *entry, kind, severity, message string, at time.Time) {
	r.eventForDevice(e, r.deviceSnapshot(e).ID, kind, severity, message, at)
}

func (r *Registry) eventForDevice(e *entry, deviceID, kind, severity, message string, at time.Time) {
	if deviceID == "" {
		deviceID = r.deviceSnapshot(e).ID
	}
	_ = r.observation(e, model.Envelope{Events: []model.Event{{ID: uuid.NewString(), DeviceID: deviceID, Type: kind, Severity: severity, Message: message, Source: "remote", Timestamp: at.UTC()}}})
}
