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
			if original.Hardware == nil {
				continue
			}
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
			var evidence []model.IdentityEvidence
			for _, value := range d.IdentityEvidence {
				if text(value.Value) == value.Value {
					evidence = append(evidence, value)
				}
			}
			d.IdentityEvidence = evidence
			inventory.Devices[i] = d
		}
		e.Inventory = &inventory
	}
	if e.Telemetry != nil && (len(e.Telemetry.HardwareHealth) > 0 || e.Telemetry.Observation != nil && e.Telemetry.Observation.Source.Type == model.SourceRedfish) {
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
		var evidence []model.IdentityEvidence
		for _, value := range s.IdentityEvidence {
			if text(value.Value) == value.Value {
				evidence = append(evidence, value)
			}
		}
		s.IdentityEvidence = evidence
		e.Telemetry = &s
	}
	return e
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
