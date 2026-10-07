// Package timesync describes OS time service setup, not a clock or NTP client.
package timesync

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"
)

type Settings struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Servers []string `yaml:"servers" json:"servers,omitempty"`
}

// Normalize accepts only bare addresses or DNS names. Values are never resolved
// during config loading, and cannot contain service options or shell syntax.
func Normalize(s Settings) (Settings, error) {
	if len(s.Servers) > 16 {
		return Settings{}, errors.New("invalid time_sync.servers (maximum 16)")
	}
	out := Settings{Enabled: s.Enabled}
	seen := make(map[string]bool, len(s.Servers))
	for _, raw := range s.Servers {
		name, err := serverName(raw)
		if err != nil {
			return Settings{}, err
		}
		if !seen[name] {
			seen[name] = true
			out.Servers = append(out.Servers, name)
		}
	}
	if s.Enabled && len(out.Servers) == 0 {
		return Settings{}, errors.New("invalid time_sync.servers (required when enabled)")
	}
	return out, nil
}

func serverName(s string) (string, error) {
	invalid := errors.New("invalid time_sync.servers (bare IPv4, IPv6 or DNS hostname required)")
	if s == "" || len(s) > 254 || strings.TrimSpace(s) != s {
		return "", invalid
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		if addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
			return "", invalid
		}
		return addr.Unmap().String(), nil
	}
	s = strings.ToLower(strings.TrimSuffix(s, "."))
	if s == "" || len(s) > 253 || strings.ContainsAny(s, ":/\\[]@%\r\n\x00") {
		return "", invalid
	}
	// A failed dotted numeric address must not turn into a DNS hostname.
	if strings.Trim(s, "0123456789.") == "" {
		return "", invalid
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", invalid
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", invalid
			}
		}
	}
	return s, nil
}

type Status struct {
	Enabled           bool      `json:"enabled"`
	Provider          string    `json:"provider,omitempty"`
	Available         bool      `json:"available"`
	Active            bool      `json:"active"`
	Synchronized      *bool     `json:"synchronized"`
	ConfiguredServers []string  `json:"configured_servers,omitempty"`
	DesiredServers    []string  `json:"desired_servers,omitempty"`
	Configured        bool      `json:"configured"`
	Changed           bool      `json:"changed"`
	ManagedFile       string    `json:"managed_file,omitempty"`
	Diagnostic        string    `json:"diagnostic,omitempty"`
	CheckedAt         time.Time `json:"checked_at"`
}

type Provider interface {
	Status(context.Context, Settings) (Status, error)
	Apply(context.Context, Settings) (Status, error)
}

// Disabled does not undo OS settings: disabling management does not disable NTP.
func Disabled() Status { return Status{CheckedAt: time.Now().UTC()} }

func Clone(s Status) Status {
	s.ConfiguredServers = append([]string(nil), s.ConfiguredServers...)
	s.DesiredServers = append([]string(nil), s.DesiredServers...)
	if s.Synchronized != nil {
		v := *s.Synchronized
		s.Synchronized = &v
	}
	return s
}
