package ipmi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/model"
	"github.com/bougou/go-ipmi/pkg/types"
)

type fixedCredentials struct{ err error }

func (p fixedCredentials) Get(ctx context.Context, _ string) (credentials.Credential, error) {
	if ctx.Err() != nil {
		return credentials.Credential{}, ctx.Err()
	}
	if p.err != nil {
		return credentials.Credential{}, p.err
	}
	return credentials.Credential{Username: ipmitest.Username, Password: ipmitest.Password}, nil
}

type fakeClient struct {
	mu                                      sync.Mutex
	device                                  DeviceInfo
	repository                              RepositoryInfo
	records                                 map[uint16][]byte
	frus                                    map[uint8][]byte
	readings                                map[uint8]Reading
	deviceErr, chassisErr, repoErr, readErr error
	failedSensor                            uint8
	closed, ops, sdrReads, fruReads         int
	ctxBlocked                              bool
	cyclic                                  bool
	shortFRU                                bool
	wordFRU                                 bool
}

func protocolFixture() *fakeClient {
	return &fakeClient{device: DeviceInfo{DeviceID: 32, Revision: 1, FirmwareMajor: 1, FirmwareMinorBCD: 0x23, IPMIVersion: 0x02, Support: 0x8b, ManufacturerID: 343, ProductID: 1, Available: true}, repository: RepositoryInfo{Version: 0x51, Records: 6, ReserveSupported: true}, records: ipmitest.SDRs(), frus: map[uint8][]byte{0: ipmitest.FRU(0), 1: ipmitest.FRU(1)}, readings: map[uint8]Reading{1: {Raw: 42, StateBytes: 1}, 2: {Raw: 120, StateBytes: 1}, 3: {Raw: 120, StateBytes: 1}, 4: {Raw: 150, StateBytes: 1}, 5: {States: 1, StateBytes: 2}}}
}
func (f *fakeClient) DeviceID(ctx context.Context) (DeviceInfo, error) {
	f.mu.Lock()
	f.ops++
	blocked := f.ctxBlocked
	f.mu.Unlock()
	if blocked {
		<-ctx.Done()
		return DeviceInfo{}, ctx.Err()
	}
	return f.device, f.deviceErr
}
func (f *fakeClient) ChassisStatus(context.Context) (ChassisInfo, error) {
	f.ops++
	return ChassisInfo{}, f.chassisErr
}
func (f *fakeClient) RepositoryInfo(context.Context) (RepositoryInfo, error) {
	f.ops++
	return f.repository, f.repoErr
}
func (f *fakeClient) ReserveSDR(context.Context) (uint16, error) { f.ops++; return 1, nil }
func (f *fakeClient) SDR(_ context.Context, _ uint16, id uint16, offset, count uint8) (uint16, []byte, error) {
	f.ops++
	f.sdrReads++
	if id == 0 {
		id = 1
	}
	record := f.records[id]
	if record == nil {
		return 0, nil, &CompletionError{Code: 0xcb}
	}
	next := id + 1
	if next > uint16(len(f.records)) {
		next = 0xffff
	}
	if f.cyclic {
		next = 1
	}
	if int(offset)+int(count) > len(record) {
		return next, nil, driver.ErrMalformed
	}
	return next, slices.Clone(record[int(offset) : int(offset)+int(count)]), nil
}
func (f *fakeClient) FRUInfo(_ context.Context, id uint8) (FRUAreaInfo, error) {
	f.ops++
	b, ok := f.frus[id]
	if !ok {
		return FRUAreaInfo{}, &CompletionError{Code: 0xcb}
	}
	return FRUAreaInfo{Bytes: uint16(len(b)), WordAccess: f.wordFRU}, nil
}
func (f *fakeClient) ReadFRU(_ context.Context, id uint8, offset uint16, count uint8) ([]byte, error) {
	f.ops++
	f.fruReads++
	if f.readErr != nil {
		return nil, f.readErr
	}
	off := int(offset)
	if f.wordFRU {
		off *= 2
	}
	if f.shortFRU {
		count = min(count, 8)
	}
	b := f.frus[id]
	if off+int(count) > len(b) {
		return nil, driver.ErrMalformed
	}
	return slices.Clone(b[off : off+int(count)]), nil
}
func (f *fakeClient) SensorReading(_ context.Context, number, _, _, _ uint8) (Reading, error) {
	f.ops++
	if number == f.failedSensor {
		return Reading{}, driver.ErrMalformed
	}
	r, ok := f.readings[number]
	if !ok {
		return Reading{}, &CompletionError{Code: 0xcb}
	}
	return r, nil
}
func (f *fakeClient) Close(context.Context) error { f.mu.Lock(); f.closed++; f.mu.Unlock(); return nil }

func fixtureAdapter(t testing.TB, f *fakeClient, options Options) *Adapter {
	t.Helper()
	a, err := NewWithFactory(driver.Options{ID: "legacy-bmc", Name: "Legacy BMC", Address: "127.0.0.1"}, fixedCredentials{}, "credential-ref", options, func(_ context.Context, _ string, _ int, c credentials.Credential, o SessionOptions) (Client, error) {
		if c.Password != ipmitest.Password || o.CipherSuite != 17 && o.CipherSuite != 3 || o.Privilege != "user" {
			t.Errorf("invalid credential/session projection")
		}
		return f, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestInventoryAndHardwareNormalization(t *testing.T) {
	f := protocolFixture()
	a := fixtureAdapter(t, f, Options{})
	inv, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Completeness != "complete" || len(inv.Devices) != 2 {
		t.Fatalf("inventory: %+v", inv)
	}
	var bmc model.Device
	for _, d := range inv.Devices {
		if d.Hardware.ResourceType == "Manager" {
			bmc = d
		} else if d.Hardware.PowerState != "Off" || d.Hardware.Status.Health != "ok" {
			t.Fatalf("off chassis is not failed: %+v", d.Hardware)
		}
	}
	if bmc.Vendor != "" || bmc.SerialNumber != "" || bmc.FirmwareVersion != "1.23" || bmc.HardwareRevision != "1" || len(bmc.Components) != 9 {
		t.Fatalf("BMC fields: %+v", bmc)
	}
	serials := map[string]bool{}
	for _, c := range bmc.Components {
		if c.SerialNumber != "" {
			serials[c.SerialNumber] = true
		}
		if c.Hardware.CapacityBytes != nil || c.Hardware.Cores != nil {
			t.Fatal("invented IPMI inventory parity")
		}
	}
	for _, serial := range []string{"BOARD-SERIAL", "CHASSIS-SERIAL", "SYSTEM-SERIAL", "PSU-SERIAL"} {
		if !serials[serial] {
			t.Fatalf("missing scoped serial %q", serial)
		}
	}
	if len(bmc.VendorEvidence) != 6 || bmc.IdentityEvidence[0].Value != "iana:343" || bmc.IdentityEvidence[0].SubjectID != bmc.ID {
		t.Fatalf("numeric identity not scoped: %+v", bmc.IdentityEvidence)
	}
	before := f.sdrReads
	samples, err := a.Hardware(context.Background())
	if err != nil || len(samples) != 1 {
		t.Fatalf("hardware: %+v %v", samples, err)
	}
	if f.sdrReads != before {
		t.Fatalf("SDR downloaded during fresh cache: %d=>%d", before, f.sdrReads)
	}
	sample := samples[0]
	if len(sample.Metrics) != 4 || len(sample.HardwareHealth) != 6 {
		t.Fatalf("sample: %+v", sample)
	}
	expected := map[string]float64{"hardware.temperature.celsius": 42, "hardware.fan.rpm": 1200, "hardware.voltage.volts": 12, "hardware.power.watts": 150}
	for _, m := range sample.Metrics {
		if m.Value != expected[m.Name] {
			t.Fatalf("wrong unit conversion: %+v", m)
		}
		if m.ComponentID == "" || m.Observation.Source.Type != model.SourceIPMI || m.Observation.Time.SourceTimestamp != nil || m.Observation.Time.ClockOffset != nil {
			t.Fatalf("metric metadata: %+v", m)
		}
	}
	for _, h := range sample.HardwareHealth {
		if h.Name == "PSU status" && h.Status.Health != "unknown" {
			t.Fatalf("presence became failure/OK: %+v", h)
		}
	}
	second, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.DeviceID != second[0].DeviceID {
		t.Fatal("unstable device identity")
	}
	for i, m := range sample.Metrics {
		if m.ComponentID != second[0].Metrics[i].ComponentID || m.ResourceID != second[0].Metrics[i].ResourceID {
			t.Fatal("unstable sensor identity")
		}
	}
	encoded, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{ipmitest.Password, ipmitest.Username, "credential-ref"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("inventory leaked %s", secret)
		}
	}
	if f.closed != 3 {
		t.Fatalf("poll sessions not cleaned: %d", f.closed)
	}
}

func TestPartialFRUAndSensorPreservation(t *testing.T) {
	f := protocolFixture()
	board := int(f.frus[0][3]) * 8
	f.frus[0][board+10] ^= 1
	f.failedSensor = 2
	a := fixtureAdapter(t, f, Options{})
	inv, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Completeness != "partial" {
		t.Fatal("corrupt FRU not partial")
	}
	serials := map[string]bool{}
	for _, d := range inv.Devices {
		for _, c := range d.Components {
			serials[c.SerialNumber] = true
		}
	}
	if serials["BOARD-SERIAL"] || !serials["CHASSIS-SERIAL"] || !serials["SYSTEM-SERIAL"] || !serials["PSU-SERIAL"] {
		t.Fatalf("sibling preservation: %+v", serials)
	}
	samples, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples[0].Metrics) != 3 {
		t.Fatal("one malformed sensor discarded siblings")
	}
	for _, m := range samples[0].Metrics {
		if m.Name == "hardware.fan.rpm" {
			t.Fatal("malformed fan invented metric")
		}
	}
}

func TestMissingFRUAndUnavailableReadings(t *testing.T) {
	f := protocolFixture()
	delete(f.frus, 0)
	f.readings[1] = Reading{Raw: 0, Unavailable: true, StateBytes: 1}
	f.readings[2] = Reading{Raw: 0, ScanningDisabled: true, StateBytes: 1}
	f.repoErr = &CompletionError{Code: 0xc1}
	a := fixtureAdapter(t, f, Options{})
	inv, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Completeness != "partial" {
		t.Fatal("missing optional FRU should be partial")
	}
	f.repoErr = nil
	samples, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples[0].Metrics) != 2 {
		t.Fatal("unavailable readings created zero metrics")
	}
	for _, h := range samples[0].HardwareHealth {
		if (h.Name == "CPU temperature" || h.Name == "Fan speed") && h.Status.Health != "unknown" {
			t.Fatal("unavailable reading treated as failure")
		}
	}
}

func TestSDRCacheInvalidationAndBounds(t *testing.T) {
	for _, scenario := range []string{"changed_repository", "expired", "changed_bmc", "cyclic", "too_many", "bytes", "operations"} {
		t.Run(scenario, func(t *testing.T) {
			f := protocolFixture()
			a := fixtureAdapter(t, f, Options{})
			if _, err := a.Hardware(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := f.sdrReads
			switch scenario {
			case "changed_repository":
				f.repository.Added++
			case "expired":
				a.cache.refreshed = time.Now().Add(-time.Hour)
			case "changed_bmc":
				f.device.ProductID++
			case "cyclic":
				f.cyclic = true
				a.cache = sdrCache{}
			case "too_many":
				f.repository.Records = 65535
			case "bytes":
				a.limits.MaxSDRBytes = 260
				a.cache = sdrCache{}
			case "operations":
				a.limits.MaxOperations = 1
				a.cache = sdrCache{}
			}
			samples, err := a.Hardware(context.Background())
			if scenario == "changed_repository" || scenario == "expired" || scenario == "changed_bmc" {
				if err != nil || f.sdrReads <= before {
					t.Fatalf("cache did not refresh: %v", err)
				}
			} else {
				if len(samples) == 0 {
					t.Fatal("valid chassis prefix discarded")
				}
				if scenario == "too_many" || scenario == "bytes" || scenario == "operations" {
					if !errors.Is(err, driver.ErrLimit) {
						t.Fatalf("missing bound error: %v", err)
					}
				}
				if f.sdrReads-before > 25 || f.closed != 2 {
					t.Fatal("unbounded traversal or leaked session")
				}
			}
		})
	}
}

func TestAnalogConversionsAndUnits(t *testing.T) {
	base := func() sensor {
		return sensor{typ: 1, readingType: 1, full: &types.SDRFull{SensorEventReadingType: types.EventReadingType(1), SensorUnit: types.SensorUnit{BaseUnit: types.SensorUnitType_DegreesC}, ReadingFactors: types.ReadingFactors{M: 1}}}
	}
	for _, tt := range []struct {
		name   string
		raw    uint8
		alter  func(*sensor)
		want   float64
		metric string
		ok     bool
	}{
		{"celsius", 42, func(*sensor) {}, 42, "hardware.temperature.celsius", true},
		{"twos_complement", 0xfb, func(s *sensor) { s.full.SensorUnit.AnalogDataFormat = types.SensorAnalogUnitFormat_2sComplement }, -5, "hardware.temperature.celsius", true},
		{"ones_complement", 0xfa, func(s *sensor) { s.full.SensorUnit.AnalogDataFormat = types.SensorAnalogUnitFormat_1sComplement }, -5, "hardware.temperature.celsius", true},
		{"coefficients", 20, func(s *sensor) { s.full.M = 2; s.full.B = 3; s.full.B_Exp = 1; s.full.R_Exp = -1 }, 7, "hardware.temperature.celsius", true},
		{"negative_coefficient", 20, func(s *sensor) { s.full.M = -1 }, -20, "hardware.temperature.celsius", true},
		{"fahrenheit", 77, func(s *sensor) { s.full.SensorUnit.BaseUnit = types.SensorUnitType_DegreesF }, 25, "hardware.temperature.celsius", true},
		{"rpm", 120, func(s *sensor) { s.typ = 4; s.full.SensorUnit.BaseUnit = types.SensorUnitType_RPM; s.full.M = 10 }, 1200, "hardware.fan.rpm", true},
		{"percent", 60, func(s *sensor) {
			s.typ = 4
			s.full.SensorUnit.Percentage = true
			s.full.SensorUnit.BaseUnit = types.SensorUnitType_Unspecified
		}, 60, "hardware.fan.percent", true},
		{"voltage", 120, func(s *sensor) { s.typ = 2; s.full.SensorUnit.BaseUnit = types.SensorUnitType_Volts; s.full.R_Exp = -1 }, 12, "hardware.voltage.volts", true},
		{"current", 24, func(s *sensor) { s.typ = 3; s.full.SensorUnit.BaseUnit = types.SensorUnitType_Amps; s.full.R_Exp = -1 }, 2.4, "hardware.current.amperes", true},
		{"power", 150, func(s *sensor) { s.typ = 0xb; s.full.SensorUnit.BaseUnit = types.SensorUnitType_Watts }, 150, "hardware.power.watts", true},
		{"unsupported_linearization", 20, func(s *sensor) { s.full.LinearizationFunc = 0x70 }, 0, "", false},
		{"invalid_sqrt", 20, func(s *sensor) { s.full.LinearizationFunc = types.LinearizationFunc_SQRT; s.full.M = -1 }, 0, "", false},
		{"infinite", 0, func(s *sensor) { s.full.LinearizationFunc = types.LinearizationFunc_1X }, 0, "", false},
		{"compound_units", 42, func(s *sensor) { s.full.SensorUnit.ModifierRelation = types.SensorModifierRelation_Div }, 0, "", false},
		{"rate_units", 42, func(s *sensor) { s.full.SensorUnit.RateUnit = types.SensorRateUnit_PerSec }, 0, "", false},
		{"wrong_sensor_type", 42, func(s *sensor) { s.typ = 4 }, 0, "", false},
		{"invalid_percentage", 110, func(s *sensor) { s.full.SensorUnit.Percentage = true; s.full.SensorUnit.BaseUnit = 0 }, 0, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := base()
			tt.alter(&s)
			metric, _, got, ok := sensorMetric(s, tt.raw)
			if ok != tt.ok || ok && (metric != tt.metric || math.Abs(got-tt.want) > 1e-9) {
				t.Fatalf("conversion %s %g %v", metric, got, ok)
			}
		})
	}
}

func TestThresholdAndDiscreteMapping(t *testing.T) {
	for _, tt := range []struct {
		name          string
		typ, event    uint8
		bits          uint16
		health, state string
	}{
		{"normal", 1, 1, 0, "ok", ""}, {"lower_warning", 1, 1, 1, "warning", ""}, {"upper_warning", 1, 1, 8, "warning", ""}, {"critical", 1, 1, 16, "critical", ""},
		{"predictive_clear", 4, 4, 1, "ok", ""}, {"predictive_failure", 4, 4, 2, "warning", ""}, {"generic_asserted_unknown", 4, 3, 2, "unknown", ""},
		{"present", 4, 8, 2, "unknown", "Present"}, {"absent", 4, 8, 1, "unknown", "Absent"}, {"disabled", 4, 9, 1, "unknown", "Disabled"}, {"enabled", 4, 9, 2, "unknown", "Enabled"},
		{"psu_present", 8, 0x6f, 1, "unknown", "Present"}, {"psu_failure", 8, 0x6f, 2, "critical", ""}, {"psu_predictive", 8, 0x6f, 4, "warning", ""}, {"psu_standby", 8, 0x6f, 0x80, "unknown", "StandbyOffline"},
		{"memory_uncorrectable", 0xc, 0x6f, 2, "critical", ""}, {"cpu_presence", 7, 0x6f, 0x80, "unknown", ""}, {"storage_presence", 0xd, 0x6f, 1, "unknown", ""},
		{"oem_asserted", 8, 0x70, 2, "unknown", ""}, {"no_discrete_event", 8, 0x6f, 0, "unknown", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := sensorStatus(sensor{typ: tt.typ, readingType: tt.event, readingMask: 0x7fff}, Reading{States: tt.bits, StateBytes: 2})
			if got.Health != tt.health || tt.state != "" && got.State != tt.state {
				t.Fatalf("status %+v", got)
			}
		})
	}
	if got := sensorStatus(sensor{readingType: 1}, Reading{StateBytes: 0}); got.Health != "unknown" {
		t.Fatal("missing status bytes mean healthy")
	}
	if got := sensorStatus(sensor{readingType: 0x6f, typ: 8, readingMask: 1}, Reading{States: 2, StateBytes: 1}); got.Health != "unknown" {
		t.Fatal("unadvertised state interpreted")
	}
}

func TestFRUEncodingValidationAndPartialReads(t *testing.T) {
	for _, tt := range []struct {
		name    string
		tl      uint8
		data    []byte
		lang    uint8
		english bool
		want    string
		fail    bool
	}{
		{"latin1", 0xc2, []byte{0xc4, 'B'}, 0, false, "ÄB", false}, {"utf16", 0xc4, []byte{0x11, 4, 0x12, 4}, 95, false, "БВ", false},
		{"serial_english_override", 0xc2, []byte{'A', 'B'}, 95, true, "AB", false}, {"bcd", 0x42, []byte{0x12, 0xbc}, 0, false, "12-.", false},
		{"sixbit", 0x83, []byte{0x29, 0xdc, 0xa6}, 0, false, "IPMI", false}, {"reserved_bcd", 0x41, []byte{0xdf}, 0, false, "", true},
		{"binary", 2, []byte{1, 2}, 0, false, "", true}, {"invalid_utf16", 0xc2, []byte{0, 0xd8}, 95, false, "", true}, {"control", 0xc2, []byte{'A', 0}, 0, false, "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, err := decodeText(tt.tl, tt.data, tt.lang, tt.english)
			if (err != nil) != tt.fail || value != tt.want {
				t.Fatalf("text %q %v", value, err)
			}
		})
	}
	for _, word := range []bool{false, true} {
		t.Run("partial_read", func(t *testing.T) {
			f := protocolFixture()
			f.shortFRU = true
			f.wordFRU = word
			a := fixtureAdapter(t, f, Options{})
			inv, err := a.InventoryBatch(context.Background())
			if err != nil || inv.Completeness != "complete" {
				t.Fatalf("word/partial reads %v %+v", err, inv)
			}
			if f.fruReads < 10 {
				t.Fatal("partial reads not iterated")
			}
		})
	}
	for _, alter := range []func([]byte){func(b []byte) { b[7] ^= 1 }, func(b []byte) {
		b[2] = 255
		b[7] = 0
		var sum byte
		for _, x := range b[:7] {
			sum += x
		}
		b[7] = -sum
	}, func(b []byte) {
		b[3] = b[2]
		b[7] = 0
		var sum byte
		for _, x := range b[:7] {
			sum += x
		}
		b[7] = -sum
	}} {
		f := protocolFixture()
		alter(f.frus[0])
		a := fixtureAdapter(t, f, Options{})
		inv, err := a.InventoryBatch(context.Background())
		if err != nil || inv.Completeness != "partial" {
			t.Fatal("invalid framing accepted")
		}
	}
}

func TestSDRFullCompactSharingAndMalformed(t *testing.T) {
	for id, raw := range ipmitest.SDRs() {
		parsed, err := parseSDR(raw, 0xffff)
		if err != nil {
			t.Fatalf("record %d: %v", id, err)
		}
		if id <= 5 && len(sensors(parsed, raw)) != 1 {
			t.Fatal("supported record missing")
		}
	}
	raw := ipmitest.SDRs()[5]
	raw[23] = 3
	raw[24] = 0x80
	parsed, err := parseSDR(raw, 0xffff)
	if err != nil {
		t.Fatal(err)
	}
	shared := sensors(parsed, raw)
	if len(shared) != 3 || shared[0].number != 5 || shared[2].number != 7 || shared[2].instance != 3 || shared[0].resource == shared[1].resource {
		t.Fatalf("compact sharing %+v", shared)
	}
	raw = ipmitest.SDRs()[1]
	raw[47] = 0xff
	if _, err := parseSDR(raw, 0xffff); err == nil {
		t.Fatal("oversized sensor name accepted")
	}
	if _, err := parseSDR([]byte{1, 0, 0x51, 1, 255}, 0xffff); err == nil {
		t.Fatal("truncated SDR accepted")
	}
	first := ipmitest.SDRs()[1]
	second := slices.Clone(first)
	second[7] = 2
	p1, _ := parseSDR(first, 0xffff)
	p2, _ := parseSDR(second, 0xffff)
	s1, s2 := sensors(p1, first)[0], sensors(p2, second)[0]
	if s1.name != s2.name || s1.resource == s2.resource {
		t.Fatal("duplicate names conflict")
	}
}

func TestCancellationCleanupAndNoMutationContract(t *testing.T) {
	f := protocolFixture()
	f.ctxBlocked = true
	a := fixtureAdapter(t, f, Options{Timeout: 50 * time.Millisecond, MaxSessionDuration: 50 * time.Millisecond, RequestTimeout: 10 * time.Millisecond})
	started := time.Now()
	if _, err := a.Hardware(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(started) > time.Second || f.closed != 1 {
		t.Fatal("timeout did not bound/cleanup session")
	}
	f = protocolFixture()
	f.ctxBlocked = true
	a = fixtureAdapter(t, f, Options{})
	finished := make(chan error, 1)
	go func() { _, err := a.Hardware(context.Background()); finished <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		ops := f.ops
		f.mu.Unlock()
		if ops > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll did not start")
		}
		time.Sleep(time.Millisecond)
	}
	_ = a.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel pending poll")
	}
	if f.closed != 1 {
		t.Fatal("session cleanup absent")
	}
	_ = a.Close()
	if _, ok := any(a).(driver.InterfaceManager); ok {
		t.Fatal("IPMI mutation interface exposed")
	}
	clientType := reflect.TypeFor[Client]()
	for i := 0; i < clientType.NumMethod(); i++ {
		name := clientType.Method(i).Name
		if strings.HasPrefix(name, "Set") || strings.Contains(name, "Write") || name == "Raw" || name == "ChassisControl" {
			t.Fatalf("unsafe method %s", name)
		}
	}
}

func TestReferenceBMCNormalization(t *testing.T) {
	bmc := ipmitest.Start(t)
	a, err := New(driver.Options{ID: "reference-bmc", Address: bmc.Address}, fixedCredentials{}, "fixture", Options{Port: bmc.Port, Provenance: model.ProvenanceSimulated, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	inv, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.Completeness != "complete" || len(inv.Devices) != 2 {
		t.Fatalf("reference inventory %+v", inv)
	}
	samples, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || len(samples[0].Metrics) != 4 {
		t.Fatalf("reference samples %+v", samples)
	}
	if bmc.ActiveSessions() != 0 {
		t.Fatalf("reference sessions leaked: %d", bmc.ActiveSessions())
	}
	for _, m := range samples[0].Metrics {
		if m.Observation.Source.Provenance != model.ProvenanceSimulated {
			t.Fatal("fixture provenance absent")
		}
	}
}

func BenchmarkHardwareCachedFixture(b *testing.B) {
	f := protocolFixture()
	a := fixtureAdapter(b, f, Options{})
	if _, err := a.Hardware(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Hardware(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
