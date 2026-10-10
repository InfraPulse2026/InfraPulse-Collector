package snmp

import (
	"context"
	"errors"
	"math"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/snmp"
)

// IEEE LLDP-MIB lldpRemManAddrEntry; optional data on the existing slow job.
const remoteManagementRoot = ".1.0.8802.1.1.2.1.4.2.1"

type localPort struct {
	number, subtype uint32
	id              []byte
	description     string
}

func (d *Driver) Topology(ctx context.Context) (model.TopologyUpdate, error) {
	now := time.Now().UTC()
	update := model.TopologyUpdate{DeviceID: d.opts.ID, ObservedAt: now, Source: "snmp.lldp", Completeness: "failed", TTLSeconds: d.opts.TopologyTTLSeconds, ExpiryAssumption: "configured collector TTL; LLDP-MIB does not expose received neighbor TTL"}
	before, beforeOK, beforeErr := d.lastChange(ctx)
	local, localErr := d.client.BulkWalk(ctx, localPortRoot)
	remote, remoteErr := d.client.BulkWalk(ctx, remoteRoot)
	management, managementErr := d.client.BulkWalk(ctx, remoteManagementRoot)
	after, afterOK, afterErr := d.lastChange(ctx)
	ports, parseLocalErr := d.localPorts(local)
	d.mu.RLock()
	interfaces := append([]model.Interface(nil), d.interfaces...)
	d.mu.RUnlock()
	if len(interfaces) == 0 && ctx.Err() == nil {
		if found, e := d.collectInterfaces(ctx); e == nil {
			interfaces = found
			d.mu.Lock()
			d.interfaces = found
			d.mu.Unlock()
		}
	}
	addresses, parseManagementErr := d.managementAddresses(management)
	observations, parseRemoteErr := d.neighbors(remote, ports, interfaces, addresses, now)
	update.Observations = observations
	update.CollectedAt = time.Now().UTC()
	var err error
	for _, e := range []error{localErr, remoteErr} {
		if e != nil {
			err = errors.Join(err, translate(e))
		}
	}
	for _, e := range []error{beforeErr, afterErr} {
		if e != nil && !errors.Is(e, protocol.ErrUnavailable) {
			err = errors.Join(err, translate(e))
		}
	}
	// Lack of the optional table means unknown management addresses, not a
	// confirmed empty address set. Transport/parsing failures make this partial.
	if managementErr != nil && !errors.Is(managementErr, protocol.ErrUnavailable) {
		err = errors.Join(err, translate(managementErr))
	}
	err = errors.Join(err, parseLocalErr, parseRemoteErr, parseManagementErr)
	supportedEvidence := len(ports) > 0 || beforeOK && afterOK
	if len(observations) > 0 || supportedEvidence {
		update.Completeness = "partial"
	}
	if localErr == nil && remoteErr == nil && parseLocalErr == nil && parseRemoteErr == nil && supportedEvidence && err == nil {
		if beforeOK && afterOK && before != after {
			err = driver.ErrUnavailable
		} else {
			update.Completeness = "complete"
		}
	}
	if !supportedEvidence && len(observations) == 0 && err == nil {
		err = driver.ErrUnavailable
	}
	if update.Completeness == "complete" {
		d.capability("topology.lldp", true, "")
	} else if errors.Is(remoteErr, protocol.ErrUnavailable) {
		d.capability("topology.lldp", false, "LLDP-MIB unavailable")
	}
	return update, err
}
func (d *Driver) lastChange(ctx context.Context) (uint64, bool, error) {
	values, err := d.client.Get(ctx, []string{lastChangeOID})
	if err != nil {
		return 0, false, err
	}
	for _, v := range values {
		if canonical(v.OID) == lastChangeOID && !absent(v) {
			n, ok := unsigned(v)
			if !ok || v.Type != protocol.TimeTicks || n > math.MaxUint32 {
				return 0, false, protocol.ErrMalformed
			}
			return n, true, nil
		}
	}
	return 0, false, protocol.ErrUnavailable
}

func (d *Driver) localPorts(values []protocol.Value) (map[uint32]localPort, error) {
	rows := make(map[uint32]map[uint32]protocol.Value)
	var err error
	for _, v := range values {
		if absent(v) {
			continue
		}
		c, i, ok := tableCell(v, localPortRoot)
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
	ports := make(map[uint32]localPort, len(rows))
	for i, r := range rows {
		p := localPort{number: i}
		if v, ok := r[2]; ok {
			n, valid := unsigned(v)
			if !valid || v.Type != protocol.Integer || n < 1 || n > 7 {
				err = driver.ErrMalformed
			} else {
				p.subtype = uint32(n)
			}
		} else {
			err = driver.ErrMalformed
		}
		if v, ok := r[3]; ok {
			id, valid := bytesValue(v)
			if !valid || len(id) == 0 || len(id) > 255 {
				err = driver.ErrMalformed
			} else {
				p.id = id
			}
		} else {
			err = driver.ErrMalformed
		}
		if v, ok := r[4]; ok {
			s, valid := textValue(v)
			if !valid || !safeTopologyText(s, 255) {
				err = driver.ErrMalformed
			} else {
				p.description = s
			}
		}
		ports[i] = p
	}
	return ports, err
}

func (d *Driver) neighbors(values []protocol.Value, ports map[uint32]localPort, interfaces []model.Interface, addresses map[string][]model.ManagementAddress, now time.Time) ([]model.Neighbor, error) {
	type row struct {
		timeMark, local, index uint32
		fields                 map[uint32]protocol.Value
	}
	rows := make(map[string]*row)
	var err error
	for _, v := range values {
		if absent(v) {
			continue
		}
		suffix, ok := strings.CutPrefix(canonical(v.OID), remoteRoot+".")
		p := strings.Split(suffix, ".")
		if !ok || len(p) != 4 {
			err = driver.ErrMalformed
			continue
		}
		var n [4]uint32
		valid := true
		for i, s := range p {
			u, e := strconv.ParseUint(s, 10, 32)
			if e != nil {
				valid = false
				break
			}
			n[i] = uint32(u)
		}
		if !valid || n[2] == 0 || n[3] == 0 {
			err = driver.ErrMalformed
			continue
		}
		if n[0] < 4 || n[0] > 12 {
			continue
		}
		key := strings.Join(p[1:], ".")
		if rows[key] == nil {
			if len(rows) >= d.opts.MaxRows {
				return nil, driver.ErrLimit
			}
			rows[key] = &row{timeMark: n[1], local: n[2], index: n[3], fields: make(map[uint32]protocol.Value)}
		}
		rows[key].fields[n[0]] = v
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []model.Neighbor
	for _, key := range keys {
		r := rows[key]
		p := ports[r.local]
		neighbor := model.Neighbor{LocalDeviceID: d.opts.ID, LocalPortNumber: r.local, LocalPortSubtype: p.subtype, LocalPortID: append([]byte(nil), p.id...), Protocol: "lldp", Source: "snmp.lldp", ObservedAt: now, ExpiresAt: now.Add(time.Duration(d.opts.TopologyTTLSeconds) * time.Second), Confidence: "unresolved", RemoteManagementAddresses: addresses[key], Evidence: []model.Evidence{{Source: "snmp.lldpRemTable.index", Value: key, Confidence: "high"}}}
		number := func(c uint32) uint32 {
			v, ok := r.fields[c]
			if !ok {
				err = driver.ErrMalformed
				return 0
			}
			n, ok := unsigned(v)
			if !ok || v.Type != protocol.Integer || n < 1 || n > 7 {
				err = driver.ErrMalformed
				return 0
			}
			return uint32(n)
		}
		identifier := func(c uint32) []byte {
			v, ok := r.fields[c]
			if !ok {
				err = driver.ErrMalformed
				return nil
			}
			id, ok := bytesValue(v)
			if !ok || len(id) == 0 || len(id) > 255 {
				err = driver.ErrMalformed
				return nil
			}
			return id
		}
		neighbor.RemoteChassisSubtype = number(4)
		neighbor.RemoteChassisID = identifier(5)
		neighbor.RemotePortSubtype = number(6)
		neighbor.RemotePortID = identifier(7)
		for c, target := range map[uint32]*string{8: &neighbor.RemotePortDescription, 9: &neighbor.RemoteSystemName, 10: &neighbor.RemoteSystemDescription} {
			if v, ok := r.fields[c]; ok {
				s, valid := textValue(v)
				if !valid || !safeTopologyText(s, 255) {
					err = driver.ErrMalformed
				} else {
					*target = s
				}
			}
		}
		for c, target := range map[uint32]*[]string{11: &neighbor.RemoteCapabilitiesSupported, 12: &neighbor.RemoteCapabilitiesEnabled} {
			if v, ok := r.fields[c]; ok {
				caps, e := lldpCapabilities(v)
				if e != nil {
					err = errors.Join(err, e)
				} else {
					*target = caps
				}
			}
		}
		if iface, evidence, confidence, ok := resolveLocalPort(p, interfaces); ok {
			if !safeTopologyText(iface.ID, 256) || !safeTopologyText(iface.Name, 255) {
				err = errors.Join(err, driver.ErrMalformed)
				neighbor.Evidence = append(neighbor.Evidence, model.Evidence{Source: "local_port.mapping", Value: "interface identity rejected by text validation", Confidence: "unresolved"})
				out = append(out, neighbor)
				continue
			}
			neighbor.LocalInterfaceID = iface.ID
			neighbor.LocalInterfaceName = iface.Name
			neighbor.Evidence = append(neighbor.Evidence, evidence)
			neighbor.Confidence = confidence
			if iface.Metadata == nil || iface.Metadata.Index {
				if iface.Index > 0 && uint64(iface.Index) <= math.MaxUint32 {
					index := uint32(iface.Index)
					neighbor.LocalInterfaceIndex = &index
				}
			}
			if mac, e := model.NormalizeMAC(iface.MAC); e == nil {
				neighbor.LocalInterfaceMAC = mac
			}
			if safeTopologyText(iface.Description, 4096) {
				neighbor.LocalInterfaceDescription = iface.Description
			} else {
				err = errors.Join(err, driver.ErrMalformed)
			}
			if safeTopologyText(iface.Alias, 255) {
				neighbor.LocalInterfaceAlias = iface.Alias
			} else {
				err = errors.Join(err, driver.ErrMalformed)
			}
		} else {
			neighbor.Evidence = append(neighbor.Evidence, model.Evidence{Source: "local_port.mapping", Value: "no unique interface evidence", Confidence: "unresolved"})
		}
		out = append(out, neighbor)
	}
	return out, err
}

func safeTopologyText(value string, max int) bool {
	if len(value) > max || !utf8.ValidString(value) {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func lldpCapabilities(value protocol.Value) ([]string, error) {
	raw, valid := bytesValue(value)
	if !valid {
		return nil, driver.ErrMalformed
	}
	if len(raw) > 2 {
		return nil, driver.ErrLimit
	}
	names := []string{"other", "repeater", "bridge", "wlan_access_point", "router", "telephone", "docsis_cable_device", "station_only"}
	capabilities := []string{}
	for octet, value := range raw {
		for bit := 0; bit < 8; bit++ {
			if value&(0x80>>bit) == 0 {
				continue
			}
			position := octet*8 + bit
			name := "bit_" + strconv.Itoa(position)
			if position < len(names) {
				name = names[position]
			}
			capabilities = append(capabilities, name)
		}
	}
	sort.Strings(capabilities)
	return capabilities, nil
}

// managementAddresses joins rows by the full LLDP remote index, preserving
// multiple IPv4/IPv6/unknown-family advertisements without network requests.
func (d *Driver) managementAddresses(values []protocol.Value) (map[string][]model.ManagementAddress, error) {
	rows := make(map[string]model.ManagementAddress)
	owners := make(map[string]string)
	counts := make(map[string]int)
	var err error
	for _, value := range values {
		if absent(value) {
			continue
		}
		suffix, ok := strings.CutPrefix(canonical(value.OID), remoteManagementRoot+".")
		parts := strings.Split(suffix, ".")
		if !ok || len(parts) < 7 || len(parts) > 37 {
			err = errors.Join(err, driver.ErrMalformed)
			continue
		}
		numbers := make([]uint32, len(parts))
		valid := true
		for i, part := range parts {
			parsed, e := strconv.ParseUint(part, 10, 32)
			if e != nil {
				valid = false
				break
			}
			numbers[i] = uint32(parsed)
		}
		if !valid || numbers[2] == 0 || numbers[3] == 0 || numbers[4] == 0 || numbers[5] == 0 || numbers[5] > 31 || int(numbers[5]) != len(parts)-6 {
			err = errors.Join(err, driver.ErrMalformed)
			continue
		}
		if numbers[0] < 3 || numbers[0] > 5 {
			continue
		}
		owner := strings.Join(parts[1:4], ".")
		key := strings.Join(parts[1:], ".")
		address, exists := rows[key]
		if !exists {
			if len(rows) >= d.opts.MaxRows || counts[owner] >= 16 {
				err = errors.Join(err, driver.ErrLimit)
				continue
			}
			raw := make([]byte, len(parts)-6)
			for i, number := range numbers[6:] {
				if number > 255 {
					valid = false
					break
				}
				raw[i] = byte(number)
			}
			if !valid {
				err = errors.Join(err, driver.ErrMalformed)
				continue
			}
			address = model.ManagementAddress{AddressFamily: numbers[4], RawAddress: raw}
			if address.AddressFamily == 1 || address.AddressFamily == 2 {
				if ip, valid := netip.AddrFromSlice(raw); valid {
					address.Address = ip.String()
				}
			}
			if address.Validate() != nil {
				err = errors.Join(err, driver.ErrMalformed)
				continue
			}
			counts[owner]++
			owners[key] = owner
		}
		if numbers[0] == 3 || numbers[0] == 4 {
			number, valid := unsigned(value)
			if !valid || value.Type != protocol.Integer || number > math.MaxUint32 || numbers[0] == 3 && (number < 1 || number > 3) {
				err = errors.Join(err, driver.ErrMalformed)
			} else {
				converted := uint32(number)
				if numbers[0] == 3 {
					address.InterfaceSubtype = &converted
				} else {
					address.InterfaceID = &converted
				}
			}
		}
		rows[key] = address
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string][]model.ManagementAddress)
	for _, key := range keys {
		owner := owners[key]
		result[owner] = append(result[owner], rows[key])
	}
	return result, err
}

func resolveLocalPort(p localPort, interfaces []model.Interface) (model.Interface, model.Evidence, string, bool) {
	type candidate struct {
		iface              model.Interface
		source, confidence string
	}
	var matches []candidate
	id := string(p.id)
	for _, iface := range interfaces {
		source, confidence := "", "high"
		switch p.subtype {
		case 5:
			if id != "" && iface.Name == id {
				source = "lldp.interfaceName=ifName"
			}
		case 1:
			if id != "" && iface.Alias == id {
				source = "lldp.interfaceAlias=ifAlias"
			}
		case 3:
			if len(p.id) > 0 && iface.MAC != "" && strings.EqualFold(net.HardwareAddr(p.id).String(), iface.MAC) {
				source = "lldp.macAddress=ifPhysAddress"
			}
		}
		if source == "" && p.description != "" && iface.Description == p.description {
			source = "lldp.localPortDesc=ifDescr"
			confidence = "medium"
		}
		if source != "" {
			matches = append(matches, candidate{iface, source, confidence})
		}
	}
	if len(matches) != 1 {
		return model.Interface{}, model.Evidence{}, "unresolved", false
	}
	m := matches[0]
	return m.iface, model.Evidence{Source: m.source, Value: m.iface.ID, Confidence: m.confidence}, m.confidence, true
}
