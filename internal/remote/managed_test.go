package remote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/telephony"
	"InfraPulseAgent/internal/testutil"
)

type managedCounts struct {
	calls, closed, active, peak atomic.Int32
	block                       bool
	started                     chan struct{}
}

func (c *managedCounts) poll(ctx context.Context) error {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for old := c.peak.Load(); active > old; old = c.peak.Load() {
		if c.peak.CompareAndSwap(old, active) {
			break
		}
	}
	if c.calls.Add(1) == 1 && c.started != nil {
		close(c.started)
	}
	if c.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (c *managedCounts) Close() error                   { c.closed.Add(1); return nil }
func (*managedCounts) Capabilities() []model.Capability { return nil }

type managedNetwork struct{ *managedCounts }

func (c *managedNetwork) Collect(ctx context.Context) (model.TelemetrySample, error) {
	err := c.poll(ctx)
	at := time.Now().UTC()
	return model.TelemetrySample{Source: "simulated", ObservedAt: at, CollectedAt: at}, err
}

type managedHardware struct{ *managedCounts }

func (c *managedHardware) Hardware(ctx context.Context) ([]model.TelemetrySample, error) {
	err := c.poll(ctx)
	at := time.Now().UTC()
	return []model.TelemetrySample{{DeviceID: "hardware", Source: "simulated", ObservedAt: at, CollectedAt: at}}, err
}

type managedSIP struct{ *managedCounts }

func (c *managedSIP) CollectTelephony(ctx context.Context) (model.TelephonyUpdate, []model.Event, error) {
	err := c.poll(ctx)
	at := time.Now().UTC()
	return model.TelephonyUpdate{Source: "sip.options", ObservedAt: at, Targets: []model.TelephonyHealth{{TargetID: "managed", Source: "sip.options", State: "up", ObservedAt: at}}}, nil, err
}

func managedDevice(protocol string) config.Device {
	d := config.Device{ID: "managed", Driver: protocol, Address: "127.0.0.1", Polling: config.Polling{FastInterval: 10 * time.Millisecond, MediumInterval: time.Hour, InventoryInterval: time.Hour, TopologyInterval: time.Hour, Timeout: time.Second}}
	if protocol == "redfish" || protocol == "ipmi" {
		d.Polling = config.Polling{MediumInterval: 10 * time.Millisecond, InventoryInterval: time.Hour, Timeout: time.Second}
	}
	if protocol == "sip" {
		d.SIP = &telephony.Endpoint{ID: d.ID, Address: d.Address, Interval: 10 * time.Millisecond, Timeout: time.Second}
	}
	return d
}
func managedWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("managed lifecycle condition not reached")
		case <-ticker.C:
		}
	}
}
func managedRun(t *testing.T, r *Registry) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("remote workers leaked")
		}
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	}
}
func managedApply(t *testing.T, r *Registry, devices []config.Device, factory AdapterFactory) {
	t.Helper()
	p, err := r.PrepareManaged(devices, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = p.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLifecycleAllProtocolsPreservesSourceAndPausesUntilCommit(t *testing.T) {
	for _, protocol := range []string{"snmp", "routeros", "sip", "redfish", "ipmi"} {
		t.Run(protocol, func(t *testing.T) {
			cfg := config.Defaults()
			var adapters []*managedCounts
			var observedMu sync.Mutex
			var observed []model.SourceRef
			factory := func(d config.Device) (driver.Adapter, error) {
				c := &managedCounts{}
				adapters = append(adapters, c)
				switch d.Driver {
				case "redfish", "ipmi":
					return &managedHardware{c}, nil
				case "sip":
					return &managedSIP{c}, nil
				default:
					return &managedNetwork{c}, nil
				}
			}
			r, err := NewAdapters(cfg, factory, func(e model.Envelope) error {
				var ref model.SourceRef
				if e.Telemetry != nil {
					ref = e.Telemetry.Observation.Source
				}
				if e.Telephony != nil {
					ref = e.Telephony.Observation.Source
				}
				if ref.TargetID != "" {
					observedMu.Lock()
					observed = append(observed, ref)
					observedMu.Unlock()
				}
				return nil
			}, WithCollectorID("collector-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			defer managedRun(t, r)()
			d := managedDevice(protocol)
			p, err := r.PrepareManaged([]config.Device{d}, factory)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Apply(context.Background()); err != nil {
				t.Fatal(err)
			}
			if adapters[0].calls.Load() != 0 {
				t.Fatal("target activated before persistence")
			}
			if err = p.Commit(); err != nil {
				t.Fatal(err)
			}
			managedWait(t, func() bool { return adapters[0].calls.Load() > 0 })
			old := r.entries[d.ID]
			managedApply(t, r, []config.Device{d}, factory)
			if len(adapters) != 1 || r.entries[d.ID] != old {
				t.Fatal("unchanged snapshot recreated target")
			}
			if protocol == "sip" {
				value := *d.SIP
				value.Interval = 20 * time.Millisecond
				d.SIP = &value
			} else if protocol == "redfish" || protocol == "ipmi" {
				d.Polling.MediumInterval = 20 * time.Millisecond
			} else {
				d.Polling.FastInterval = 20 * time.Millisecond
			}
			managedApply(t, r, []config.Device{d}, factory)
			managedWait(t, func() bool { return adapters[1].calls.Load() > 0 })
			if adapters[0].closed.Load() != 1 || adapters[0].active.Load() != 0 {
				t.Fatal("updated adapter not drained and closed")
			}
			d.Disabled = true
			managedApply(t, r, []config.Device{d}, factory)
			if len(r.Stats()) != 0 || len(r.Targets()) != 1 || r.Targets()[0].Enabled || adapters[1].closed.Load() != 1 {
				t.Fatal("disabled target remains runnable")
			}
			d.Disabled = false
			managedApply(t, r, []config.Device{d}, factory)
			managedWait(t, func() bool { return adapters[2].calls.Load() > 0 })
			managedApply(t, r, nil, factory)
			if len(r.Targets()) != 0 || len(r.Stats()) != 0 || adapters[2].closed.Load() != 1 {
				t.Fatal("removed target remains active")
			}
			observedMu.Lock()
			defer observedMu.Unlock()
			if len(observed) == 0 {
				t.Fatal("no managed observations")
			}
			for _, source := range observed {
				if source.CollectorID != "collector-fixture" || source.TargetID != "remote:managed" || source.Protocol != protocol {
					t.Fatalf("unstable source: %+v", source)
				}
			}
		})
	}
}

func TestManagedPrepareFailureAndStaticConflictPreserveMonitoring(t *testing.T) {
	cfg := config.Defaults()
	cfg.Devices = []config.Device{{ID: "static", Driver: "snmp"}}
	static := &managedNetwork{&managedCounts{}}
	r, err := NewAdapters(cfg, func(config.Device) (driver.Adapter, error) { return static, nil }, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	factory := func(config.Device) (driver.Adapter, error) { return nil, errors.New("fixture initialization failure") }
	if p, err := r.PrepareManaged([]config.Device{{ID: "static", Driver: "snmp"}}, factory); err == nil {
		p.Close()
		t.Fatal("static conflict accepted")
	}
	created := &managedNetwork{&managedCounts{}}
	factory = func(d config.Device) (driver.Adapter, error) {
		if d.ID == "failure" {
			return nil, errors.New("fixture initialization failure")
		}
		return created, nil
	}
	first := managedDevice("snmp")
	second := first
	second.ID = "failure"
	if p, err := r.PrepareManaged([]config.Device{first, second}, factory); err == nil {
		p.Close()
		t.Fatal("preparation failure ignored")
	}
	if len(r.Targets()) != 1 || r.Targets()[0].ID != "remote:static" || static.closed.Load() != 0 || created.closed.Load() != 1 {
		t.Fatal("failed preparation affected current monitoring or leaked adapter")
	}
	managedApply(t, r, nil, factory)
	if len(r.Targets()) != 1 {
		t.Fatal("empty managed snapshot deleted local target")
	}
}

func TestManagedRollbackRestoresCanceledPollAndReleasesPreparedAdapter(t *testing.T) {
	cfg := config.Defaults()
	r, err := NewAdapters(cfg, func(config.Device) (driver.Adapter, error) { return nil, errors.New("unused") }, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer managedRun(t, r)()
	d := managedDevice("snmp")
	old := &managedNetwork{&managedCounts{block: true, started: make(chan struct{})}}
	managedApply(t, r, []config.Device{d}, func(config.Device) (driver.Adapter, error) { return old, nil })
	<-old.started
	newAdapter := &managedNetwork{&managedCounts{}}
	d.Polling.FastInterval = 20 * time.Millisecond
	p, err := r.PrepareManaged([]config.Device{d}, func(config.Device) (driver.Adapter, error) { return newAdapter, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if old.active.Load() != 0 || newAdapter.calls.Load() != 0 {
		t.Fatal("apply did not drain or replacement started early")
	}
	if err = p.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	managedWait(t, func() bool { return old.calls.Load() >= 2 })
	if old.closed.Load() != 0 || newAdapter.closed.Load() != 1 || old.peak.Load() != 1 {
		t.Fatal("rollback did not restore runnable original adapter")
	}
}

func TestManagedTransitionsConcurrentDiagnosticsAndBurst(t *testing.T) {
	cfg := config.Defaults()
	factory := func(config.Device) (driver.Adapter, error) { return &managedNetwork{&managedCounts{}}, nil }
	r, err := NewAdapters(cfg, factory, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer managedRun(t, r)()
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.Snapshot()
				r.Devices()
				r.Targets()
				r.Stats()
				r.Telephony()
			}
		}
	}()
	defer func() { close(stop); readers.Wait() }()
	for i := range 20 {
		d := managedDevice("snmp")
		d.Polling.FastInterval = time.Duration(10+i) * time.Millisecond
		p, err := r.PrepareManaged([]config.Device{d}, factory)
		if err != nil {
			t.Fatal(err)
		}
		if concurrent, err := r.PrepareManaged(nil, factory); err == nil {
			concurrent.Close()
			t.Fatal("unbounded concurrent transition accepted")
		}
		if err = p.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err = p.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	managedApply(t, r, nil, factory)
}

func TestManagedRemovalPreservesRetainedDurableSourceAndReplay(t *testing.T) {
	cfg := config.Defaults()
	opts := cfg.Queue.DurableOptions(testutil.TempDir(t))
	opts.BatchDelay = 0
	q, err := queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			q.Close()
		}
	}()
	factory := func(config.Device) (driver.Adapter, error) { return &managedNetwork{&managedCounts{}}, nil }
	r, err := NewAdapters(cfg, factory, q.Push, WithCollectorID("collector-retained"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	d := managedDevice("snmp")
	managedApply(t, r, []config.Device{d}, factory)
	if err = r.poll(context.Background(), r.entries[d.ID], time.Now()); err != nil {
		t.Fatal(err)
	}
	retained := q.State().Messages
	if retained == 0 {
		t.Fatal("no durable messages before removal")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := q.Pop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	managedApply(t, r, nil, factory)
	if q.State().Messages != retained {
		t.Fatal("target removal deleted retained observations")
	}
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	replay, err := reopened.Pop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replay.MessageID != first.MessageID {
		t.Fatal("removal/restart changed retained message identity")
	}
	// Pop reserves this replay until its receipt ACK. Drain must acknowledge
	// it before waiting for the remaining messages, rather than rely on expiry.
	if _, err = reopened.Acknowledge(replay.MessageID); err != nil {
		t.Fatal(err)
	}
	for reopened.State().Messages > 0 {
		e, err := reopened.Pop(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e.Telemetry != nil {
			ref := e.Telemetry.Observation.Source
			if ref.TargetID != "remote:managed" || ref.CollectorID != "collector-retained" || ref.Protocol != "snmp" {
				t.Fatal("retained source changed", ref)
			}
		}
		if _, err = reopened.Acknowledge(e.MessageID); err != nil {
			t.Fatal(err)
		}
	}
}
