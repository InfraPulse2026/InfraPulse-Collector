package redfish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/stmcginnis/gofish/schemas"
)

// InventoryBatch preserves one configured Target and multiple independently
// identified resources without imposing a new contract on SNMP/RouterOS.
func (a *Adapter) InventoryBatch(ctx context.Context) (model.InventoryUpdate, error) {
	result, err := a.collect(ctx)
	return result.inventory, err
}
func (a *Adapter) Hardware(ctx context.Context) ([]model.TelemetrySample, error) {
	return a.hardware(ctx)
}

type result struct {
	inventory model.InventoryUpdate
	samples   []model.TelemetrySample
}
type discovery struct {
	p       *poll
	result  result
	failure error
	plan    []readPlan
}
type readPlan struct {
	uri, kind, deviceID, parentID string
	top                           bool
}

func (a *Adapter) collect(ctx context.Context) (result, error) {
	p, finish, err := a.start(ctx)
	if err != nil {
		return result{}, err
	}
	defer finish()
	d := &discovery{p: p}
	root, _, err := p.get("/redfish/v1/")
	if err != nil {
		return result{}, safeError(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(root, &fields)
	collections := 0
	for _, entry := range []struct{ field, kind string }{{"Systems", "ComputerSystem"}, {"Chassis", "Chassis"}, {"Managers", "Manager"}} {
		collection, linkErr := checkedLink(fields, entry.field)
		if linkErr != nil {
			d.record(linkErr)
			if d.failure == nil {
				d.failure = driver.ErrMalformed
			}
			continue
		}
		if collection == "" {
			for _, previous := range a.plan {
				if previous.top && previous.kind == entry.kind {
					// An omitted previously advertised collection is not an
					// authoritative empty Members array.
					p.partial = true
					break
				}
			}
			continue
		}
		collections++
		members, e := p.members(collection)
		if e != nil {
			d.record(e)
			if errors.Is(e, driver.ErrMalformed) && d.failure == nil {
				d.failure = driver.ErrMalformed
			}
		}
		for _, uri := range members {
			device, sample, e := d.device(uri, entry.kind)
			if e != nil {
				d.record(e)
				if errors.Is(e, driver.ErrMalformed) && d.failure == nil {
					d.failure = driver.ErrMalformed
				}
				continue
			}
			d.result.inventory.Devices = append(d.result.inventory.Devices, device)
			d.result.samples = append(d.result.samples, sample)
			if p.ctx.Err() != nil {
				d.record(p.ctx.Err())
				break
			}
		}
		if p.ctx.Err() != nil {
			break
		}
	}
	if collections == 0 {
		// A service without any supported collection is unavailable for this
		// adapter, rather than an authoritative empty equipment snapshot.
		d.record(driver.ErrUnavailable)
	}
	at := time.Now().UTC()
	d.result.inventory.ObservedAt = at
	d.result.inventory.Observation = p.observation("/redfish/v1/", at, nil)
	d.result.inventory.Completeness = "complete"
	if p.partial {
		d.result.inventory.Completeness = "partial"
	}
	sort.Slice(d.result.inventory.Devices, func(i, j int) bool { return d.result.inventory.Devices[i].ID < d.result.inventory.Devices[j].ID })
	sort.Slice(d.result.samples, func(i, j int) bool { return d.result.samples[i].DeviceID < d.result.samples[j].DeviceID })
	if !p.partial && d.failure == nil {
		a.plan = d.plan
	} else {
		a.mergePlan(d.plan)
	}
	return d.result, d.failure
}

func (a *Adapter) mergePlan(next []readPlan) {
	seen := map[string]bool{}
	for _, node := range a.plan {
		seen[node.deviceID+"\x00"+node.uri] = true
	}
	for _, node := range next {
		key := node.deviceID + "\x00" + node.uri
		if !seen[key] && len(a.plan) < a.limits.MaxResources {
			seen[key] = true
			a.plan = append(a.plan, node)
		}
	}
}
func (d *discovery) remember(uri, kind, deviceID, parentID string, top bool) {
	for _, node := range d.plan {
		if node.uri == uri && node.deviceID == deviceID {
			return
		}
	}
	if len(d.plan) >= d.p.a.limits.MaxResources {
		d.record(driver.ErrLimit)
		return
	}
	d.plan = append(d.plan, readPlan{uri: uri, kind: kind, deviceID: deviceID, parentID: parentID, top: top})
}

// Inventory refreshes a small URI plan. Hardware polls then GET only known
// resource/status/sensor URIs without repeated collection discovery or network
// interface inventory. Partial refreshes preserve the previous plan.
func (a *Adapter) hardware(ctx context.Context) ([]model.TelemetrySample, error) {
	p, finish, err := a.start(ctx)
	if err != nil {
		return nil, err
	}
	if len(a.plan) == 0 {
		finish()
		result, e := a.collect(ctx)
		return result.samples, e
	}
	defer finish()
	d := &discovery{p: p}
	samples := map[string]*model.TelemetrySample{}
	for _, node := range a.plan {
		sample := samples[node.deviceID]
		if sample == nil {
			at := time.Now().UTC()
			available := false
			sample = &model.TelemetrySample{DeviceID: node.deviceID, ObservedAt: at, CollectedAt: at, Source: p.source(), UptimeAvailable: &available}
			samples[node.deviceID] = sample
		}
		body, uri, e := p.get(node.uri)
		if e != nil {
			d.record(e)
			if node.top && errors.Is(e, driver.ErrMalformed) && d.failure == nil {
				d.failure = driver.ErrMalformed
			}
			continue
		}
		if node.kind == "Thermal" || node.kind == "Power" {
			device := model.Device{ID: node.deviceID}
			d.legacy(&device, sample, body, uri, node.kind, node.parentID)
			sample.CollectedAt = time.Now().UTC()
			continue
		}
		base, h, e := resource(body, node.kind)
		if e != nil {
			d.record(e)
			if node.top && d.failure == nil {
				d.failure = driver.ErrMalformed
			}
			continue
		}
		original, originalUUID := base, h.HardwareUUID
		d.boundResource(&base, h)
		at := time.Now().UTC()
		observation := p.observation(uri, at, nil)
		subject := stableID(a.opts.ID, node.kind, uri)
		if node.top {
			subject = ""
		}
		d.resourceIdentity(&sample.IdentityEvidence, original, originalUUID, node.kind, subject, observation, h)
		if node.top {
			sample.Observation = observation
			sample.ObservedAt = at
		}
		sample.CollectedAt = at
		d.addHealth(sample, stableID(a.opts.ID, node.kind, uri), uri, base.Name, h.Status, observation)
		d.metrics(sample, body, uri, stableID(a.opts.ID, node.kind, uri), node.kind, h.Status)
	}
	var out []model.TelemetrySample
	for _, sample := range samples {
		if len(sample.HardwareHealth) > 0 || len(sample.Metrics) > 0 {
			if sample.Observation == nil {
				sample.Observation = p.observation(sample.HardwareHealth[0].ResourceID, sample.ObservedAt, nil)
			}
			sample.Observation.Time.CollectedAt = sample.CollectedAt
			out = append(out, *sample)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out, d.failure
}

// Optional 404 and one malformed optional resource preserve other observations.
// Authentication, transport and poll budget failures remain actual failures.
func (d *discovery) record(err error) {
	if err == nil {
		return
	}
	d.p.partial = true
	var status *httpFailure
	if errors.As(err, &status) && status.status == 404 {
		return
	}
	if errors.Is(err, driver.ErrMalformed) {
		return
	}
	if d.failure == nil {
		d.failure = safeError(err)
	}
}

func stableID(target, kind, uri string) string {
	digest := sha256.Sum256([]byte(target + "\x00" + kind + "\x00" + uri))
	return "redfish-" + hex.EncodeToString(digest[:16])
}
func (p *poll) observation(uri string, at time.Time, source *time.Time) *model.ObservationMetadata {
	return model.NewObservation(model.SourceRef{Type: model.SourceRedfish, TargetID: "remote:" + p.a.opts.ID, Protocol: "redfish", ResourceID: uri, Provenance: p.a.limits.Provenance}, p.source(), at, at, source)
}
func (p *poll) source() string {
	if p.a.limits.Provenance == model.ProvenanceSimulated {
		return "simulated"
	}
	return "redfish"
}
func health(value schemas.Health) string {
	switch value {
	case schemas.OKHealth:
		return "ok"
	case schemas.WarningHealth:
		return "warning"
	case schemas.CriticalHealth:
		return "critical"
	default:
		return "unknown"
	}
}
func status(value schemas.Status) model.HardwareStatus {
	state := string(value.State)
	if state == "" {
		state = "unknown"
	}
	return model.HardwareStatus{Health: health(value.Health), HealthRollup: health(value.HealthRollup), State: state}
}
func link(fields map[string]json.RawMessage, key string) string {
	var ref linkRef
	_ = json.Unmarshal(fields[key], &ref)
	return ref.ID
}

// Optional absent/null properties are distinct from malformed advertised links.
// In particular, a malformed link cannot imply authoritative component removal.
func checkedLink(fields map[string]json.RawMessage, key string) (string, error) {
	raw := fields[key]
	if raw == nil || string(raw) == "null" {
		return "", nil
	}
	var ref linkRef
	if json.Unmarshal(raw, &ref) != nil || ref.ID == "" {
		return "", driver.ErrMalformed
	}
	return ref.ID, nil
}

func (d *discovery) link(fields map[string]json.RawMessage, key string) string {
	value, err := checkedLink(fields, key)
	d.record(err)
	return value
}

func (d *discovery) links(fields map[string]json.RawMessage, key string) []string {
	raw := fields[key]
	if raw == nil || string(raw) == "null" {
		return nil
	}
	var refs []json.RawMessage
	if json.Unmarshal(raw, &refs) != nil {
		d.record(driver.ErrMalformed)
		return nil
	}
	var out []string
	for _, rawRef := range refs {
		var ref linkRef
		if json.Unmarshal(rawRef, &ref) != nil || ref.ID == "" {
			d.record(driver.ErrMalformed)
			continue
		}
		if len(out) >= 1024 {
			d.record(driver.ErrLimit)
			break
		}
		out = append(out, ref.ID)
	}
	return out
}

// These bounds match the normalized wire contract before durable admission.
// Suppress malformed optional facts and retain the rest of the resource so a
// single property cannot create an unencodable record in the replay outbox.
func (d *discovery) boundResource(b *baseResource, h *model.HardwareInventory) {
	for _, value := range []*string{&b.Name, &b.Manufacturer, &b.Model, &b.SerialNumber, &b.FirmwareVersion,
		&h.HardwareUUID, &h.SKU, &h.PartNumber, &h.BIOSVersion, &h.PowerState, &h.ChassisType,
		&h.ManagerType, &h.AssetTag, &h.PhysicalSecurity, &h.DeviceLocation, &h.MemoryType,
		&h.ErrorCorrection, &h.Protocol, &h.MediaType} {
		if !safeResourceText(*value, 1024) {
			*value = ""
			d.record(driver.ErrMalformed)
		}
	}
	d.boundStatus(&h.Status)
}

func safeResourceText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func (d *discovery) boundStatus(st *model.HardwareStatus) {
	if len(st.State) > 128 {
		st.State = "unknown"
		d.record(driver.ErrMalformed)
	}
}

func (d *discovery) boundedURI(uri string) string {
	canonical, err := d.p.a.canonical(uri, true)
	if err != nil {
		d.record(err)
		return ""
	}
	return canonical
}

type baseResource struct {
	Name, Manufacturer, Model, SerialNumber, FirmwareVersion string
	Status                                                   schemas.Status
}

func resource(body json.RawMessage, kind string) (baseResource, *model.HardwareInventory, error) {
	var b baseResource
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &b) != nil || json.Unmarshal(body, &fields) != nil {
		return b, nil, driver.ErrMalformed
	}
	if typ := text(fields["@odata.type"]); typ != "" && !strings.HasSuffix(typ, "."+kind) {
		return b, nil, driver.ErrMalformed
	}
	h := &model.HardwareInventory{ResourceType: kind, Status: status(b.Status)}
	var err error
	switch kind {
	case "ComputerSystem":
		var v schemas.ComputerSystem
		err = json.Unmarshal(body, &v)
		h.HardwareUUID = v.UUID
		h.SKU = v.SKU
		h.PartNumber = v.PartNumber
		h.BIOSVersion = v.BiosVersion
		h.PowerState = string(v.PowerState)
	case "Chassis":
		var v schemas.Chassis
		err = json.Unmarshal(body, &v)
		h.HardwareUUID = v.UUID
		h.PartNumber = v.PartNumber
		h.ChassisType = string(v.ChassisType)
		h.AssetTag = v.AssetTag
		h.PhysicalSecurity = string(v.PhysicalSecurity.IntrusionSensor)
	case "Manager":
		var v schemas.Manager
		err = json.Unmarshal(body, &v)
		h.HardwareUUID = v.UUID
		h.ManagerType = string(v.ManagerType)
	case "Processor":
		var v schemas.Processor
		err = json.Unmarshal(body, &v)
		h.Cores = uint32Value(v.TotalCores)
		h.Threads = uint32Value(v.TotalThreads)
		h.DeviceLocation = v.Socket
	case "Memory":
		var capacity struct{ CapacityMiB *uint64 }
		err = json.Unmarshal(body, &capacity)
		delete(fields, "CapacityMiB")
		safeBody, _ := json.Marshal(fields)
		var v schemas.Memory
		if e := json.Unmarshal(safeBody, &v); e != nil {
			err = e
		}
		h.DeviceLocation = v.DeviceLocator
		h.MemoryType = string(v.MemoryDeviceType)
		if h.MemoryType == "" {
			h.MemoryType = string(v.MemoryType)
		}
		h.ErrorCorrection = string(v.ErrorCorrection)
		if capacity.CapacityMiB != nil {
			if *capacity.CapacityMiB > math.MaxUint64/(1024*1024) {
				err = driver.ErrMalformed
			} else {
				bytes := *capacity.CapacityMiB * 1024 * 1024
				h.CapacityBytes = &bytes
			}
		}
	case "Storage":
		var v schemas.Storage
		err = json.Unmarshal(body, &v)
	case "Drive":
		// gofish exposes CapacityBytes as *int. Keep the standard property as
		// uint64 so 32-bit OpenWrt collectors can observe drives larger than 2GiB.
		var capacity struct{ CapacityBytes *uint64 }
		err = json.Unmarshal(body, &capacity)
		h.CapacityBytes = capacity.CapacityBytes
		delete(fields, "CapacityBytes")
		safeBody, _ := json.Marshal(fields)
		var v schemas.Drive
		if e := json.Unmarshal(safeBody, &v); e != nil {
			err = e
		}
		h.Protocol = string(v.Protocol)
		h.MediaType = string(v.MediaType)
		h.PartNumber = v.PartNumber
	case "PowerSupply":
		var v schemas.PowerSupply
		err = json.Unmarshal(body, &v)
		h.PartNumber = v.PartNumber
	case "Fan":
		var v schemas.Fan
		err = json.Unmarshal(body, &v)
		h.PartNumber = v.PartNumber
	case "Sensor":
		var v schemas.Sensor
		err = json.Unmarshal(body, &v)
	case "ThermalSubsystem":
		var v schemas.ThermalSubsystem
		err = json.Unmarshal(body, &v)
	case "PowerSubsystem":
		var v schemas.PowerSubsystem
		err = json.Unmarshal(body, &v)
	case "EnvironmentMetrics":
		var v schemas.EnvironmentMetrics
		err = json.Unmarshal(body, &v)
	case "PowerSupplyMetrics":
		var v schemas.PowerSupplyMetrics
		err = json.Unmarshal(body, &v)
	case "ThermalMetrics":
		var v schemas.ThermalMetrics
		err = json.Unmarshal(body, &v)
	default:
		err = driver.ErrMalformed
	}
	if err != nil {
		return b, nil, driver.ErrMalformed
	}
	return b, h, nil
}
func uint32Value(value *int) *uint32 {
	if value == nil || *value < 0 || uint64(*value) > math.MaxUint32 {
		return nil
	}
	v := uint32(*value)
	return &v
}

func (d *discovery) device(uri, kind string) (model.Device, model.TelemetrySample, error) {
	if d.p.emitted >= d.p.a.limits.MaxResources {
		return model.Device{}, model.TelemetrySample{}, driver.ErrLimit
	}
	d.p.emitted++
	body, uri, err := d.p.get(uri)
	if err != nil {
		return model.Device{}, model.TelemetrySample{}, err
	}
	b, h, err := resource(body, kind)
	if err != nil {
		return model.Device{}, model.TelemetrySample{}, err
	}
	original, originalUUID := b, h.HardwareUUID
	d.boundResource(&b, h)
	at := time.Now().UTC()
	observation := d.p.observation(uri, at, nil)
	device := model.Device{ID: stableID(d.p.a.opts.ID, kind, uri), Name: b.Name, Type: "unknown", Vendor: b.Manufacturer, Model: b.Model, SerialNumber: b.SerialNumber, FirmwareVersion: b.FirmwareVersion, Driver: "redfish", Source: d.p.source(), Online: true, LastSeen: at, Location: d.p.a.opts.Location, Hardware: h, Observation: observation, Capabilities: d.p.a.Capabilities()}
	d.remember(uri, kind, device.ID, "", true)
	available := false
	device.UptimeAvailable = &available
	if kind == "ComputerSystem" {
		device.FirmwareVersion = ""
	} // BIOS lives in the explicitly named field.
	d.resourceIdentity(&device.IdentityEvidence, original, originalUUID, kind, "", observation, h)
	device.Observation.Time.CollectedAt = time.Now().UTC()
	sample := model.TelemetrySample{DeviceID: device.ID, ObservedAt: at, CollectedAt: at, Source: d.p.source(), Observation: model.CloneObservation(observation), UptimeAvailable: &available}
	d.addHealth(&sample, device.ID, uri, b.Name, h.Status, observation)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	d.relations(fields, h)
	for _, entry := range []struct{ field, kind string }{{"Processors", "Processor"}, {"Memory", "Memory"}, {"Storage", "Storage"}, {"Sensors", "Sensor"}, {"EthernetInterfaces", "EthernetInterface"}} {
		collection := d.link(fields, entry.field)
		if collection == "" {
			continue
		}
		members, e := d.p.members(collection)
		if e != nil {
			d.record(e)
		}
		for _, member := range members {
			d.component(&device, &sample, member, entry.kind, device.ID)
		}
	}
	for _, entry := range []struct{ field, kind string }{{"ThermalSubsystem", "ThermalSubsystem"}, {"PowerSubsystem", "PowerSubsystem"}, {"EnvironmentMetrics", "EnvironmentMetrics"}, {"Thermal", "Thermal"}, {"Power", "Power"}} {
		if child := d.link(fields, entry.field); child != "" {
			d.component(&device, &sample, child, entry.kind, device.ID)
		}
	}
	sort.Slice(device.Components, func(i, j int) bool { return device.Components[i].ID < device.Components[j].ID })
	device.IdentityEvidence = model.MergeIdentityEvidence(device.IdentityEvidence, model.DeviceIdentityEvidence(device, observation.Source))
	return device, sample, nil
}

func resourceSubject(kind string) string {
	switch kind {
	case "ComputerSystem":
		return "computer_system"
	case "Chassis":
		return "chassis"
	case "Manager":
		return "manager"
	case "EthernetInterface":
		return "network_interface"
	default:
		return "component"
	}
}

// Explicit resource facts preserve the original protocol property and subject.
// Invalid optional values remain bounded diagnostics, never fabricated IDs.
func (d *discovery) identity(values *[]model.IdentityEvidence, kind model.IdentityEvidenceType, value, subject, subjectKind, origin string, observation *model.ObservationMetadata) *model.IdentityEvidence {
	if value == "" {
		return nil
	}
	fact, err := model.NewIdentityFact(kind, value, observation.Source, "medium", subject, subjectKind, origin, &observation.Time)
	if kind == model.IdentitySerial || kind == model.IdentityHardwareUUID {
		fact.Confidence = "high"
	} else if kind == model.IdentityIP {
		fact.Confidence = "low"
	}
	if err != nil {
		fact.Confidence = "unresolved"
		d.record(driver.ErrMalformed)
	}
	if fact.Validate() != nil {
		return nil
	}
	if !d.identityBudget(*values, subject, observation.Source.ResourceID) {
		d.record(driver.ErrLimit)
		return nil
	}
	*values = append(*values, fact)
	return &(*values)[len(*values)-1]
}

func (d *discovery) identityBudget(values []model.IdentityEvidence, subject, resource string) bool {
	if len(values) >= model.MaxIdentityEvidence {
		return false
	}
	count := 0
	for _, fact := range values {
		if fact.SubjectID == subject && fact.Source.ResourceID == resource {
			count++
		}
	}
	return count < model.MaxIdentityPerSubject
}

func (d *discovery) rejectedIdentity(values *[]model.IdentityEvidence, subject, kind string, observation *model.ObservationMetadata) {
	rejections := d.p.rejectedIdentity[observation.Source.ResourceID]
	sort.Slice(rejections, func(i, j int) bool { return rejections[i].origin < rejections[j].origin })
	for _, rejection := range rejections {
		if !d.identityBudget(*values, subject, observation.Source.ResourceID) {
			d.record(driver.ErrLimit)
			return
		}
		// A fixed hostile input asks the common normalizer for its bounded
		// rejected marker. The original value has already been discarded.
		fact, _ := model.NewIdentityFact(rejection.kind, "\x00", observation.Source, "unresolved", subject, resourceSubject(kind), rejection.origin, &observation.Time)
		if fact.Validate() == nil {
			*values = append(*values, fact)
		}
	}
}

func (d *discovery) resourceIdentity(values *[]model.IdentityEvidence, b baseResource, uuid, kind, subject string, observation *model.ObservationMetadata, h *model.HardwareInventory) {
	for _, field := range []struct {
		kind          model.IdentityEvidenceType
		value, origin string
	}{{model.IdentitySerial, b.SerialNumber, "redfish.SerialNumber"}, {model.IdentityManufacturer, b.Manufacturer, "redfish.Manufacturer"}, {model.IdentityModel, b.Model, "redfish.Model"}} {
		d.identity(values, field.kind, field.value, subject, resourceSubject(kind), field.origin, observation)
	}
	if fact := d.identity(values, model.IdentityHardwareUUID, uuid, subject, resourceSubject(kind), "redfish.UUID", observation); fact != nil && fact.Normalization != "invalid" && fact.Normalization != "rejected" {
		h.HardwareUUID = fact.Value
	} else if uuid != "" {
		h.HardwareUUID = ""
	}
	d.rejectedIdentity(values, subject, kind, observation)
}

func (d *discovery) relations(fields map[string]json.RawMessage, h *model.HardwareInventory) {
	var relationFields map[string]json.RawMessage
	if raw := fields["Links"]; raw != nil && string(raw) != "null" && json.Unmarshal(raw, &relationFields) != nil {
		d.record(driver.ErrMalformed)
		return
	}
	for _, entry := range []struct{ field, typ string }{{"Chassis", "SYSTEM_IN_CHASSIS"}, {"ComputerSystems", "CHASSIS_CONTAINS_SYSTEM"}, {"ManagerForServers", "BMC_MANAGES_SYSTEM"}, {"ManagerForChassis", "BMC_MANAGES_CHASSIS"}, {"ManagedBy", "MANAGED_BY_BMC"}} {
		for _, target := range d.links(relationFields, entry.field) {
			if len(h.Relations) >= 1024 {
				d.record(driver.ErrLimit)
				break
			}
			uri, err := d.p.a.canonical(target, false)
			if err != nil {
				d.record(err)
				continue
			}
			h.Relations = append(h.Relations, model.ResourceRelationship{Type: entry.typ, TargetResourceID: uri})
		}
	}
	sort.Slice(h.Relations, func(i, j int) bool {
		if h.Relations[i].Type == h.Relations[j].Type {
			return h.Relations[i].TargetResourceID < h.Relations[j].TargetResourceID
		}
		return h.Relations[i].Type < h.Relations[j].Type
	})
}

func (d *discovery) addHealth(sample *model.TelemetrySample, id, uri, name string, st model.HardwareStatus, observation *model.ObservationMetadata) {
	for _, existing := range sample.HardwareHealth {
		if existing.ID == id {
			return
		}
	}
	sample.HardwareHealth = append(sample.HardwareHealth, model.ComponentHealth{ID: id, ResourceID: uri, Name: name, Status: st, Observation: model.CloneObservation(observation)})
}
func (d *discovery) metric(sample *model.TelemetrySample, name, unit, uri, id string, value *float64, st model.HardwareStatus, source *time.Time) {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return
	}
	uri = d.boundedURI(uri)
	if uri == "" {
		return
	}
	if d.p.metrics >= d.p.a.limits.MaxResources*8 {
		d.record(driver.ErrLimit)
		return
	}
	d.p.metrics++
	at := time.Now().UTC()
	sample.Metrics = append(sample.Metrics, model.Metric{Name: name, Unit: unit, Value: *value, ResourceID: uri, ComponentID: id, Status: &st, Observation: d.p.observation(uri, at, source)})
	sample.CollectedAt = at
}

// component follows only the explicit standard child links below, to a fixed
// depth; it never traverses arbitrary Links or Oem graphs.
func (d *discovery) component(device *model.Device, sample *model.TelemetrySample, uri, kind, parent string) {
	if d.p.ctx.Err() != nil {
		d.record(d.p.ctx.Err())
		return
	}
	body, uri, err := d.p.get(uri)
	if err != nil {
		d.record(err)
		return
	}
	if kind == "Thermal" || kind == "Power" {
		d.remember(uri, kind, device.ID, parent, false)
		d.legacy(device, sample, body, uri, kind, parent)
		return
	}
	if kind == "EthernetInterface" {
		var v schemas.EthernetInterface
		if json.Unmarshal(body, &v) != nil {
			d.record(driver.ErrMalformed)
			return
		}
		id := stableID(d.p.a.opts.ID, kind, uri)
		for _, existing := range device.Components {
			if existing.ID == id {
				return
			}
		}
		if d.p.emitted >= d.p.a.limits.MaxResources {
			d.record(driver.ErrLimit)
			return
		}
		d.p.emitted++
		at := time.Now().UTC()
		observation := d.p.observation(uri, at, nil)
		h := &model.HardwareInventory{ResourceType: kind, Status: status(v.Status)}
		b := baseResource{Name: v.Name}
		d.boundResource(&b, h)
		component := model.InventoryComponent{ID: id, ParentID: parent, Class: kind, Name: b.Name, Hardware: h, Observation: observation}
		for _, field := range []struct{ value, origin string }{{v.MACAddress, "redfish.MACAddress"}, {v.PermanentMACAddress, "redfish.PermanentMACAddress"}} {
			d.identity(&device.IdentityEvidence, model.IdentityMAC, field.value, id, "network_interface", field.origin, observation)
		}
		d.rejectedIdentity(&device.IdentityEvidence, id, kind, observation)
		addAddress := func(address, origin string) {
			if len(h.NetworkAddresses) >= 64 {
				d.record(driver.ErrLimit)
				return
			}
			fact := d.identity(&device.IdentityEvidence, model.IdentityIP, address, id, "network_interface", origin, observation)
			if fact == nil || fact.Normalization == "invalid" || fact.Normalization == "rejected" {
				return
			}
			h.NetworkAddresses = append(h.NetworkAddresses, fact.Value)
			if len(device.Hardware.NetworkAddresses) < 64 {
				// The parent aggregate remains available for legacy consumers;
				// resource-scoped addresses and evidence live on the interface.
				device.Hardware.NetworkAddresses = append(device.Hardware.NetworkAddresses, fact.Value)
			} else {
				d.record(driver.ErrLimit)
			}
		}
		for i, address := range v.IPv4Addresses {
			if i >= 64 {
				d.record(driver.ErrLimit)
				break
			}
			addAddress(address.Address, "redfish.IPv4Addresses.Address")
		}
		for i, address := range v.IPv6Addresses {
			if i >= 64 || len(h.NetworkAddresses) >= 64 {
				d.record(driver.ErrLimit)
				break
			}
			addAddress(address.Address, "redfish.IPv6Addresses.Address")
		}
		device.Components = append(device.Components, component)
		// Interface inventory is already fetched above. Do not add this URI to
		// the hardware polling plan or create another network inventory poll.
		return
	}
	b, h, err := resource(body, kind)
	if err != nil {
		d.record(err)
		return
	}
	original, originalUUID := b, h.HardwareUUID
	d.boundResource(&b, h)
	id := stableID(d.p.a.opts.ID, kind, uri)
	at := time.Now().UTC()
	observation := d.p.observation(uri, at, nil)
	for _, component := range device.Components {
		if component.ID == id {
			return
		}
	}
	component := model.InventoryComponent{ID: id, ParentID: parent, Class: kind, Name: b.Name, Manufacturer: b.Manufacturer, Model: b.Model, SerialNumber: b.SerialNumber, FirmwareVersion: b.FirmwareVersion, Hardware: h, Observation: observation}
	if d.p.emitted >= d.p.a.limits.MaxResources {
		d.record(driver.ErrLimit)
		return
	}
	d.p.emitted++
	d.resourceIdentity(&device.IdentityEvidence, original, originalUUID, kind, id, observation, h)
	device.Components = append(device.Components, component)
	d.remember(uri, kind, device.ID, parent, false)
	d.addHealth(sample, id, uri, b.Name, h.Status, observation)
	d.metrics(sample, body, uri, id, kind, h.Status)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	for _, entry := range []struct{ field, kind string }{{"Fans", "Fan"}, {"PowerSupplies", "PowerSupply"}} {
		if (kind == "ThermalSubsystem" && entry.field == "Fans") || (kind == "PowerSubsystem" && entry.field == "PowerSupplies") {
			if child := d.link(fields, entry.field); child != "" {
				members, e := d.p.members(child)
				if e != nil {
					d.record(e)
				}
				for _, member := range members {
					d.component(device, sample, member, entry.kind, id)
				}
			}
		}
	}
	if kind == "Storage" {
		for _, drive := range d.links(fields, "Drives") {
			d.component(device, sample, drive, "Drive", id)
		}
	}
	for _, entry := range []struct{ field, kind string }{{"Metrics", "PowerSupplyMetrics"}, {"EnvironmentMetrics", "EnvironmentMetrics"}, {"ThermalMetrics", "ThermalMetrics"}} {
		if kind == "PowerSupply" && entry.field == "Metrics" || kind == "ThermalSubsystem" && entry.field == "ThermalMetrics" || entry.field == "EnvironmentMetrics" && (kind == "Processor" || kind == "Memory" || kind == "Drive" || kind == "PowerSupply" || kind == "Fan") {
			if child := d.link(fields, entry.field); child != "" {
				d.component(device, sample, child, entry.kind, id)
			}
		}
	}
}

func sourceTime(value string) *time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	at = at.UTC()
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil
	}
	return &at
}
func (d *discovery) metrics(sample *model.TelemetrySample, body json.RawMessage, uri, id, kind string, st model.HardwareStatus) {
	switch kind {
	case "Sensor":
		var v schemas.Sensor
		_ = json.Unmarshal(body, &v)
		timestamp := sourceTime(v.ReadingTime)
		switch v.ReadingUnits {
		case "Cel":
			if v.ReadingType == schemas.TemperatureReadingType || v.ReadingType == "" {
				d.metric(sample, "hardware.temperature.celsius", "Cel", uri, id, v.Reading, st, timestamp)
			}
		case "RPM", "{rev}/min":
			if v.ReadingType == schemas.RotationalReadingType {
				d.metric(sample, "hardware.fan.rpm", "RPM", uri, id, v.Reading, st, timestamp)
			}
		case "%":
			if v.ReadingType == schemas.PercentReadingType {
				name := "hardware.sensor.percent"
				if v.PhysicalContext == schemas.FanPhysicalContext {
					name = "hardware.fan.percent"
				}
				d.metric(sample, name, "%", uri, id, v.Reading, st, timestamp)
			}
		case "W":
			if v.ReadingType == schemas.PowerReadingType || v.ReadingType == "" {
				d.metric(sample, "hardware.power.watts", "W", uri, id, v.Reading, st, timestamp)
			}
		}
		// SpeedRPM is a distinct standard field, not an inferred conversion.
		if v.ReadingUnits != "RPM" && v.ReadingUnits != "{rev}/min" {
			d.metric(sample, "hardware.fan.rpm", "RPM", uri, id, v.SpeedRPM, st, timestamp)
		}
	case "Fan":
		var v schemas.Fan
		_ = json.Unmarshal(body, &v)
		d.metric(sample, "hardware.fan.percent", "%", uri, id, v.SpeedPercent.Reading, st, nil)
		d.metric(sample, "hardware.fan.rpm", "RPM", uri, id, v.SpeedPercent.SpeedRPM, st, nil)
		d.metric(sample, "hardware.power.watts", "W", uri, id, v.PowerWatts.Reading, st, nil)
	case "EnvironmentMetrics":
		var v schemas.EnvironmentMetrics
		_ = json.Unmarshal(body, &v)
		d.metric(sample, "hardware.temperature.celsius", "Cel", uri, id, v.TemperatureCelsius.Reading, st, nil)
		d.metric(sample, "hardware.power.watts", "W", uri, id, v.PowerWatts.Reading, st, nil)
		d.metric(sample, "hardware.temperature.ambient.celsius", "Cel", uri+"#/AmbientTemperatureCelsius", id, v.AmbientTemperatureCelsius.Reading, st, nil)
		for i, fan := range v.FanSpeedsPercent {
			if i >= d.p.a.limits.MaxResources {
				d.record(driver.ErrLimit)
				break
			}
			ref := d.excerptURI(uri, "FanSpeedsPercent", fan.DataSourceURI, string(fan.PhysicalContext)+string(fan.PhysicalSubContext)+fan.DeviceName)
			if ref == "" {
				continue
			}
			d.metric(sample, "hardware.fan.percent", "%", ref, id, fan.Reading, st, nil)
			d.metric(sample, "hardware.fan.rpm", "RPM", ref, id, fan.SpeedRPM, st, nil)
		}
	case "PowerSupply":
		var v schemas.PowerSupply
		_ = json.Unmarshal(body, &v)
		if v.PowerInputWatts != nil {
			x := float64(*v.PowerInputWatts)
			d.metric(sample, "hardware.power.input.watts", "W", uri, id, &x, st, nil)
		}
		if v.PowerOutputWatts != nil {
			x := float64(*v.PowerOutputWatts)
			d.metric(sample, "hardware.power.output.watts", "W", uri, id, &x, st, nil)
		}
	case "PowerSupplyMetrics":
		var v schemas.PowerSupplyMetrics
		_ = json.Unmarshal(body, &v)
		d.metric(sample, "hardware.power.input.watts", "W", uri, id, v.InputPowerWatts.Reading, st, nil)
		d.metric(sample, "hardware.power.output.watts", "W", uri, id, v.OutputPowerWatts.Reading, st, nil)
		d.metric(sample, "hardware.temperature.celsius", "Cel", uri, id, v.TemperatureCelsius.Reading, st, nil)
	case "ThermalMetrics":
		var v schemas.ThermalMetrics
		_ = json.Unmarshal(body, &v)
		d.metric(sample, "hardware.power.watts", "W", uri, id, v.PowerWatts.Reading, st, nil)
		for i, temperature := range v.TemperatureReadingsCelsius {
			if i >= d.p.a.limits.MaxResources {
				d.record(driver.ErrLimit)
				break
			}
			ref := d.excerptURI(uri, "TemperatureReadingsCelsius", temperature.DataSourceURI, string(temperature.PhysicalContext)+string(temperature.PhysicalSubContext)+temperature.DeviceName)
			if ref == "" {
				continue
			}
			d.metric(sample, "hardware.temperature.celsius", "Cel", ref, id, temperature.Reading, st, nil)
		}
		for _, entry := range []struct {
			name  string
			value *float64
		}{{"Ambient", v.TemperatureSummaryCelsius.Ambient.Reading}, {"Intake", v.TemperatureSummaryCelsius.Intake.Reading}, {"Exhaust", v.TemperatureSummaryCelsius.Exhaust.Reading}, {"Internal", v.TemperatureSummaryCelsius.Internal.Reading}} {
			d.metric(sample, "hardware.temperature."+strings.ToLower(entry.name)+".celsius", "Cel", uri+"#/TemperatureSummaryCelsius/"+entry.name, id, entry.value, st, nil)
		}
	}
}

func (d *discovery) excerptURI(uri, field, source, key string) string {
	if source != "" {
		ref, err := d.p.a.canonical(source, true)
		if err != nil {
			d.record(err)
			return ""
		}
		return ref
	}
	if key == "" {
		d.record(driver.ErrMalformed)
		return ""
	}
	hash := sha256.Sum256([]byte(key))
	return d.boundedURI(uri + "#/" + field + "/" + hex.EncodeToString(hash[:8]))
}

// Embedded legacy arrays are decoded member by member: one malformed sensor
// does not discard the remaining valid readings. MemberId/name identity does
// not depend on the current array position.
func (d *discovery) legacy(device *model.Device, sample *model.TelemetrySample, body json.RawMessage, uri, kind, parent string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		d.record(driver.ErrMalformed)
		return
	}
	arrays := []string{"Temperatures", "Fans"}
	if kind == "Power" {
		arrays = []string{"PowerSupplies", "PowerControl"}
	}
	for _, key := range arrays {
		var entries []json.RawMessage
		if fields[key] == nil {
			continue
		}
		if json.Unmarshal(fields[key], &entries) != nil {
			d.record(driver.ErrMalformed)
			continue
		}
		if len(entries) > d.p.a.limits.MaxResources {
			d.record(driver.ErrLimit)
			entries = entries[:d.p.a.limits.MaxResources]
		}
		for _, entry := range entries {
			if d.p.resources >= d.p.a.limits.MaxResources {
				d.record(driver.ErrLimit)
				return
			}
			d.p.resources++
			var raw map[string]json.RawMessage
			if json.Unmarshal(entry, &raw) != nil {
				d.record(driver.ErrMalformed)
				continue
			}
			member := text(raw["MemberId"])
			if member == "" {
				member = text(raw["Name"])
			}
			resourceURI := text(raw["@odata.id"])
			if resourceURI != "" {
				var e error
				resourceURI, e = d.p.a.canonical(resourceURI, true)
				if e != nil {
					d.record(e)
					continue
				}
			} else {
				if member == "" {
					d.record(driver.ErrMalformed)
					continue
				}
				hash := sha256.Sum256([]byte(member))
				resourceURI = uri + "#/" + key + "/" + hex.EncodeToString(hash[:8])
			}
			resourceURI = d.boundedURI(resourceURI)
			if resourceURI == "" {
				continue
			}
			var base baseResource
			if json.Unmarshal(entry, &base) != nil {
				d.record(driver.ErrMalformed)
				continue
			}
			hardware := &model.HardwareInventory{ResourceType: key, Status: status(base.Status)}
			d.boundResource(&base, hardware)
			st := hardware.Status
			id := stableID(d.p.a.opts.ID, key, resourceURI)
			at := time.Now().UTC()
			observation := d.p.observation(resourceURI, at, nil)
			duplicate := false
			for _, existing := range device.Components {
				if existing.ID == id {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			var decodeErr error
			switch key {
			case "Temperatures":
				var v schemas.Temperature
				decodeErr = json.Unmarshal(entry, &v)
				if decodeErr == nil {
					d.metric(sample, "hardware.temperature.celsius", "Cel", resourceURI, id, v.ReadingCelsius, st, nil)
				}
			case "Fans":
				var v schemas.ThermalFan
				decodeErr = json.Unmarshal(entry, &v)
				if decodeErr == nil && v.Reading != nil {
					value := float64(*v.Reading)
					if v.ReadingUnits == schemas.RPMReadingUnits {
						d.metric(sample, "hardware.fan.rpm", "RPM", resourceURI, id, &value, st, nil)
					} else if v.ReadingUnits == schemas.PercentReadingUnits {
						d.metric(sample, "hardware.fan.percent", "%", resourceURI, id, &value, st, nil)
					}
				}
			case "PowerSupplies":
				var v schemas.PowerSupply
				decodeErr = json.Unmarshal(entry, &v)
				if decodeErr == nil {
					d.metrics(sample, entry, resourceURI, id, "PowerSupply", st)
					if v.PowerOutputWatts == nil && v.LastPowerOutputWatts != nil {
						value := float64(*v.LastPowerOutputWatts)
						d.metric(sample, "hardware.power.output.watts", "W", resourceURI, id, &value, st, nil)
					}
				}
			case "PowerControl":
				var v schemas.PowerControl
				decodeErr = json.Unmarshal(entry, &v)
				if decodeErr == nil && v.PowerConsumedWatts != nil {
					value := float64(*v.PowerConsumedWatts)
					d.metric(sample, "hardware.power.watts", "W", resourceURI, id, &value, st, nil)
				}
			}
			if decodeErr != nil {
				d.record(driver.ErrMalformed)
				continue
			}
			device.Components = append(device.Components, model.InventoryComponent{ID: id, ParentID: parent, Class: key, Name: base.Name, Manufacturer: base.Manufacturer, Model: base.Model, SerialNumber: base.SerialNumber, FirmwareVersion: base.FirmwareVersion, Hardware: hardware, Observation: observation})
			d.addHealth(sample, id, resourceURI, base.Name, st, observation)
		}
	}
}
