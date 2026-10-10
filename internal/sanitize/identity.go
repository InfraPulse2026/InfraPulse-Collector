package sanitize

import (
	"errors"
	"net/url"

	"InfraPulseAgent/internal/model"
)

// ValidateIdentitySources rejects secret-bearing source/resource keys rather
// than rewriting them into a misleading identity. The safe error never echoes
// an untrusted identifier. This check does not claim unknown-secret detection.
func (k KnownSecrets) ValidateIdentitySources(e model.Envelope) error {
	safe := true
	check := func(s string) {
		if TextWithSecrets(s, k.Values) != s {
			safe = false
		}
	}
	checkURI := func(s string) {
		check(s)
		// Resource URIs are transmitted, not fetched here. Check the standard
		// decoded representation too, without rewriting the resource identity.
		if decoded, err := url.PathUnescape(s); err == nil && decoded != s {
			check(decoded)
		}
		if decoded, err := url.QueryUnescape(s); err == nil && decoded != s {
			check(decoded)
		}
	}
	source := func(s model.SourceRef) {
		for _, v := range []string{s.CollectorID, s.TargetID, s.InstanceID, s.Protocol} {
			check(v)
		}
		checkURI(s.ResourceID)
	}
	observation := func(o *model.ObservationMetadata) {
		if o != nil {
			source(o.Source)
		}
	}
	identity := func(values []model.IdentityEvidence) {
		for _, v := range values {
			source(v.Source)
			check(v.SubjectID)
			check(v.Origin)
		}
	}
	hardware := func(h *model.HardwareInventory) {
		if h != nil {
			for _, r := range h.Relations {
				checkURI(r.TargetResourceID)
			}
		}
	}
	device := func(d model.Device) {
		check(d.ID)
		hardware(d.Hardware)
		observation(d.Observation)
		identity(d.IdentityEvidence)
		for _, c := range d.Components {
			hardware(c.Hardware)
			check(c.ID)
			check(c.ParentID)
			observation(c.Observation)
		}
	}
	if e.Hello != nil {
		device(e.Hello.Device)
	}
	if e.Inventory != nil {
		observation(e.Inventory.Observation)
		for _, d := range e.Inventory.Devices {
			device(d)
		}
	}
	if e.Telemetry != nil {
		s := e.Telemetry
		observation(s.Observation)
		identity(s.IdentityEvidence)
		for _, i := range s.Interfaces {
			check(i.ID)
			check(i.Name)
			check(i.MAC)
		}
		for _, m := range s.Metrics {
			check(m.ResourceID)
			check(m.ComponentID)
			observation(m.Observation)
		}
		for _, h := range s.HardwareHealth {
			check(h.ID)
			check(h.ResourceID)
			observation(h.Observation)
		}
	}
	if e.Topology != nil {
		observation(e.Topology.Observation)
		for _, n := range e.Topology.Observations {
			observation(n.Observation)
			identity(n.IdentityEvidence)
			for _, v := range []string{n.LocalDeviceID, n.LocalInterfaceID, n.LocalInterfaceName, n.LocalInterfaceMAC, string(n.LocalPortID), string(n.RemoteChassisID), string(n.RemotePortID)} {
				check(v)
			}
			for _, address := range n.RemoteManagementAddresses {
				check(address.Address)
				check(string(address.RawAddress))
			}
		}
	}
	for _, v := range e.Events {
		observation(v.Observation)
	}
	if e.Logs != nil {
		observation(e.Logs.Observation)
		for _, v := range e.Logs.Records {
			observation(v.Observation)
		}
	}
	if !safe {
		return errors.New("source identity rejected: credential or secret pattern in identifier")
	}
	return nil
}
