package model

import "slices"

func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// CloneDevice owns all mutable inventory values. New nested P2 snapshots must
// not share their storage with collectors or diagnostics callers.
func CloneDevice(d Device) Device {
	d.Capabilities = slices.Clone(d.Capabilities)
	d.Components = slices.Clone(d.Components)
	d.VendorEvidence = slices.Clone(d.VendorEvidence)
	d.PlatformEvidence = slices.Clone(d.PlatformEvidence)
	d.UptimeAvailable = clonePointer(d.UptimeAvailable)
	d.Services = slices.Clone(d.Services)
	for i := range d.Services {
		s := &d.Services[i]
		s.Enabled = clonePointer(s.Enabled)
		s.Instances = slices.Clone(s.Instances)
		for j := range s.Instances {
			s.Instances[j].Running = clonePointer(s.Instances[j].Running)
			s.Instances[j].PID = clonePointer(s.Instances[j].PID)
		}
	}
	d.Networks = slices.Clone(d.Networks)
	for i := range d.Networks {
		n := &d.Networks[i]
		n.Up = clonePointer(n.Up)
		n.Pending = clonePointer(n.Pending)
		n.Addresses = slices.Clone(n.Addresses)
	}
	if d.Firewall != nil {
		f := *d.Firewall
		f.Enabled = clonePointer(f.Enabled)
		f.Zones = slices.Clone(f.Zones)
		for i := range f.Zones {
			f.Zones[i].Enabled = clonePointer(f.Zones[i].Enabled)
			f.Zones[i].Networks = slices.Clone(f.Zones[i].Networks)
		}
		f.Forwardings = slices.Clone(f.Forwardings)
		for i := range f.Forwardings {
			f.Forwardings[i].Enabled = clonePointer(f.Forwardings[i].Enabled)
		}
		f.Rules = slices.Clone(f.Rules)
		for i := range f.Rules {
			f.Rules[i].Enabled = clonePointer(f.Rules[i].Enabled)
		}
		f.Redirects = slices.Clone(f.Redirects)
		for i := range f.Redirects {
			f.Redirects[i].Enabled = clonePointer(f.Redirects[i].Enabled)
		}
		d.Firewall = &f
	}
	return d
}
