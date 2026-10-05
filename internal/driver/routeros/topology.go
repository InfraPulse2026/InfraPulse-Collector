package routeros

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

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
		if age := row["age"]; age != "" {
			elapsed, valid := parseUptime(age)
			if !valid || elapsed > float64((365*24*time.Hour)/time.Second) {
				update.Completeness = "partial"
				err = errors.Join(err, driver.ErrMalformed)
			} else {
				observed = now.Add(-time.Duration(elapsed * float64(time.Second)))
			}
		}
		protocols := []string{}
		seenProtocols := map[string]bool{}
		for _, p := range strings.Split(row["discovered-by"], ",") {
			p = strings.TrimSpace(strings.ToLower(p))
			if (p == "lldp" || p == "cdp" || p == "mndp") && !seenProtocols[p] {
				seenProtocols[p] = true
				protocols = append(protocols, p)
			}
		}
		if len(protocols) == 0 {
			protocols = []string{"unknown"}
		}
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
			n := model.Neighbor{LocalDeviceID: d.opts.ID, LocalPortID: []byte(row["interface"]), RemoteChassisID: remoteMAC, RemoteSystemName: row["identity"], Protocol: discovery, Source: "routeros-neighbor", ObservedAt: observed, ExpiresAt: observed.Add(time.Duration(d.opts.TopologyTTLSeconds) * time.Second), Confidence: "unresolved", Evidence: []model.Evidence{{Source: "routeros-neighbor/interface", Value: row["interface"], Confidence: "medium"}, {Source: "routeros-neighbor/discovered-by", Value: row["discovered-by"], Confidence: "high"}}}
			// Neighbor MAC is not promised to be the LLDP chassis ID. Preserve the
			// raw value but leave its LLDP subtype unknown instead of inventing 4.
			if len(remoteMAC) > 0 {
				n.Evidence = append(n.Evidence, model.Evidence{Source: "routeros-neighbor/mac-address", Value: row["mac-address"], Confidence: "low"})
			}
			if discovery == "cdp" && row["interface-name"] != "" {
				n.RemotePortID = []byte(row["interface-name"])
				n.RemotePortDescription = row["interface-name"]
				n.Evidence = append(n.Evidence, model.Evidence{Source: "routeros-neighbor/interface-name", Value: row["interface-name"], Confidence: "medium"})
			}
			matched := -1
			for i, iface := range interfaces {
				if iface.Name == row["interface"] {
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

var _ driver.Driver = (*Driver)(nil)
var _ driver.InterfaceManager = (*Driver)(nil)
