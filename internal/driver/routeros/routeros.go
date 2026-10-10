// Package routeros normalizes the fixed RouterOS HTTPS REST operations.
package routeros

import (
	"context"
	"errors"
	"math"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/routeros"
)

type Driver struct {
	opts          driver.Options
	client        *protocol.Client
	provider      credentials.Provider
	credentialRef string
	mu            sync.Mutex
	available     map[string]bool
	addressCache  []model.Interface
}

func New(opts driver.Options, provider credentials.Provider, credentialRef string, clientOptions protocol.Options) (*Driver, error) {
	if opts.ID == "" || provider == nil || credentialRef == "" {
		return nil, errors.New("RouterOS requires device ID and credential provider")
	}
	if opts.MaxRows == 0 {
		opts.MaxRows = 4096
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 1 << 20
	}
	if opts.TopologyTTLSeconds == 0 {
		opts.TopologyTTLSeconds = 900
	}
	clientOptions.Address = opts.Address
	clientOptions.MaxRecords = opts.MaxRows
	clientOptions.MaxResponseBytes = opts.MaxResponseBytes
	auth := func(ctx context.Context) (protocol.Authentication, error) {
		c, e := provider.Get(ctx, credentialRef)
		return protocol.Authentication{Username: c.Username, Password: c.Password}, e
	}
	client, err := protocol.New(clientOptions, auth)
	if err != nil {
		return nil, err
	}
	return &Driver{opts: opts, client: client, provider: provider, credentialRef: credentialRef, available: map[string]bool{}}, nil
}

func (d *Driver) Close() error { return d.client.Close() }

func (d *Driver) SnapshotIdentity(ctx context.Context) (model.Device, error) { return d.Probe(ctx) }
func (d *Driver) mark(name string, ok bool)                                  { d.mu.Lock(); d.available[name] = ok; d.mu.Unlock() }
func (d *Driver) Capabilities() []model.Capability {
	d.mu.Lock()
	defer d.mu.Unlock()
	var caps []model.Capability
	for _, name := range []string{"system.read", "inventory.read", "interfaces.read", "telemetry.read", "topology.read", "interfaces.enable", "interfaces.disable", "snapshots.create", "resource.rollback"} {
		ok := d.available[name]
		if name == "snapshots.create" || name == "resource.rollback" {
			ok = d.available["interfaces.enable"] && d.available["interfaces.disable"]
		}
		reason := ""
		if !ok {
			reason = "operation not yet observed or unavailable"
		}
		if strings.HasPrefix(name, "interfaces.") && (name == "interfaces.enable" || name == "interfaces.disable") && !d.opts.EnableMutations {
			reason = "mutations disabled for this device"
		}
		caps = append(caps, model.Capability{Name: name, Supported: ok, Reason: reason})
	}
	return caps
}
func mapError(err error) error {
	if err == nil {
		return nil
	}
	for _, pair := range [][2]error{{protocol.ErrConnectivity, driver.ErrConnectivity}, {protocol.ErrUnavailable, driver.ErrUnavailable}, {protocol.ErrMalformed, driver.ErrMalformed}, {protocol.ErrLimit, driver.ErrLimit}} {
		if errors.Is(err, pair[0]) {
			return errors.Join(pair[1], err)
		}
	}
	return err
}

// Probe is the medium poll: identity/resource and permission refresh only. Slow
// inventory is the sole place for chassis, address and interface metadata walks.
func (d *Driver) Probe(ctx context.Context) (model.Device, error) {
	identity, err := d.client.Identity(ctx)
	if err != nil {
		d.mark("system.read", false)
		return model.Device{}, mapError(err)
	}
	resource, err := d.client.Resource(ctx)
	if err != nil {
		return model.Device{}, mapError(err)
	}
	device, err := d.device(identity, resource)
	if err != nil {
		return model.Device{}, err
	}
	d.mark("system.read", true)
	if d.opts.EnableMutations {
		_ = d.checkPermissions(ctx)
	}
	device.Capabilities = d.Capabilities()
	return device, nil
}
func (d *Driver) Inventory(ctx context.Context) (model.Device, error) {
	device, err := d.Probe(ctx)
	if err != nil {
		return model.Device{}, err
	}
	d.mark("inventory.read", true)
	board, e := d.client.Routerboard(ctx)
	if e == nil {
		device.Model = first(board["model"], device.Model)
		device.SerialNumber = board["serial-number"]
		device.FirmwareVersion = board["current-firmware"]
		device.HardwareRevision = board["revision"]
	} else if !errors.Is(e, protocol.ErrUnavailable) {
		return device, mapError(e)
	}
	rows, e := d.client.Interfaces(ctx)
	if e == nil {
		var interfaces []model.Interface
		interfaces, e = mapInterfaces(rows)
		d.mark("interfaces.read", e == nil)
		if e == nil {
			addressErr := d.addAddresses(ctx, interfaces)
			ref := model.SourceRef{Type: model.SourceRouterOS, TargetID: "remote:" + d.opts.ID, Protocol: "https", Provenance: model.ProvenanceReal}
			device.IdentityEvidence = model.InterfaceIdentityEvidence(interfaces, ref, nil)
			if addressErr == nil {
				d.mu.Lock()
				d.addressCache = interfaces
				d.mu.Unlock()
			} else if !errors.Is(addressErr, driver.ErrUnavailable) {
				return device, addressErr
			}
		}
	}
	if e != nil && !errors.Is(e, protocol.ErrUnavailable) {
		return device, mapError(e)
	}
	device.Capabilities = d.Capabilities()
	return device, nil
}
func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
func boolValue(s string) (bool, bool) {
	switch s {
	case "true", "yes":
		return true, true
	case "false", "no":
		return false, true
	}
	return false, false
}
func (d *Driver) device(identity, resource protocol.Record) (model.Device, error) {
	if identity["name"] == "" || resource["version"] == "" {
		return model.Device{}, driver.ErrMalformed
	}
	uptime, available := parseUptime(resource["uptime"])
	if resource["uptime"] != "" && !available {
		return model.Device{}, driver.ErrMalformed
	}
	dev := model.Device{ID: d.opts.ID, Name: first(d.opts.Name, identity["name"]), Hostname: identity["name"], ManagementAddress: d.opts.Address, Type: "unknown", Vendor: "MikroTik", Model: resource["board-name"], OS: "RouterOS", Driver: "routeros-rest", SoftwareVersion: resource["version"], Architecture: resource["architecture-name"], UptimeSeconds: uptime, UptimeAvailable: &available, UptimeSource: "routeros-system-resource", Online: true, LastSeen: time.Now().UTC(), Location: d.opts.Location, VendorHint: d.opts.VendorHint, VendorEvidence: []model.Evidence{{Source: "routeros-rest/system/resource", Value: "RouterOS resource response", Confidence: "high"}}}
	return dev, nil
}
func (d *Driver) Collect(ctx context.Context) (model.TelemetrySample, error) {
	observed := time.Now().UTC()
	resource, err := d.client.Resource(ctx)
	if err != nil {
		return model.TelemetrySample{}, mapError(err)
	}
	rows, err := d.client.Interfaces(ctx)
	if err != nil {
		d.mark("interfaces.read", false)
		return model.TelemetrySample{}, mapError(err)
	}
	interfaces, err := mapInterfaces(rows)
	if err != nil {
		return model.TelemetrySample{}, err
	}
	d.mu.Lock()
	for i := range interfaces {
		for _, cached := range d.addressCache {
			if cached.ID == interfaces[i].ID && cached.Name == interfaces[i].Name && cached.MAC == interfaces[i].MAC {
				interfaces[i].Addresses = append([]string(nil), cached.Addresses...)
				break
			}
		}
	}
	d.mu.Unlock()
	uptime, available := parseUptime(resource["uptime"])
	if resource["uptime"] != "" && !available {
		return model.TelemetrySample{}, driver.ErrMalformed
	}
	sample := model.TelemetrySample{DeviceID: d.opts.ID, ObservedAt: observed, CollectedAt: time.Now().UTC(), Source: "routeros-rest", UptimeSeconds: uptime, UptimeAvailable: &available, UptimeSource: "routeros-system-resource", Interfaces: interfaces}
	// RouterOS does not expose Linux load/swap semantics. Only returned values
	// become metrics, so unavailable CPU/memory fields are never invented zeroes.
	for _, field := range []struct{ name, unit string }{{"cpu-load", "percent"}, {"total-memory", "bytes"}, {"free-memory", "bytes"}, {"cpu-count", "count"}} {
		if value, ok := resource[field.name]; ok {
			v, e := strconv.ParseUint(value, 10, 64)
			if e != nil || (field.name == "cpu-load" && v > 100) {
				return model.TelemetrySample{}, driver.ErrMalformed
			}
			sample.Metrics = append(sample.Metrics, model.Metric{Name: "system." + field.name, Value: float64(v), Unit: field.unit})
		}
	}
	d.mark("interfaces.read", true)
	d.mark("telemetry.read", true)
	return sample, nil
}
func mapInterfaces(rows []protocol.Record) ([]model.Interface, error) {
	interfaces := make([]model.Interface, 0, len(rows))
	seen := map[string]bool{}
	names := map[string]bool{}
	for _, row := range rows {
		id, name := row[".id"], row["name"]
		if len(name) > 128 || len(row["default-name"]) > 128 {
			return nil, driver.ErrLimit
		}
		if strings.IndexFunc(name, unicode.IsControl) >= 0 || strings.IndexFunc(row["default-name"], unicode.IsControl) >= 0 {
			return nil, driver.ErrMalformed
		}
		if !protocol.ValidRecordID(id) || name == "" || seen[id] || names[name] {
			return nil, driver.ErrMalformed
		}
		seen[id] = true
		names[name] = true
		iface := model.Interface{ID: id, Name: name, Description: row["type"], Alias: row["default-name"], OperState: "unknown", Metadata: &model.InterfaceMetadata{}, CounterMetadata: &model.CounterMetadata{}}
		if value, ok := row["mac-address"]; ok && value != "" {
			mac, e := net.ParseMAC(value)
			if e != nil {
				return nil, driver.ErrMalformed
			}
			iface.MAC = mac.String()
		}
		if value := first(row["actual-mtu"], row["mtu"]); value != "" {
			n, e := strconv.ParseUint(value, 10, 31)
			if e != nil {
				return nil, driver.ErrMalformed
			}
			iface.MTU = int(n)
			iface.Metadata.MTU = true
		}
		if value, ok := row["disabled"]; ok {
			v, valid := boolValue(value)
			if !valid {
				return nil, driver.ErrMalformed
			}
			iface.AdminUp = !v
			iface.Metadata.AdminStatus = true
		}
		if value, ok := row["running"]; ok {
			v, valid := boolValue(value)
			if !valid {
				return nil, driver.ErrMalformed
			}
			iface.OperState = "down"
			if v {
				iface.OperState = "up"
			}
		}
		c, m := &iface.Counters, iface.CounterMetadata
		fields := []struct {
			key       string
			v         *uint64
			available *bool
			width     *uint8
		}{{"rx-byte", &c.RXBytes, &m.Available.RXBytes, &m.Widths.RXBytes}, {"tx-byte", &c.TXBytes, &m.Available.TXBytes, &m.Widths.TXBytes}, {"rx-packet", &c.RXPackets, &m.Available.RXPackets, &m.Widths.RXPackets}, {"tx-packet", &c.TXPackets, &m.Available.TXPackets, &m.Widths.TXPackets}, {"rx-error", &c.RXErrors, &m.Available.RXErrors, &m.Widths.RXErrors}, {"tx-error", &c.TXErrors, &m.Available.TXErrors, &m.Widths.TXErrors}, {"rx-drop", &c.RXDropped, &m.Available.RXDropped, &m.Widths.RXDropped}, {"tx-drop", &c.TXDropped, &m.Available.TXDropped, &m.Widths.TXDropped}}
		for _, field := range fields {
			if value, ok := row[field.key]; ok {
				v, e := strconv.ParseUint(value, 10, 64)
				if e != nil {
					return nil, driver.ErrMalformed
				}
				*field.v = v
				*field.available = true
				*field.width = 64
			}
		}
		interfaces = append(interfaces, iface)
	}
	return interfaces, nil
}

// Permissions are explicit positive evidence from the authenticated local
// user's current group. RADIUS-only/hidden account metadata remains unsupported.
func (d *Driver) checkPermissions(ctx context.Context) error {
	allowed := false
	defer func() { d.mark("interfaces.enable", allowed); d.mark("interfaces.disable", allowed) }()
	if !d.opts.EnableMutations {
		return driver.ErrUnavailable
	}
	cred, err := d.provider.Get(ctx, d.credentialRef)
	if err != nil {
		return driver.ErrUnavailable
	}
	users, err := d.client.Users(ctx, cred.Username)
	if err != nil {
		return mapError(err)
	}
	if len(users) != 1 || users[0]["name"] != cred.Username || users[0]["group"] == "" {
		return driver.ErrUnavailable
	}
	if value, exists := users[0]["disabled"]; exists {
		disabled, valid := boolValue(value)
		if !valid || disabled {
			return driver.ErrUnavailable
		}
	}
	groups, err := d.client.Groups(ctx, users[0]["group"])
	if err != nil {
		return mapError(err)
	}
	if len(groups) != 1 || groups[0]["name"] != users[0]["group"] {
		return driver.ErrUnavailable
	}
	policy := map[string]bool{}
	revoked := map[string]bool{}
	for _, p := range strings.Split(groups[0]["policy"], ",") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "!") {
			policy[strings.TrimPrefix(p, "!")] = false
			revoked[strings.TrimPrefix(p, "!")] = true
		} else {
			policy[p] = true
		}
	}
	for _, required := range []string{"read", "write", "api", "rest-api"} {
		if !policy[required] || revoked[required] {
			return driver.ErrUnavailable
		}
	}
	allowed = true
	return nil
}

func (d *Driver) ResolveInterface(ctx context.Context, reference string) (driver.ResolvedInterface, error) {
	rows, err := d.client.Interfaces(ctx)
	if err != nil {
		return driver.ResolvedInterface{}, mapError(err)
	}
	interfaces, err := mapInterfaces(rows)
	if err != nil {
		return driver.ResolvedInterface{}, err
	}
	index := -1
	for i, iface := range interfaces {
		if matches(reference, iface) {
			if index >= 0 {
				return driver.ResolvedInterface{}, driver.ErrUnavailable
			}
			index = i
		}
	}
	if index < 0 {
		return driver.ResolvedInterface{}, driver.ErrUnavailable
	}
	if !interfaces[index].Metadata.AdminStatus {
		return driver.ResolvedInterface{}, driver.ErrUnavailable
	}
	if value, exists := rows[index]["dynamic"]; exists {
		dynamic, valid := boolValue(value)
		if !valid || dynamic {
			return driver.ResolvedInterface{}, driver.ErrUnavailable
		}
	}
	// Every dependency menu must be observed. Missing/partial metadata cannot
	// safely prove a protected management interface does not depend on this port.
	bridge, err := d.client.BridgePorts(ctx)
	if err != nil {
		return driver.ResolvedInterface{}, mapError(err)
	}
	bond, err := d.client.Bondings(ctx)
	if err != nil {
		return driver.ResolvedInterface{}, mapError(err)
	}
	vlan, err := d.client.VLANs(ctx)
	if err != nil {
		return driver.ResolvedInterface{}, mapError(err)
	}
	graph := map[string][]string{}
	names := map[string]model.Interface{}
	for _, iface := range interfaces {
		if _, ok := names[iface.Name]; ok {
			return driver.ResolvedInterface{}, driver.ErrMalformed
		}
		names[iface.Name] = iface
	}
	connect := func(a, b string) bool {
		_, aok := names[a]
		_, bok := names[b]
		if !aok || !bok || a == b {
			return false
		}
		graph[a] = append(graph[a], b)
		graph[b] = append(graph[b], a)
		return true
	}
	for _, r := range bridge {
		if !connect(r["interface"], r["bridge"]) {
			return driver.ResolvedInterface{}, driver.ErrUnavailable
		}
	}
	for _, r := range vlan {
		if !connect(r["name"], r["interface"]) {
			return driver.ResolvedInterface{}, driver.ErrUnavailable
		}
	}
	for _, r := range bond {
		slaves := strings.Split(r["slaves"], ",")
		if r["slaves"] == "" {
			return driver.ResolvedInterface{}, driver.ErrUnavailable
		}
		for _, s := range slaves {
			if !connect(r["name"], strings.TrimSpace(s)) {
				return driver.ResolvedInterface{}, driver.ErrUnavailable
			}
		}
	}
	seen := map[string]bool{}
	todo := []string{interfaces[index].Name}
	aliases := map[string]bool{}
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		iface := names[name]
		for _, alias := range []string{iface.ID, iface.Name, iface.Alias, iface.MAC} {
			if alias != "" {
				aliases[alias] = true
				if alias == iface.MAC {
					aliases[strings.ToUpper(alias)] = true
				}
			}
		}
		for _, n := range graph[name] {
			if !seen[n] {
				todo = append(todo, n)
			}
		}
	}
	resolved := driver.ResolvedInterface{Interface: interfaces[index]}
	for alias := range aliases {
		resolved.Aliases = append(resolved.Aliases, alias)
	}
	sort.Strings(resolved.Aliases)
	return resolved, nil
}
func matches(ref string, iface model.Interface) bool {
	return ref == iface.ID || ref == iface.Name || (iface.Alias != "" && ref == iface.Alias) || (iface.MAC != "" && strings.EqualFold(ref, iface.MAC))
}

func (d *Driver) SetInterface(ctx context.Context, resolved driver.ResolvedInterface, adminUp bool) (model.InterfaceVerification, error) {
	v := model.InterfaceVerification{InterfaceID: resolved.Interface.ID, Name: resolved.Interface.Name, ExpectedAdminUp: adminUp}
	if err := d.checkPermissions(ctx); err != nil {
		return v, err
	}
	// Permission/protected checks used a previous resolution. Re-observe its
	// complete dependency closure and reject changes before the actual write.
	latest, err := d.ResolveInterface(ctx, resolved.Interface.ID)
	if err != nil {
		return v, err
	}
	if !slices.Equal(latest.Aliases, resolved.Aliases) {
		return v, driver.ErrUnavailable
	}
	before, err := d.client.Interface(ctx, resolved.Interface.ID)
	if err != nil {
		return v, mapError(err)
	}
	interfaces, err := mapInterfaces([]protocol.Record{before})
	if err != nil {
		return v, err
	}
	current := interfaces[0]
	if current.ID != resolved.Interface.ID || current.Name != resolved.Interface.Name || current.Alias != resolved.Interface.Alias || current.Description != resolved.Interface.Description || current.MAC != resolved.Interface.MAC || current.AdminUp != resolved.Interface.AdminUp || !current.Metadata.AdminStatus {
		return v, driver.ErrUnavailable
	}
	if current.AdminUp == adminUp {
		observed := adminUp
		v.ObservedAdminUp = &observed
		v.ObservedAt = time.Now().UTC()
		v.Verified = true
		v.Evidence = "current canonical record already has requested disabled state"
		return v, nil
	}
	// Reserve time within the caller deadline for verification after a write
	// timeout. Retrying a PATCH is forbidden; only a safe GET may be repeated.
	writeCtx := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return v, context.DeadlineExceeded
		}
		writeCtx, cancel = context.WithTimeout(ctx, remaining*2/3)
	}
	writeErr := d.client.SetInterfaceDisabled(writeCtx, current.ID, !adminUp)
	cancel()
	// Even HTTP 200 may apply a write before a malformed/oversized response is
	// encountered. Every submitted attempt therefore needs a safe readback.
	after, readErr := d.client.Interface(ctx, current.ID)
	if readErr != nil {
		v.Uncertain = true
		v.Evidence = "mutation sent; canonical state could not be verified"
		return v, errors.Join(driver.ErrUncertain, mapError(readErr), mapError(writeErr))
	}
	mapped, readErr := mapInterfaces([]protocol.Record{after})
	if readErr != nil || mapped[0].ID != current.ID || mapped[0].Name != current.Name || mapped[0].Alias != current.Alias || mapped[0].Description != current.Description || mapped[0].MAC != current.MAC || !mapped[0].Metadata.AdminStatus {
		v.Uncertain = true
		v.Evidence = "mutation sent; canonical identity or state unavailable"
		return v, errors.Join(driver.ErrUncertain, readErr)
	}
	observed := mapped[0].AdminUp
	v.ObservedAdminUp = &observed
	v.ObservedAt = time.Now().UTC()
	v.Verified = observed == adminUp
	v.Changed = observed != current.AdminUp
	v.Evidence = "HTTPS GET canonical .id after PATCH: disabled state observed"
	if !v.Verified && writeErr != nil {
		var statusError *protocol.HTTPError
		if errors.As(writeErr, &statusError) && statusError.StatusCode >= 400 && statusError.StatusCode < 500 {
			v.Evidence = "PATCH rejected by API; HTTPS GET confirmed unchanged canonical disabled state"
			return v, mapError(writeErr)
		}
		v.Uncertain = true
		v.Evidence = "PATCH outcome incomplete; current canonical state differs and application could not be confirmed"
		return v, errors.Join(driver.ErrUncertain, mapError(writeErr))
	}
	if !v.Verified {
		return v, errors.New("RouterOS interface verification did not match requested state")
	}
	return v, nil
}

var durationToken = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)(w|d|h|m|s)`)

func parseUptime(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	matches := durationToken.FindAllStringSubmatch(value, -1)
	consumed := ""
	total := float64(0)
	units := map[string]float64{"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}
	for _, match := range matches {
		v, e := strconv.ParseFloat(match[1], 64)
		if e != nil {
			return 0, false
		}
		total += v * units[match[2]]
		consumed += match[0]
	}
	if consumed == value && !math.IsInf(total, 0) {
		return total, true
	}
	// Older RouterOS API represents time as weeks/days followed by hh:mm:ss.
	parts := strings.Split(value, ":")
	if len(parts) == 3 {
		hours, e := strconv.ParseUint(parts[0], 10, 32)
		minutes, e2 := strconv.ParseUint(parts[1], 10, 8)
		seconds, e3 := strconv.ParseFloat(parts[2], 64)
		if e == nil && e2 == nil && e3 == nil && minutes < 60 && seconds >= 0 && seconds < 60 {
			return float64(hours)*3600 + float64(minutes)*60 + seconds, true
		}
	}
	return 0, false
}

func (d *Driver) addAddresses(ctx context.Context, interfaces []model.Interface) error {
	rows, err := d.client.IPv4Addresses(ctx)
	if err != nil {
		return mapError(err)
	}
	v6, err := d.client.IPv6Addresses(ctx)
	if err != nil && !errors.Is(err, protocol.ErrUnavailable) {
		return mapError(err)
	}
	rows = append(rows, v6...)
	byName := map[string]*model.Interface{}
	for i := range interfaces {
		byName[interfaces[i].Name] = &interfaces[i]
	}
	for _, r := range rows {
		if disabled, _ := boolValue(r["disabled"]); disabled {
			continue
		}
		if invalid, _ := boolValue(r["invalid"]); invalid {
			continue
		}
		prefix, e := netip.ParsePrefix(r["address"])
		if e != nil {
			return driver.ErrMalformed
		}
		if iface := byName[r["interface"]]; iface != nil {
			iface.Addresses = append(iface.Addresses, prefix.String())
		}
	}
	return nil
}
