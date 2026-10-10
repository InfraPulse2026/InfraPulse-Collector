package remote

import (
	"container/list"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/scheduler"
	"InfraPulseAgent/internal/telemetry"
)

func cloneSpec(d config.Device) config.Device {
	if d.SIP != nil {
		value := *d.SIP
		d.SIP = &value
	}
	if d.IPMI.Port != nil {
		value := *d.IPMI.Port
		d.IPMI.Port = &value
	}
	if d.IPMI.CipherSuite != nil {
		value := *d.IPMI.CipherSuite
		d.IPMI.CipherSuite = &value
	}
	return d
}

func (e *entry) close() error {
	e.closeOnce.Do(func() {
		if e.driver != nil {
			e.closeErr = e.driver.Close()
		}
	})
	return e.closeErr
}

func (r *Registry) prepareEntry(input config.Device, factory AdapterFactory, managed bool) (*entry, *scheduler.Job, error) {
	dc := cloneSpec(input)
	spec := cloneSpec(dc)
	defaults := config.Polling{FastInterval: r.cfg.Telemetry.FastInterval, MediumInterval: r.cfg.Telemetry.MediumInterval, InventoryInterval: r.cfg.Telemetry.InventoryInterval, TopologyInterval: r.cfg.Telemetry.TopologyInterval, Timeout: r.cfg.Telemetry.CollectTimeout}
	target := dc.Target(defaults)
	poll := config.Polling{FastInterval: target.Polling.FastInterval, MediumInterval: target.Polling.MediumInterval, InventoryInterval: target.Polling.InventoryInterval, TopologyInterval: target.Polling.TopologyInterval, Timeout: target.Polling.Timeout}
	dc.Polling = poll
	e := &entry{cfg: dc, spec: spec, managed: managed, target: target, gate: make(chan struct{}, 1), rates: telemetry.NewRateTracker(), interfaces: make(map[string]string), links: make(map[string]model.Neighbor), maxLinks: r.cfg.Remote.MaxRows, state: State{Target: target, ConnectivityState: "unknown", Device: model.Device{ID: dc.ID, Name: dc.Name, ManagementAddress: dc.Address, Driver: dc.Driver, Type: "unknown", Location: dc.Location, VendorHint: dc.VendorHint}}, linkOrder: list.New(), linkPositions: make(map[string]*list.Element)}
	if dc.Disabled {
		return e, nil, nil
	}
	d, err := factory(dc)
	if err != nil {
		if d != nil {
			_ = d.Close()
		}
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, errors.New("remote adapter factory returned no adapter")
	}
	e.driver = d
	_, inventoryBatch := d.(driver.InventoryBatchReader)
	_, hardwareBatch := d.(driver.HardwareReader)
	e.batch = inventoryBatch || hardwareBatch
	e.maxDevices = r.cfg.Remote.MaxRows
	if dc.Driver == "redfish" {
		e.maxDevices = min(e.maxDevices, dc.Redfish.WithDefaults().MaxResources)
	}
	e.maxBatchItems = 16 * e.maxDevices
	interval := min(poll.FastInterval, poll.MediumInterval, poll.InventoryInterval, poll.TopologyInterval)
	jobTimeout := 4 * poll.Timeout
	if e.batch {
		interval, jobTimeout = batchInterval(d, poll), poll.Timeout
	}
	if _, ok := d.(driver.TelephonyReader); ok {
		interval, jobTimeout = poll.FastInterval, poll.Timeout+250*time.Millisecond
	}
	job := &scheduler.Job{Name: "remote:" + dc.ID, Interval: interval, Timeout: jobTimeout, Collect: func(ctx context.Context) error { return r.poll(ctx, e, time.Now()) }}
	return e, job, nil
}

// PreparedManaged owns only newly prepared adapters. Apply publishes a complete
// registry snapshot with changed jobs paused; Commit starts them only after the
// caller has persisted its revision. Local entries are never changed.
type PreparedManaged struct {
	mu                     sync.Mutex
	registry               *Registry
	change                 *scheduler.Change
	oldEntries, newEntries map[string]*entry
	oldOrder, newOrder     []string
	removed, created, held []*entry
	applied, finished      bool
	committed              bool
}

func (r *Registry) PrepareManaged(devices []config.Device, factory AdapterFactory) (*PreparedManaged, error) {
	if factory == nil || len(devices) > 1024 {
		return nil, errors.New("invalid managed adapter factory or target count")
	}
	r.mu.Lock()
	if r.closed || r.pending != nil {
		r.mu.Unlock()
		return nil, errors.New("remote registry unavailable or transition pending")
	}
	p := &PreparedManaged{registry: r, oldEntries: r.entries, oldOrder: append([]string(nil), r.order...), newEntries: make(map[string]*entry, len(r.entries)+len(devices))}
	r.pending = p
	p.mu.Lock()
	for _, id := range r.order {
		e := r.entries[id]
		if !e.managed {
			p.newEntries[id] = e
			p.newOrder = append(p.newOrder, id)
		}
	}
	r.mu.Unlock()
	failed := true
	defer func() {
		p.mu.Unlock()
		if failed {
			_ = p.Close()
		}
	}()
	seen := make(map[string]bool, len(devices))
	var add []scheduler.Job
	for _, dc := range devices {
		if dc.ID == "" || len(dc.ID) > 128 || strings.HasPrefix(dc.ID, "local:") || seen[dc.ID] || dc.EnableMutations {
			return nil, errors.New("invalid managed target identity or read-only options")
		}
		switch dc.Driver {
		case "snmp", "routeros", "sip", "redfish", "ipmi":
		default:
			return nil, errors.New("managed protocol unsupported")
		}
		seen[dc.ID] = true
		if local, exists := p.oldEntries[dc.ID]; exists && !local.managed {
			return nil, errors.New("managed target conflicts with static target")
		}
		old := p.oldEntries[dc.ID]
		if old != nil && old.managed && reflect.DeepEqual(old.spec, dc) {
			p.newEntries[dc.ID] = old
			p.newOrder = append(p.newOrder, dc.ID)
			continue
		}
		e, job, err := r.prepareEntry(dc, factory, true)
		if err != nil {
			return nil, err
		}
		p.created = append(p.created, e)
		p.newEntries[dc.ID] = e
		p.newOrder = append(p.newOrder, dc.ID)
		if job != nil {
			add = append(add, *job)
		}
	}
	var remove []string
	for _, id := range p.oldOrder {
		e := p.oldEntries[id]
		if e.managed && p.newEntries[id] != e {
			p.removed = append(p.removed, e)
			if e.target.Enabled {
				remove = append(remove, "remote:"+id)
			}
		}
	}
	change, err := r.scheduler.PrepareChanges(remove, add)
	if err != nil {
		return nil, err
	}
	p.change = change
	failed = false
	return p, nil
}

func (p *PreparedManaged) Apply(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.applied || p.change == nil {
		return errors.New("managed transition is not prepared")
	}
	r := p.registry
	r.mu.Lock()
	if r.closed || r.pending != p {
		r.mu.Unlock()
		return errors.New("managed registry unavailable")
	}
	for _, e := range p.removed {
		e.retired = true
	}
	r.mu.Unlock()
	if err := p.change.Apply(ctx); err != nil {
		p.restoreEntries()
		return err
	}
	for _, e := range p.removed {
		select {
		case e.gate <- struct{}{}:
			p.held = append(p.held, e)
		case <-ctx.Done():
			p.restoreEntries()
			_ = p.change.Rollback(ctx)
			return ctx.Err()
		}
	}
	r.mu.Lock()
	if err := ctx.Err(); err != nil || r.closed {
		r.mu.Unlock()
		p.restoreEntries()
		_ = p.change.Rollback(ctx)
		if err != nil {
			return err
		}
		return errors.New("managed registry closed")
	}
	r.entries, r.order = p.newEntries, p.newOrder
	p.applied = true
	r.mu.Unlock()
	return nil
}

func (p *PreparedManaged) restoreEntries() {
	r := p.registry
	r.mu.Lock()
	if p.applied {
		r.entries, r.order = p.oldEntries, p.oldOrder
		p.applied = false
	}
	for _, e := range p.removed {
		e.retired = false
	}
	r.mu.Unlock()
	for _, e := range p.held {
		<-e.gate
	}
	p.held = nil
}

func (p *PreparedManaged) Commit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		if p.committed {
			return nil
		}
		return errors.New("managed transition was rolled back")
	}
	if !p.applied {
		return errors.New("managed transition is not applied")
	}
	if err := p.change.Commit(); err != nil {
		return err
	}
	for _, e := range p.held {
		<-e.gate
	}
	p.held = nil
	for _, e := range p.removed {
		_ = e.close()
	}
	p.committed = true
	p.finish()
	return nil
}

func (p *PreparedManaged) Rollback(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return nil
	}
	var err error
	// Publish the old registry before making its jobs runnable again. Otherwise
	// a restored job can observe a retired entry and skip its first full period.
	p.restoreEntries()
	if p.change != nil {
		err = p.change.Rollback(ctx)
	}
	for _, e := range p.created {
		err = errors.Join(err, e.close())
	}
	p.finish()
	return err
}

func (p *PreparedManaged) finish() {
	p.finished = true
	p.registry.mu.Lock()
	if p.registry.pending == p {
		p.registry.pending = nil
	}
	p.registry.mu.Unlock()
}

func (p *PreparedManaged) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Rollback(ctx)
}

func (r *Registry) Telephony() []model.TelephonyHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	var states []model.TelephonyHealth
	for _, id := range r.order {
		if reader, ok := r.entries[id].driver.(interface{ State() model.TelephonyHealth }); ok {
			states = append(states, reader.State())
		}
	}
	return states
}
