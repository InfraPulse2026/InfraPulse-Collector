package sanitize

import (
	"slices"

	"InfraPulseAgent/internal/model"
)

// Redfish returns only allowlisted fields. This boundary also redacts configured
// secrets echoed by an untrusted BMC without changing caller-owned inventory.
func sanitizeHardware(e model.Envelope, text func(string) string) model.Envelope {
	if e.Inventory != nil {
		inventory := *e.Inventory
		inventory.Devices = slices.Clone(inventory.Devices)
		for i, original := range inventory.Devices {
			d := model.CloneDevice(original)
			for _, field := range []*string{&d.Name, &d.Hostname, &d.Vendor, &d.Model, &d.FirmwareVersion, &d.SystemDescription} {
				*field = text(*field)
			}
			if text(d.SerialNumber) != d.SerialNumber {
				d.SerialNumber = ""
			}
			cleanHardware(d.Hardware, text)
			for j := range d.Components {
				c := &d.Components[j]
				for _, field := range []*string{&c.Name, &c.Description, &c.Manufacturer, &c.Model, &c.FirmwareVersion, &c.SoftwareVersion, &c.HardwareRevision} {
					*field = text(*field)
				}
				if text(c.SerialNumber) != c.SerialNumber {
					c.SerialNumber = ""
				}
				cleanHardware(c.Hardware, text)
			}
			d.IdentityEvidence = cleanIdentity(d.IdentityEvidence, text)
			inventory.Devices[i] = d
		}
		e.Inventory = &inventory
	}
	if e.Hello != nil {
		h := *e.Hello
		clean := sanitizeHardware(model.Envelope{Inventory: &model.InventoryUpdate{Devices: []model.Device{h.Device}}}, text)
		h.Device = clean.Inventory.Devices[0]
		e.Hello = &h
	}
	if e.Telemetry != nil {
		s := *e.Telemetry
		s.Metrics = slices.Clone(s.Metrics)
		for i := range s.Metrics {
			s.Metrics[i].Name = text(s.Metrics[i].Name)
			if s.Metrics[i].Status != nil {
				st := *s.Metrics[i].Status
				cleanStatus(&st, text)
				s.Metrics[i].Status = &st
			}
		}
		s.HardwareHealth = slices.Clone(s.HardwareHealth)
		for i := range s.HardwareHealth {
			s.HardwareHealth[i].Name = text(s.HardwareHealth[i].Name)
			cleanStatus(&s.HardwareHealth[i].Status, text)
		}
		s.IdentityEvidence = cleanIdentity(s.IdentityEvidence, text)
		e.Telemetry = &s
	}
	if e.Topology != nil {
		t := *e.Topology
		t.Observations = slices.Clone(t.Observations)
		for i := range t.Observations {
			n := model.CloneNeighbor(t.Observations[i])
			n.IdentityEvidence = cleanIdentity(n.IdentityEvidence, text)
			for _, field := range []*string{&n.RemoteSystemName, &n.RemotePortDescription, &n.RemoteSystemDescription, &n.RemotePlatform, &n.RemoteModel, &n.RemoteSoftwareVersion} {
				*field = text(*field)
			}
			for j := range n.Evidence {
				n.Evidence[j].Value = text(n.Evidence[j].Value)
			}
			t.Observations[i] = n
		}
		e.Topology = &t
	}
	return e
}

func cleanIdentity(values []model.IdentityEvidence, text func(string) string) []model.IdentityEvidence {
	result := slices.Clone(values)
	for i := range result {
		v := &result[i]
		if text(v.Value) != v.Value || text(v.OriginalValue) != v.OriginalValue || text(string(v.RawValue)) != string(v.RawValue) {
			v.Value, v.OriginalValue, v.RawValue, v.Subtype, v.Normalization = "[rejected identifier]", "", nil, nil, "rejected"
		}
	}
	return result
}

func cleanHardware(h *model.HardwareInventory, text func(string) string) {
	if h == nil {
		return
	}
	if text(h.HardwareUUID) != h.HardwareUUID {
		h.HardwareUUID = ""
	}
	for _, field := range []*string{&h.ResourceType, &h.SKU, &h.PartNumber, &h.BIOSVersion, &h.PowerState, &h.ChassisType, &h.ManagerType, &h.AssetTag, &h.PhysicalSecurity, &h.DeviceLocation, &h.MemoryType, &h.ErrorCorrection, &h.Protocol, &h.MediaType} {
		*field = text(*field)
	}
	cleanStatus(&h.Status, text)
	for i := range h.NetworkAddresses {
		h.NetworkAddresses[i] = text(h.NetworkAddresses[i])
	}
}

func cleanStatus(s *model.HardwareStatus, text func(string) string) {
	if text(s.State) != s.State {
		s.State = "unknown"
	}
}
