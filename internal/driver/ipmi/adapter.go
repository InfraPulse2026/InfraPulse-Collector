// Package ipmi adapts explicit IPMI v2.0 LANplus targets to the existing
// hardware observation contracts. It exposes no equipment mutation methods.
package ipmi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

// Options bounds both protocol operations and retained SDR metadata. Network
// sessions live for one poll; sensor requests reuse that session sequentially.
type Options struct {
	Port, CipherSuite, Retries                             int
	Privilege                                              string
	Timeout, RequestTimeout, MaxSessionDuration            time.Duration
	SDRCacheTTL                                            time.Duration
	MaxOperations, MaxResponseBytes, MaxPollBytes          int
	MaxSDRRecords, MaxSDRBytes, MaxFRURecords, MaxFRUBytes int
	Provenance                                             model.Provenance
}

type Adapter struct {
	opts        driver.Options
	limits      Options
	provider    credentials.Provider
	ref         string
	factory     Factory
	gate        chan struct{}
	lifecycle   context.Context
	cancel      context.CancelFunc
	closeOnce   sync.Once
	cache       sdrCache // owned by the poll gate
	fingerprint string
}

// New validates configuration without contacting the BMC.
func New(opts driver.Options, provider credentials.Provider, ref string, limits Options) (*Adapter, error) {
	return NewWithFactory(opts, provider, ref, limits, newClient)
}

// NewWithFactory supplies the same typed read-only boundary to deterministic
// protocol fixtures. Production registration always uses New.
func NewWithFactory(opts driver.Options, provider credentials.Provider, ref string, limits Options, factory Factory) (*Adapter, error) {
	if opts.ID == "" || len(opts.ID) > 256 || provider == nil || ref == "" || factory == nil || opts.EnableMutations ||
		opts.Address == "" || len(opts.Address) > 253 || strings.ContainsAny(opts.Address, "/\\@?#\r\n\x00") {
		return nil, errors.New("IPMI requires a host, target ID, credential reference and read-only options")
	}
	if net.ParseIP(opts.Address) == nil {
		for _, label := range strings.Split(strings.TrimSuffix(opts.Address, "."), ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, errors.New("invalid IPMI host")
			}
			for _, c := range label {
				if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
					return nil, errors.New("invalid IPMI host")
				}
			}
		}
	}
	for _, c := range opts.ID + ref {
		if unicode.IsControl(c) {
			return nil, errors.New("invalid IPMI identifier")
		}
	}
	if limits.Port == 0 {
		limits.Port = 623
	}
	if limits.CipherSuite == 0 {
		limits.CipherSuite = 17
	}
	if limits.Privilege == "" {
		limits.Privilege = "user"
	}
	if limits.Timeout == 0 {
		limits.Timeout = 30 * time.Second
	}
	if limits.RequestTimeout == 0 {
		limits.RequestTimeout = 2 * time.Second
	}
	if limits.MaxSessionDuration == 0 {
		limits.MaxSessionDuration = 30 * time.Second
	}
	if limits.SDRCacheTTL == 0 {
		limits.SDRCacheTTL = 5 * time.Minute
	}
	if limits.MaxOperations == 0 {
		limits.MaxOperations = 1024
	}
	if limits.MaxResponseBytes == 0 {
		limits.MaxResponseBytes = 4096
	}
	if limits.MaxPollBytes == 0 {
		limits.MaxPollBytes = 1 << 20
	}
	if limits.MaxSDRRecords == 0 {
		limits.MaxSDRRecords = 128
	}
	if limits.MaxSDRBytes == 0 {
		limits.MaxSDRBytes = 128 << 10
	}
	if limits.MaxFRURecords == 0 {
		limits.MaxFRURecords = 16
	}
	if limits.MaxFRUBytes == 0 {
		limits.MaxFRUBytes = 64 << 10
	}
	if limits.Provenance == "" {
		limits.Provenance = model.ProvenanceReal
	}
	if limits.Port < 1 || limits.Port > 65535 || (limits.CipherSuite != 17 && limits.CipherSuite != 3) ||
		(limits.Privilege != "user" && limits.Privilege != "operator") || limits.Retries < 0 || limits.Retries > 3 ||
		limits.Timeout < 10*time.Millisecond || limits.Timeout > 5*time.Minute || limits.RequestTimeout < 10*time.Millisecond || limits.RequestTimeout > 30*time.Second ||
		limits.MaxSessionDuration < 10*time.Millisecond || limits.MaxSessionDuration > 5*time.Minute || limits.SDRCacheTTL < time.Second || limits.SDRCacheTTL > time.Hour ||
		limits.MaxOperations < 1 || limits.MaxOperations > 8192 || limits.MaxResponseBytes < 128 || limits.MaxResponseBytes > 4096 ||
		limits.MaxPollBytes < limits.MaxResponseBytes || limits.MaxPollBytes > 8<<20 || limits.MaxSDRRecords < 1 || limits.MaxSDRRecords > 1024 ||
		limits.MaxSDRBytes < 260 || limits.MaxSDRBytes > 1<<20 || limits.MaxFRURecords < 1 || limits.MaxFRURecords > 128 ||
		limits.MaxFRUBytes < 8 || limits.MaxFRUBytes > 64<<10 ||
		(limits.Provenance != model.ProvenanceReal && limits.Provenance != model.ProvenanceSimulated) {
		return nil, errors.New("invalid IPMI cipher, privilege or polling limits")
	}
	limits.RequestTimeout = min(limits.RequestTimeout, limits.Timeout, limits.MaxSessionDuration)
	ctx, cancel := context.WithCancel(context.Background())
	return &Adapter{opts: opts, limits: limits, provider: provider, ref: ref, factory: factory, gate: make(chan struct{}, 1), lifecycle: ctx, cancel: cancel}, nil
}

func (a *Adapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "inventory.read", Supported: true}, {Name: "health.read", Supported: true}, {Name: "telemetry.read", Supported: true}}
}
func (a *Adapter) Close() error { a.closeOnce.Do(a.cancel); return nil }

type poll struct {
	a                 *Adapter
	ctx               context.Context
	client            Client
	operations, bytes int
	partial           bool
	failure           error
}

func (a *Adapter) start(ctx context.Context) (*poll, func(), error) {
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-a.lifecycle.Done():
		return nil, nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, min(a.limits.Timeout, a.limits.MaxSessionDuration))
	stop := context.AfterFunc(a.lifecycle, cancel)
	release := func() { stop(); cancel(); <-a.gate }
	if a.lifecycle.Err() != nil {
		release()
		return nil, nil, context.Canceled
	}
	credential, err := a.provider.Get(ctx, a.ref)
	if err != nil {
		contextError := ctx.Err()
		release()
		if contextError != nil {
			return nil, nil, contextError
		}
		return nil, nil, errors.New("IPMI credential reference unavailable")
	}
	client, err := a.factory(ctx, a.opts.Address, a.limits.Port, credential, SessionOptions{RequestTimeout: a.limits.RequestTimeout, Retries: a.limits.Retries, CipherSuite: a.limits.CipherSuite, Privilege: a.limits.Privilege, MaxResponseBytes: a.limits.MaxResponseBytes, MaxOperations: a.limits.MaxOperations, MaxPollBytes: a.limits.MaxPollBytes})
	if err != nil {
		release()
		return nil, nil, safePollError(err)
	}
	p := &poll{a: a, ctx: ctx, client: client}
	finish := func() {
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), min(a.limits.RequestTimeout, time.Second))
		_ = client.Close(cleanup)
		cancelCleanup()
		release()
	}
	return p, finish, nil
}

func (p *poll) operation() error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.operations >= p.a.limits.MaxOperations {
		return driver.ErrLimit
	}
	p.operations++
	return nil
}
func (p *poll) received(data []byte) error {
	if len(data) > p.a.limits.MaxResponseBytes || len(data) > p.a.limits.MaxPollBytes-p.bytes {
		return driver.ErrLimit
	}
	p.bytes += len(data)
	return nil
}
func (p *poll) record(err error) {
	if err == nil {
		return
	}
	p.partial = true
	// Individual absent/malformed FRUs and sensors do not erase the valid prefix.
	// Session/authentication and connectivity failures stay visible to scheduling.
	if errors.Is(err, ErrAuthentication) || errors.Is(err, ErrPermission) || errors.Is(err, driver.ErrConnectivity) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrLimit) {
		if p.failure == nil {
			p.failure = safePollError(err)
		}
	}
}
func safePollError(err error) error {
	for _, category := range []error{context.Canceled, context.DeadlineExceeded, ErrAuthentication, ErrPermission, ErrCipherSuite, driver.ErrLimit, driver.ErrConnectivity, driver.ErrMalformed, driver.ErrUnavailable} {
		if errors.Is(err, category) {
			return category
		}
	}
	return driver.ErrUnavailable
}
func (p *poll) source() string {
	if p.a.limits.Provenance == model.ProvenanceSimulated {
		return "simulated"
	}
	return "ipmi"
}
func (p *poll) observation(resource string, at time.Time) *model.ObservationMetadata {
	return model.NewObservation(model.SourceRef{Type: model.SourceIPMI, TargetID: "remote:" + p.a.opts.ID, Protocol: "ipmi", ResourceID: resource, Provenance: p.a.limits.Provenance}, p.source(), at, at, nil)
}
func stableID(target, kind, resource string) string {
	h := sha256.Sum256([]byte(target + "\x00" + kind + "\x00" + resource))
	return "ipmi-" + hex.EncodeToString(h[:16])
}
func unknownStatus(state string) model.HardwareStatus {
	return model.HardwareStatus{Health: "unknown", HealthRollup: "unknown", State: state}
}

func (p *poll) deviceInfo() (DeviceInfo, error) {
	if err := p.operation(); err != nil {
		return DeviceInfo{}, err
	}
	info, err := p.client.DeviceID(p.ctx)
	if err != nil {
		return info, err
	}
	if info.ManufacturerID > 0xfffff || info.FirmwareMajor > 127 || info.FirmwareMinorBCD>>4 > 9 || info.FirmwareMinorBCD&15 > 9 || info.IPMIVersion&15 == 0 || info.IPMIVersion&15 > 9 || info.IPMIVersion>>4 > 9 {
		return DeviceInfo{}, driver.ErrMalformed
	}
	fingerprint := fmt.Sprintf("%d/%d/%d/%d/%d/%d", info.DeviceID, info.Revision, info.ManufacturerID, info.ProductID, info.FirmwareMajor, info.FirmwareMinorBCD)
	if p.a.fingerprint != "" && p.a.fingerprint != fingerprint {
		p.a.cache = sdrCache{}
	}
	p.a.fingerprint = fingerprint
	return info, nil
}
func (p *poll) bmc(info DeviceInfo) model.Device {
	at := time.Now().UTC()
	uri := "/ipmi/bmc"
	available := false
	d := model.Device{ID: stableID(p.a.opts.ID, "Manager", uri), Name: p.a.opts.Name, Type: "unknown", Driver: "ipmi", Source: p.source(), Online: true, LastSeen: at, Location: p.a.opts.Location, UptimeAvailable: &available, Capabilities: p.a.Capabilities(), Observation: p.observation(uri, at), Hardware: &model.HardwareInventory{ResourceType: "Manager", ManagerType: "BMC", Protocol: "ipmi", Status: unknownStatus("unknown")}}
	minor := (info.FirmwareMinorBCD>>4)*10 + (info.FirmwareMinorBCD & 15)
	d.FirmwareVersion = fmt.Sprintf("%d.%02d", info.FirmwareMajor, minor)
	d.HardwareRevision = strconv.Itoa(int(info.Revision & 15))
	if !info.Available {
		d.Hardware.Status.State = "Updating"
	}
	for _, field := range []struct {
		name  string
		value uint64
	}{{"device_id", uint64(info.DeviceID)}, {"manufacturer_id", uint64(info.ManufacturerID)}, {"product_id", uint64(info.ProductID)}, {"additional_device_support", uint64(info.Support)}, {"ipmi_version_raw", uint64(info.IPMIVersion)}} {
		d.VendorEvidence = append(d.VendorEvidence, model.Evidence{Source: "ipmi.get-device-id." + field.name, Value: strconv.FormatUint(field.value, 10), Confidence: "high"})
	}
	d.VendorEvidence = append(d.VendorEvidence, model.Evidence{Source: "ipmi.get-device-id.ipmi_version", Value: fmt.Sprintf("%d.%d", info.IPMIVersion&15, info.IPMIVersion>>4), Confidence: "high"})
	// Numeric IANA ownership is BMC evidence; it is not a verified vendor name.
	if info.ManufacturerID != 0 && info.ManufacturerID != 0xfffff {
		p.identity(&d.IdentityEvidence, model.IdentityManufacturer, "iana:"+strconv.FormatUint(uint64(info.ManufacturerID), 10), d.ID, "manager", "ipmi.get-device-id.manufacturer_id", d.Observation)
	}
	if _, _, err := model.NormalizeIP(p.a.opts.Address); err == nil {
		p.identity(&d.IdentityEvidence, model.IdentityManagementIP, p.a.opts.Address, d.ID, "manager", "configured.ipmi.endpoint", d.Observation)
	}
	return d
}

func (a *Adapter) Probe(ctx context.Context) (model.Device, error) {
	p, finish, err := a.start(ctx)
	if err != nil {
		return model.Device{}, err
	}
	defer finish()
	info, err := p.deviceInfo()
	if err != nil {
		return model.Device{}, safePollError(err)
	}
	return p.bmc(info), nil
}

func (a *Adapter) InventoryBatch(ctx context.Context) (model.InventoryUpdate, error) {
	p, finish, err := a.start(ctx)
	if err != nil {
		return model.InventoryUpdate{}, err
	}
	defer finish()
	info, err := p.deviceInfo()
	if err != nil {
		return model.InventoryUpdate{}, safePollError(err)
	}
	bmc := p.bmc(info)
	chassis, _, err := p.chassis()
	p.record(err)
	cache, err := p.discoverSDR(true)
	p.record(err)
	if info.Support&8 != 0 {
		frus := []uint8{0}
		for _, id := range cache.frus {
			if id == 0 {
				continue
			}
			if len(frus) >= p.a.limits.MaxFRURecords {
				p.record(driver.ErrLimit)
				break
			}
			frus = append(frus, id)
		}
		cache.frus = frus
	}
	for _, fru := range cache.frus {
		components, e := p.fru(fru, bmc.ID)
		p.record(e)
		bmc.Components = append(bmc.Components, components...)
		if p.ctx.Err() != nil {
			break
		}
	}
	for _, sensor := range cache.sensors {
		bmc.Components = append(bmc.Components, p.sensorComponent(sensor, bmc.ID))
	}
	p.componentEvidence(&bmc)
	result := model.InventoryUpdate{Devices: []model.Device{bmc}, ObservedAt: time.Now().UTC(), Completeness: "complete"}
	if chassis.ID != "" {
		bmc.Hardware.Relations = []model.ResourceRelationship{{Type: "BMC_MANAGES_CHASSIS", TargetResourceID: "/ipmi/chassis"}}
		result.Devices[0] = bmc
		result.Devices = append(result.Devices, chassis)
	}
	if p.partial {
		result.Completeness = "partial"
	}
	result.Observation = p.observation("/ipmi", result.ObservedAt)
	sort.Slice(result.Devices, func(i, j int) bool { return result.Devices[i].ID < result.Devices[j].ID })
	for i := range result.Devices {
		sort.Slice(result.Devices[i].Components, func(x, y int) bool { return result.Devices[i].Components[x].ID < result.Devices[i].Components[y].ID })
		result.Devices[i].Observation.Time.CollectedAt = result.ObservedAt
	}
	return result, p.failure
}

func (a *Adapter) Hardware(ctx context.Context) ([]model.TelemetrySample, error) {
	p, finish, err := a.start(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	info, err := p.deviceInfo()
	if err != nil {
		return nil, safePollError(err)
	}
	bmc := p.bmc(info)
	available := false
	at := time.Now().UTC()
	sample := model.TelemetrySample{DeviceID: bmc.ID, Source: p.source(), ObservedAt: at, CollectedAt: at, UptimeAvailable: &available, Observation: p.observation("/ipmi/bmc", at), IdentityEvidence: bmc.IdentityEvidence}
	_, chassisHealth, err := p.chassis()
	p.record(err)
	if chassisHealth.ID != "" {
		sample.HardwareHealth = append(sample.HardwareHealth, chassisHealth)
	}
	cache, err := p.discoverSDR(false)
	p.record(err)
	for index, sensor := range cache.sensors {
		health, metric, e := p.sensorReading(sensor)
		p.record(e)
		sample.HardwareHealth = append(sample.HardwareHealth, health)
		if metric != nil {
			sample.Metrics = append(sample.Metrics, *metric)
		}
		if p.ctx.Err() != nil || p.operations >= p.a.limits.MaxOperations {
			if p.ctx.Err() != nil {
				p.record(p.ctx.Err())
			} else if index+1 < len(cache.sensors) {
				p.record(driver.ErrLimit)
			}
			break
		}
	}
	if len(sample.HardwareHealth) == 0 && len(sample.Metrics) == 0 {
		sample.HardwareHealth = []model.ComponentHealth{{ID: bmc.ID, ResourceID: "/ipmi/bmc", Status: unknownStatus("unknown"), Observation: model.CloneObservation(sample.Observation)}}
	}
	sample.CollectedAt = time.Now().UTC()
	sample.Observation.Time.CollectedAt = sample.CollectedAt
	return []model.TelemetrySample{sample}, p.failure
}

func (p *poll) chassis() (model.Device, model.ComponentHealth, error) {
	if err := p.operation(); err != nil {
		return model.Device{}, model.ComponentHealth{}, err
	}
	info, err := p.client.ChassisStatus(p.ctx)
	if err != nil {
		return model.Device{}, model.ComponentHealth{}, err
	}
	uri := "/ipmi/chassis"
	at := time.Now().UTC()
	status := unknownStatus("unknown")
	if info.PowerFault || info.PowerControlFault || info.PowerOverload || info.DriveFault || info.CoolingFault {
		status.Health = "critical"
	} else {
		status.Health = "ok"
	}
	power := "Off"
	if info.PowerOn {
		power = "On"
	}
	status.State = "power_" + strings.ToLower(power)
	for _, flag := range []struct {
		set  bool
		name string
	}{{info.PowerFault, "power_fault"}, {info.PowerControlFault, "power_control_fault"}, {info.PowerOverload, "power_overload"}, {info.DriveFault, "drive_fault"}, {info.CoolingFault, "cooling_fault"}} {
		if flag.set {
			status.State += ";" + flag.name
		}
	}
	if info.Intrusion {
		status.State += ";intrusion"
		if status.Health == "ok" {
			status.Health = "warning"
		}
	}
	h := &model.HardwareInventory{ResourceType: "Chassis", PowerState: power, Status: status, Relations: []model.ResourceRelationship{{Type: "MANAGED_BY_BMC", TargetResourceID: "/ipmi/bmc"}}}
	if info.Intrusion {
		h.PhysicalSecurity = "intrusion"
	}
	available := false
	id := stableID(p.a.opts.ID, "Chassis", uri)
	obs := p.observation(uri, at)
	d := model.Device{ID: id, Type: "unknown", Driver: "ipmi", Source: p.source(), Online: true, LastSeen: at, Location: p.a.opts.Location, UptimeAvailable: &available, Capabilities: p.a.Capabilities(), Hardware: h, Observation: obs}
	return d, model.ComponentHealth{ID: id, ResourceID: uri, Status: status, Observation: model.CloneObservation(obs)}, nil
}

func (p *poll) identity(values *[]model.IdentityEvidence, kind model.IdentityEvidenceType, value, subject, subjectKind, origin string, observation *model.ObservationMetadata) {
	if value == "" {
		return
	}
	confidence := "medium"
	if kind == model.IdentitySerial {
		confidence = "high"
	} else if kind == model.IdentityManagementIP {
		confidence = "low"
	}
	fact, err := model.NewIdentityFact(kind, value, observation.Source, confidence, subject, subjectKind, origin, &observation.Time)
	if err != nil {
		fact.Confidence = "unresolved"
		p.record(driver.ErrMalformed)
	}
	if fact.Validate() == nil {
		if len(*values) >= 4096 {
			p.record(driver.ErrLimit)
			return
		}
		*values = append(*values, fact)
	}
}

func (p *poll) componentEvidence(d *model.Device) {
	for _, c := range d.Components {
		subjectKind := "component"
		if c.Class == "Chassis" {
			subjectKind = "chassis"
		}
		for _, entry := range []struct {
			kind         model.IdentityEvidenceType
			value, field string
		}{{model.IdentitySerial, c.SerialNumber, "serial_number"}, {model.IdentityManufacturer, c.Manufacturer, "manufacturer"}, {model.IdentityModel, c.Model, "model"}} {
			p.identity(&d.IdentityEvidence, entry.kind, entry.value, c.ID, subjectKind, "ipmi.fru."+strings.ToLower(c.Class)+"."+entry.field, c.Observation)
		}
	}
}
