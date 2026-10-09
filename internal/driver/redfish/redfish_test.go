package redfish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

const testPassword = "redfish-fixture-password-2479"

type testProvider struct{ bad bool }

func (p testProvider) Get(context.Context, string) (credentials.Credential, error) {
	if p.bad {
		return credentials.Credential{}, fmt.Errorf("provider secret %s", testPassword)
	}
	return credentials.Credential{Username: "reader", Password: testPassword}, nil
}

type fixture struct {
	t                *testing.T
	server           *httptest.Server
	mu               sync.Mutex
	doc              map[string]any
	override         map[string]http.HandlerFunc
	requests         map[string]int
	tokens           map[string]string
	created, deleted int
	active, peak     atomic.Int32
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, doc: documents(), override: map[string]http.HandlerFunc{}, requests: map[string]int{}, tokens: map[string]string{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	active := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		peak := f.peak.Load()
		if active <= peak || f.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	f.mu.Lock()
	f.requests[r.Method+" "+r.URL.RequestURI()]++
	handler := f.override[r.URL.Path]
	value := f.doc[r.URL.RequestURI()]
	if value == nil {
		value = f.doc[r.URL.Path]
	}
	f.mu.Unlock()
	if handler != nil {
		handler(w, r)
		return
	}
	if r.URL.Path == "/redfish/v1/" || r.URL.Path == "/redfish/v1" || r.URL.Path == "/redfish/v1/SessionService" {
		writeJSON(w, value)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/redfish/v1/SessionService/Sessions" {
		var auth struct{ UserName, Password string }
		_ = json.NewDecoder(r.Body).Decode(&auth)
		if auth.UserName != "reader" || auth.Password != testPassword {
			w.WriteHeader(401)
			return
		}
		f.mu.Lock()
		f.created++
		session := fmt.Sprintf("/redfish/v1/SessionService/Sessions/%d", f.created)
		token := fmt.Sprintf("fixture-session-token-%d-393039", f.created)
		f.tokens[session] = token
		f.mu.Unlock()
		w.Header().Set("Location", session)
		w.Header().Set("X-Auth-Token", token)
		w.WriteHeader(201)
		writeJSON(w, map[string]any{"@odata.id": session})
		return
	}
	user, password, basic := r.BasicAuth()
	authorized := basic && user == "reader" && password == testPassword
	f.mu.Lock()
	for _, token := range f.tokens {
		if token == r.Header.Get("X-Auth-Token") {
			authorized = true
		}
	}
	if r.Method == http.MethodDelete && f.tokens[r.URL.Path] == r.Header.Get("X-Auth-Token") && authorized {
		delete(f.tokens, r.URL.Path)
		f.deleted++
		f.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	f.mu.Unlock()
	if !authorized {
		w.WriteHeader(401)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if value == nil {
		w.WriteHeader(404)
		return
	}
	writeJSON(w, value)
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (f *fixture) adapter(options Options) *Adapter {
	if options.TLSConfig == nil {
		pool := x509.NewCertPool()
		pool.AddCert(f.server.Certificate())
		options.TLSConfig = &tls.Config{RootCAs: pool}
	}
	a, err := New(driver.Options{ID: "bmc", Address: f.server.URL}, testProvider{}, "fixture", options)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = a.Close() })
	return a
}
func (f *fixture) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[method+" "+path]
}

func documents() map[string]any {
	ref := func(id string) any { return map[string]any{"@odata.id": id} }
	collection := func(ids ...string) any {
		members := []any{}
		for _, id := range ids {
			members = append(members, ref(id))
		}
		return map[string]any{"Members": members, "Members@odata.count": len(ids)}
	}
	ok := map[string]any{"Health": "OK", "HealthRollup": "Warning", "State": "Enabled"}
	warning := map[string]any{"Health": "Warning", "State": "Enabled"}
	return map[string]any{
		"/redfish/v1/":                                                  map[string]any{"@odata.id": "/redfish/v1/", "Systems": ref("/redfish/v1/Systems"), "Chassis": ref("/redfish/v1/Chassis"), "Managers": ref("/redfish/v1/Managers"), "SessionService": ref("/redfish/v1/SessionService"), "Links": map[string]any{"Sessions": ref("/redfish/v1/SessionService/Sessions")}},
		"/redfish/v1/SessionService":                                    map[string]any{"@odata.type": "#SessionService.v1_2_0.SessionService", "Sessions": ref("/redfish/v1/SessionService/Sessions")},
		"/redfish/v1/Systems":                                           collection("/redfish/v1/Systems/A", "/redfish/v1/Systems/B"),
		"/redfish/v1/Systems/A":                                         map[string]any{"@odata.id": "/redfish/v1/Systems/A", "@odata.type": "#ComputerSystem.v1_0_0.ComputerSystem", "Name": "system A", "Manufacturer": "Generic DMTF", "Model": "Rack 42", "SerialNumber": "system-serial-A", "UUID": "71a31700-031a-4e72-87ba-889ae57e3900", "BiosVersion": "BIOS 2", "PowerState": "Off", "Status": ok, "Processors": ref("/redfish/v1/Systems/A/Processors"), "Memory": ref("/redfish/v1/Systems/A/Memory"), "Storage": ref("/redfish/v1/Systems/A/Storage"), "EthernetInterfaces": ref("/redfish/v1/Systems/A/EthernetInterfaces"), "Links": map[string]any{"Chassis": []any{ref("/redfish/v1/Chassis/C")}}},
		"/redfish/v1/Systems/B":                                         map[string]any{"Name": "system B", "Model": "Blade", "PowerState": "On"},
		"/redfish/v1/Systems/A/Processors":                              collection("/redfish/v1/Systems/A/Processors/CPU"),
		"/redfish/v1/Systems/A/Processors/CPU":                          map[string]any{"Name": "CPU 1", "Manufacturer": "CPU Vendor", "Model": "CPU Hardware", "TotalCores": 12, "TotalThreads": 24, "Status": warning},
		"/redfish/v1/Systems/A/Memory":                                  collection("/redfish/v1/Systems/A/Memory/DIMM"),
		"/redfish/v1/Systems/A/Memory/DIMM":                             map[string]any{"Name": "DIMM", "CapacityMiB": 32768, "Manufacturer": "Memory Vendor", "DeviceLocator": "DIMM_A1", "MemoryDeviceType": "DDR4", "ErrorCorrection": "MultiBitECC", "Status": ok},
		"/redfish/v1/Systems/A/Storage":                                 collection("/redfish/v1/Systems/A/Storage/RAID"),
		"/redfish/v1/Systems/A/Storage/RAID":                            map[string]any{"Name": "RAID", "Status": ok, "Drives": []any{ref("/redfish/v1/Drives/D")}},
		"/redfish/v1/Drives/D":                                          map[string]any{"Name": "SSD", "SerialNumber": "drive-serial-D", "Model": "SSD 1TB", "CapacityBytes": uint64(1000000000000), "Protocol": "SAS", "MediaType": "SSD", "Status": map[string]any{"Health": "Critical", "State": "Enabled"}},
		"/redfish/v1/Systems/A/EthernetInterfaces":                      collection("/redfish/v1/Systems/A/EthernetInterfaces/NIC"),
		"/redfish/v1/Systems/A/EthernetInterfaces/NIC":                  map[string]any{"IPv4Addresses": []any{map[string]any{"Address": "192.0.2.42"}}},
		"/redfish/v1/Chassis":                                           collection("/redfish/v1/Chassis/C", "/redfish/v1/Chassis/D"),
		"/redfish/v1/Chassis/C":                                         map[string]any{"Name": "chassis C", "SerialNumber": "chassis-serial-C", "ChassisType": "RackMount", "Status": ok, "Sensors": ref("/redfish/v1/Chassis/C/Sensors"), "ThermalSubsystem": ref("/redfish/v1/Chassis/C/ThermalSubsystem"), "PowerSubsystem": ref("/redfish/v1/Chassis/C/PowerSubsystem"), "EnvironmentMetrics": ref("/redfish/v1/Chassis/C/EnvironmentMetrics")},
		"/redfish/v1/Chassis/D":                                         map[string]any{"Name": "legacy chassis", "Thermal": ref("/redfish/v1/Chassis/D/Thermal"), "Power": ref("/redfish/v1/Chassis/D/Power")},
		"/redfish/v1/Chassis/C/Sensors":                                 collection("/redfish/v1/Chassis/C/Sensors/Temp", "/redfish/v1/Chassis/C/Sensors/Absent", "/redfish/v1/Chassis/C/Sensors/Unknown", "/redfish/v1/Chassis/C/Sensors/Percent"),
		"/redfish/v1/Chassis/C/Sensors/Temp":                            map[string]any{"Name": "temperature", "Reading": 42.5, "ReadingType": "Temperature", "ReadingUnits": "Cel", "ReadingTime": "2026-10-09T00:00:00Z", "Status": warning},
		"/redfish/v1/Chassis/C/Sensors/Absent":                          map[string]any{"Name": "health only", "Reading": nil, "ReadingType": "Temperature", "ReadingUnits": "Cel", "Status": ok},
		"/redfish/v1/Chassis/C/Sensors/Unknown":                         map[string]any{"Name": "unknown units", "Reading": 72, "ReadingType": "Temperature", "ReadingUnits": "F"},
		"/redfish/v1/Chassis/C/Sensors/Percent":                         map[string]any{"Name": "generic percentage", "Reading": 50, "ReadingType": "Percent", "ReadingUnits": "%"},
		"/redfish/v1/Chassis/C/ThermalSubsystem":                        map[string]any{"Fans": ref("/redfish/v1/Chassis/C/ThermalSubsystem/Fans"), "ThermalMetrics": ref("/redfish/v1/Chassis/C/ThermalSubsystem/ThermalMetrics"), "Status": ok},
		"/redfish/v1/Chassis/C/ThermalSubsystem/Fans":                   collection("/redfish/v1/Chassis/C/ThermalSubsystem/Fans/F1"),
		"/redfish/v1/Chassis/C/ThermalSubsystem/Fans/F1":                map[string]any{"Name": "modern fan", "SpeedPercent": map[string]any{"Reading": 25, "SpeedRPM": 1200}, "Status": ok},
		"/redfish/v1/Chassis/C/ThermalSubsystem/ThermalMetrics":         map[string]any{"TemperatureReadingsCelsius": []any{map[string]any{"Reading": 44, "DataSourceUri": "/redfish/v1/Chassis/C/Sensors/Temp"}}},
		"/redfish/v1/Chassis/C/PowerSubsystem":                          map[string]any{"PowerSupplies": ref("/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies"), "Status": ok},
		"/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies":            collection("/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies/P1"),
		"/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies/P1":         map[string]any{"Name": "PSU modern", "Status": map[string]any{"State": "Absent", "Health": "Warning"}, "Metrics": ref("/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies/P1/Metrics")},
		"/redfish/v1/Chassis/C/PowerSubsystem/PowerSupplies/P1/Metrics": map[string]any{"InputPowerWatts": map[string]any{"Reading": 100}, "OutputPowerWatts": map[string]any{"Reading": 80}},
		"/redfish/v1/Chassis/C/EnvironmentMetrics":                      map[string]any{"TemperatureCelsius": map[string]any{"Reading": 40}, "PowerWatts": map[string]any{"Reading": 110}},
		"/redfish/v1/Chassis/D/Thermal":                                 map[string]any{"Temperatures": []any{map[string]any{"MemberId": "T1", "Name": "legacy temp", "ReadingCelsius": 38}}, "Fans": []any{map[string]any{"MemberId": "F1", "Name": "legacy RPM", "Reading": 900, "ReadingUnits": "RPM"}, map[string]any{"MemberId": "F2", "Name": "legacy percent", "Reading": 30, "ReadingUnits": "Percent"}}},
		"/redfish/v1/Chassis/D/Power":                                   map[string]any{"PowerSupplies": []any{map[string]any{"MemberId": "P1", "Name": "health only PSU", "Status": warning}}, "PowerControl": []any{map[string]any{"MemberId": "PC", "PowerConsumedWatts": 90}}},
		"/redfish/v1/Managers":                                          collection("/redfish/v1/Managers/M", "/redfish/v1/Managers/N"),
		"/redfish/v1/Managers/M":                                        map[string]any{"Name": "BMC M", "ManagerType": "BMC", "Model": "BMC", "FirmwareVersion": "BMC 3", "UUID": "BMC-UUID", "Status": ok, "Links": map[string]any{"ManagerForServers": []any{ref("/redfish/v1/Systems/A")}, "ManagerForChassis": []any{ref("/redfish/v1/Chassis/C")}}},
		"/redfish/v1/Managers/N":                                        map[string]any{"Name": "BMC N", "ManagerType": "BMC"},
	}
}

func TestInventoryMultiResourceStableIdentityAndHealth(t *testing.T) {
	f := newFixture(t)
	a := f.adapter(Options{Provenance: model.ProvenanceSimulated})
	first, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Devices) != 6 || first.Completeness != "complete" {
		t.Fatalf("inventory count=%d completeness=%s", len(first.Devices), first.Completeness)
	}
	second, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, device := range first.Devices {
		if device.ID != second.Devices[i].ID || device.Type != "unknown" {
			t.Fatal("unstable resource identity or incorrect asset type")
		}
		if e := device.Observation.Validate(); e != nil {
			t.Fatal(e)
		}
		if device.Observation.Source.TargetID != "remote:bmc" || device.Observation.Source.Type != model.SourceRedfish || device.Observation.Source.Provenance != model.ProvenanceSimulated || device.Source != "simulated" {
			t.Fatal("wrong provenance")
		}
		if device.Observation.Time.SourceTimestamp != nil || device.Observation.Time.ClockOffset != nil || device.Observation.Time.Quality != model.ClockUnknown {
			t.Fatal("fabricated source clock")
		}
		for _, evidence := range device.IdentityEvidence {
			if e := evidence.Validate(); e != nil {
				t.Fatal(e)
			}
		}
		if device.Name == "system A" {
			if device.Hardware.HardwareUUID != "71a31700-031a-4e72-87ba-889ae57e3900" || device.Hardware.BIOSVersion != "BIOS 2" || device.FirmwareVersion != "" || device.Hardware.PowerState != "Off" || device.Hardware.Status.Health != "ok" || device.Hardware.Status.HealthRollup != "warning" {
				t.Fatal("incorrect system normalization")
			}
			classes := map[string]model.InventoryComponent{}
			for _, component := range device.Components {
				classes[component.Class] = component
			}
			if classes["Processor"].Hardware.Cores == nil || *classes["Processor"].Hardware.Cores != 12 || classes["Processor"].Hardware.Status.Health != "warning" || classes["Memory"].Hardware.CapacityBytes == nil || *classes["Memory"].Hardware.CapacityBytes != uint64(32768)*1024*1024 || classes["Memory"].Hardware.ErrorCorrection != "MultiBitECC" || classes["Drive"].Hardware.CapacityBytes == nil || *classes["Drive"].Hardware.CapacityBytes != 1000000000000 || classes["Drive"].Hardware.Status.Health != "critical" {
				t.Fatal("component inventory or health missing")
			}
			if len(device.Hardware.Relations) != 1 || device.Hardware.Relations[0].TargetResourceID != "/redfish/v1/Chassis/C" || len(device.Hardware.NetworkAddresses) != 1 {
				t.Fatal("relationships/network metadata missing")
			}
		}
		if device.Name == "system B" && (device.SerialNumber != "" || device.Hardware.HardwareUUID != "" || device.Hardware.Status.Health != "unknown") {
			t.Fatal("fabricated missing identity/health")
		}
		if device.Name == "BMC M" && (device.Hardware.ResourceType != "Manager" || device.FirmwareVersion != "BMC 3" || len(device.Hardware.Relations) != 2) {
			t.Fatal("manager collapsed with server")
		}
	}
	if f.created != f.deleted || len(f.tokens) != 0 {
		t.Fatal("leaked sessions")
	}
}

func TestModernLegacyTelemetryUnitsTimeAndHealthOnly(t *testing.T) {
	f := newFixture(t)
	a := f.adapter(Options{})
	samples, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, sample := range samples {
		if sample.CPU != nil || sample.Memory != nil {
			t.Fatal("fabricated utilization")
		}
		for _, metric := range sample.Metrics {
			found[fmt.Sprintf("%s/%g", metric.Name, metric.Value)] = true
			if strings.Contains(metric.ResourceID, "Absent") || strings.Contains(metric.ResourceID, "Unknown") {
				t.Fatal("fake missing/unknown metric")
			}
			if metric.Name == "hardware.fan.rpm" && (metric.Value == 30 || metric.Value == 25 || metric.Value == 50) {
				t.Fatal("percentage relabeled as RPM")
			}
			if metric.Value == 42.5 && (metric.Observation.Time.SourceTimestamp == nil || metric.Observation.Time.SourceTimestamp.Format(time.RFC3339) != "2026-10-09T00:00:00Z") {
				t.Fatal("source reading timestamp missing")
			}
			if e := metric.Observation.Validate(); e != nil {
				t.Fatal(e)
			}
		}
	}
	for _, key := range []string{"hardware.temperature.celsius/42.5", "hardware.temperature.celsius/38", "hardware.fan.rpm/1200", "hardware.fan.rpm/900", "hardware.fan.percent/25", "hardware.fan.percent/30", "hardware.sensor.percent/50", "hardware.power.input.watts/100", "hardware.power.output.watts/80", "hardware.power.watts/90"} {
		if !found[key] {
			t.Errorf("missing metric %s", key)
		}
	}
	before := f.count("GET", "/redfish/v1/Systems")
	if _, err = a.Hardware(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count("GET", "/redfish/v1/Systems") != before || f.count("GET", "/redfish/v1/Systems/A/EthernetInterfaces") != 1 {
		t.Fatal("hardware poll repeated inventory discovery")
	}
	if f.peak.Load() != 1 {
		t.Fatal("HTTP requests were concurrent")
	}
}

func TestSessionServiceFallbackAndExplicitBasic(t *testing.T) {
	for _, auth := range []string{"session", "basic"} {
		t.Run(auth, func(t *testing.T) {
			f := newFixture(t)
			root := f.doc["/redfish/v1/"].(map[string]any)
			delete(root, "Links")
			a := f.adapter(Options{Auth: auth})
			if _, err := a.InventoryBatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if auth == "basic" && f.created != 0 {
				t.Fatal("implicit session in Basic mode")
			}
			if auth == "session" && (f.created != 1 || f.deleted != 1) {
				t.Fatal("fallback session lifecycle failed")
			}
		})
	}
}

func TestPartialMissingMalformedAndCriticalResponse(t *testing.T) {
	f := newFixture(t)
	f.doc["/redfish/v1/Chassis/C/Sensors/Temp"] = map[string]any{"Reading": "malformed-number", "ReadingType": "Temperature", "ReadingUnits": "Cel"}
	delete(f.doc, "/redfish/v1/Chassis/C/PowerSubsystem")
	a := f.adapter(Options{})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil || inventory.Completeness != "partial" || len(inventory.Devices) != 6 {
		t.Fatalf("partial discarded: %s count=%d err=%v", inventory.Completeness, len(inventory.Devices), err)
	}
	f.doc["/redfish/v1/Systems/A"] = map[string]any{"Status": 42}
	partial, err := a.InventoryBatch(context.Background())
	if !errors.Is(err, driver.ErrMalformed) || len(partial.Devices) != 5 {
		t.Fatalf("critical malformed treated as successful: count=%d err=%v", len(partial.Devices), err)
	}
}

func TestHTTPAuthenticationAndBoundedReauthentication(t *testing.T) {
	for _, code := range []int{401, 403, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFixture(t)
			f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, testPassword+" fixture-session-token-1-393039")
			}
			a := f.adapter(Options{})
			_, err := a.InventoryBatch(context.Background())
			if err == nil || strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), "fixture-session-token") {
				t.Fatal("missing/sensitive HTTP error")
			}
			if code == 401 && f.created != 2 {
				t.Fatalf("reauth not bounded: %d", f.created)
			}
			if code == 403 && f.created != 1 {
				t.Fatal("403 login storm")
			}
			if f.created != f.deleted {
				t.Fatal("session cleanup failed")
			}
		})
	}
	f := newFixture(t)
	f.override["/redfish/v1/SessionService/Sessions"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }
	a := f.adapter(Options{})
	_, err := a.InventoryBatch(context.Background())
	if !errors.Is(err, ErrAuthentication) || f.count("POST", "/redfish/v1/SessionService/Sessions") != 1 {
		t.Fatal("failed login retried or missing category")
	}
}

func TestExpiredSessionRecoversOnce(t *testing.T) {
	f := newFixture(t)
	var once atomic.Bool
	f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) {
		if !once.Swap(true) {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, f.doc["/redfish/v1/Systems"])
	}
	a := f.adapter(Options{})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil || len(inventory.Devices) != 6 || f.created != 2 || f.deleted != 2 {
		t.Fatalf("session recovery failed %v", err)
	}
}

func TestTraversalPaginationAndLimits(t *testing.T) {
	t.Run("pagination", func(t *testing.T) {
		f := newFixture(t)
		f.doc["/redfish/v1/Systems"] = map[string]any{"@odata.id": "/redfish/v1/Systems", "Members": []any{map[string]any{"@odata.id": "/redfish/v1/Systems/A"}}, "Members@odata.nextLink": "/redfish/v1/Systems?$skip=1"}
		f.doc["/redfish/v1/Systems?$skip=1"] = map[string]any{"@odata.id": "/redfish/v1/Systems", "Members": []any{map[string]any{"@odata.id": "/redfish/v1/Systems/B"}}}
		a := f.adapter(Options{})
		value, err := a.InventoryBatch(context.Background())
		if err != nil || len(value.Devices) != 6 {
			t.Fatal("standard pagination failed", err)
		}
	})
	for _, kind := range []string{"cycle", "pages", "resources", "requests", "bytes", "aggregate"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			options := Options{}
			switch kind {
			case "cycle", "pages":
				f.doc["/redfish/v1/Systems"] = map[string]any{"Members": []any{}, "Members@odata.nextLink": "/redfish/v1/Systems"}
				if kind == "pages" {
					options.MaxPages = 1
				}
			case "resources":
				options.MaxResources = 2
			case "requests":
				options.MaxRequests = 3
			case "bytes":
				options.MaxResponseBytes = 512
				f.doc["/redfish/v1/Systems/A"] = map[string]any{"Name": strings.Repeat("x", 1024)}
			case "aggregate":
				options.MaxResponseBytes = 512
				options.MaxPollBytes = 512
			}
			a := f.adapter(options)
			_, err := a.InventoryBatch(context.Background())
			if err == nil {
				t.Fatal("missing traversal/size failure")
			}
			if f.count("GET", "/redfish/v1/Systems") > 1 {
				t.Fatal("cyclic/unbounded collection")
			}
		})
	}
}

func TestMaliciousLinksRedirectAndReadOnlyBoundary(t *testing.T) {
	externalHits := atomic.Int32{}
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalHits.Add(1) }))
	defer external.Close()
	for _, link := range []string{external.URL + "/redfish/v1/Systems", "/redfish/v1/../Systems", "/redfish/v1/%2e%2e/Systems", "/redfish/v1/Systems?password=bad", "/redfish/v1/Systems?x=redfish-fixture-%70assword-2479", "//foreign.example/redfish/v1/Systems", "/redfish/v1/Systems%2fEscape"} {
		t.Run(link, func(t *testing.T) {
			f := newFixture(t)
			f.doc["/redfish/v1/"].(map[string]any)["Systems"] = map[string]any{"@odata.id": link}
			a := f.adapter(Options{})
			_, err := a.InventoryBatch(context.Background())
			if err == nil {
				t.Fatal("unsafe link accepted")
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		f := newFixture(t)
		f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, external.URL, 302) }
		a := f.adapter(Options{})
		if _, err := a.InventoryBatch(context.Background()); err == nil {
			t.Fatal("external redirect followed")
		}
	})
	for _, path := range []string{"/redfish/v1/Systems/A/Actions/ComputerSystem.Reset", "/redfish/v1/Systems/A/Actions/Reset/Sessions", "/redfish/v1/SessionService/Sessions?x=1"} {
		t.Run("session:"+path, func(t *testing.T) {
			f := newFixture(t)
			f.doc["/redfish/v1/"].(map[string]any)["Links"] = map[string]any{"Sessions": map[string]any{"@odata.id": path}}
			a := f.adapter(Options{})
			if _, err := a.InventoryBatch(context.Background()); err == nil {
				t.Fatal("arbitrary action auth accepted")
			}
			if f.count("POST", path) != 0 {
				t.Fatal("mutation performed")
			}
		})
	}
	if externalHits.Load() != 0 {
		t.Fatal("credentials could reach external host")
	}
}

func TestTLSValidationAndCustomCA(t *testing.T) {
	for _, kind := range []string{"valid", "unknown-ca", "hostname", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			config := &tls.Config{}
			if kind != "unknown-ca" {
				pool := x509.NewCertPool()
				pool.AddCert(f.server.Certificate())
				config.RootCAs = pool
			}
			if kind == "hostname" {
				config.ServerName = "other.invalid"
			}
			if kind == "expired" {
				f.server.Close()
				f.server = expiredServer(t, http.HandlerFunc(f.serve))
				t.Cleanup(f.server.Close)
				pool := x509.NewCertPool()
				pool.AddCert(f.server.Certificate())
				config.RootCAs = pool
			}
			a := f.adapter(Options{TLSConfig: config})
			_, err := a.InventoryBatch(context.Background())
			if kind == "valid" && err != nil {
				t.Fatal(err)
			}
			if kind != "valid" && !errors.Is(err, driver.ErrConnectivity) {
				t.Fatalf("invalid certificate accepted: %v", err)
			}
		})
	}
}
func expiredServer(t *testing.T, handler http.Handler) *httptest.Server {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	return server
}

func TestCancellationTimeoutCloseAndCooldown(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		f := newFixture(t)
		f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
		a := f.adapter(Options{Timeout: 50 * time.Millisecond})
		started := time.Now()
		_, err := a.InventoryBatch(context.Background())
		if err == nil || time.Since(started) > time.Second {
			t.Fatal("unbounded BMC timeout")
		}
	})
	t.Run("cancel-close", func(t *testing.T) {
		f := newFixture(t)
		entered := make(chan struct{})
		f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }
		a := f.adapter(Options{})
		done := make(chan error, 1)
		go func() { _, err := a.InventoryBatch(context.Background()); done <- err }()
		<-entered
		_ = a.Close()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not cancel poll")
		}
		if _, err := a.InventoryBatch(context.Background()); err == nil {
			t.Fatal("poll after Close")
		}
	})
	for _, code := range []int{429, 503} {
		t.Run(fmt.Sprintf("retry-%d", code), func(t *testing.T) {
			f := newFixture(t)
			f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "0"); w.WriteHeader(code) }
			a := f.adapter(Options{})
			_, err := a.InventoryBatch(context.Background())
			if err == nil || f.count("GET", "/redfish/v1/Systems") != 2 {
				t.Fatal("retry policy not bounded")
			}
		})
	}
	t.Run("long-retry-after", func(t *testing.T) {
		f := newFixture(t)
		f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "120"); w.WriteHeader(429) }
		a := f.adapter(Options{})
		_, _ = a.InventoryBatch(context.Background())
		before := f.count("GET", "/redfish/v1/")
		_, err := a.InventoryBatch(context.Background())
		if err == nil || f.count("GET", "/redfish/v1/") != before {
			t.Fatal("Retry-After cooldown not honored across polls")
		}
	})
}

func TestCredentialAndSessionSecretRedaction(t *testing.T) {
	f := newFixture(t)
	f.override["/redfish/v1/Systems/A"] = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"Name": "reflected " + testPassword, "SerialNumber": "fixture-session-token-1-393039", "UUID": testPassword, "Model": "safe model"})
	}
	a := f.adapter(Options{})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(inventory)
	if strings.Contains(string(body), testPassword) || strings.Contains(string(body), "fixture-session-token") || strings.Contains(string(body), "[redacted]") {
		t.Fatal("secret or fake redacted identity published")
	}
	a.provider = testProvider{bad: true}
	_, err = a.InventoryBatch(context.Background())
	if err == nil || strings.Contains(err.Error(), testPassword) {
		t.Fatal("provider secret leaked")
	}
}

func TestConstructorAndHealthMapping(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1", "https://user:secret@127.0.0.1", "https://127.0.0.1/redfish/v1", "https://127.0.0.1?q=1"} {
		if _, err := New(driver.Options{ID: "bmc", Address: endpoint}, testProvider{}, "fixture", Options{}); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	if _, err := New(driver.Options{ID: "bmc", Address: "https://127.0.0.1"}, testProvider{}, "fixture", Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}}); err == nil {
		t.Fatal("insecure TLS accepted")
	}
	if _, err := New(driver.Options{ID: "bmc", Address: "https://127.0.0.1"}, nil, "", Options{}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	for _, value := range []string{"Enabled", "Disabled", "Absent", "UnavailableOffline", "Starting", "StandbyOffline", "Updating", "Degraded"} {
		base, h, err := resource(json.RawMessage(fmt.Sprintf(`{"Status":{"State":%q}}`, value)), "ComputerSystem")
		_ = base
		if err != nil || h.Status.State != value || h.Status.Health != "unknown" {
			t.Fatal("state conflated with health")
		}
	}
}

func TestResourceUsageBoundedRepeatedPolling(t *testing.T) {
	f := newFixture(t)
	a := f.adapter(Options{})
	initial, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutines := runtime.NumGoroutine()
	started := time.Now()
	var payloadBytes, maxMetrics int
	for i := 0; i < 50; i++ {
		samples, e := a.Hardware(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		for _, sample := range samples {
			body, _ := json.Marshal(sample)
			if len(body) > payloadBytes {
				payloadBytes = len(body)
			}
			if len(sample.Metrics) > maxMetrics {
				maxMetrics = len(sample.Metrics)
			}
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if growth > 8<<20 || runtime.NumGoroutine() > goroutines+8 || f.peak.Load() != 1 || len(a.plan) > a.limits.MaxResources || f.created != f.deleted || len(f.tokens) != 0 {
		t.Fatalf("resource/session bounds exceeded heap=%d goroutines=%d peak=%d", growth, runtime.NumGoroutine()-goroutines, f.peak.Load())
	}
	encoded, _ := json.Marshal(initial)
	t.Logf("50 health polls duration=%s retained_heap_delta_bytes=%d goroutine_delta=%d request_concurrency_peak=%d inventory_json_bytes=%d max_sample_json_bytes=%d max_sample_metrics=%d max_poll_bytes=%d plan_resources=%d", time.Since(started), growth, runtime.NumGoroutine()-goroutines, f.peak.Load(), len(encoded), payloadBytes, maxMetrics, a.limits.MaxPollBytes, len(a.plan))
}
