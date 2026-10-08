package model

import (
	"slices"
	"time"
)

// ObserveDevice attaches source-bound inventory facts. The observation time
// comes from the inventory operation, not Device.LastSeen (which can describe
// an earlier successful contact for an offline device). Legacy fields are left
// unchanged, and callers retain ownership of their original mutable values.
func ObserveDevice(d Device, ref SourceRef, observed, collected time.Time) Device {
	d = CloneDevice(d)
	d.Observation = observationFor(d.Observation, ref, d.Source, observed, collected, nil)
	if len(d.IdentityEvidence) == 0 {
		d.IdentityEvidence = DeviceIdentityEvidence(d, d.Observation.Source)
	} else {
		d.IdentityEvidence = bindIdentityEvidence(d.IdentityEvidence, ref.CollectorID, d.Source)
	}
	for i := range d.Services {
		s := &d.Services[i]
		s.Observation = observationFor(s.Observation, ref, s.Source, s.ObservedAt, collected, nil)
	}
	for i := range d.Networks {
		n := &d.Networks[i]
		n.Observation = observationFor(n.Observation, ref, n.Source, n.ObservedAt, collected, nil)
	}
	if f := d.Firewall; f != nil {
		f.Observation = observationFor(f.Observation, ref, f.Source, f.ObservedAt, collected, nil)
	}
	return d
}

// ObserveEnvelope is the normalized boundary between existing collectors and
// delivery. It attaches metadata without changing any historical wire fields.
// A SourceRef is an explicit orchestration binding, never a DeviceID lookup.
// Already populated source/time metadata survives, including clock estimates;
// only a previously unavailable CollectorID is filled at runtime binding.
func ObserveEnvelope(e Envelope, ref SourceRef, collected time.Time) Envelope {
	if e.Hello != nil {
		h := *e.Hello
		h.Device = ObserveDevice(h.Device, ref, collected, collected)
		e.Hello = &h
	}
	if e.Inventory != nil {
		i := *e.Inventory
		i.Observation = observationFor(i.Observation, ref, "", i.ObservedAt, collected, nil)
		i.Devices = slices.Clone(i.Devices)
		for j := range i.Devices {
			i.Devices[j] = ObserveDevice(i.Devices[j], ref, i.ObservedAt, collected)
		}
		e.Inventory = &i
	}
	if e.Telemetry != nil {
		s := *e.Telemetry
		finished := s.CollectedAt
		if finished.IsZero() {
			finished = collected
		}
		s.Observation = observationFor(s.Observation, ref, s.Source, s.ObservedAt, finished, nil)
		if len(s.IdentityEvidence) == 0 {
			s.IdentityEvidence = interfaceIdentityEvidence(s.Interfaces, s.Observation.Source)
		} else {
			s.IdentityEvidence = bindIdentityEvidence(s.IdentityEvidence, ref.CollectorID, s.Source)
		}
		e.Telemetry = &s
	}
	if e.Topology != nil {
		t := *e.Topology
		topologyRef := ref
		if t.Source == "snmp.lldp" {
			topologyRef.Type = SourceLLDP
		}
		finished := t.CollectedAt
		if finished.IsZero() {
			finished = collected
		}
		t.Observation = observationFor(t.Observation, topologyRef, t.Source, t.ObservedAt, finished, nil)
		t.Observations = slices.Clone(t.Observations)
		for i := range t.Observations {
			n := &t.Observations[i]
			neighborRef := ref
			switch n.Protocol {
			case "lldp":
				neighborRef.Type = SourceLLDP
			case "cdp":
				neighborRef.Type = SourceCDP
			case "mndp":
				neighborRef.Type = SourceMNDP
			}
			n.Observation = observationFor(n.Observation, neighborRef, n.Source, n.ObservedAt, finished, nil)
			if len(n.IdentityEvidence) == 0 {
				n.IdentityEvidence = neighborIdentityEvidence(*n, n.Observation.Source)
			} else {
				n.IdentityEvidence = bindIdentityEvidence(n.IdentityEvidence, ref.CollectorID, n.Source)
			}
		}
		e.Topology = &t
	}
	if e.Logs != nil {
		b := CloneLogBatch(*e.Logs)
		batchRef := ref
		if b.Source == "auditd" {
			batchRef.Type = SourceAuditd
		}
		b.Observation = observationFor(b.Observation, batchRef, b.Source, b.ObservedAt, collected, nil)
		for i := range b.Records {
			r := &b.Records[i]
			recordRef := batchRef
			if r.Audit != nil || r.Source == "auditd" {
				recordRef.Type = SourceAuditd
			}
			if recordRef.InstanceID == "" {
				recordRef.InstanceID = r.SourceID
			}
			r.Observation = observationFor(r.Observation, recordRef, r.Source, r.ObservedAt, collected, r.OriginalTimestamp)
		}
		e.Logs = &b
	}
	if e.Telephony != nil {
		u := *e.Telephony
		u.Observation = observationFor(u.Observation, ref, u.Source, u.ObservedAt, collected, nil)
		u.Targets = slices.Clone(u.Targets)
		for i := range u.Targets {
			h := &u.Targets[i]
			h.Observation = observationFor(h.Observation, ref, h.Source, h.ObservedAt, collected, nil)
		}
		e.Telephony = &u
	}
	if len(e.Events) > 0 {
		e.Events = slices.Clone(e.Events)
		for i := range e.Events {
			v := &e.Events[i]
			v.Observation = observationFor(v.Observation, ref, v.Source, v.Timestamp, collected, nil)
		}
	}
	return e
}
