// Package snmp normalizes standard MIB observations from read-only SNMP v2c.
package snmp

import (
	"context"
	"errors"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/snmp"
	"InfraPulseAgent/internal/vendorprofiles"
)

// Standard mappings are defined by RFC 3418, RFC 2863, RFC 6933 and the
// IEEE LLDP-MIB-200505060000Z. Numeric OIDs are not vendor heuristics.
const (
	systemRoot    = ".1.3.6.1.2.1.1"
	ifRoot        = ".1.3.6.1.2.1.2.2.1"
	ifXRoot       = ".1.3.6.1.2.1.31.1.1.1"
	entityRoot    = ".1.3.6.1.2.1.47.1.1.1.1"
	localPortRoot = ".1.0.8802.1.1.2.1.3.7.1"
	remoteRoot    = ".1.0.8802.1.1.2.1.4.1.1"
	lastChangeOID = ".1.0.8802.1.1.2.1.2.1.0"
)

type Client interface {
	Get(context.Context, []string) ([]protocol.Value, error)
	BulkWalk(context.Context, string) ([]protocol.Value, error)
	Close() error
}
type Driver struct {
	opts               driver.Options
	client             Client
	mu                 sync.RWMutex
	device             model.Device
	interfaces         []model.Interface
	ifBase, ifExtended []protocol.Value
	ifCacheInitialized bool
	caps               map[string]model.Capability
}

func New(opts driver.Options, provider credentials.Provider, credentialRef string, popts protocol.Options) (*Driver, error) {
	if popts.Address == "" {
		popts.Address = opts.Address
	}
	client, err := protocol.New(popts, provider, credentialRef)
	if err != nil {
		return nil, err
	}
	return NewWithClient(opts, client), nil
}
func NewWithClient(opts driver.Options, client Client) *Driver {
	if opts.MaxRows == 0 {
		opts.MaxRows = 1024
	}
	if opts.TopologyTTLSeconds == 0 {
		opts.TopologyTTLSeconds = 900
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 4 << 20
	}
	d := &Driver{opts: opts, client: client, caps: make(map[string]model.Capability)}
	d.device = model.Device{ID: opts.ID, Name: opts.Name, ManagementAddress: opts.Address, Driver: "generic-snmp", Type: "unknown", Location: opts.Location, VendorHint: opts.VendorHint}
	for _, name := range []string{"system.read", "inventory.read", "interfaces.read", "telemetry.read", "topology.lldp"} {
		d.caps[name] = model.Capability{Name: name, Reason: "not observed"}
	}
	return d
}
func (d *Driver) capability(name string, supported bool, reason string) {
	d.mu.Lock()
	d.caps[name] = model.Capability{Name: name, Supported: supported, Reason: reason}
	d.mu.Unlock()
}
func (d *Driver) Capabilities() []model.Capability {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.capabilitiesLocked()
}
func (d *Driver) capabilitiesLocked() []model.Capability {
	var out []model.Capability
	for _, c := range d.caps {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (d *Driver) Close() error { return d.client.Close() }

func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, protocol.ErrConnectivity) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(driver.ErrConnectivity, errCategoryContext(err))
	}
	if errors.Is(err, protocol.ErrLimit) {
		return driver.ErrLimit
	}
	if errors.Is(err, protocol.ErrMalformed) {
		return driver.ErrMalformed
	}
	return driver.ErrUnavailable
}
func errCategoryContext(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context) (model.Device, error) { return d.system(ctx) }
func (d *Driver) system(ctx context.Context) (model.Device, error) {
	oids := []string{systemRoot + ".1.0", systemRoot + ".2.0", systemRoot + ".3.0", systemRoot + ".4.0", systemRoot + ".5.0", systemRoot + ".6.0"}
	values, err := d.client.Get(ctx, oids)
	if err != nil {
		return model.Device{}, translate(err)
	}
	d.mu.RLock()
	dev := d.device
	d.mu.RUnlock()
	dev.SystemDescription, dev.SysObjectID, dev.SystemLocation, dev.SystemContact, dev.Hostname = "", "", "", "", ""
	dev.UptimeSeconds = 0
	var malformed bool
	available := false
	uptimeAvailable := false
	for _, v := range values {
		if absent(v) {
			continue
		}
		switch canonical(v.OID) {
		case oids[0]:
			s, ok := textValue(v)
			if !ok {
				malformed = true
			} else {
				dev.SystemDescription = s
				available = true
			}
		case oids[1]:
			s, ok := v.Data.(string)
			if v.Type != protocol.ObjectIdentifier || !ok || !validObjectID(s) {
				malformed = true
			} else {
				dev.SysObjectID = canonical(s)
				available = true
			}
		case oids[2]:
			n, ok := unsigned(v)
			if v.Type != protocol.TimeTicks || !ok || n > math.MaxUint32 {
				malformed = true
			} else {
				dev.UptimeSeconds = float64(n) / 100
				uptimeAvailable = true
				available = true
			}
		case oids[3]:
			s, ok := textValue(v)
			if !ok {
				malformed = true
			} else {
				dev.SystemContact = s
				available = true
			}
		case oids[4]:
			s, ok := textValue(v)
			if !ok {
				malformed = true
			} else {
				dev.Hostname = s
				available = true
			}
		case oids[5]:
			s, ok := textValue(v)
			if !ok {
				malformed = true
			} else {
				dev.SystemLocation = s
				available = true
			}
		}
	}
	dev.UptimeAvailable = &uptimeAvailable
	dev.UptimeSource = "snmp.sysUpTime.management_subsystem"
	dev.Vendor, dev.VendorEvidence = detectVendor(dev.SysObjectID, dev.SystemDescription, d.opts.VendorHint)
	if !available {
		d.capability("system.read", false, "SYSTEM-MIB unavailable")
		return dev, driver.ErrUnavailable
	}
	dev.Online = true
	dev.LastSeen = time.Now().UTC()
	d.capability("system.read", true, "")
	d.capability("inventory.read", true, "")
	d.mu.Lock()
	dev.Capabilities = d.capabilitiesLocked()
	d.device = dev
	d.mu.Unlock()
	if malformed {
		return dev, driver.ErrMalformed
	}
	return dev, nil
}

func (d *Driver) Inventory(ctx context.Context) (model.Device, error) {
	dev, err := d.system(ctx)
	if err != nil {
		return dev, err
	}
	dev.Components = nil
	dev.Model, dev.SerialNumber, dev.HardwareRevision, dev.FirmwareVersion, dev.SoftwareVersion = "", "", "", "", ""
	values, e := d.client.BulkWalk(ctx, entityRoot)
	if e == nil {
		components, parseErr := d.entities(values)
		dev.Components = components
		selectChassis(&dev)
		if parseErr != nil {
			err = parseErr
		}
	}
	// Missing ENTITY-MIB is optional. Connectivity, cancellation and resource
	// failures still remain visible instead of silently completing inventory.
	if e != nil && !errors.Is(e, protocol.ErrUnavailable) {
		err = translate(e)
	}
	// Interface metadata and discovery use the slow inventory interval. Fast
	// observations below query bounded batches for these discovered indexes.
	_, interfaceErr := d.collectInterfaces(ctx)
	if interfaceErr != nil && !errors.Is(interfaceErr, driver.ErrUnavailable) {
		err = errors.Join(err, interfaceErr)
	}
	d.mu.Lock()
	dev.Capabilities = d.capabilitiesLocked()
	d.device = dev
	d.mu.Unlock()
	return dev, err
}

func (d *Driver) Collect(ctx context.Context) (model.TelemetrySample, error) {
	now := time.Now()
	sample := model.TelemetrySample{DeviceID: d.opts.ID, ObservedAt: now, Source: "snmp", UptimeSource: "snmp.sysUpTime.management_subsystem"}
	values, e := d.client.Get(ctx, []string{systemRoot + ".3.0"})
	uptimeAvailable := false
	var err error
	if e != nil {
		return sample, translate(e)
	}
	for _, v := range values {
		if canonical(v.OID) != systemRoot+".3.0" || absent(v) {
			continue
		}
		n, ok := unsigned(v)
		if !ok || v.Type != protocol.TimeTicks || n > math.MaxUint32 {
			err = driver.ErrMalformed
		} else {
			sample.UptimeSeconds = float64(n) / 100
			uptimeAvailable = true
		}
	}
	sample.UptimeAvailable = &uptimeAvailable
	interfaces, e := d.fastInterfaces(ctx)
	sample.Interfaces = interfaces
	sample.CollectedAt = time.Now().UTC()
	if e != nil {
		err = errors.Join(err, e)
	}
	if len(interfaces) > 0 && e == nil {
		d.capability("interfaces.read", true, "")
		d.capability("telemetry.read", true, "")
	}
	d.mu.Lock()
	d.interfaces = interfaces
	d.mu.Unlock()
	return sample, err
}
func (d *Driver) collectInterfaces(ctx context.Context) ([]model.Interface, error) {
	a, e := d.client.BulkWalk(ctx, ifRoot)
	if e != nil {
		return nil, translate(e)
	}
	b, e := d.client.BulkWalk(ctx, ifXRoot)
	if e != nil && !errors.Is(e, protocol.ErrUnavailable) {
		return nil, translate(e)
	}
	interfaces, err := d.mapInterfaces(a, b)
	if err == nil {
		d.mu.Lock()
		d.ifBase = a
		d.ifExtended = b
		d.ifCacheInitialized = true
		d.interfaces = interfaces
		d.mu.Unlock()
		if len(interfaces) > 0 {
			d.capability("interfaces.read", true, "")
			d.capability("telemetry.read", true, "")
		}
	}
	return interfaces, err
}

func (d *Driver) fastInterfaces(ctx context.Context) ([]model.Interface, error) {
	d.mu.RLock()
	initialized := d.ifCacheInitialized
	base := append([]protocol.Value(nil), d.ifBase...)
	extended := append([]protocol.Value(nil), d.ifExtended...)
	d.mu.RUnlock()
	if !initialized {
		return d.collectInterfaces(ctx)
	}
	indexes := make(map[uint32]bool)
	for _, v := range base {
		_, index, ok := tableCell(v, ifRoot)
		if ok && !absent(v) {
			indexes[index] = true
		}
	}
	ordered := make([]uint32, 0, len(indexes))
	for index := range indexes {
		ordered = append(ordered, index)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	// Retain slow metadata, replacing every requested dynamic field, including
	// NoSuchInstance, so a missing counter never keeps a stale measurement.
	cells := make(map[string]protocol.Value, len(base)+len(extended))
	for _, v := range base {
		cells[canonical(v.OID)] = v
	}
	for _, v := range extended {
		cells[canonical(v.OID)] = v
	}
	var batch []string
	alive := make(map[uint32]bool)
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		values, err := d.client.Get(ctx, batch)
		if err != nil {
			return translate(err)
		}
		if len(values) != len(batch) {
			return driver.ErrMalformed
		}
		for _, v := range values {
			key := canonical(v.OID)
			cells[key] = v
			size += len(key) + 32
			switch x := v.Data.(type) {
			case []byte:
				size += len(x)
			case string:
				size += len(x)
			}
			if size > d.opts.MaxResponseBytes {
				return driver.ErrLimit
			}
			column, index, ok := tableCell(v, ifRoot)
			if ok && column == 1 && !absent(v) {
				n, valid := unsigned(v)
				if !valid || v.Type != protocol.Integer || n != uint64(index) {
					return driver.ErrMalformed
				}
				alive[index] = true
			}
		}
		batch = nil
		return nil
	}
	appendOID := func(oid string) error {
		batch = append(batch, oid)
		if len(batch) == 60 {
			return flush()
		}
		return nil
	}
	for _, index := range ordered {
		suffix := "." + strconv.FormatUint(uint64(index), 10)
		for _, column := range []uint32{1, 2, 6, 7, 8, 10, 11, 12, 13, 14, 16, 17, 18, 19, 20} {
			if err := appendOID(ifRoot + "." + strconv.FormatUint(uint64(column), 10) + suffix); err != nil {
				return nil, err
			}
		}
		for _, column := range []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 19} {
			if err := appendOID(ifXRoot + "." + strconv.FormatUint(uint64(column), 10) + suffix); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	base = nil
	extended = nil
	for _, v := range cells {
		if _, index, ok := tableCell(v, ifRoot); ok && alive[index] {
			base = append(base, v)
		} else if _, index, ok := tableCell(v, ifXRoot); ok && alive[index] {
			extended = append(extended, v)
		}
	}
	interfaces, err := d.mapInterfaces(base, extended)
	if err == nil {
		d.mu.Lock()
		d.ifBase = base
		d.ifExtended = extended
		d.interfaces = interfaces
		d.mu.Unlock()
	}
	return interfaces, err
}

func canonical(oid string) string { return "." + strings.Trim(oid, ".") }
func validObjectID(s string) bool {
	parts := strings.Split(strings.Trim(s, "."), ".")
	if len(parts) < 2 || len(parts) > 128 {
		return false
	}
	for _, p := range parts {
		if _, e := strconv.ParseUint(p, 10, 32); e != nil {
			return false
		}
	}
	return true
}
func absent(v protocol.Value) bool {
	return v.Type == protocol.NoSuchObject || v.Type == protocol.NoSuchInstance || v.Type == protocol.EndOfMibView
}
func textValue(v protocol.Value) (string, bool) {
	if v.Type != protocol.OctetString {
		return "", false
	}
	switch x := v.Data.(type) {
	case string:
		if len(x) > 255 {
			return "", false
		}
		return x, true
	case []byte:
		if len(x) > 255 {
			return "", false
		}
		return string(x), true
	}
	return "", false
}
func bytesValue(v protocol.Value) ([]byte, bool) {
	if v.Type != protocol.OctetString {
		return nil, false
	}
	switch x := v.Data.(type) {
	case []byte:
		if len(x) > 4096 {
			return nil, false
		}
		return append([]byte(nil), x...), true
	case string:
		if len(x) > 4096 {
			return nil, false
		}
		return []byte(x), true
	}
	return nil, false
}
func unsigned(v protocol.Value) (uint64, bool) {
	switch v.Type {
	case protocol.Integer, protocol.Counter32, protocol.Counter64, protocol.Gauge32, protocol.TimeTicks:
	default:
		return 0, false
	}
	switch n := v.Data.(type) {
	case uint:
		return uint64(n), true
	case uint8:
		return uint64(n), true
	case uint16:
		return uint64(n), true
	case uint32:
		return uint64(n), true
	case uint64:
		return n, true
	case int:
		if n >= 0 {
			return uint64(n), true
		}
	case int32:
		if n >= 0 {
			return uint64(n), true
		}
	case int64:
		if n >= 0 {
			return uint64(n), true
		}
	}
	return 0, false
}
func tableCell(v protocol.Value, root string) (column, index uint32, ok bool) {
	suffix, found := strings.CutPrefix(canonical(v.OID), root+".")
	if !found {
		return
	}
	p := strings.Split(suffix, ".")
	if len(p) != 2 {
		return
	}
	c, e := strconv.ParseUint(p[0], 10, 32)
	if e != nil {
		return
	}
	i, e := strconv.ParseUint(p[1], 10, 32)
	if e != nil || i == 0 || i > math.MaxInt32 {
		return
	}
	return uint32(c), uint32(i), true
}

func (d *Driver) mapInterfaces(base, extended []protocol.Value) ([]model.Interface, error) {
	rows := make(map[uint32]map[uint32]protocol.Value)
	xrows := make(map[uint32]map[uint32]protocol.Value)
	malformed := false
	limited := false
	add := func(values []protocol.Value, root string, target map[uint32]map[uint32]protocol.Value) {
		for _, v := range values {
			if absent(v) {
				continue
			}
			c, i, ok := tableCell(v, root)
			if !ok {
				malformed = true
				continue
			}
			if target[i] == nil {
				if len(target) >= d.opts.MaxRows {
					limited = true
					continue
				}
				target[i] = make(map[uint32]protocol.Value)
			}
			target[i][c] = v
		}
	}
	add(base, ifRoot, rows)
	add(extended, ifXRoot, xrows)
	indexes := make([]uint32, 0, len(rows))
	for i := range rows {
		indexes = append(indexes, i)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	var interfaces []model.Interface
	for _, i := range indexes {
		r, x := rows[i], xrows[i]
		iface := model.Interface{ID: "ifindex:" + strconv.FormatUint(uint64(i), 10), Index: int(i), OperState: "unknown", Metadata: &model.InterfaceMetadata{Index: true}, CounterMetadata: &model.CounterMetadata{}}
		text := func(row map[uint32]protocol.Value, c uint32) string {
			v, ok := row[c]
			if !ok {
				return ""
			}
			s, ok := textValue(v)
			if c == 18 && len(s) > 64 {
				ok = false
				s = ""
			}
			if !ok {
				malformed = true
			}
			return s
		}
		number := func(row map[uint32]protocol.Value, c uint32, expected protocol.Type) (uint64, bool) {
			v, ok := row[c]
			if !ok {
				return 0, false
			}
			n, ok := unsigned(v)
			if !ok || v.Type != expected || n > math.MaxUint32 {
				malformed = true
				return 0, false
			}
			return n, ok
		}
		iface.Description = text(r, 2)
		iface.Name = text(x, 1)
		iface.Alias = text(x, 18)
		if n, ok := number(r, 1, protocol.Integer); ok && n != uint64(i) {
			malformed = true
			continue
		}
		if n, ok := number(r, 3, protocol.Integer); ok && n <= math.MaxInt32 {
			t := uint32(n)
			iface.Type = &t
		}
		if n, ok := number(r, 4, protocol.Integer); ok {
			if n <= math.MaxInt32 {
				iface.MTU = int(n)
				iface.Metadata.MTU = true
			} else {
				malformed = true
			}
		}
		if v, ok := r[6]; ok {
			mac, ok := bytesValue(v)
			if !ok || len(mac) != 0 && len(mac) != 6 && len(mac) != 8 {
				malformed = true
			} else if len(mac) > 0 {
				iface.MAC = net.HardwareAddr(mac).String()
			}
		}
		if n, ok := number(r, 7, protocol.Integer); ok {
			if n >= 1 && n <= 3 {
				iface.AdminUp = n == 1
				iface.Metadata.AdminStatus = true
			} else {
				malformed = true
			}
		}
		if n, ok := number(r, 8, protocol.Integer); ok {
			states := map[uint64]string{1: "up", 2: "down", 3: "testing", 4: "unknown", 5: "dormant", 6: "notPresent", 7: "lowerLayerDown"}
			if s, found := states[n]; found {
				iface.OperState = s
			} else {
				malformed = true
			}
		}
		if n, ok := number(x, 15, protocol.Gauge32); ok && n > 0 && n <= math.MaxUint32 {
			n *= 1000000
			iface.SpeedBitsPerSecond = &n
		} else if n, ok := number(r, 5, protocol.Gauge32); ok && n > 0 && n < math.MaxUint32 {
			iface.SpeedBitsPerSecond = &n
		}
		if n, ok := number(x, 19, protocol.TimeTicks); ok && n <= math.MaxUint32 {
			ticks := uint32(n)
			iface.CounterMetadata.DiscontinuityTicks = &ticks
		}
		counter := func(row map[uint32]protocol.Value, c uint32, bits uint8) (uint64, bool) {
			v, ok := row[c]
			if !ok {
				return 0, false
			}
			if bits == 64 && v.Type != protocol.Counter64 || bits == 32 && v.Type != protocol.Counter32 {
				malformed = true
				return 0, false
			}
			n, ok := unsigned(v)
			if !ok || bits == 32 && n > math.MaxUint32 {
				malformed = true
				return 0, false
			}
			return n, true
		}
		choose := func(hc, legacy uint32) (uint64, bool, uint8) {
			if n, ok := counter(x, hc, 64); ok {
				return n, true, 64
			}
			n, ok := counter(r, legacy, 32)
			return n, ok, 32
		}
		iface.Counters.RXBytes, iface.CounterMetadata.Available.RXBytes, iface.CounterMetadata.Widths.RXBytes = choose(6, 10)
		iface.Counters.TXBytes, iface.CounterMetadata.Available.TXBytes, iface.CounterMetadata.Widths.TXBytes = choose(10, 16)
		packets := func(hcs []uint32, legacy []uint32, extended32 []uint32) (uint64, bool, uint8) {
			sum := uint64(0)
			all := true
			for _, c := range hcs {
				n, ok := counter(x, c, 64)
				if !ok || sum > math.MaxUint64-n {
					all = false
					break
				}
				sum += n
			}
			if all {
				return sum, true, 64
			}
			sum = 0
			for _, c := range legacy {
				n, ok := counter(r, c, 32)
				if !ok {
					// Some agents omit deprecated ifIn/OutNUcastPkts but
					// expose separate 32-bit multicast/broadcast counters.
					u, valid := counter(r, legacy[0], 32)
					if !valid {
						return 0, false, 0
					}
					for _, xc := range extended32 {
						count, valid := counter(x, xc, 32)
						if !valid {
							return 0, false, 0
						}
						u += count
					}
					return u, true, 0
				}
				sum += n
			}
			return sum, true, 0
		}
		iface.Counters.RXPackets, iface.CounterMetadata.Available.RXPackets, iface.CounterMetadata.Widths.RXPackets = packets([]uint32{7, 8, 9}, []uint32{11, 12}, []uint32{2, 3})
		iface.Counters.TXPackets, iface.CounterMetadata.Available.TXPackets, iface.CounterMetadata.Widths.TXPackets = packets([]uint32{11, 12, 13}, []uint32{17, 18}, []uint32{4, 5})
		iface.Counters.RXErrors, iface.CounterMetadata.Available.RXErrors = counter(r, 14, 32)
		iface.CounterMetadata.Widths.RXErrors = 32
		iface.Counters.TXErrors, iface.CounterMetadata.Available.TXErrors = counter(r, 20, 32)
		iface.CounterMetadata.Widths.TXErrors = 32
		iface.Counters.RXDropped, iface.CounterMetadata.Available.RXDropped = counter(r, 13, 32)
		iface.CounterMetadata.Widths.RXDropped = 32
		iface.Counters.TXDropped, iface.CounterMetadata.Available.TXDropped = counter(r, 19, 32)
		iface.CounterMetadata.Widths.TXDropped = 32
		interfaces = append(interfaces, iface)
	}
	if limited {
		return interfaces, driver.ErrLimit
	}
	if malformed {
		return interfaces, driver.ErrMalformed
	}
	return interfaces, nil
}

func (d *Driver) entities(values []protocol.Value) ([]model.InventoryComponent, error) {
	rows := make(map[uint32]map[uint32]protocol.Value)
	var err error
	for _, v := range values {
		if absent(v) {
			continue
		}
		c, i, ok := tableCell(v, entityRoot)
		if !ok {
			err = driver.ErrMalformed
			continue
		}
		if rows[i] == nil {
			if len(rows) >= d.opts.MaxRows {
				return nil, driver.ErrLimit
			}
			rows[i] = make(map[uint32]protocol.Value)
		}
		rows[i][c] = v
	}
	classes := map[uint64]string{1: "other", 2: "unknown", 3: "chassis", 4: "backplane", 5: "container", 6: "powerSupply", 7: "fan", 8: "sensor", 9: "module", 10: "port", 11: "stack", 12: "cpu"}
	var components []model.InventoryComponent
	for i, r := range rows {
		component := model.InventoryComponent{ID: "entity:" + strconv.FormatUint(uint64(i), 10), Class: "unknown"}
		if v, ok := r[4]; ok {
			n, valid := unsigned(v)
			if !valid || v.Type != protocol.Integer || n > math.MaxInt32 {
				err = driver.ErrMalformed
			} else if n != 0 {
				component.ParentID = "entity:" + strconv.FormatUint(n, 10)
			}
		}
		if v, ok := r[5]; ok {
			n, valid := unsigned(v)
			if !valid || v.Type != protocol.Integer {
				err = driver.ErrMalformed
			} else if class, found := classes[n]; found {
				component.Class = class
			}
		}
		fields := map[uint32]*string{2: &component.Description, 7: &component.Name, 8: &component.HardwareRevision, 9: &component.FirmwareVersion, 10: &component.SoftwareVersion, 11: &component.SerialNumber, 12: &component.Manufacturer, 13: &component.Model}
		for c, p := range fields {
			if v, ok := r[c]; ok {
				s, ok := textValue(v)
				if !ok {
					err = driver.ErrMalformed
				} else {
					*p = s
				}
			}
		}
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].ID < components[j].ID })
	return components, err
}
func selectChassis(dev *model.Device) {
	var chassis *model.InventoryComponent
	for i := range dev.Components {
		c := &dev.Components[i]
		if c.Class == "chassis" {
			if chassis != nil {
				return
			}
			chassis = c
		}
	}
	if chassis != nil {
		dev.Model = chassis.Model
		dev.SerialNumber = chassis.SerialNumber
		dev.HardwareRevision = chassis.HardwareRevision
		dev.FirmwareVersion = chassis.FirmwareVersion
		dev.SoftwareVersion = chassis.SoftwareVersion
	}
}

func detectVendor(objectID, description, hint string) (string, []model.Evidence) {
	return vendorprofiles.Detect(objectID, description, hint)
}

var _ driver.Driver = (*Driver)(nil)
