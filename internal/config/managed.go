package config

import (
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"time"
)

// RemoteManagement is exclusively administrator-owned. No desired snapshot can
// change these permissions, trust roots, resource limits or state paths.
type RemoteManagement struct {
	Enabled               bool              `yaml:"enabled"`
	AllowedProtocols      []string          `yaml:"allowed_protocols"`
	AllowedCredentialRefs []string          `yaml:"allowed_credential_refs"`
	AllowedDestinations   []string          `yaml:"allowed_destinations"`
	TLSCARefs             map[string]string `yaml:"tls_ca_refs"`
	MaxManagedTargets     int               `yaml:"max_managed_targets"`
	MaxConfigBytes        int               `yaml:"max_config_bytes"`
	MinPollingInterval    time.Duration     `yaml:"min_polling_interval"`
	MinHardwareInterval   time.Duration     `yaml:"min_hardware_interval"`
	ApplyTimeout          time.Duration     `yaml:"apply_timeout"`
}

func (r RemoteManagement) WithDefaults() RemoteManagement {
	if r.AllowedProtocols == nil {
		r.AllowedProtocols = []string{"snmp", "routeros", "sip", "redfish", "ipmi"}
	}
	if r.MaxManagedTargets == 0 {
		r.MaxManagedTargets = 128
	}
	if r.MaxConfigBytes == 0 {
		r.MaxConfigBytes = 256 << 10
	}
	if r.MinPollingInterval == 0 {
		r.MinPollingInterval = time.Second
	}
	if r.MinHardwareInterval == 0 {
		r.MinHardwareInterval = 10 * time.Second
	}
	if r.ApplyTimeout == 0 {
		r.ApplyTimeout = 10 * time.Second
	}
	return r
}

func (r RemoteManagement) Validate(c Config) error {
	r = r.WithDefaults()
	if r.MaxManagedTargets < 1 || r.MaxManagedTargets > 1024 || r.MaxConfigBytes < 1024 || r.MaxConfigBytes > 1<<20 ||
		r.MinPollingInterval < 10*time.Millisecond || r.MinPollingInterval > 24*time.Hour ||
		r.MinHardwareInterval < r.MinPollingInterval || r.MinHardwareInterval > 24*time.Hour ||
		r.ApplyTimeout < 100*time.Millisecond || r.ApplyTimeout > time.Minute {
		return errors.New("invalid remote_management resource limits")
	}
	if len(r.AllowedProtocols) > 5 || len(r.AllowedCredentialRefs) > 1024 || len(r.AllowedDestinations) > 128 || len(r.TLSCARefs) > 128 {
		return errors.New("remote_management policy exceeds limits")
	}
	seen := map[string]bool{}
	for _, p := range r.AllowedProtocols {
		if seen[p] || p != "snmp" && p != "routeros" && p != "sip" && p != "redfish" && p != "ipmi" {
			return errors.New("invalid remote_management allowed_protocols")
		}
		seen[p] = true
	}
	seen = map[string]bool{}
	for _, ref := range r.AllowedCredentialRefs {
		if _, ok := c.Credentials[ref]; !ok || seen[ref] {
			return errors.New("invalid remote_management credential reference policy")
		}
		seen[ref] = true
	}
	for _, cidr := range r.AllowedDestinations {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return errors.New("invalid remote_management destination CIDR")
		}
	}
	for ref, path := range r.TLSCARefs {
		if !ManagedIdentifier(ref) || path == "" || len(path) > 4096 || strings.ContainsAny(path, "\r\n\x00") {
			return errors.New("invalid remote_management TLS CA reference")
		}
	}
	// Opt-in without mTLS would misleadingly advertise a management boundary.
	if r.Enabled && c.Security.Mode != "production" {
		return errors.New("remote_management requires production mTLS")
	}
	return nil
}

func ManagedIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func ResolveManagedPaths(c *Config, base string) {
	for ref, path := range c.RemoteManagement.TLSCARefs {
		if !filepath.IsAbs(path) {
			c.RemoteManagement.TLSCARefs[ref] = filepath.Join(base, path)
		}
	}
}
