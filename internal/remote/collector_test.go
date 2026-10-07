package remote

import (
	"context"
	"fmt"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

type telemetryOnlyAdapter struct{ deviceID string }

func (*telemetryOnlyAdapter) Close() error { return nil }
func (*telemetryOnlyAdapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "telemetry.read", Supported: true}}
}
func (a *telemetryOnlyAdapter) Collect(context.Context) (model.TelemetrySample, error) {
	at := time.Now().UTC()
	return model.TelemetrySample{DeviceID: a.deviceID, ObservedAt: at, CollectedAt: at, Source: "simulated"}, nil
}

type inventoryOnlyAdapter struct {
	id  string
	err error
}

func (*inventoryOnlyAdapter) Close() error { return nil }
func (*inventoryOnlyAdapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "inventory.read", Supported: true}}
}
func (a *inventoryOnlyAdapter) Inventory(context.Context) (model.Device, error) {
	return model.Device{ID: a.id, Source: "simulated", Hostname: "inventory-host"}, a.err
}

func TestInventoryOnlyAdapterEstablishesReachabilityAndOfflineThreshold(t *testing.T) {
	for _, id := range []string{"connection", "observed-device"} {
		t.Run(id, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Devices = []config.Device{{ID: "connection", Driver: "snmp"}}
			adapter := &inventoryOnlyAdapter{id: id}
			var events []model.Event
			r, err := NewAdapters(cfg, func(config.Device) (driver.Adapter, error) { return adapter, nil }, func(e model.Envelope) error {
				if e.Telemetry != nil || e.Topology != nil {
					t.Fatal("inventory-only adapter fabricated another reader's observation")
				}
				events = append(events, e.Events...)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			e := r.entries["connection"]
			at := time.Now().UTC()
			if err := r.poll(context.Background(), e, at); err != nil {
				t.Fatal(err)
			}
			state := r.Snapshot()[0]
			if state.Device.ID != id || !state.Device.Online || !state.Device.LastSeen.Equal(at) || state.ConnectivityState != "online" {
				t.Fatal("successful inventory-only read did not establish observed entity reachability")
			}
			adapter.err = driver.ErrConnectivity
			for i := 1; i <= 3; i++ {
				if err := r.poll(context.Background(), e, at.Add(time.Duration(i)*cfg.Telemetry.InventoryInterval)); err == nil {
					t.Fatal("inventory-only connectivity failure was hidden")
				}
				if i < 3 && !r.Devices()[0].Online {
					t.Fatal("inventory-only target became offline before the existing threshold")
				}
			}
			state = r.Snapshot()[0]
			if state.Device.Online || state.ConnectivityState != "offline" || len(events) != 3 || events[0].DeviceID != id || events[1].DeviceID != id || events[2].DeviceID != id || events[0].Type != "device.online" || events[2].Type != "device.offline" {
				t.Fatal("inventory-only target lost state transitions or observed identity", events)
			}
		})
	}
}

func TestAdapterPollingSkipsUnsupportedReaders(t *testing.T) {
	cfg := config.Defaults()
	cfg.Devices = []config.Device{{ID: "connection-a", Driver: "snmp", Address: "192.0.2.10"}}
	var envelopes []model.Envelope
	r, err := NewAdapters(cfg, func(config.Device) (driver.Adapter, error) {
		return &telemetryOnlyAdapter{deviceID: "observed-device-a"}, nil
	}, func(e model.Envelope) error { envelopes = append(envelopes, e); return nil }, WithCollectorID("collector-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.poll(context.Background(), r.entries["connection-a"], time.Now()); err != nil {
		t.Fatal(err)
	}
	telemetry := 0
	for _, e := range envelopes {
		if e.Inventory != nil || e.Topology != nil {
			t.Fatal("missing reader fabricated an observation")
		}
		if e.Telemetry != nil {
			telemetry++
			s := e.Telemetry
			if s.DeviceID != "observed-device-a" || s.Observation == nil || s.Source != "simulated" {
				t.Fatal("target adaptation overwrote observed identity or provenance", s)
			}
			ref := s.Observation.Source
			if ref.CollectorID != "collector-a" || ref.TargetID != "remote:connection-a" || ref.Type != model.SourceSNMP || ref.Protocol != "snmp" || ref.Provenance != model.ProvenanceSimulated {
				t.Fatal("explicit source reference was lost", ref)
			}
			if s.Observation.Time.SourceTimestamp != nil || s.Observation.Time.ClockOffset != nil || s.Observation.Time.Uncertainty != nil || s.Observation.Time.Quality != model.ClockUnknown {
				t.Fatal("adapter invented a source clock measurement")
			}
		}
	}
	if telemetry != 1 || len(r.Stats()) != 1 || r.Targets()[0].ID != "remote:connection-a" {
		t.Fatal("adapter changed bounded target job orchestration")
	}
}

type distinctDeviceDriver struct {
	fixtureDriver
	id string
}

func (d *distinctDeviceDriver) Probe(context.Context) (model.Device, error) {
	return model.Device{ID: d.id, Source: "simulated", Hostname: "observed-host"}, nil
}
func (d *distinctDeviceDriver) Inventory(ctx context.Context) (model.Device, error) {
	return d.Probe(ctx)
}
func (d *distinctDeviceDriver) Collect(ctx context.Context) (model.TelemetrySample, error) {
	s, err := d.fixtureDriver.Collect(ctx)
	s.DeviceID = d.id
	return s, err
}
func (d *distinctDeviceDriver) Topology(context.Context) (model.TopologyUpdate, error) {
	at := time.Now().UTC()
	return model.TopologyUpdate{DeviceID: d.id, ObservedAt: at, CollectedAt: at, Source: "simulated", Completeness: "complete",
		Observations: []model.Neighbor{{LocalDeviceID: d.id, Protocol: "lldp", Source: "simulated", ObservedAt: at}}}, nil
}

func TestTargetAndDeviceIdentitiesRemainSeparateAcrossReaders(t *testing.T) {
	for _, observedID := range []string{"observed-device", ""} {
		t.Run(observedID, func(t *testing.T) {
			wantID := observedID
			if wantID == "" {
				wantID = "switch"
			}
			var envelopes []model.Envelope
			r := registryForTest(t, &distinctDeviceDriver{id: observedID}, func(e model.Envelope) error { envelopes = append(envelopes, e); return nil })
			defer r.Close()
			if err := r.poll(context.Background(), r.entries["switch"], time.Now()); err != nil {
				t.Fatal(err)
			}
			if r.Devices()[0].ID != wantID || r.Targets()[0].ID != "remote:switch" {
				t.Fatal("registry mixed configured target with observed device")
			}
			var telemetry, inventory, topology bool
			for _, e := range envelopes {
				if e.Telemetry != nil {
					telemetry = true
					if e.Telemetry.DeviceID != wantID || e.Telemetry.Observation.Source.TargetID != "remote:switch" {
						t.Fatal("telemetry identity was overwritten")
					}
				}
				if e.Inventory != nil {
					inventory = true
					if e.Inventory.Devices[0].ID != wantID || e.Inventory.Devices[0].Observation.Source.TargetID != "remote:switch" {
						t.Fatal("inventory identity was overwritten")
					}
				}
				if e.Topology != nil {
					topology = true
					if e.Topology.DeviceID != wantID || e.Topology.Observation.Source.TargetID != "remote:switch" {
						t.Fatal("topology identity was overwritten")
					}
				}
			}
			if !telemetry || !inventory || !topology {
				t.Fatal("existing full driver lost an observation path")
			}
		})
	}
}

func TestCommandsRejectAmbiguousOrChangedDeviceIdentity(t *testing.T) {
	cfg := config.Defaults()
	cfg.Devices = []config.Device{{ID: "a", Driver: "snmp"}, {ID: "b", Driver: "snmp"}}
	r, err := New(cfg, func(config.Device) (driver.Driver, error) { return &fixtureDriver{}, nil }, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	called := false
	execute := func(driver.Adapter) model.CommandResult { called = true; return model.CommandResult{} }
	if _, err = r.WithDevice(context.Background(), "a", execute); err != nil || !called {
		t.Fatal("legacy exact command route changed", err)
	}
	called = false
	r.device(r.entries["b"], model.Device{ID: "a"}, time.Now(), true)
	if _, err = r.WithDevice(context.Background(), "a", execute); err == nil || called {
		t.Fatal("ambiguous observed device identity admitted mutation")
	}
	r.device(r.entries["b"], model.Device{ID: "b"}, time.Now(), true)
	r.device(r.entries["a"], model.Device{ID: "observed-a"}, time.Now(), true)
	for _, requested := range []string{"a", "observed-a", "remote:a"} {
		if _, err = r.WithDevice(context.Background(), requested, execute); err == nil || called {
			t.Fatal("target/device alias admitted a new or stale mutation route")
		}
	}
}

func TestMediumProbeDoesNotReuseInventoryFromAnotherDevice(t *testing.T) {
	r := registryForTest(t, &fixtureDriver{}, func(model.Envelope) error { return nil })
	defer r.Close()
	e := r.entries["switch"]
	r.device(e, model.Device{ID: "old-device", SerialNumber: "old-serial", Model: "old-model", Components: []model.InventoryComponent{{ID: "old-chassis"}}}, time.Now(), true)
	r.device(e, model.Device{ID: "new-device", Hostname: "new-host"}, time.Now(), false)
	d := r.Devices()[0]
	if d.ID != "new-device" || d.SerialNumber != "" || d.Model != "" || len(d.Components) != 0 {
		t.Fatal("identity change inherited another device's hardware evidence")
	}
}

func rateSample(deviceID string, at time.Time, bytes uint64, state string) model.TelemetrySample {
	return model.TelemetrySample{DeviceID: deviceID, ObservedAt: at, CollectedAt: at, UptimeSeconds: 1000,
		Interfaces: []model.Interface{{ID: "eth0", Name: "eth0", OperState: state, Counters: model.Counters{RXBytes: bytes}}}}
}

func TestSameDeviceIDOnDifferentTargetsDoesNotMixCounterBaselines(t *testing.T) {
	cfg := config.Defaults()
	cfg.Devices = []config.Device{{ID: "a", Driver: "snmp"}, {ID: "b", Driver: "snmp"}}
	r, err := New(cfg, func(config.Device) (driver.Driver, error) { return &fixtureDriver{}, nil }, func(model.Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	at := time.Now().UTC()
	a1 := rateSample("shared-observed-id", at, 100, "up")
	b1 := rateSample("shared-observed-id", at.Add(500*time.Millisecond), 1000, "up")
	r.telemetry(r.entries["a"], &a1)
	r.telemetry(r.entries["b"], &b1)
	if a1.Interfaces[0].Rates != nil || b1.Interfaces[0].Rates != nil {
		t.Fatal("another target supplied a first-sample baseline")
	}
	a2 := rateSample("shared-observed-id", at.Add(time.Second), 200, "up")
	b2 := rateSample("shared-observed-id", at.Add(1500*time.Millisecond), 1300, "up")
	r.telemetry(r.entries["a"], &a2)
	r.telemetry(r.entries["b"], &b2)
	if a2.Interfaces[0].Rates == nil || b2.Interfaces[0].Rates == nil || a2.Interfaces[0].Rates.RXBytesPerSecond != 100 || b2.Interfaces[0].Rates.RXBytesPerSecond != 300 {
		t.Fatal("rates mixed counters from independently configured targets")
	}
}

func TestChangingObservedIdentityDiscardsPriorBaselinesAndInterfaceHistory(t *testing.T) {
	var events []model.Event
	r := registryForTest(t, &fixtureDriver{}, func(e model.Envelope) error { events = append(events, e.Events...); return nil })
	defer r.Close()
	e := r.entries["switch"]
	at := time.Now().UTC()
	for i := range 256 {
		state := "up"
		if i%2 != 0 {
			state = "down"
		}
		sample := rateSample(fmt.Sprintf("entity-%d", i), at.Add(time.Duration(i)*time.Second), uint64(i+100), state)
		r.telemetry(e, &sample)
		r.interfaceEvents(e, sample)
		if sample.Interfaces[0].Rates != nil {
			t.Fatal("new entity reused another entity's counter baseline")
		}
	}
	// Returning to an old ID also starts from a new baseline; the target does
	// not retain a map of all previously observed primary identities.
	sample := rateSample("entity-0", at.Add(300*time.Second), 1000, "down")
	r.telemetry(e, &sample)
	r.interfaceEvents(e, sample)
	if sample.Interfaces[0].Rates != nil || len(events) != 0 {
		t.Fatal("identity churn retained old rates or fabricated interface flaps")
	}
	// A real state change on the same current entity still emits its DeviceID.
	sample = rateSample("entity-0", at.Add(301*time.Second), 1100, "up")
	r.telemetry(e, &sample)
	r.interfaceEvents(e, sample)
	if len(events) != 1 || events[0].DeviceID != "entity-0" || events[0].Type != "interface.up" || events[0].Observation.Source.TargetID != "remote:switch" {
		t.Fatal("interface event used configured target identity instead of its sample entity")
	}
}

func TestChangedPrimaryEntityStartsWithUnknownConnectivityAndNewHistory(t *testing.T) {
	r := registryForTest(t, &fixtureDriver{}, func(model.Envelope) error { return nil })
	defer r.Close()
	e := r.entries["switch"]
	at := time.Now().UTC()
	r.device(e, model.Device{ID: "old-device"}, at, true)
	r.connectivity(e, nil, at)
	e.state.ConsecutiveConnectivityFailures = 2
	r.topologyEvents(e, model.TopologyUpdate{Completeness: "partial", ObservedAt: at,
		Observations: []model.Neighbor{{LocalDeviceID: "old-device", RemoteChassisID: []byte("peer"), Protocol: "lldp"}}})
	if !r.device(e, model.Device{ID: "new-device", Online: true, LastSeen: at}, at.Add(time.Second), true) {
		t.Fatal("new primary entity was not distinguished")
	}
	s := r.Snapshot()[0]
	if s.Device.Online || !s.Device.LastSeen.IsZero() || s.ConnectivityState != "unknown" || s.ConsecutiveConnectivityFailures != 0 || len(e.links) != 0 {
		t.Fatal("new primary entity inherited another device's reachability or topology history")
	}
	r.connectivity(e, nil, at.Add(time.Second))
	if !r.Devices()[0].Online || !r.Devices()[0].LastSeen.Equal(at.Add(time.Second)) {
		t.Fatal("successful current-entity observation did not restore online state")
	}
}

func TestTopologyHistoryScopesIdenticalLinksToLocalEntity(t *testing.T) {
	var events []model.Event
	r := registryForTest(t, &fixtureDriver{}, func(e model.Envelope) error { events = append(events, e.Events...); return nil })
	defer r.Close()
	e := r.entries["switch"]
	at := time.Now().UTC()
	for _, id := range []string{"entity-a", "entity-b"} {
		r.topologyEvents(e, model.TopologyUpdate{DeviceID: id, Completeness: "partial", ObservedAt: at,
			Observations: []model.Neighbor{{LocalDeviceID: id, LocalInterfaceID: "eth0", RemoteChassisID: []byte("same-peer"), Protocol: "lldp"}}})
	}
	if len(e.links) != 2 || len(events) != 2 || events[0].DeviceID != "entity-a" || events[1].DeviceID != "entity-b" {
		t.Fatal("topology history or event merged observations from different local entities")
	}
}
