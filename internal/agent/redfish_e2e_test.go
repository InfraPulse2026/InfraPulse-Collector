package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
)

// These are deliberately synthetic schema fixtures, not hardware responses.
// The configured simulated provenance is carried through the real adapter,
// scheduler, sanitizer, durable outbox and gRPC mapping.
type redfishAgentFixture struct {
	resources map[string]any
	logins    atomic.Int32
	logouts   atomic.Int32
	writes    atomic.Int32
}

func newRedfishAgentFixture() *redfishAgentFixture {
	link := func(path string) map[string]string { return map[string]string{"@odata.id": path} }
	status := func(health string) map[string]string {
		return map[string]string{"State": "Enabled", "Health": health, "HealthRollup": health}
	}
	collection := func(kind, path string, members ...string) map[string]any {
		refs := make([]any, 0, len(members))
		for _, member := range members {
			refs = append(refs, link(member))
		}
		return map[string]any{"@odata.type": "#" + kind + "Collection." + kind + "Collection", "@odata.id": path, "Name": kind + " Collection", "Members": refs, "Members@odata.count": len(refs)}
	}
	f := &redfishAgentFixture{resources: map[string]any{}}
	f.resources["/redfish/v1"] = map[string]any{"@odata.id": "/redfish/v1/", "@odata.type": "#ServiceRoot.v1_16_0.ServiceRoot", "RedfishVersion": "1.16.0", "Systems": link("/redfish/v1/Systems"), "Chassis": link("/redfish/v1/Chassis"), "Managers": link("/redfish/v1/Managers"), "SessionService": link("/redfish/v1/SessionService"), "Links": map[string]any{"Sessions": link("/redfish/v1/SessionService/Sessions")}}
	f.resources["/redfish/v1/SessionService"] = map[string]any{"@odata.id": "/redfish/v1/SessionService", "@odata.type": "#SessionService.v1_2_0.SessionService", "Sessions": link("/redfish/v1/SessionService/Sessions")}
	f.resources["/redfish/v1/Systems"] = collection("ComputerSystem", "/redfish/v1/Systems", "/redfish/v1/Systems/A", "/redfish/v1/Systems/B")
	for _, id := range []string{"A", "B"} {
		path := "/redfish/v1/Systems/" + id
		power := "On"
		if id == "B" {
			power = "Off"
		}
		f.resources[path] = map[string]any{"@odata.id": path, "@odata.type": "#ComputerSystem.v1_22_0.ComputerSystem", "Id": id, "Name": "fixture-system-" + id, "Manufacturer": "Fixture Equipment", "Model": "FixtureModel", "SerialNumber": "SERIAL-" + id, "UUID": "38947555-7742-3448-3784-82334782383" + map[string]string{"A": "4", "B": "5"}[id], "BiosVersion": "fixture-bios-1", "PowerState": power, "Status": status("OK"), "Links": map[string]any{"Chassis": []any{link("/redfish/v1/Chassis/C")}, "ManagedBy": []any{link("/redfish/v1/Managers/M")}}}
	}
	a := f.resources["/redfish/v1/Systems/A"].(map[string]any)
	a["Processors"] = link("/redfish/v1/Systems/A/Processors")
	a["Memory"] = link("/redfish/v1/Systems/A/Memory")
	a["Storage"] = link("/redfish/v1/Systems/A/Storage")
	f.resources["/redfish/v1/Systems/A/Processors"] = collection("Processor", "/redfish/v1/Systems/A/Processors", "/redfish/v1/Systems/A/Processors/CPU")
	f.resources["/redfish/v1/Systems/A/Processors/CPU"] = map[string]any{"@odata.id": "/redfish/v1/Systems/A/Processors/CPU", "@odata.type": "#Processor.v1_20_0.Processor", "Id": "CPU", "Manufacturer": "Fixture CPU", "Model": "Fixture Processor", "TotalCores": 8, "TotalThreads": 16, "Status": status("Warning")}
	f.resources["/redfish/v1/Systems/A/Memory"] = collection("Memory", "/redfish/v1/Systems/A/Memory", "/redfish/v1/Systems/A/Memory/DIMM")
	f.resources["/redfish/v1/Systems/A/Memory/DIMM"] = map[string]any{"@odata.id": "/redfish/v1/Systems/A/Memory/DIMM", "@odata.type": "#Memory.v1_20_0.Memory", "Id": "DIMM", "CapacityMiB": 16384, "Manufacturer": "Fixture Memory", "DeviceLocator": "DIMM_A1", "ErrorCorrection": "MultiBitECC", "Status": status("OK")}
	f.resources["/redfish/v1/Systems/A/Storage"] = collection("Storage", "/redfish/v1/Systems/A/Storage", "/redfish/v1/Systems/A/Storage/S")
	f.resources["/redfish/v1/Systems/A/Storage/S"] = map[string]any{"@odata.id": "/redfish/v1/Systems/A/Storage/S", "@odata.type": "#Storage.v1_20_0.Storage", "Id": "S", "Status": status("OK"), "Drives": []any{link("/redfish/v1/Systems/A/Storage/S/Drives/D")}}
	f.resources["/redfish/v1/Systems/A/Storage/S/Drives/D"] = map[string]any{"@odata.id": "/redfish/v1/Systems/A/Storage/S/Drives/D", "@odata.type": "#Drive.v1_20_0.Drive", "Id": "D", "Model": "Fixture SSD", "SerialNumber": "DRIVE-SERIAL", "CapacityBytes": 1000000000, "Protocol": "SAS", "MediaType": "SSD", "Status": status("Critical")}
	f.resources["/redfish/v1/Chassis"] = collection("Chassis", "/redfish/v1/Chassis", "/redfish/v1/Chassis/C")
	f.resources["/redfish/v1/Chassis/C"] = map[string]any{"@odata.id": "/redfish/v1/Chassis/C", "@odata.type": "#Chassis.v1_30_0.Chassis", "Id": "C", "Name": "Fixture Chassis", "ChassisType": "RackMount", "SerialNumber": "CHASSIS-SERIAL", "Status": status("Warning"), "Thermal": link("/redfish/v1/Chassis/C/Thermal"), "Power": link("/redfish/v1/Chassis/C/Power"), "Sensors": link("/redfish/v1/Chassis/C/Sensors"), "Links": map[string]any{"ComputerSystems": []any{link("/redfish/v1/Systems/A"), link("/redfish/v1/Systems/B")}}}
	f.resources["/redfish/v1/Chassis/C/Sensors"] = collection("Sensor", "/redfish/v1/Chassis/C/Sensors", "/redfish/v1/Chassis/C/Sensors/T")
	f.resources["/redfish/v1/Chassis/C/Sensors/T"] = map[string]any{"@odata.id": "/redfish/v1/Chassis/C/Sensors/T", "@odata.type": "#Sensor.v1_11_0.Sensor", "Id": "T", "Name": "Inlet", "ReadingType": "Temperature", "Reading": 42, "ReadingUnits": "Cel", "Status": status("OK")}
	f.resources["/redfish/v1/Chassis/C/Thermal"] = map[string]any{"@odata.id": "/redfish/v1/Chassis/C/Thermal", "@odata.type": "#Thermal.v1_7_0.Thermal", "Id": "Thermal", "Temperatures": []any{map[string]any{"MemberId": "CPU", "Name": "CPU Temperature", "ReadingCelsius": 51, "Status": status("OK")}}, "Fans": []any{map[string]any{"MemberId": "FAN-RPM", "Name": "Fan RPM", "Reading": 4200, "ReadingUnits": "RPM", "Status": status("OK")}, map[string]any{"MemberId": "FAN-PERCENT", "Name": "Fan Percent", "Reading": 37, "ReadingUnits": "Percent", "Status": status("Warning")}}}
	f.resources["/redfish/v1/Chassis/C/Power"] = map[string]any{"@odata.id": "/redfish/v1/Chassis/C/Power", "@odata.type": "#Power.v1_7_0.Power", "Id": "Power", "PowerSupplies": []any{map[string]any{"MemberId": "PSU", "Name": "Fixture PSU", "Model": "PSU-1", "Status": status("Critical")}}}
	f.resources["/redfish/v1/Managers"] = collection("Manager", "/redfish/v1/Managers", "/redfish/v1/Managers/M")
	f.resources["/redfish/v1/Managers/M"] = map[string]any{"@odata.id": "/redfish/v1/Managers/M", "@odata.type": "#Manager.v1_22_0.Manager", "Id": "M", "Name": "Fixture BMC", "ManagerType": "BMC", "Model": "FixtureBMC", "FirmwareVersion": "fixture-bmc-2", "Status": status("OK"), "Links": map[string]any{"ManagerForServers": []any{link("/redfish/v1/Systems/A"), link("/redfish/v1/Systems/B")}, "ManagerForChassis": []any{link("/redfish/v1/Chassis/C")}}}
	return f
}

func (f *redfishAgentFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	w.Header().Set("Content-Type", "application/json")
	if path == "/redfish/v1/SessionService/Sessions" && r.Method == http.MethodPost {
		var credentials struct{ UserName, Password string }
		if json.NewDecoder(r.Body).Decode(&credentials) != nil || credentials.UserName != "fixture-monitor" || credentials.Password != "private-redfish-fixture-password" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.logins.Add(1)
		w.Header().Set("X-Auth-Token", "private-redfish-fixture-token")
		w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/fixture")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"@odata.id": "/redfish/v1/SessionService/Sessions/fixture", "Id": "fixture", "Name": "Session"})
		return
	}
	if path == "/redfish/v1/SessionService/Sessions/fixture" && r.Method == http.MethodDelete {
		f.logouts.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		f.writes.Add(1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if path != "/redfish/v1" && r.Header.Get("X-Auth-Token") != "private-redfish-fixture-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	resource, ok := f.resources[path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(resource)
}

func redfishFixtureConfig(t *testing.T, c *config.Config, endpoint, ca, auth string) {
	t.Helper()
	t.Setenv("REDFISH_E2E_USER", "fixture-monitor")
	t.Setenv("REDFISH_E2E_PASSWORD", "private-redfish-fixture-password")
	c.Local.Enabled = false
	c.Credentials = map[string]credentials.Reference{"redfish-fixture": {UsernameEnv: "REDFISH_E2E_USER", PasswordEnv: "REDFISH_E2E_PASSWORD"}}
	c.Devices = []config.Device{{ID: "bmc-fixture", Driver: "redfish", Address: endpoint, CredentialRef: "redfish-fixture", TLS: config.RemoteTLS{CAFile: ca}, Polling: config.Polling{MediumInterval: 80 * time.Millisecond, InventoryInterval: 120 * time.Millisecond, Timeout: 2 * time.Second}, Redfish: config.Redfish{Auth: auth, Provenance: "simulated", RequestTimeout: time.Second}}}
	c.Telemetry.HeartbeatInterval = time.Hour
	configureDurable(c)
	c.Queue.MaxMessages = 128
}

func TestAgentRedfishMultiResourceDurableOfflineRestartACK(t *testing.T) {
	fixture := newRedfishAgentFixture()
	server := httptest.NewTLSServer(fixture)
	t.Cleanup(server.Close)
	ca := filepath.Join(testTempDir(t), "bmc-fixture-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close() // Controller is offline during the first collection.
	c := testConfig(t, address)
	redfishFixtureConfig(t, &c, server.URL, ca, "session")
	c.Queue.MaxInflight = c.Queue.MaxMessages
	c.Queue.ACKTimeout = 30 * time.Second
	a := newTestAgent(t, c, nil)
	stopA := runTestAgent(t, a)
	awaitP2(t, func() bool {
		states := a.Snapshot().RemoteDevices
		return len(states) == 1 && states[0].ConnectivityState == "online" &&
			len(states[0].Devices) >= 4 && a.queue.State().Messages >= 5
	})
	stopA()
	b := newTestAgent(t, c, nil)
	var pendingInventory, pendingTelemetry model.Envelope
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Inspect retained records before starting the restarted runtime. A second
	// Pop consumer while the offline sender resets leases on reconnect could
	// repeatedly select inventory and starve this test's telemetry inspection.
	for pendingInventory.Inventory == nil || pendingTelemetry.Telemetry == nil {
		e, err := b.queue.Pop(ctx)
		if err != nil {
			t.Fatal("offline hardware observations were not durably admitted", err, a.Snapshot().RecentErrors)
		}
		if e.Inventory != nil && len(e.Inventory.Devices) >= 4 {
			pendingInventory = e
		}
		if e.Telemetry != nil && len(e.Telemetry.Metrics) != 0 {
			pendingTelemetry = e
		}
	}
	b.queue.ResetInflight()
	if !b.queue.Contains(pendingInventory.MessageID) || !b.queue.Contains(pendingTelemetry.MessageID) {
		t.Fatal("offline observation lost before ACK")
	}
	observed := make(chan model.Envelope, 512)
	_ = startTCPController(t, address, mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	stopB := runTestAgent(t, b)
	defer stopB()
	replayed := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == pendingInventory.MessageID })
	assertRedfishInventory(t, replayed.Inventory, b.Snapshot().AgentID)
	if replayed.AgentID != pendingInventory.AgentID || replayed.SessionID == pendingInventory.SessionID {
		t.Fatal("restart changed Agent identity or did not rebind replay session")
	}
	metrics := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == pendingTelemetry.MessageID })
	assertRedfishMetrics(t, metrics.Telemetry)
	awaitP2(t, func() bool {
		return !b.queue.Contains(pendingInventory.MessageID) && !b.queue.Contains(pendingTelemetry.MessageID)
	})
	fresh := waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Inventory != nil && e.MessageID != pendingInventory.MessageID && len(e.Inventory.Devices) >= 4
	})
	ids := map[string]string{}
	for _, device := range replayed.Inventory.Devices {
		ids[device.Observation.Source.ResourceID] = device.ID
	}
	for _, device := range fresh.Inventory.Devices {
		if ids[device.Observation.Source.ResourceID] != device.ID {
			t.Fatal("resource identity changed after polling/restart", device.ID)
		}
	}
	data, _ := json.Marshal(b.Snapshot())
	if strings.Contains(string(data), "private-redfish-fixture") || fixture.writes.Load() != 0 {
		t.Fatal("secrets in diagnostics or hardware mutation reached fixture")
	}
	stopB()
	if fixture.logins.Load() != fixture.logouts.Load() || fixture.logins.Load() == 0 {
		t.Fatalf("session lifecycle leaked: login=%d logout=%d", fixture.logins.Load(), fixture.logouts.Load())
	}
	t.Logf("HTTPS verified; resources=%d; multi-system durable offline/restart replay and application ACK; sessions=%d/%d", len(replayed.Inventory.Devices), fixture.logins.Load(), fixture.logouts.Load())
}

func assertRedfishInventory(t *testing.T, inventory *model.InventoryUpdate, collectorID string) {
	t.Helper()
	systems := 0
	for _, device := range inventory.Devices {
		if device.Hardware == nil || device.Observation == nil || device.Observation.Source.Type != model.SourceRedfish || device.Observation.Source.TargetID != "remote:bmc-fixture" || device.Observation.Source.CollectorID != collectorID || device.Observation.Source.ResourceID == "" || device.Observation.Source.Provenance != model.ProvenanceSimulated || device.Source != "simulated" {
			t.Fatal("hardware provenance was not preserved", device)
		}
		if device.Hardware.ResourceType == "ComputerSystem" {
			systems++
			if device.Vendor != "Fixture Equipment" || device.Model != "FixtureModel" || !strings.HasPrefix(device.SerialNumber, "SERIAL-") || device.Hardware.HardwareUUID == "" || device.Hardware.BIOSVersion != "fixture-bios-1" || device.Hardware.Status.Health != "ok" || len(device.IdentityEvidence) == 0 || len(device.Hardware.Relations) != 2 {
				t.Fatal("system inventory or relationship evidence lost", device)
			}
			if strings.HasSuffix(device.Observation.Source.ResourceID, "/B") && device.Hardware.PowerState != "Off" {
				t.Fatal("power state lost")
			}
			if strings.HasSuffix(device.Observation.Source.ResourceID, "/A") {
				found := map[string]bool{}
				for _, component := range device.Components {
					if component.Hardware == nil || component.Observation == nil {
						t.Fatal("component hardware metadata lost", component)
					}
					h := component.Hardware
					switch h.ResourceType {
					case "Processor":
						found["cpu"] = h.Status.Health == "warning" && h.Cores != nil && *h.Cores == 8 && h.Threads != nil && *h.Threads == 16
					case "Memory":
						found["memory"] = h.Status.Health == "ok" && h.CapacityBytes != nil && *h.CapacityBytes == 16384*1024*1024 && h.ErrorCorrection == "MultiBitECC"
					case "Drive":
						found["drive"] = h.Status.Health == "critical" && h.CapacityBytes != nil && *h.CapacityBytes == 1000000000 && h.Protocol == "SAS" && component.SerialNumber == "DRIVE-SERIAL"
					}
				}
				if !found["cpu"] || !found["memory"] || !found["drive"] {
					t.Fatal("CPU/memory/storage inventory and health lost", found)
				}
			}
		}
	}
	if systems != 2 {
		t.Fatalf("one endpoint must preserve both ComputerSystems: got %d", systems)
	}
}

func assertRedfishMetrics(t *testing.T, sample *model.TelemetrySample) {
	t.Helper()
	if sample.Source != "simulated" || sample.Observation == nil || sample.Observation.Source.Type != model.SourceRedfish || sample.Observation.Source.Provenance != model.ProvenanceSimulated || sample.Observation.Time.SourceTimestamp != nil || sample.CPU != nil || sample.Memory != nil {
		t.Fatal("sensor time/provenance or hardware/utilization separation lost", sample)
	}
	found := map[string]bool{}
	for _, metric := range sample.Metrics {
		if metric.Name == "hardware.temperature.celsius" && metric.Value == 42 && metric.Unit == "Cel" && metric.ResourceID != "" && metric.Observation != nil {
			found["temperature"] = true
		}
		if metric.Name == "hardware.fan.rpm" && metric.Value == 4200 && metric.Unit == "RPM" {
			found["rpm"] = true
		}
		if metric.Name == "hardware.fan.percent" && metric.Value == 37 && metric.Unit == "%" {
			found["percent"] = true
		}
		if strings.HasPrefix(metric.Name, "hardware.power.") {
			t.Fatal("fixture with health-only PSU fabricated a power reading", metric)
		}
	}
	for _, component := range sample.HardwareHealth {
		if component.Name == "Fixture PSU" && component.Status.Health == "critical" {
			found["psu"] = true
		}
	}
	if !found["temperature"] || !found["rpm"] || !found["percent"] || !found["psu"] {
		t.Fatal("temperature/fan/PSU normalized observations lost", found, sample.Metrics)
	}
}
