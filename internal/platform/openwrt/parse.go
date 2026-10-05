package openwrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"InfraPulseAgent/internal/model"
)

type object map[string]json.RawMessage

func decodeObject(data []byte) (object, error) {
	if len(data) > maxResponseBytes {
		return nil, errOutputLimit
	}
	var obj object
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, errors.New("malformed OpenWrt JSON object")
	}
	if len(obj) > maxEntries {
		return nil, errors.New("OpenWrt object count exceeds limit")
	}
	return obj, nil
}

func stringField(obj object, key string) (string, error) {
	raw, present := obj[key]
	if !present {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
		return "", fmt.Errorf("malformed OpenWrt string field %s", key)
	}
	if len(value) > maxFieldBytes || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("invalid OpenWrt string field %s", key)
	}
	return strings.TrimSpace(value), nil
}

func boolField(obj object, key string) (*bool, error) {
	raw, present := obj[key]
	if !present {
		return nil, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
		return nil, fmt.Errorf("malformed OpenWrt boolean field %s", key)
	}
	return &value, nil
}

func stringList(obj object, key string) ([]string, error) {
	raw, present := obj[key]
	if !present {
		return nil, nil
	}
	var list []string
	if len(raw) > 0 && raw[0] == '"' {
		value, err := stringField(obj, key)
		if err != nil {
			return nil, err
		}
		list = strings.Fields(value)
	} else if err := json.Unmarshal(raw, &list); err != nil || string(raw) == "null" {
		return nil, fmt.Errorf("malformed OpenWrt list field %s", key)
	}
	if len(list) > maxEntries {
		return nil, errors.New("OpenWrt list count exceeds limit")
	}
	for _, value := range list {
		if len(value) > maxFieldBytes || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("invalid OpenWrt list field %s", key)
		}
	}
	return list, nil
}

func sortedKeys(obj object) []string {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type board struct {
	hostname, kernel, model, name, system, rootfs        string
	distribution, version, revision, description, target string
}

func parseBoard(data []byte) (board, error) {
	obj, err := decodeObject(data)
	if err != nil {
		return board{}, err
	}
	var b board
	for key, dest := range map[string]*string{
		"hostname": &b.hostname, "kernel": &b.kernel, "model": &b.model,
		"board_name": &b.name, "system": &b.system, "rootfs_type": &b.rootfs,
	} {
		*dest, err = stringField(obj, key)
		if err != nil {
			return board{}, err
		}
	}
	if raw, present := obj["release"]; present {
		release, e := decodeObject(raw)
		if e != nil {
			return board{}, e
		}
		for key, dest := range map[string]*string{
			"distribution": &b.distribution, "version": &b.version,
			"revision": &b.revision, "description": &b.description, "target": &b.target,
		} {
			*dest, err = stringField(release, key)
			if err != nil {
				return board{}, err
			}
		}
		if b.distribution != "" && b.distribution != "OpenWrt" {
			return board{}, errors.New("ubus distribution conflicts with OpenWrt evidence")
		}
	}
	return b, nil
}

func parseNetworks(data []byte, observed time.Time) ([]model.NetworkState, error) {
	obj, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	raw, present := obj["interface"]
	if !present {
		return nil, errors.New("OpenWrt network dump lacks interface array")
	}
	var entries []json.RawMessage
	if err = json.Unmarshal(raw, &entries); err != nil || string(raw) == "null" {
		return nil, errors.New("malformed OpenWrt network array")
	}
	if len(entries) > maxEntries {
		return nil, errors.New("OpenWrt network count exceeds limit")
	}
	result := make([]model.NetworkState, 0, len(entries))
	for _, raw := range entries {
		entry, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		n := model.NetworkState{Source: "ubus.network.interface.dump", ObservedAt: observed}
		for key, dest := range map[string]*string{"interface": &n.Name, "device": &n.Device, "proto": &n.Protocol} {
			*dest, err = stringField(entry, key)
			if err != nil {
				return nil, err
			}
		}
		if n.Name == "" {
			return nil, errors.New("OpenWrt network lacks logical interface name")
		}
		if n.Device == "" {
			n.Device, err = stringField(entry, "l3_device")
			if err != nil {
				return nil, err
			}
		}
		n.Up, err = boolField(entry, "up")
		if err != nil {
			return nil, err
		}
		n.Pending, err = boolField(entry, "pending")
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"ipv4-address", "ipv6-address"} {
			addresses, err := parseAddresses(entry, key)
			if err != nil {
				return nil, err
			}
			n.Addresses = append(n.Addresses, addresses...)
			if len(n.Addresses) > maxEntries {
				return nil, errors.New("OpenWrt combined address count exceeds limit")
			}
		}
		result = append(result, n)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func parseAddresses(obj object, key string) ([]string, error) {
	raw, present := obj[key]
	if !present {
		return nil, nil
	}
	var entries []struct {
		Address string `json:"address"`
		Mask    *int   `json:"mask"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil || string(raw) == "null" {
		return nil, errors.New("malformed OpenWrt address array")
	}
	if len(entries) > maxEntries {
		return nil, errors.New("OpenWrt address count exceeds limit")
	}
	addresses := make([]string, 0, len(entries))
	for _, entry := range entries {
		address, err := netip.ParseAddr(entry.Address)
		if err != nil || entry.Mask == nil || *entry.Mask < 0 || *entry.Mask > address.BitLen() ||
			(key == "ipv4-address" && !address.Is4()) || (key == "ipv6-address" && !address.Is6()) {
			return nil, errors.New("malformed OpenWrt interface address")
		}
		addresses = append(addresses, netip.PrefixFrom(address, *entry.Mask).String())
	}
	return addresses, nil
}

func parseServices(data []byte, observed time.Time) ([]model.Service, error) {
	obj, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	result := make([]model.Service, 0, len(obj))
	instanceCount := 0
	for _, name := range sortedKeys(obj) {
		if name == "" || len(name) > maxFieldBytes || strings.ContainsAny(name, "\x00\r\n/") {
			return nil, errors.New("malformed OpenWrt service name")
		}
		entry, err := decodeObject(obj[name])
		if err != nil {
			return nil, err
		}
		s := model.Service{Name: name, State: "unknown", Source: "ubus.service.list", ObservedAt: observed}
		if raw, present := entry["instances"]; present {
			instances, err := decodeObject(raw)
			if err != nil {
				return nil, err
			}
			instanceCount += len(instances)
			if instanceCount > maxEntries {
				return nil, errors.New("OpenWrt service instance count exceeds limit")
			}
			known, running := false, false
			unknown := false
			for _, instanceName := range sortedKeys(instances) {
				if instanceName == "" || len(instanceName) > maxFieldBytes || strings.ContainsAny(instanceName, "\x00\r\n") {
					return nil, errors.New("malformed OpenWrt instance name")
				}
				instance, err := decodeObject(instances[instanceName])
				if err != nil {
					return nil, err
				}
				i := model.ServiceInstance{Name: instanceName}
				i.Running, err = boolField(instance, "running")
				if err != nil {
					return nil, err
				}
				if i.Running != nil {
					known, running = true, running || *i.Running
				} else {
					unknown = true
				}
				if raw, ok := instance["pid"]; ok {
					var pid uint32
					if err := json.Unmarshal(raw, &pid); err != nil || pid == 0 {
						return nil, errors.New("malformed OpenWrt instance pid")
					}
					i.PID = &pid
				}
				s.Instances = append(s.Instances, i)
			}
			if running {
				s.State = "running"
			} else if known && !unknown {
				s.State = "stopped"
			}
		}
		// procd autostart applies to registered instances, not boot enablement.
		// command/env/data fields are intentionally excluded from inventory.
		result = append(result, s)
	}
	return result, nil
}
