package routeros

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func (d *Driver) Topology(ctx context.Context) (model.TopologyUpdate, error) {
	now := time.Now().UTC()
	update := model.TopologyUpdate{DeviceID: d.opts.ID, ObservedAt: now, CollectedAt: now, Source: "routeros-neighbor", Completeness: "failed", TTLSeconds: d.opts.TopologyTTLSeconds, ExpiryAssumption: "configured collector observation TTL; not advertised LLDP TTL", Observations: []model.Neighbor{}}
	rows, err := d.client.Neighbors(ctx)
	if err != nil {
		d.mark("topology.read", false)
		return update, mapError(err)
	}
	d.mark("topology.read", true)
	update.Completeness = "complete"
	ifaceRows, ifaceErr := d.client.Interfaces(ctx)
	interfaces, parseErr := mapInterfaces(ifaceRows)
	if ifaceErr != nil || parseErr != nil {
		update.Completeness = "partial"
		err = errors.Join(mapError(ifaceErr), parseErr)
	}
	for _, row := range rows {
		observed := now
		var ageSeconds *uint32
		if age := row["age"]; age != "" {
			elapsed, valid := parseUptime(age)
			if !valid || elapsed > float64((365*24*time.Hour)/time.Second) {
				update.Completeness = "partial"
				err = errors.Join(err, driver.ErrMalformed)
			} else {
				observed = now.Add(-time.Duration(elapsed * float64(time.Second)))
				age := uint32(elapsed)
				ageSeconds = &age
			}
		}
		// Fixed documented fields are bounded independently so a malformed
		// optional fact does not discard other neighbors or block delivery.
		text := func(key string, limit int) string {
			value := row[key]
			if len(value) > limit {
				err = errors.Join(err, driver.ErrLimit)
				update.Completeness = "partial"
				return ""
			}
			if !validTopologyText(value, limit) {
				err = errors.Join(err, driver.ErrMalformed)
				update.Completeness = "partial"
				return ""
			}
			return value
		}
		localPort := text("interface", 255)
		identity := text("identity", 255)
		remotePort := text("interface-name", 255)
		platform, board, version := text("platform", 255), text("board", 255), text("version", 255)
		systemDescription := text("system-description", 4096)
		discoveredBy := text("discovered-by", 128)
		addresses, addressErr := neighborManagementAddresses(row)
		if addressErr != nil {
			err = errors.Join(err, addressErr)
			update.Completeness = "partial"
		}
		supported, supportedErr := neighborCapabilities(row["system-caps"])
		enabled, enabledErr := neighborCapabilities(row["system-caps-enabled"])
		if supportedErr != nil || enabledErr != nil {
			err = errors.Join(err, supportedErr, enabledErr)
			update.Completeness = "partial"
		}
		protocols := []string{}
		seenProtocols := map[string]bool{}
		for _, p := range strings.Split(discoveredBy, ",") {
			p = strings.TrimSpace(strings.ToLower(p))
			if (p == "lldp" || p == "cdp" || p == "mndp") && !seenProtocols[p] {
				seenProtocols[p] = true
				protocols = append(protocols, p)
			}
		}
		if len(protocols) == 0 {
			protocols = []string{"unknown"}
		}
		sort.Strings(protocols)
		var remoteMAC []byte
		if mac := row["mac-address"]; mac != "" {
			parsed, e := net.ParseMAC(mac)
			if e != nil {
				update.Completeness = "partial"
				err = errors.Join(err, driver.ErrMalformed)
			} else {
				remoteMAC = append([]byte(nil), parsed...)
			}
		}
		for _, discovery := range protocols {
			if len(update.Observations) >= d.opts.MaxRows {
				update.Completeness = "partial"
				update.CollectedAt = time.Now().UTC()
				return update, errors.Join(err, driver.ErrLimit)
			}
			n := model.Neighbor{LocalDeviceID: d.opts.ID, LocalPortID: []byte(localPort), RemoteChassisID: remoteMAC, RemoteSystemName: identity, Protocol: discovery, Source: "routeros-neighbor", ObservedAt: observed, ExpiresAt: observed.Add(time.Duration(d.opts.TopologyTTLSeconds) * time.Second), AgeSeconds: ageSeconds, RemoteManagementAddresses: addresses, RemotePlatform: platform, RemoteModel: board, RemoteSoftwareVersion: version, Confidence: "unresolved", Evidence: []model.Evidence{{Source: "routeros-neighbor/interface", Value: localPort, Confidence: "medium"}, {Source: "routeros-neighbor/discovered-by", Value: discoveredBy, Confidence: "high"}}}
			// These fields are explicitly LLDP claims in the RouterOS API. A
			// mixed-protocol row must not attribute them to CDP or MNDP.
			if discovery == "lldp" {
				n.RemoteSystemDescription = systemDescription
				n.RemoteCapabilitiesSupported, n.RemoteCapabilitiesEnabled = supported, enabled
			}
			// Neighbor MAC is not promised to be the LLDP chassis ID. Preserve the
			// raw value but leave its LLDP subtype unknown instead of inventing 4.
			if len(remoteMAC) > 0 {
				n.Evidence = append(n.Evidence, model.Evidence{Source: "routeros-neighbor/mac-address", Value: row["mac-address"], Confidence: "low"})
			}
			if discovery == "cdp" && remotePort != "" {
				n.RemotePortID = []byte(remotePort)
				n.RemotePortDescription = remotePort
				n.Evidence = append(n.Evidence, model.Evidence{Source: "routeros-neighbor/interface-name", Value: remotePort, Confidence: "medium"})
			}
			matched := -1
			for i, iface := range interfaces {
				if localPort != "" && iface.Name == localPort {
					if matched >= 0 {
						matched = -2
						break
					}
					matched = i
				}
			}
			if matched >= 0 {
				n.LocalInterfaceID = interfaces[matched].ID
				n.LocalInterfaceName = interfaces[matched].Name
				if mac, e := model.NormalizeMAC(interfaces[matched].MAC); e == nil {
					n.LocalInterfaceMAC = mac
				}
				if validTopologyText(interfaces[matched].Description, 4096) {
					n.LocalInterfaceDescription = interfaces[matched].Description
				}
				if validTopologyText(interfaces[matched].Alias, 255) {
					n.LocalInterfaceAlias = interfaces[matched].Alias
				}
				n.LocalPortSubtype = 5
				n.Confidence = "low"
				if discovery == "lldp" || discovery == "cdp" {
					n.Confidence = "medium"
				}
				n.Evidence = append(n.Evidence, model.Evidence{Source: "routeros-interface/name", Value: interfaces[matched].Name, Confidence: "high"})
			}
			update.Observations = append(update.Observations, n)
		}
	}
	update.CollectedAt = time.Now().UTC()
	return update, err
}

func validTopologyText(value string, limit int) bool {
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

func neighborCapabilities(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if !validTopologyText(value, 4096) {
		return nil, driver.ErrMalformed
	}
	aliases := map[string]string{"wlan-ap": "wlan_access_point", "station-only": "station_only", "docsis-cable-device": "docsis_cable_device"}
	seen := make(map[string]bool)
	var result []string
	var err error
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !validTopologyText(name, 64) {
			err = errors.Join(err, driver.ErrMalformed)
			continue
		}
		if canonical, exists := aliases[strings.ToLower(name)]; exists {
			name = canonical
		}
		if seen[name] {
			continue
		}
		if len(result) >= 16 {
			err = errors.Join(err, driver.ErrLimit)
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	sort.Strings(result)
	return result, err
}

func neighborManagementAddresses(row map[string]string) ([]model.ManagementAddress, error) {
	var result []model.ManagementAddress
	seen := make(map[string]bool)
	var err error
	for _, field := range []string{"address", "address4", "address6"} {
		value := row[field]
		if value == "" {
			continue
		}
		canonical, addressFamily, e := model.NormalizeIP(value)
		if e != nil || field == "address4" && addressFamily != "ipv4" || field == "address6" && addressFamily != "ipv6" {
			err = errors.Join(err, driver.ErrMalformed)
			continue
		}
		family := uint32(2)
		ip, _ := netip.ParseAddr(canonical)
		raw := ip.AsSlice()
		if addressFamily == "ipv4" {
			family = 1
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		result = append(result, model.ManagementAddress{AddressFamily: family, Address: canonical, RawAddress: append([]byte(nil), raw...)})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AddressFamily != result[j].AddressFamily {
			return result[i].AddressFamily < result[j].AddressFamily
		}
		return result[i].Address < result[j].Address
	})
	return result, err
}

var _ driver.Driver = (*Driver)(nil)
var _ driver.InterfaceManager = (*Driver)(nil)
