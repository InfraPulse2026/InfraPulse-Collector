package managedconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/model"
	protocolsnmp "InfraPulseAgent/internal/protocol/snmp"
	"InfraPulseAgent/internal/telephony"
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Validate reuses existing typed configuration validators, then applies the
// administrator's tighter management policy. Returned settings contain only
// credential references, never the resolved values checked during prepare.
func Validate(ctx context.Context, cfg config.Config, in model.DesiredCollectorConfig, provider credentials.Provider, resolver Resolver) ([]config.Device, error) {
	p := cfg.RemoteManagement.WithDefaults()
	if !p.Enabled {
		return nil, errors.New("management_disabled")
	}
	if len(in.Targets) > p.MaxManagedTargets {
		return nil, errors.New("target_limit")
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > p.MaxConfigBytes {
		return nil, errors.New("payload_limit")
	}
	s, err := Normalize(in)
	if err != nil {
		return nil, err
	}
	check := cfg
	check.Devices = slices.Clone(cfg.Devices)
	check.Telephony.Endpoints = slices.Clone(cfg.Telephony.Endpoints)
	local := map[string]bool{}
	for _, d := range cfg.Devices {
		local[d.ID] = true
	}
	for _, e := range cfg.Telephony.Endpoints {
		local[e.ID] = true
	}
	seen := map[string]bool{}
	result := make([]config.Device, 0, len(s.Targets))
	for _, t := range s.Targets {
		if ctx.Err() != nil {
			return nil, errors.New("apply_cancelled")
		}
		if !config.ManagedIdentifier(t.ID) || seen[t.ID] {
			return nil, errors.New("invalid_target_id")
		}
		if local[t.ID] {
			return nil, errors.New("local_target_conflict")
		}
		seen[t.ID] = true
		if !slices.Contains(p.AllowedProtocols, t.Protocol) {
			return nil, errors.New("protocol_forbidden")
		}
		if t.SNMP != nil && t.Protocol != "snmp" || t.SIP != nil && t.Protocol != "sip" || t.Redfish != nil && t.Protocol != "redfish" || t.IPMI != nil && t.Protocol != "ipmi" {
			return nil, errors.New("invalid_protocol_options")
		}
		if t.Protocol == "sip" && t.CredentialRef != "" {
			return nil, errors.New("sip_auth_unsupported")
		}
		if t.Protocol != "sip" && (!slices.Contains(p.AllowedCredentialRefs, t.CredentialRef) || !config.ManagedIdentifier(t.CredentialRef)) {
			return nil, errors.New("credential_forbidden")
		}
		ca := ""
		if t.CARef != "" {
			var ok bool
			ca, ok = p.TLSCARefs[t.CARef]
			if !ok || t.Protocol == "snmp" || t.Protocol == "ipmi" {
				return nil, errors.New("ca_reference_forbidden")
			}
		}
		minInterval := p.MinPollingInterval
		if t.Protocol == "redfish" || t.Protocol == "ipmi" {
			minInterval = p.MinHardwareInterval
		}
		for _, v := range []time.Duration{t.Polling.FastInterval, t.Polling.MediumInterval, t.Polling.InventoryInterval, t.Polling.TopologyInterval} {
			if v != 0 && (v < minInterval || v > 24*time.Hour) {
				return nil, errors.New("polling_interval_forbidden")
			}
		}
		if t.Polling.Timeout < 10*time.Millisecond || t.Polling.Timeout > 5*time.Minute {
			return nil, errors.New("invalid_polling_timeout")
		}
		d := config.Device{ID: t.ID, Name: t.Name, Driver: t.Protocol, Address: t.Endpoint, CredentialRef: t.CredentialRef, VendorHint: t.VendorHint, Location: t.Location, Disabled: !t.Enabled, TLS: config.RemoteTLS{CAFile: ca}, Polling: config.Polling{FastInterval: t.Polling.FastInterval, MediumInterval: t.Polling.MediumInterval, InventoryInterval: t.Polling.InventoryInterval, TopologyInterval: t.Polling.TopologyInterval, Timeout: t.Polling.Timeout}}
		if v := t.SNMP; v != nil {
			d.SNMP = config.SNMP{Security: protocolsnmp.Security{Version: v.Version, SecurityLevel: v.SecurityLevel, AuthProtocol: v.AuthProtocol, PrivacyProtocol: v.PrivacyProtocol, ContextName: v.ContextName}, Port: v.Port, Retries: v.Retries}
		}
		if v := t.Redfish; v != nil {
			d.Redfish = config.Redfish{Auth: v.Auth, RequestTimeout: v.RequestTimeout, MaxResources: v.MaxResources, MaxPages: v.MaxPages, MaxRequests: v.MaxRequests}
		}
		if v := t.IPMI; v != nil {
			d.IPMI = config.IPMI{Port: v.Port, CipherSuite: v.CipherSuite, Privilege: v.Privilege, Retries: v.Retries, RequestTimeout: v.RequestTimeout, MaxSessionDuration: v.MaxSessionDuration, MaxOperations: v.MaxOperations, MaxSDRRecords: v.MaxSDRRecords, MaxSDRBytes: v.MaxSDRBytes, MaxFRURecords: v.MaxFRURecords, MaxFRUBytes: v.MaxFRUBytes, MaxPollBytes: v.MaxPollBytes, SDRCacheTTL: v.SDRCacheTTL}
		}
		if v := t.SIP; v != nil {
			if t.Polling.MediumInterval != 0 || t.Polling.InventoryInterval != 0 || t.Polling.TopologyInterval != 0 || len(t.Name) > 128 || len(t.VendorHint) > 128 {
				return nil, errors.New("invalid_sip_options")
			}
			d.TLS = config.RemoteTLS{}
			e := telephony.Endpoint{ID: t.ID, Address: t.Endpoint, Port: v.Port, Transport: v.Transport, Interval: t.Polling.FastInterval, Timeout: t.Polling.Timeout, FromUser: v.FromUser, ToUser: v.ToUser, Domain: v.Domain, FailureThreshold: v.FailureThreshold, LatencyThreshold: v.LatencyThreshold, TLS: telephony.TLS{CAFile: ca, ServerName: v.ServerName}}
			if e.Validate() != nil {
				return nil, errors.New("invalid_sip_options")
			}
			d.SIP = &e
			check.Telephony.Endpoints = append(check.Telephony.Endpoints, e)
		} else {
			check.Devices = append(check.Devices, d)
		}
		if d.Target(config.Polling{}).Validate() != nil {
			return nil, errors.New("invalid_target")
		}
		result = append(result, d)
	}
	// Never return existing validator messages containing endpoint/config data.
	if check.Validate() != nil {
		return nil, errors.New("invalid_target_settings")
	}
	for i := range result {
		d := &result[i]
		if d.Driver != "sip" {
			if provider == nil {
				return nil, errors.New("credential_unavailable")
			}
			c, e := provider.Get(ctx, d.CredentialRef)
			if e != nil {
				return nil, errors.New("credential_unavailable")
			}
			if d.Driver == "snmp" {
				if d.SNMP.Version == "v2c" && c.Community == "" || d.SNMP.Version == "v3" && (c.Username == "" || c.AuthPassword == "" || d.SNMP.SecurityLevel == "authPriv" && c.PrivacyPassword == "") {
					return nil, errors.New("credential_unavailable")
				}
			} else if c.Username == "" || c.Password == "" {
				return nil, errors.New("credential_unavailable")
			}
			if d.Driver == "ipmi" && (len(c.Username) > 16 || len(c.Password) > 20) {
				return nil, errors.New("credential_incompatible")
			}
		}
		if len(p.AllowedDestinations) > 0 {
			host := d.Address
			if d.Driver == "redfish" || d.Driver == "routeros" {
				u, e := url.Parse(d.Address)
				if e != nil {
					return nil, errors.New("invalid_endpoint")
				}
				host = u.Hostname()
				if u.Port() != "" {
					port, e := strconv.Atoi(u.Port())
					if e != nil || port < 1 || port > 65535 {
						return nil, errors.New("invalid_endpoint")
					}
				}
			}
			ip, e := approvedIP(ctx, host, p.AllowedDestinations, resolver)
			if e != nil {
				return nil, e
			}
			d.DialAddress = ip
		}
	}
	return result, nil
}

func approvedIP(ctx context.Context, host string, cidrs []string, resolver Resolver) (string, error) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		var err error
		ips, err = resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return "", errors.New("destination_unavailable")
		}
	}
	if len(ips) == 0 || len(ips) > 16 {
		return "", errors.New("destination_limit")
	}
	for _, ip := range ips {
		allowed := false
		ip = ip.Unmap()
		for _, cidr := range cidrs {
			p, e := netip.ParsePrefix(cidr)
			if e == nil && p.Contains(ip) {
				allowed = true
				break
			}
		}
		if !ip.IsValid() || ip.Zone() != "" || !allowed {
			return "", errors.New("destination_forbidden")
		}
	}
	// Only this approved numeric address reaches the adapter. It cannot trigger
	// a second DNS lookup or follow a redirect outside the configured policy.
	return ips[0].Unmap().String(), nil
}
