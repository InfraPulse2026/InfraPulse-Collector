//go:build integration

package agent

import (
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
)

// This opt-in integration consumes an independently running official DMTF
// Mockup Server or Interface Emulator. It only permits a loopback HTTPS fixture,
// so normal tests never connect to a production BMC.
func TestDMTFRedfishServiceCollectorDurableACK(t *testing.T) {
	endpoint := os.Getenv("INFRAPULSE_REDFISH_FIXTURE_ENDPOINT")
	if endpoint == "" {
		t.Skip("official DMTF fixture endpoint not configured")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
		t.Fatal("DMTF integration accepts only explicit loopback HTTPS fixtures")
	}
	ca := os.Getenv("INFRAPULSE_REDFISH_FIXTURE_CA")
	if ca == "" {
		t.Fatal("fixture CA is required; TLS verification cannot be disabled")
	}
	auth := os.Getenv("INFRAPULSE_REDFISH_FIXTURE_AUTH")
	if auth == "" {
		auth = "basic"
	}
	minimumSystems := 1
	if text := os.Getenv("INFRAPULSE_REDFISH_FIXTURE_MIN_SYSTEMS"); text != "" {
		minimumSystems, err = strconv.Atoi(text)
		if err != nil || minimumSystems < 1 || minimumSystems > 32 {
			t.Fatal("invalid expected fixture system count")
		}
	}
	minimumChassis := 1
	if text := os.Getenv("INFRAPULSE_REDFISH_FIXTURE_MIN_CHASSIS"); text != "" {
		minimumChassis, err = strconv.Atoi(text)
		if err != nil || minimumChassis < 1 || minimumChassis > 32 {
			t.Fatal("invalid expected fixture chassis count")
		}
	}
	observed := make(chan model.Envelope, 1024)
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	redfishFixtureConfig(t, &c, endpoint, ca, auth)
	c.Devices[0].Polling.MediumInterval = time.Second
	c.Devices[0].Polling.InventoryInterval = time.Second
	c.Devices[0].Polling.Timeout = 30 * time.Second
	c.Devices[0].Redfish.RequestTimeout = 5 * time.Second
	c.Devices[0].Redfish.MaxResources = 512
	c.Devices[0].Redfish.MaxRequests = 2048
	a := newTestAgent(t, c, nil)
	stop := runTestAgent(t, a)
	defer stop()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	identities := map[string]string{}
	var inventories, telemetry int
	var lastInventory, lastTelemetry string
	for inventories < 2 || telemetry == 0 {
		select {
		case envelope := <-observed:
			if update := envelope.Inventory; update != nil && len(update.Devices) > 0 {
				systems, managers, chassis := 0, 0, 0
				for _, device := range update.Devices {
					if device.Hardware == nil || device.Observation == nil || device.Source != "simulated" || device.Observation.Source.Type != model.SourceRedfish || device.Observation.Source.Provenance != model.ProvenanceSimulated {
						t.Fatal("DMTF inventory lost normalized hardware/provenance", device)
					}
					resource := device.Observation.Source.ResourceID
					if old := identities[resource]; old != "" && old != device.ID {
						t.Fatal("DMTF fixture resource identity changed across polls", resource)
					}
					identities[resource] = device.ID
					switch device.Hardware.ResourceType {
					case "ComputerSystem":
						systems++
						if device.Vendor == "" || device.Model == "" || device.SerialNumber == "" || device.Hardware.HardwareUUID == "" || device.Hardware.PowerState == "" {
							t.Fatal("official provided system identity/power fields lost", device)
						}
						t.Logf("official system: manufacturer=%s model=%s serial=%s uuid=%s BIOS=%s power=%s health=%s", device.Vendor, device.Model, device.SerialNumber, device.Hardware.HardwareUUID, device.Hardware.BIOSVersion, device.Hardware.PowerState, device.Hardware.Status.Health)
					case "Manager":
						managers++
					case "Chassis":
						chassis++
					}
				}
				if systems < minimumSystems || managers == 0 || chassis < minimumChassis {
					t.Fatalf("official discovery: systems=%d expected>=%d managers=%d chassis=%d expected>=%d", systems, minimumSystems, managers, chassis, minimumChassis)
				}
				inventories++
				lastInventory = envelope.MessageID
				t.Logf("official fixture inventory: resources=%d systems=%d managers=%d chassis=%d completeness=%s", len(update.Devices), systems, managers, chassis, update.Completeness)
			}
			if sample := envelope.Telemetry; sample != nil && len(sample.Metrics) > 0 {
				if sample.Source != "simulated" || sample.CPU != nil || sample.Memory != nil {
					t.Fatal("DMTF hardware telemetry was represented as host utilization")
				}
				telemetry++
				lastTelemetry = envelope.MessageID
				t.Logf("official fixture telemetry: metrics=%d component_health=%d", len(sample.Metrics), len(sample.HardwareHealth))
			}
		case <-deadline.C:
			t.Fatalf("DMTF Collector->Mock inventory/telemetry timed out: inventories=%d telemetry=%d errors=%v", inventories, telemetry, a.Snapshot().RecentErrors)
		}
	}
	awaitP2(t, func() bool { return !a.queue.Contains(lastInventory) && !a.queue.Contains(lastTelemetry) })
	t.Logf("PASS: real official DMTF process -> adapter -> Collector -> durable outbox -> gRPC Mock application ACK; resources=%d auth=%s TLS verified; hardware/Backend validation not performed", len(identities), auth)
}
