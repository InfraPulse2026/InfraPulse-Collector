package grpc

import (
	"errors"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
)

const maxP2Entries = 1024

func stringsBound(limit int, fields ...string) error {
	for _, s := range fields {
		if len(s) > limit {
			return errors.New("P2 text limit exceeded")
		}
	}
	return nil
}
func validateLogs(b model.LogBatch) error {
	if b.Source == "" || len(b.Source) > 128 || len(b.Records) == 0 || len(b.Records) > maxP2Entries {
		return errors.New("invalid log batch source or count")
	}
	if err := validateTime(b.ObservedAt, "logs.observed_at"); err != nil {
		return err
	}
	if err := logStringsBound(128, b.Source); err != nil {
		return err
	}
	if err := validateLogObservation(b.Observation); err != nil {
		return err
	}
	for _, r := range b.Records {
		if r.ID == "" || r.DeviceID == "" || r.Source == "" || r.SourceID == "" || r.Offset < 0 {
			return errors.New("invalid log identity or offset")
		}
		if err := logStringsBound(1024, r.ID, r.DeviceID, r.Source, r.SourceID, r.FileIdentity, r.Host, r.Process, r.ProcessID); err != nil {
			return err
		}
		if err := logStringsBound(65536, r.Message, r.Raw); err != nil {
			return err
		}
		if err := validateTime(r.ObservedAt, "log.observed_at"); err != nil {
			return err
		}
		if r.OriginalTimestamp != nil {
			if err := validateTime(*r.OriginalTimestamp, "log.original_timestamp"); err != nil {
				return err
			}
		}
		if r.Severity != nil && *r.Severity > 7 || r.Facility != nil && *r.Facility > 23 {
			return errors.New("invalid syslog priority")
		}
		if err := validateSyslogMetadata(r.Syslog); err != nil {
			return err
		}
		if err := validateLogObservation(r.Observation); err != nil {
			return err
		}
		if a := r.Audit; a != nil {
			if err := logStringsBound(4096, a.EventID, a.RecordType, a.Executable, a.Syscall, a.Result, a.Host); err != nil {
				return err
			}
		}
	}
	return nil
}
func logsToProto(b model.LogBatch) *pb.LogBatch {
	p := &pb.LogBatch{ObservedAt: timestamp(b.ObservedAt), Source: b.Source, Observation: logObservationToProto(b.Observation)}
	for _, r := range b.Records {
		v := &pb.LogRecord{Id: r.ID, DeviceId: r.DeviceID, Source: r.Source, SourceId: r.SourceID, ObservedAt: timestamp(r.ObservedAt), Severity: r.Severity, Facility: r.Facility, Host: r.Host, Process: r.Process, ProcessId: r.ProcessID, Message: r.Message, Raw: r.Raw, Truncated: r.Truncated, FileIdentity: r.FileIdentity, Offset: r.Offset, Syslog: syslogToProto(r.Syslog), Observation: logObservationToProto(r.Observation)}
		if r.OriginalTimestamp != nil {
			v.OriginalTimestamp = timestamp(*r.OriginalTimestamp)
		}
		if a := r.Audit; a != nil {
			v.Audit = &pb.AuditRecord{EventId: a.EventID, RecordType: a.RecordType, Uid: a.UID, Auid: a.AUID, Pid: a.PID, Executable: a.Executable, Syscall: a.Syscall, Success: a.Success, Result: a.Result, Host: a.Host}
		}
		p.Records = append(p.Records, v)
	}
	return p
}
func logsFromProto(p *pb.LogBatch) (model.LogBatch, error) {
	if p == nil || len(p.Records) > maxP2Entries {
		return model.LogBatch{}, errors.New("nil or oversized logs")
	}
	at, err := requiredTime(p.ObservedAt, "logs.observed_at")
	if err != nil {
		return model.LogBatch{}, err
	}
	b := model.LogBatch{ObservedAt: at, Source: p.Source}
	b.Observation, err = logObservationFromProto(p.Observation)
	if err != nil {
		return b, err
	}
	for _, v := range p.Records {
		if v == nil {
			return b, errors.New("nil log record")
		}
		at, err := requiredTime(v.ObservedAt, "log.observed_at")
		if err != nil {
			return b, err
		}
		r := model.LogRecord{ID: v.Id, DeviceID: v.DeviceId, Source: v.Source, SourceID: v.SourceId, ObservedAt: at, Severity: v.Severity, Facility: v.Facility, Host: v.Host, Process: v.Process, ProcessID: v.ProcessId, Message: v.Message, Raw: v.Raw, Truncated: v.Truncated, FileIdentity: v.FileIdentity, Offset: v.Offset}
		r.Syslog, err = syslogFromProto(v.Syslog)
		if err != nil {
			return b, err
		}
		r.Observation, err = logObservationFromProto(v.Observation)
		if err != nil {
			return b, err
		}
		if v.OriginalTimestamp != nil {
			t, err := requiredTime(v.OriginalTimestamp, "log.original_timestamp")
			if err != nil {
				return b, err
			}
			r.OriginalTimestamp = &t
		}
		if a := v.Audit; a != nil {
			r.Audit = &model.AuditRecord{EventID: a.EventId, RecordType: a.RecordType, UID: a.Uid, AUID: a.Auid, PID: a.Pid, Executable: a.Executable, Syscall: a.Syscall, Success: a.Success, Result: a.Result, Host: a.Host}
		}
		b.Records = append(b.Records, r)
	}
	return b, validateLogs(b)
}
func validateTelephony(u model.TelephonyUpdate) error {
	if u.Source == "" || len(u.Source) > 128 || len(u.Targets) == 0 || len(u.Targets) > 256 {
		return errors.New("invalid telephony source or count")
	}
	if err := validateTime(u.ObservedAt, "telephony.observed_at"); err != nil {
		return err
	}
	for _, h := range u.Targets {
		if h.TargetID == "" || h.Address == "" || h.Source == "" || h.Transport != "udp" && h.Transport != "tcp" && h.Transport != "tls" {
			return errors.New("invalid telephony target or transport")
		}
		if h.State != "unknown" && h.State != "up" && h.State != "healthy" && h.State != "down" && h.State != "degraded" {
			return errors.New("invalid telephony state")
		}
		if err := stringsBound(1024, h.TargetID, h.Address, h.Source, h.ErrorCategory); err != nil {
			return err
		}
		if err := validateTime(h.ObservedAt, "sip.observed_at"); err != nil {
			return err
		}
		if h.ResponseCode != nil && (*h.ResponseCode < 100 || *h.ResponseCode > 699 || !h.Reachable) {
			return errors.New("invalid SIP response code")
		}
		if h.LatencyMillis != nil {
			if err := finite(*h.LatencyMillis); err != nil {
				return err
			}
			if *h.LatencyMillis < 0 {
				return errors.New("negative SIP latency")
			}
		}
	}
	return nil
}
func telephonyToProto(u model.TelephonyUpdate) *pb.TelephonyUpdate {
	p := &pb.TelephonyUpdate{ObservedAt: timestamp(u.ObservedAt), Source: u.Source}
	for _, h := range u.Targets {
		p.Targets = append(p.Targets, &pb.TelephonyHealth{TargetId: h.TargetID, Address: h.Address, Transport: h.Transport, Reachable: h.Reachable, ResponseCode: h.ResponseCode, LatencyMs: h.LatencyMillis, ObservedAt: timestamp(h.ObservedAt), ConsecutiveFailures: h.ConsecutiveFailures, State: h.State, ErrorCategory: h.ErrorCategory, Source: h.Source})
	}
	return p
}
func telephonyFromProto(p *pb.TelephonyUpdate) (model.TelephonyUpdate, error) {
	if p == nil || len(p.Targets) > 256 {
		return model.TelephonyUpdate{}, errors.New("nil or oversized telephony")
	}
	at, err := requiredTime(p.ObservedAt, "telephony.observed_at")
	if err != nil {
		return model.TelephonyUpdate{}, err
	}
	u := model.TelephonyUpdate{ObservedAt: at, Source: p.Source}
	for _, h := range p.Targets {
		if h == nil {
			return u, errors.New("nil telephony target")
		}
		at, err := requiredTime(h.ObservedAt, "sip.observed_at")
		if err != nil {
			return u, err
		}
		u.Targets = append(u.Targets, model.TelephonyHealth{TargetID: h.TargetId, Address: h.Address, Transport: h.Transport, Reachable: h.Reachable, ResponseCode: h.ResponseCode, LatencyMillis: h.LatencyMs, ObservedAt: at, ConsecutiveFailures: h.ConsecutiveFailures, State: h.State, ErrorCategory: h.ErrorCategory, Source: h.Source})
	}
	return u, validateTelephony(u)
}
func ruleToProto(r model.FirewallRule) *pb.FirewallRule {
	return &pb.FirewallRule{Id: r.ID, Name: r.Name, SourceZone: r.SourceZone, DestinationZone: r.DestinationZone, Protocol: r.Protocol, SourceIp: r.SourceIP, DestinationIp: r.DestinationIP, SourcePort: r.SourcePort, DestinationPort: r.DestinationPort, Target: r.Target, Family: r.Family, Enabled: r.Enabled}
}
func ruleFromProto(r *pb.FirewallRule) (model.FirewallRule, error) {
	if r == nil {
		return model.FirewallRule{}, errors.New("nil firewall rule")
	}
	return model.FirewallRule{ID: r.Id, Name: r.Name, SourceZone: r.SourceZone, DestinationZone: r.DestinationZone, Protocol: r.Protocol, SourceIP: r.SourceIp, DestinationIP: r.DestinationIp, SourcePort: r.SourcePort, DestinationPort: r.DestinationPort, Target: r.Target, Family: r.Family, Enabled: r.Enabled}, nil
}
func p2DeviceToProto(d model.Device, p *pb.Device) {
	p.Source, p.BoardName, p.PlatformEvidence = d.Source, d.BoardName, evidenceToProto(d.PlatformEvidence)
	for _, s := range d.Services {
		v := &pb.Service{Name: s.Name, State: s.State, Enabled: s.Enabled, ObservedAt: timestamp(s.ObservedAt), Source: s.Source}
		for _, i := range s.Instances {
			v.Instances = append(v.Instances, &pb.ServiceInstance{Name: i.Name, Running: i.Running, Pid: i.PID})
		}
		p.Services = append(p.Services, v)
	}
	for _, n := range d.Networks {
		p.Networks = append(p.Networks, &pb.NetworkState{Name: n.Name, Device: n.Device, Protocol: n.Protocol, Up: n.Up, Pending: n.Pending, Addresses: append([]string(nil), n.Addresses...), Source: n.Source, ObservedAt: timestamp(n.ObservedAt)})
	}
	if f := d.Firewall; f != nil {
		v := &pb.FirewallSnapshot{ObservedAt: timestamp(f.ObservedAt), Source: f.Source, Completeness: f.Completeness, Enabled: f.Enabled}
		for _, z := range f.Zones {
			v.Zones = append(v.Zones, &pb.FirewallZone{Id: z.ID, Name: z.Name, Networks: append([]string(nil), z.Networks...), Input: z.Input, Output: z.Output, Forward: z.Forward, Enabled: z.Enabled})
		}
		for _, r := range f.Forwardings {
			v.Forwardings = append(v.Forwardings, &pb.FirewallForwarding{Id: r.ID, SourceZone: r.SourceZone, DestinationZone: r.DestinationZone, Enabled: r.Enabled})
		}
		for _, r := range f.Rules {
			v.Rules = append(v.Rules, ruleToProto(r))
		}
		for _, r := range f.Redirects {
			v.Redirects = append(v.Redirects, &pb.FirewallRedirect{Rule: ruleToProto(r.FirewallRule), ExternalPort: r.ExternalPort, InternalIp: r.InternalIP, InternalPort: r.InternalPort})
		}
		p.Firewall = v
	}
}
func p2DeviceFromProto(p *pb.Device, d *model.Device) error {
	d.Source, d.BoardName = p.Source, p.BoardName
	var err error
	d.PlatformEvidence, err = evidenceFromProto(p.PlatformEvidence)
	if err != nil {
		return err
	}
	if len(p.Services) > maxP2Entries || len(p.Networks) > maxP2Entries {
		return errors.New("OpenWrt inventory limit exceeded")
	}
	for _, s := range p.Services {
		if s == nil || len(s.Instances) > maxP2Entries {
			return errors.New("nil or oversized service")
		}
		at, err := requiredTime(s.ObservedAt, "service.observed_at")
		if err != nil {
			return err
		}
		v := model.Service{Name: s.Name, State: s.State, Enabled: s.Enabled, ObservedAt: at, Source: s.Source}
		for _, i := range s.Instances {
			if i == nil {
				return errors.New("nil service instance")
			}
			v.Instances = append(v.Instances, model.ServiceInstance{Name: i.Name, Running: i.Running, PID: i.Pid})
		}
		d.Services = append(d.Services, v)
	}
	for _, n := range p.Networks {
		if n == nil {
			return errors.New("nil network")
		}
		at, err := requiredTime(n.ObservedAt, "network.observed_at")
		if err != nil {
			return err
		}
		d.Networks = append(d.Networks, model.NetworkState{Name: n.Name, Device: n.Device, Protocol: n.Protocol, Up: n.Up, Pending: n.Pending, Addresses: append([]string(nil), n.Addresses...), Source: n.Source, ObservedAt: at})
	}
	if f := p.Firewall; f != nil {
		if len(f.Zones)+len(f.Forwardings)+len(f.Rules)+len(f.Redirects) > maxP2Entries {
			return errors.New("firewall inventory limit exceeded")
		}
		at, err := requiredTime(f.ObservedAt, "firewall.observed_at")
		if err != nil {
			return err
		}
		v := &model.FirewallSnapshot{ObservedAt: at, Source: f.Source, Completeness: f.Completeness, Enabled: f.Enabled}
		for _, z := range f.Zones {
			if z == nil {
				return errors.New("nil firewall zone")
			}
			v.Zones = append(v.Zones, model.FirewallZone{ID: z.Id, Name: z.Name, Networks: append([]string(nil), z.Networks...), Input: z.Input, Output: z.Output, Forward: z.Forward, Enabled: z.Enabled})
		}
		for _, r := range f.Forwardings {
			if r == nil {
				return errors.New("nil forwarding")
			}
			v.Forwardings = append(v.Forwardings, model.FirewallForwarding{ID: r.Id, SourceZone: r.SourceZone, DestinationZone: r.DestinationZone, Enabled: r.Enabled})
		}
		for _, r := range f.Rules {
			r, err := ruleFromProto(r)
			if err != nil {
				return err
			}
			v.Rules = append(v.Rules, r)
		}
		for _, r := range f.Redirects {
			if r == nil {
				return errors.New("nil redirect")
			}
			rule, err := ruleFromProto(r.Rule)
			if err != nil {
				return err
			}
			v.Redirects = append(v.Redirects, model.FirewallRedirect{FirewallRule: rule, ExternalPort: r.ExternalPort, InternalIP: r.InternalIp, InternalPort: r.InternalPort})
		}
		d.Firewall = v
	}
	return validateP2Device(*d)
}
func validateP2Device(d model.Device) error {
	if err := stringsBound(1024, d.Source, d.BoardName); err != nil {
		return err
	}
	if err := validateEvidence(d.PlatformEvidence); err != nil {
		return err
	}
	if len(d.Services) > maxP2Entries || len(d.Networks) > maxP2Entries {
		return errors.New("OpenWrt inventory limit exceeded")
	}
	for _, s := range d.Services {
		if s.Name == "" || s.Source == "" || (s.State != "running" && s.State != "stopped" && s.State != "unknown") || len(s.Instances) > maxP2Entries {
			return errors.New("invalid service")
		}
		if err := stringsBound(1024, s.Name, s.Source); err != nil {
			return err
		}
		if err := validateTime(s.ObservedAt, "service.observed_at"); err != nil {
			return err
		}
		for _, i := range s.Instances {
			if i.Name == "" || len(i.Name) > 1024 {
				return errors.New("invalid service instance")
			}
		}
	}
	for _, n := range d.Networks {
		if n.Name == "" || n.Source == "" || len(n.Addresses) > maxP2Entries {
			return errors.New("invalid logical network")
		}
		if err := stringsBound(1024, n.Name, n.Device, n.Protocol, n.Source); err != nil {
			return err
		}
		if err := stringsBound(128, n.Addresses...); err != nil {
			return err
		}
		if err := validateTime(n.ObservedAt, "network.observed_at"); err != nil {
			return err
		}
	}
	if f := d.Firewall; f != nil {
		if f.Source == "" || len(f.Source) > 128 || (f.Completeness != "complete" && f.Completeness != "partial" && f.Completeness != "failed") || len(f.Zones)+len(f.Forwardings)+len(f.Rules)+len(f.Redirects) > maxP2Entries {
			return errors.New("invalid firewall snapshot")
		}
		if err := validateTime(f.ObservedAt, "firewall.observed_at"); err != nil {
			return err
		}
		for _, z := range f.Zones {
			if z.ID == "" || len(z.Networks) > maxP2Entries {
				return errors.New("invalid zone")
			}
			if err := stringsBound(1024, z.ID, z.Name, z.Input, z.Output, z.Forward); err != nil {
				return err
			}
			if err := stringsBound(1024, z.Networks...); err != nil {
				return err
			}
		}
		for _, r := range f.Forwardings {
			if r.ID == "" {
				return errors.New("invalid forwarding")
			}
			if err := stringsBound(1024, r.ID, r.SourceZone, r.DestinationZone); err != nil {
				return err
			}
		}
		for _, r := range f.Rules {
			if err := validateFirewallRule(r); err != nil {
				return err
			}
		}
		for _, r := range f.Redirects {
			if err := validateFirewallRule(r.FirewallRule); err != nil {
				return err
			}
			if err := stringsBound(1024, r.ExternalPort, r.InternalIP, r.InternalPort); err != nil {
				return err
			}
		}
	}
	return nil
}
func validateFirewallRule(r model.FirewallRule) error {
	if r.ID == "" {
		return errors.New("invalid firewall rule")
	}
	return stringsBound(1024, r.ID, r.Name, r.SourceZone, r.DestinationZone, r.Protocol, r.SourceIP, r.DestinationIP, r.SourcePort, r.DestinationPort, r.Target, r.Family)
}
