package remote

import (
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureDriver struct {
	active, peak *atomic.Int32
	err          error
	block        bool
}

func (f *fixtureDriver) Probe(context.Context) (model.Device, error)     { return model.Device{}, nil }
func (f *fixtureDriver) Inventory(context.Context) (model.Device, error) { return model.Device{}, nil }
func (f *fixtureDriver) Topology(context.Context) (model.TopologyUpdate, error) {
	return model.TopologyUpdate{Completeness: "complete", Source: "simulated"}, nil
}
func (f *fixtureDriver) Capabilities() []model.Capability { return nil }
func (f *fixtureDriver) Close() error                     { return nil }
func (f *fixtureDriver) Collect(ctx context.Context) (model.TelemetrySample, error) {
	if f.active != nil {
		n := f.active.Add(1)
		defer f.active.Add(-1)
		for old := f.peak.Load(); n > old; old = f.peak.Load() {
			if f.peak.CompareAndSwap(old, n) {
				break
			}
		}
	}
	if f.block {
		<-ctx.Done()
		return model.TelemetrySample{}, ctx.Err()
	}
	now := time.Now().UTC()
	return model.TelemetrySample{Source: "simulated", ObservedAt: now, CollectedAt: now}, f.err
}
func registryForTest(t *testing.T, f driver.Driver, emit func(model.Envelope) error) *Registry {
	t.Helper()
	cfg := config.Defaults()
	cfg.Devices = []config.Device{{ID: "switch", Driver: "snmp"}}
	r, err := New(cfg, func(config.Device) (driver.Driver, error) { return f, nil }, emit)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestOfflineTransitionsIgnoreUnsupported(t *testing.T) {
	var kinds []string
	f := &fixtureDriver{}
	r := registryForTest(t, f, func(e model.Envelope) error {
		for _, v := range e.Events {
			kinds = append(kinds, v.Type)
		}
		return nil
	})
	e := r.entries["switch"]
	at := time.Now()
	r.connectivity(e, nil, at)
	r.connectivity(e, driver.ErrConnectivity, at)
	r.connectivity(e, driver.ErrUnavailable, at)
	if e.state.ConsecutiveConnectivityFailures != 0 || !e.state.Device.Online {
		t.Fatal("unsupported marked offline")
	}
	for range 2 {
		r.connectivity(e, driver.ErrConnectivity, at)
	}
	if !e.state.Device.Online {
		t.Fatal("early offline")
	}
	r.connectivity(e, driver.ErrConnectivity, at)
	if e.state.Device.Online {
		t.Fatal("three failures did not mark offline")
	}
	r.connectivity(e, driver.ErrConnectivity, at)
	r.connectivity(e, nil, at)
	if len(kinds) != 3 || kinds[0] != "device.online" || kinds[1] != "device.offline" || kinds[2] != "device.online" {
		t.Fatal(kinds)
	}
}
func TestTopologyPartialDoesNotExpire(t *testing.T) {
	var observed, expired int
	r := registryForTest(t, &fixtureDriver{}, func(e model.Envelope) error {
		for _, v := range e.Events {
			if v.Type == "topology.link.observed" {
				observed++
			}
			if v.Type == "topology.link.expired" {
				expired++
			}
		}
		return nil
	})
	e := r.entries["switch"]
	at := time.Now()
	n := model.Neighbor{RemoteChassisID: []byte("unknown"), RemotePortID: []byte("p1"), Protocol: "lldp", Source: "simulated", ExpiresAt: at.Add(time.Second)}
	r.topologyEvents(e, model.TopologyUpdate{Completeness: "complete", ObservedAt: at, Observations: []model.Neighbor{n}})
	r.topologyEvents(e, model.TopologyUpdate{Completeness: "complete", ObservedAt: at, Observations: []model.Neighbor{n}})
	for _, state := range []string{"partial", "failed"} {
		r.topologyEvents(e, model.TopologyUpdate{Completeness: state, ObservedAt: at.Add(2 * time.Second)})
	}
	if observed != 1 || expired != 0 || len(e.links) != 1 {
		t.Fatal("partial removed neighbor")
	}
	r.topologyEvents(e, model.TopologyUpdate{Completeness: "complete", ObservedAt: at.Add(2 * time.Second)})
	if expired != 1 || len(e.links) != 0 {
		t.Fatal("complete expiry failed")
	}
}
func TestBoundedWorkersAndDeviceExclusion(t *testing.T) {
	var active, peak atomic.Int32
	cfg := config.Defaults()
	cfg.Remote.Concurrency = 2
	cfg.Telemetry.FastInterval = 10 * time.Millisecond
	cfg.Telemetry.CollectTimeout = 20 * time.Millisecond
	for i := range 8 {
		cfg.Devices = append(cfg.Devices, config.Device{ID: string(rune('a' + i))})
	}
	r, err := New(cfg, func(config.Device) (driver.Driver, error) {
		return &fixtureDriver{active: &active, peak: &peak, block: true}, nil
	}, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err = r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 || active.Load() != 0 {
		t.Fatal("unbounded or leaked workers", peak.Load(), active.Load())
	}
	e := r.entries["a"]
	e.gate <- struct{}{}
	c, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	_, err = r.WithDevice(c, "a", func(driver.Adapter) model.CommandResult { return model.CommandResult{} })
	<-e.gate
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { _ = r.Snapshot() })
	}
	wg.Wait()
}
func TestDeliveryBackpressureDoesNotMarkSuccessfulDeviceOffline(t *testing.T) {
	r := registryForTest(t, &fixtureDriver{}, func(model.Envelope) error { return queue.ErrFull })
	e := r.entries["switch"]
	if err := r.poll(context.Background(), e, time.Now()); err == nil {
		t.Fatal("delivery failure lost")
	}
	if !e.state.Device.Online || e.state.Device.LastSeen.IsZero() || e.state.ConsecutiveConnectivityFailures != 0 {
		t.Fatal("delivery failure changed connectivity", e.state)
	}
}
func TestNeverSeenDeviceBecomesOfflineOnlyAfterThreeFailures(t *testing.T) {
	r := registryForTest(t, &fixtureDriver{}, func(model.Envelope) error { return nil })
	e := r.entries["switch"]
	at := time.Now()
	for range 2 {
		r.connectivity(e, driver.ErrConnectivity, at)
	}
	if e.state.ConnectivityState != "unknown" {
		t.Fatal("unobserved device marked offline early")
	}
	r.connectivity(e, driver.ErrConnectivity, at)
	if e.state.ConnectivityState != "offline" {
		t.Fatal("threshold not applied")
	}
}
func TestSuccessfulProbeWinsOverUnavailableOrFailedTelemetry(t *testing.T) {
	for _, err := range []error{driver.ErrUnavailable, driver.ErrConnectivity} {
		r := registryForTest(t, &fixtureDriver{err: err}, func(model.Envelope) error { return nil })
		e := r.entries["switch"]
		_ = r.poll(context.Background(), e, time.Now())
		if !e.state.Device.Online || e.state.ConsecutiveConnectivityFailures != 0 {
			t.Fatal("successful probe ignored", e.state)
		}
	}
}
func TestPartialTopologyHistoryBoundedWithoutExpiryEvents(t *testing.T) {
	expired := 0
	r := registryForTest(t, &fixtureDriver{}, func(e model.Envelope) error {
		for _, event := range e.Events {
			if event.Type == "topology.link.expired" {
				expired++
			}
		}
		return nil
	})
	e := r.entries["switch"]
	e.maxLinks = 2
	at := time.Now()
	for i := range 20 {
		r.topologyEvents(e, model.TopologyUpdate{Completeness: "partial", ObservedAt: at, Observations: []model.Neighbor{{RemoteChassisID: []byte{byte(i)}, ObservedAt: at.Add(time.Duration(i) * time.Second), Protocol: "lldp"}}})
		if len(e.links) > 2 {
			t.Fatal("unbounded observation history")
		}
	}
	if expired != 0 {
		t.Fatal("memory eviction reported physical expiry")
	}
}
func TestMediumProbePreservesSlowInventory(t *testing.T) {
	r := registryForTest(t, &fixtureDriver{}, func(model.Envelope) error { return nil })
	e := r.entries["switch"]
	at := time.Now()
	r.device(e, model.Device{Model: "exact chassis", SerialNumber: "serial", FirmwareVersion: "firmware", Components: []model.InventoryComponent{{ID: "chassis"}}}, at, true)
	r.device(e, model.Device{Model: "generic resource", Hostname: "new-name"}, at, false)
	if e.state.Device.Model != "exact chassis" || e.state.Device.SerialNumber != "serial" || len(e.state.Device.Components) != 1 || e.state.Device.Hostname != "new-name" {
		t.Fatal("medium erased inventory")
	}
	r.device(e, model.Device{}, at, true)
	if e.state.Device.SerialNumber != "" || len(e.state.Device.Components) != 0 {
		t.Fatal("complete inventory did not clear missing data")
	}
}
