package openwrt

import (
	"errors"
	"strings"
	"time"

	"InfraPulseAgent/internal/model"
)

// This is configured UCI evidence, not a dump or verification of active nftables
// rules. Only documented fw4 fields are mapped; includes and commands are never
// executed or forwarded.
func parseFirewall(data []byte, observed time.Time) (*model.FirewallSnapshot, error) {
	obj, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	raw, present := obj["values"]
	if !present {
		return nil, errors.New("OpenWrt firewall lacks UCI values")
	}
	sections, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	f := &model.FirewallSnapshot{ObservedAt: observed, Source: "ubus.uci.firewall.configuration", Completeness: "complete"}
	defaults := make(map[string]string)
	for _, key := range sortedKeys(sections) {
		entry, err := decodeObject(sections[key])
		if err != nil {
			return nil, err
		}
		kind, err := stringField(entry, ".type")
		if err != nil || kind == "" {
			return nil, errors.New("OpenWrt firewall section lacks type")
		}
		if kind == "defaults" {
			for _, key := range []string{"input", "output", "forward"} {
				defaults[key], err = stringField(entry, key)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	for _, key := range sortedKeys(sections) {
		entry, _ := decodeObject(sections[key])
		kind, _ := stringField(entry, ".type")
		id, err := stringField(entry, ".name")
		if err != nil {
			return nil, err
		}
		if id == "" {
			id = key
		}
		if id == "" || len(id) > maxFieldBytes || strings.ContainsAny(id, "\x00\r\n") {
			return nil, errors.New("malformed OpenWrt firewall section ID")
		}
		if kind != "zone" && kind != "forwarding" && kind != "rule" && kind != "redirect" {
			if kind != "defaults" {
				// nft includes, ipsets and NAT sections are outside this normalized
				// P2 inventory. Keep the snapshot explicitly partial.
				f.Completeness = "partial"
			}
			continue
		}
		enabled, err := configuredEnabled(entry)
		if err != nil {
			return nil, err
		}
		switch kind {
		case "zone":
			z := model.FirewallZone{ID: id, Enabled: enabled}
			for key, dest := range map[string]*string{"name": &z.Name, "input": &z.Input, "output": &z.Output, "forward": &z.Forward} {
				*dest, err = stringField(entry, key)
				if err != nil {
					return nil, err
				}
				if key != "name" && *dest == "" {
					*dest = defaults[key]
				}
			}
			if z.Name == "" {
				return nil, errors.New("OpenWrt firewall zone lacks name")
			}
			z.Networks, err = stringList(entry, "network")
			if err != nil {
				return nil, err
			}
			f.Zones = append(f.Zones, z)
		case "forwarding":
			v := model.FirewallForwarding{ID: id, Enabled: enabled}
			v.SourceZone, err = stringField(entry, "src")
			if err != nil {
				return nil, err
			}
			v.DestinationZone, err = stringField(entry, "dest")
			if err != nil {
				return nil, err
			}
			if v.SourceZone == "" || v.DestinationZone == "" {
				return nil, errors.New("OpenWrt forwarding lacks source or destination zone")
			}
			f.Forwardings = append(f.Forwardings, v)
		case "rule", "redirect":
			r, err := firewallRule(entry, id, enabled)
			if err != nil {
				return nil, err
			}
			if kind == "rule" {
				f.Rules = append(f.Rules, r)
				continue
			}
			v := model.FirewallRedirect{FirewallRule: r}
			for key, dest := range map[string]*string{"src_dport": &v.ExternalPort, "dest_ip": &v.InternalIP, "dest_port": &v.InternalPort} {
				*dest, err = stringField(entry, key)
				if err != nil {
					return nil, err
				}
			}
			// A configured redirect without target uses fw4's documented DNAT
			// default. Unknown internal port remains empty, not invented.
			if v.Target == "" {
				v.Target = "DNAT"
			}
			f.Redirects = append(f.Redirects, v)
		}
	}
	return f, nil
}

func configuredEnabled(entry object) (*bool, error) {
	// fw4 defaults enabled to "1" for these four supported section types.
	if _, present := entry["enabled"]; !present {
		value := true
		return &value, nil
	}
	value, err := stringField(entry, "enabled")
	if err != nil {
		return nil, err
	}
	var enabled bool
	switch value {
	case "1", "on", "true", "yes":
		enabled = true
	case "0", "off", "false", "no":
		enabled = false
	default:
		return nil, errors.New("malformed OpenWrt firewall enabled flag")
	}
	return &enabled, nil
}

func firewallRule(entry object, id string, enabled *bool) (model.FirewallRule, error) {
	r := model.FirewallRule{ID: id, Enabled: enabled}
	for key, dest := range map[string]*string{
		"name": &r.Name, "src": &r.SourceZone, "dest": &r.DestinationZone,
		"target": &r.Target, "family": &r.Family,
	} {
		var err error
		*dest, err = stringField(entry, key)
		if err != nil {
			return r, err
		}
	}
	for key, dest := range map[string]*string{
		"proto": &r.Protocol, "src_ip": &r.SourceIP, "dest_ip": &r.DestinationIP,
		"src_port": &r.SourcePort, "dest_port": &r.DestinationPort,
	} {
		values, err := stringList(entry, key)
		if err != nil {
			return r, err
		}
		*dest = strings.Join(values, " ")
		if len(*dest) > maxFieldBytes {
			return r, errors.New("OpenWrt firewall field exceeds limit")
		}
	}
	return r, nil
}
