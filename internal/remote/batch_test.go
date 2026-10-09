package remote

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

type batchFixtureAdapter struct {
	inventory                     model.InventoryUpdate
	samples                       []model.TelemetrySample
	inventoryErr, hardwareErr     error
	inventoryCalls, hardwareCalls int
	block                         bool
}

func (*batchFixtureAdapter) Close() error { return nil }
func (*batchFixtureAdapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "inventory.read", Supported: true}, {Name: "health.read", Supported: true}}
}
func (f *batchFixtureAdapter) InventoryBatch(ctx context.Context) (model.InventoryUpdate, error) {
	f.inventoryCalls++
	if f.block {
		<-ctx.Done()
		return model.InventoryUpdate{}, ctx.Err()
	}
	return f.inventory, f.inventoryErr
}
func (f *batchFixtureAdapter) Hardware(context.Context) ([]model.TelemetrySample, error) {
	f.hardwareCalls++
	return f.samples, f.hardwareErr
}

func newBatchRegistry(t *testing.T, adapter *batchFixtureAdapter, maxResources int, emit func(model.Envelope) error) *Registry {
	t.Helper()
	c := config.Defaults()
	c.Devices = []config.Device{{ID: "bmc", Driver: "redfish", Address: "https://bmc.example.test", Redfish: config.Redfish{MaxResources: maxResources}}}
	r, err := NewAdapters(c, func(config.Device) (driver.Adapter, error) { return adapter, nil }, emit, WithCollectorID("collector-test"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestBatchReaderUsesExistingSinkAndMediumCadence(t *testing.T) {
	at := time.Now().UTC()
	f := &batchFixtureAdapter{inventory: model.InventoryUpdate{Completeness: "complete", Devices: []model.Device{{ID: "system-b", Type: "server", Source: "redfish"}, {ID: "system-a", Type: "server", Source: "redfish"}}}, samples: []model.TelemetrySample{{DeviceID: "system-a", Source: "redfish", ObservedAt: at, CollectedAt: at, Metrics: []model.Metric{{Name: "temperature", Value: 25, Unit: "Celsius"}}}, {DeviceID: "system-b", Source: "redfish", ObservedAt: at, CollectedAt: at}}}
	var envelopes []model.Envelope
	r := newBatchRegistry(t, f, 128, func(e model.Envelope) error { envelopes = append(envelopes, e); return nil })
	if len(r.Devices()) != 0 {
		t.Fatal("configured endpoint fabricated a Device before discovery")
	}
	if _, err := r.WithDevice(context.Background(), "bmc", func(driver.Adapter) model.CommandResult {
		t.Fatal("batch endpoint admitted command")
		return model.CommandResult{}
	}); err == nil {
		t.Fatal("batch command route not rejected")
	}
	e := r.entries["bmc"]
	if err := r.poll(context.Background(), e, at); err != nil {
		t.Fatal(err)
	}
	if got := r.Devices(); len(got) != 2 || got[0].ID != "system-a" || got[1].ID != "system-b" {
		t.Fatal("multi-resource cache", got)
	}
	telemetry, inventory := 0, 0
	for _, envelope := range envelopes {
		if envelope.Inventory != nil {
			inventory++
			if len(envelope.Inventory.Devices) != 2 {
				t.Fatal("inventory truncated")
			}
		}
		if envelope.Telemetry != nil {
			telemetry++
			ref := envelope.Telemetry.Observation.Source
			if ref.TargetID != "remote:bmc" || ref.CollectorID != "collector-test" || ref.Type != model.SourceRedfish || ref.Protocol != "redfish" || ref.Provenance != model.ProvenanceReal {
				t.Fatal("batch source binding", ref)
			}
			if envelope.Telemetry.Observation.Time.ClockOffset != nil || envelope.Telemetry.Observation.Time.SourceTimestamp != nil {
				t.Fatal("batch fabricated device clock")
			}
		}
		for _, event := range envelope.Events {
			if event.Type == "target.online" && event.DeviceID != "" {
				t.Fatal("source endpoint event fabricated equipment identity")
			}
		}
	}
	if telemetry != 2 || inventory != 1 || f.inventoryCalls != 1 || f.hardwareCalls != 1 {
		t.Fatal("batch observations lost", telemetry, inventory)
	}
	if err := r.poll(context.Background(), e, at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if f.inventoryCalls != 1 || f.hardwareCalls != 1 {
		t.Fatal("BMC used 3s fast polling")
	}
	if err := r.poll(context.Background(), e, at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if f.inventoryCalls != 1 || f.hardwareCalls != 2 {
		t.Fatal("medium/inventory cadence not separated")
	}
	if e.cfg.Polling.FastInterval != 0 || e.cfg.Polling.TopologyInterval != 0 || len(r.Stats()) != 1 {
		t.Fatal("extra batch runtime/scheduler introduced")
	}
	states := r.Snapshot()
	states[0].Devices[0].Name = "mutated"
	if r.Devices()[0].Name == "mutated" {
		t.Fatal("diagnostics shared cache ownership")
	}
}

func TestPartialBatchRetainsPriorResourceAndHardwareFacts(t *testing.T) {
	at := time.Now().UTC()
	cores := uint32(8)
	f := &batchFixtureAdapter{inventory: model.InventoryUpdate{Completeness: "complete", Devices: []model.Device{{ID: "system-a", Source: "redfish", Model: "Server", SerialNumber: "SERIAL", Hardware: &model.HardwareInventory{HardwareUUID: "UUID", BIOSVersion: "BIOS"}, Components: []model.InventoryComponent{{ID: "cpu", Class: "processor", Manufacturer: "CPU vendor", Hardware: &model.HardwareInventory{Cores: &cores}}, {ID: "memory", Class: "memory"}}}, {ID: "system-b", Source: "redfish", Model: "Other"}}}}
	var inventories []model.InventoryUpdate
	r := newBatchRegistry(t, f, 128, func(e model.Envelope) error {
		if e.Inventory != nil {
			inventories = append(inventories, *e.Inventory)
		}
		return nil
	})
	if err := r.poll(context.Background(), r.entries["bmc"], at); err != nil {
		t.Fatal(err)
	}
	f.inventory = model.InventoryUpdate{Completeness: "partial", Devices: []model.Device{{ID: "system-a", Source: "redfish", Components: []model.InventoryComponent{{ID: "cpu", Hardware: &model.HardwareInventory{Status: model.HardwareStatus{Health: "warning"}}}}}}}
	f.inventoryErr = driver.ErrUnavailable
	if err := r.poll(context.Background(), r.entries["bmc"], at.Add(5*time.Minute)); err == nil {
		t.Fatal("partial failure hidden")
	}
	d := r.Devices()[0]
	if len(r.Devices()) != 2 || d.Model != "Server" || d.SerialNumber != "SERIAL" || d.Hardware.HardwareUUID != "UUID" || d.Hardware.BIOSVersion != "BIOS" || len(d.Components) != 2 || d.Components[0].Manufacturer != "CPU vendor" || *d.Components[0].Hardware.Cores != 8 || d.Components[0].Hardware.Status.Health != "warning" {
		t.Fatal("partial response deleted retained inventory", d)
	}
	if len(inventories) != 2 || inventories[1].Completeness != "partial" || inventories[1].Devices[0].SerialNumber != "SERIAL" {
		t.Fatal("valid partial inventory was not delivered")
	}
	d.Components[0].Hardware.Cores = nil
	if r.Devices()[0].Components[0].Hardware.Cores == nil {
		t.Fatal("partial merge shared mutable hardware")
	}
}

func TestBatchFailurePreservesValidPrefixAndDoesNotClaimReachability(t *testing.T) {
	f := &batchFixtureAdapter{inventory: model.InventoryUpdate{Completeness: "complete", Devices: []model.Device{{ID: "system", Source: "redfish"}}}, inventoryErr: driver.ErrConnectivity}
	var observations int
	r := newBatchRegistry(t, f, 128, func(e model.Envelope) error {
		if e.Inventory != nil {
			observations++
			if e.Inventory.Completeness != "partial" {
				t.Fatal("error claimed complete inventory")
			}
		}
		return nil
	})
	at := time.Now().UTC()
	for i := range 3 {
		if err := r.poll(context.Background(), r.entries["bmc"], at.Add(time.Duration(i)*5*time.Minute)); err == nil {
			t.Fatal("connectivity error hidden")
		}
	}
	state := r.Snapshot()[0]
	if observations != 3 || len(state.Devices) != 1 || state.Device.Online || state.ConnectivityState != "offline" || state.ConsecutiveConnectivityFailures != 3 {
		t.Fatal("prefix suppressed failure or target reachability", state)
	}
}

func TestBatchAdmissionBoundsAndPartialCacheRemainFinite(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		inventory model.InventoryUpdate
		samples   []model.TelemetrySample
	}{
		{name: "resource count", inventory: model.InventoryUpdate{Devices: []model.Device{{ID: "a"}, {ID: "b"}}}},
		{name: "component count", inventory: model.InventoryUpdate{Devices: []model.Device{{ID: "a", Components: make([]model.InventoryComponent, 16)}}}},
		{name: "duplicate identities", inventory: model.InventoryUpdate{Devices: []model.Device{{ID: "a"}, {ID: "a"}}}},
		{name: "empty identity", inventory: model.InventoryUpdate{Devices: []model.Device{{}}}},
		{name: "hardware items", samples: []model.TelemetrySample{{DeviceID: "a", Metrics: make([]model.Metric, 16)}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := &batchFixtureAdapter{inventory: fixture.inventory, samples: fixture.samples}
			r := newBatchRegistry(t, f, 1, func(e model.Envelope) error {
				if e.Inventory != nil && len(e.Inventory.Devices) > 0 || e.Telemetry != nil {
					t.Fatal("over-budget/invalid batch delivered")
				}
				return nil
			})
			if err := r.poll(context.Background(), r.entries["bmc"], time.Now()); err == nil {
				t.Fatal("invalid batch admitted")
			}
		})
	}
	f := &batchFixtureAdapter{inventory: model.InventoryUpdate{Completeness: "complete", Devices: []model.Device{{ID: "old"}}}}
	r := newBatchRegistry(t, f, 1, func(model.Envelope) error { return nil })
	at := time.Now()
	if err := r.poll(context.Background(), r.entries["bmc"], at); err != nil {
		t.Fatal(err)
	}
	f.inventory = model.InventoryUpdate{Completeness: "partial", Devices: []model.Device{{ID: "new"}}}
	if err := r.poll(context.Background(), r.entries["bmc"], at.Add(5*time.Minute)); err == nil {
		t.Fatal("partial retention grew past cache bound")
	}
	if len(r.Devices()) != 1 || r.Devices()[0].ID != "old" {
		t.Fatal("bounded cache lost prior facts on partial overflow")
	}
}

func TestBatchCancellationAndFailureIsolation(t *testing.T) {
	f := &batchFixtureAdapter{block: true}
	r := newBatchRegistry(t, f, 128, func(model.Envelope) error { return nil })
	r.entries["bmc"].cfg.Polling.Timeout = 20 * time.Millisecond
	started := time.Now()
	if err := r.poll(context.Background(), r.entries["bmc"], started); err == nil {
		t.Fatal("poll timeout hidden")
	}
	if time.Since(started) > time.Second || f.hardwareCalls != 0 {
		t.Fatal("whole poll deadline not bounded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.poll(ctx, r.entries["bmc"], started.Add(5*time.Minute)); err == nil {
		t.Fatal("cancellation hidden")
	}
	if !strings.Contains(r.Snapshot()[0].LastError, "incomplete") {
		t.Fatal("safe cancellation error aggregate")
	}
	if !errors.Is(boundInventory(model.InventoryUpdate{Devices: []model.Device{{ID: "a"}, {ID: "a"}}}, 2, 8), driver.ErrMalformed) {
		t.Fatal("duplicate ID not classified")
	}
}

func TestBatchBackoffIsBoundedPreservesHealthAndResetsOnRecovery(t *testing.T) {
	f := &batchFixtureAdapter{hardwareErr: driver.ErrConnectivity}
	r := newBatchRegistry(t, f, 128, func(model.Envelope) error { return nil })
	e := r.entries["bmc"]
	at := time.Now().UTC()
	if err := r.poll(context.Background(), e, at); err == nil {
		t.Fatal("failed poll hidden")
	}
	if e.batchFailures != 1 || e.nextBatchAllowed.Before(at.Add(30*time.Second)) || e.nextBatchAllowed.After(at.Add(33*time.Second)) {
		t.Fatal("first backoff/jitter out of bounds")
	}
	if err := r.poll(context.Background(), e, at.Add(time.Second)); err == nil {
		t.Fatal("cooldown erased degraded health")
	}
	if f.hardwareCalls != 1 || e.state.ConsecutiveConnectivityFailures != 1 || e.batchFailures != 1 {
		t.Fatal("cooldown performed request or counted failure")
	}
	at = e.nextBatchAllowed
	if err := r.poll(context.Background(), e, at); err == nil {
		t.Fatal("second failure hidden")
	}
	if e.batchFailures != 2 || e.nextBatchAllowed.Before(at.Add(time.Minute)) || e.nextBatchAllowed.After(at.Add(66*time.Second)) {
		t.Fatal("exponential backoff/jitter out of bounds")
	}
	for range 32 {
		at = e.nextBatchAllowed
		_ = r.poll(context.Background(), e, at)
		if e.nextBatchAllowed.Sub(at) > 5*time.Minute {
			t.Fatal("backoff exceeded cap")
		}
	}
	f.hardwareErr = nil
	at = e.nextBatchAllowed
	if err := r.poll(context.Background(), e, at); err != nil {
		t.Fatal(err)
	}
	if e.batchFailures != 0 || !e.nextBatchAllowed.IsZero() || r.Snapshot()[0].ConnectivityState != "online" || r.Snapshot()[0].LastError != "" {
		t.Fatal("recovery did not restore cadence/health")
	}
}

func TestBatchOfflineTargetDoesNotBlockHealthyTarget(t *testing.T) {
	c := config.Defaults()
	c.Remote.Concurrency = 2
	for _, id := range []string{"offline", "healthy"} {
		c.Devices = append(c.Devices, config.Device{ID: id, Driver: "redfish", Address: "https://" + id + ".example.test", Polling: config.Polling{MediumInterval: 10 * time.Millisecond, InventoryInterval: 10 * time.Millisecond, Timeout: 20 * time.Millisecond}})
	}
	blocked := &batchFixtureAdapter{block: true}
	healthy := &batchFixtureAdapter{inventory: model.InventoryUpdate{Completeness: "complete", Devices: []model.Device{{ID: "healthy-system", Source: "simulated"}}}}
	observations := make(chan string, 32)
	r, err := NewAdapters(c, func(d config.Device) (driver.Adapter, error) {
		if d.ID == "offline" {
			return blocked, nil
		}
		return healthy, nil
	}, func(e model.Envelope) error {
		if e.Inventory != nil && len(e.Inventory.Devices) > 0 {
			select {
			case observations <- e.Inventory.Devices[0].ID:
			default:
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(observations) == 0 || healthy.inventoryCalls == 0 || blocked.hardwareCalls != 0 {
		t.Fatal("offline target blocked other targets or escaped deadline")
	}
	if got := <-observations; got != "healthy-system" {
		t.Fatal("healthy target identity lost")
	}
}
