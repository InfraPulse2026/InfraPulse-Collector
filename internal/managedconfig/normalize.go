// Package managedconfig reconciles administrator-bounded monitoring snapshots.
// It never accepts system settings, secrets, paths, or equipment commands.
package managedconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/model"
	protocolsnmp "InfraPulseAgent/internal/protocol/snmp"
	"InfraPulseAgent/internal/telephony"
)

// Normalize uses fixed v1 defaults, independent of the administrator's polling
// defaults. Explicit negative values are retained for validation, never defaulted.
func Normalize(in model.DesiredCollectorConfig) (model.DesiredCollectorConfig, error) {
	if len(in.Targets) > 1024 {
		return model.DesiredCollectorConfig{}, errors.New("target_limit")
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > 4<<20 {
		return model.DesiredCollectorConfig{}, errors.New("payload_limit")
	}
	var out model.DesiredCollectorConfig
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, errors.New("invalid_snapshot")
	}
	if out.Targets == nil {
		out.Targets = []model.ManagedTargetSpec{}
	}
	sort.Slice(out.Targets, func(i, j int) bool { return out.Targets[i].ID < out.Targets[j].ID })
	for n := range out.Targets {
		t := &out.Targets[n]
		if t.SNMP != nil && t.Protocol != "snmp" || t.SIP != nil && t.Protocol != "sip" || t.Redfish != nil && t.Protocol != "redfish" || t.IPMI != nil && t.Protocol != "ipmi" {
			return out, errors.New("invalid_protocol_options")
		}
		p := &t.Polling
		fill := func(v *time.Duration, d time.Duration) {
			if *v == 0 {
				*v = d
			}
		}
		switch t.Protocol {
		case "snmp", "routeros":
			fill(&p.FastInterval, 3*time.Second)
			fill(&p.MediumInterval, 30*time.Second)
			fill(&p.InventoryInterval, 5*time.Minute)
			fill(&p.TopologyInterval, 5*time.Minute)
			fill(&p.Timeout, 2*time.Second)
		case "redfish", "ipmi":
			fill(&p.MediumInterval, 30*time.Second)
			fill(&p.InventoryInterval, 5*time.Minute)
			fill(&p.Timeout, 30*time.Second)
		case "sip":
			fill(&p.FastInterval, 30*time.Second)
			fill(&p.Timeout, 2*time.Second)
		default:
			return out, errors.New("invalid_protocol")
		}
		switch t.Protocol {
		case "snmp":
			if t.SNMP == nil {
				t.SNMP = &model.ManagedSNMPOptions{}
			}
			s := protocolsnmp.Security{Version: t.SNMP.Version, SecurityLevel: t.SNMP.SecurityLevel, AuthProtocol: t.SNMP.AuthProtocol, PrivacyProtocol: t.SNMP.PrivacyProtocol, ContextName: t.SNMP.ContextName}.WithDefaults()
			t.SNMP.Version, t.SNMP.SecurityLevel, t.SNMP.AuthProtocol, t.SNMP.PrivacyProtocol = s.Version, s.SecurityLevel, s.AuthProtocol, s.PrivacyProtocol
			if t.SNMP.Port == 0 {
				t.SNMP.Port = 161
			}
		case "sip":
			if t.SIP == nil {
				t.SIP = &model.ManagedSIPOptions{}
			}
			e := telephony.Endpoint{Transport: t.SIP.Transport, Port: t.SIP.Port, FromUser: t.SIP.FromUser, FailureThreshold: t.SIP.FailureThreshold, LatencyThreshold: t.SIP.LatencyThreshold}.WithDefaults()
			t.SIP.Transport, t.SIP.Port, t.SIP.FromUser, t.SIP.FailureThreshold, t.SIP.LatencyThreshold = e.Transport, e.Port, e.FromUser, e.FailureThreshold, e.LatencyThreshold
		case "redfish":
			if t.Redfish == nil {
				t.Redfish = &model.ManagedRedfishOptions{}
			}
			r := config.Redfish{Auth: t.Redfish.Auth, RequestTimeout: t.Redfish.RequestTimeout, MaxResources: t.Redfish.MaxResources, MaxPages: t.Redfish.MaxPages, MaxRequests: t.Redfish.MaxRequests}.WithDefaults()
			t.Redfish = &model.ManagedRedfishOptions{Auth: r.Auth, RequestTimeout: r.RequestTimeout, MaxResources: r.MaxResources, MaxPages: r.MaxPages, MaxRequests: r.MaxRequests}
		case "ipmi":
			if t.IPMI == nil {
				t.IPMI = &model.ManagedIPMIOptions{}
			}
			i := t.IPMI
			v := config.IPMI{Port: i.Port, CipherSuite: i.CipherSuite, Privilege: i.Privilege, Retries: i.Retries, RequestTimeout: i.RequestTimeout, MaxSessionDuration: i.MaxSessionDuration, MaxOperations: i.MaxOperations, MaxSDRRecords: i.MaxSDRRecords, MaxSDRBytes: i.MaxSDRBytes, MaxFRURecords: i.MaxFRURecords, MaxFRUBytes: i.MaxFRUBytes, MaxPollBytes: i.MaxPollBytes, SDRCacheTTL: i.SDRCacheTTL}.WithDefaults()
			t.IPMI = &model.ManagedIPMIOptions{Port: v.Port, CipherSuite: v.CipherSuite, Privilege: v.Privilege, Retries: v.Retries, RequestTimeout: v.RequestTimeout, MaxSessionDuration: v.MaxSessionDuration, MaxOperations: v.MaxOperations, MaxSDRRecords: v.MaxSDRRecords, MaxSDRBytes: v.MaxSDRBytes, MaxFRURecords: v.MaxFRURecords, MaxFRUBytes: v.MaxFRUBytes, MaxPollBytes: v.MaxPollBytes, SDRCacheTTL: v.SDRCacheTTL}
		}
	}
	return out, nil
}

// CanonicalBytes is a deliberately small, language-independent v1 encoding.
// Strings are UTF-8 preceded by an unsigned big-endian 64-bit byte length;
// numbers are signed big-endian int64; booleans are one byte. No maps exist.
// Revision, delivery session, issued_at and the supplied digest are excluded.
func CanonicalBytes(in model.DesiredCollectorConfig) ([]byte, error) {
	s, err := Normalize(in)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("InfraPulse managed config v1\n")
	str := func(v string) { _ = binary.Write(&b, binary.BigEndian, uint64(len(v))); b.WriteString(v) }
	num := func(v int64) { _ = binary.Write(&b, binary.BigEndian, v) }
	str(s.CollectorID)
	num(int64(len(s.Targets)))
	for _, t := range s.Targets {
		for _, v := range []string{t.ID, t.Name, t.Protocol, t.Endpoint, t.CredentialRef, t.VendorHint, t.CARef} {
			str(v)
		}
		if t.Enabled {
			b.WriteByte(1)
		} else {
			b.WriteByte(0)
		}
		for _, v := range []string{t.Location.Site, t.Location.Datacenter, t.Location.Building, t.Location.Room, t.Location.Rack, t.Location.Position, t.Location.Comment} {
			str(v)
		}
		num(int64(t.Location.RackUnit))
		for _, v := range []time.Duration{t.Polling.FastInterval, t.Polling.MediumInterval, t.Polling.InventoryInterval, t.Polling.TopologyInterval, t.Polling.Timeout} {
			num(int64(v))
		}
		// Presence flags preserve invalid mismatched options in the fingerprint;
		// validation rejects them, rather than silently discarding their content.
		for _, present := range []bool{t.SNMP != nil, t.SIP != nil, t.Redfish != nil, t.IPMI != nil} {
			if present {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		}
		if v := t.SNMP; v != nil {
			for _, x := range []string{v.Version, v.SecurityLevel, v.AuthProtocol, v.PrivacyProtocol, v.ContextName} {
				str(x)
			}
			num(int64(v.Port))
			num(int64(v.Retries))
		}
		if v := t.SIP; v != nil {
			for _, x := range []string{v.Transport, v.FromUser, v.ToUser, v.Domain, v.ServerName} {
				str(x)
			}
			num(int64(v.Port))
			num(int64(v.FailureThreshold))
			num(int64(v.LatencyThreshold))
		}
		if v := t.Redfish; v != nil {
			str(v.Auth)
			for _, x := range []int64{int64(v.RequestTimeout), int64(v.MaxResources), int64(v.MaxPages), int64(v.MaxRequests)} {
				num(x)
			}
		}
		if v := t.IPMI; v != nil {
			num(int64(*v.Port))
			num(int64(*v.CipherSuite))
			str(v.Privilege)
			for _, x := range []int64{int64(v.Retries), int64(v.RequestTimeout), int64(v.MaxSessionDuration), int64(v.MaxOperations), int64(v.MaxSDRRecords), int64(v.MaxSDRBytes), int64(v.MaxFRURecords), int64(v.MaxFRUBytes), int64(v.MaxPollBytes), int64(v.SDRCacheTTL)} {
				num(x)
			}
		}
	}
	return b.Bytes(), nil
}

func Digest(in model.DesiredCollectorConfig) (string, error) {
	b, err := CanonicalBytes(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
