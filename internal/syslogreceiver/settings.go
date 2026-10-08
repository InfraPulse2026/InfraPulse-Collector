// Package syslogreceiver is a bounded network source of the existing log
// pipeline. It owns no outbox, scheduler, protobuf or Backend connection.
package syslogreceiver

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"InfraPulseAgent/internal/model"
)

type Settings struct {
	Enabled             bool     `yaml:"enabled"`
	TLS                 TLS      `yaml:"tls"`
	TCP                 TCP      `yaml:"tcp"`
	Limits              Limits   `yaml:"limits"`
	UnknownSourcePolicy string   `yaml:"unknown_source_policy"`
	AllowedPeers        []string `yaml:"allowed_peers"`
	Sources             []Source `yaml:"sources"`
}

type TLS struct {
	Enabled      bool   `yaml:"enabled"`
	Listen       string `yaml:"listen"`
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientAuth   string `yaml:"client_auth"`
	ClientCAFile string `yaml:"client_ca_file"`
}

type TCP struct {
	Enabled          bool   `yaml:"enabled"`
	Listen           string `yaml:"listen"`
	Framing          string `yaml:"framing"`
	AllowNonLoopback bool   `yaml:"allow_non_loopback"`
}

type Limits struct {
	MaxMessageBytes         int           `yaml:"max_message_bytes"`
	MaxConnections          int           `yaml:"max_connections"`
	MaxConnectionsPerSource int           `yaml:"max_connections_per_source"`
	ReadTimeout             time.Duration `yaml:"read_timeout"`
	IdleTimeout             time.Duration `yaml:"idle_timeout"`
	AdmissionTimeout        time.Duration `yaml:"admission_timeout"`
}

// Source is a configured attribution rule, not a listener or an observed Asset.
// Both selectors, when present, must match. Hostnames in messages never match.
type Source struct {
	ID          string           `yaml:"id"`
	TLSIdentity string           `yaml:"tls_identity"`
	PeerCIDRs   []string         `yaml:"peer_cidrs"`
	AuditApps   []string         `yaml:"audit_apps"`
	Provenance  model.Provenance `yaml:"provenance"`
}

func Defaults() Settings {
	return Settings{
		TLS:                 TLS{Enabled: true, Listen: "0.0.0.0:6514", ClientAuth: "require_and_verify"},
		TCP:                 TCP{Listen: "127.0.0.1:1514", Framing: "octet_counting"},
		UnknownSourcePolicy: "reject",
		Limits: Limits{MaxMessageBytes: 8192, MaxConnections: 64, MaxConnectionsPerSource: 8,
			ReadTimeout: 10 * time.Second, IdleTimeout: time.Minute, AdmissionTimeout: 2 * time.Second},
	}
}

func (s Settings) Validate() error {
	var problems []error
	check := func(ok bool, name string) {
		if !ok {
			problems = append(problems, fmt.Errorf("invalid syslog_receiver.%s", name))
		}
	}
	check(s.Limits.MaxMessageBytes >= 2048 && s.Limits.MaxMessageBytes <= 65536, "limits.max_message_bytes")
	check(s.Limits.MaxConnections >= 1 && s.Limits.MaxConnections <= 256, "limits.max_connections")
	check(s.Limits.MaxConnectionsPerSource >= 1 && s.Limits.MaxConnectionsPerSource <= s.Limits.MaxConnections, "limits.max_connections_per_source")
	for name, d := range map[string]time.Duration{"read_timeout": s.Limits.ReadTimeout, "idle_timeout": s.Limits.IdleTimeout, "admission_timeout": s.Limits.AdmissionTimeout} {
		check(d >= 10*time.Millisecond && d <= 5*time.Minute, "limits."+name)
	}
	check(s.UnknownSourcePolicy == "reject" || s.UnknownSourcePolicy == "unmanaged", "unknown_source_policy")
	check(len(s.AllowedPeers) <= 256, "allowed_peers count")
	check(validCIDRs(s.AllowedPeers), "allowed_peers")
	check(s.TLS.ClientAuth == "none" || s.TLS.ClientAuth == "require_and_verify", "tls.client_auth")
	check(s.TCP.Framing == "octet_counting" || s.TCP.Framing == "lf", "tcp.framing")
	if s.Enabled {
		check(s.TLS.Enabled || s.TCP.Enabled, "listeners (none enabled)")
		if s.TLS.Enabled {
			check(validListen(s.TLS.Listen), "tls.listen")
			check(s.TLS.CertFile != "" && s.TLS.KeyFile != "", "tls cert/key files")
			if s.TLS.ClientAuth == "require_and_verify" {
				check(s.TLS.ClientCAFile != "", "tls.client_ca_file")
			}
			if s.TLS.ClientAuth == "none" {
				check(s.TLS.ClientCAFile == "" && len(s.AllowedPeers) > 0, "tls server authentication requires allowed_peers and no client_ca_file")
			}
		}
		if s.TCP.Enabled {
			check(validListen(s.TCP.Listen), "tcp.listen")
			host, _, _ := net.SplitHostPort(s.TCP.Listen)
			ip, _ := netip.ParseAddr(host)
			check(ip.IsLoopback() || s.TCP.AllowNonLoopback && len(s.AllowedPeers) > 0, "tcp public listener requires allow_non_loopback and allowed_peers")
		}
		if s.TLS.Enabled && s.TCP.Enabled {
			check(!listenConflict(s.TLS.Listen, s.TCP.Listen), "conflicting listeners")
		}
	}
	check(len(s.Sources) <= 256, "sources count")
	ids := map[string]bool{}
	for _, src := range s.Sources {
		ref := model.SourceRef{Type: model.SourceSyslog, InstanceID: src.ID, Provenance: src.Provenance}
		check(src.ID != "" && len(src.ID) <= 128 && !ids[src.ID] && ref.Validate() == nil, "source.id/provenance")
		ids[src.ID] = true
		check(src.Provenance == "" || src.Provenance == model.ProvenanceReal || src.Provenance == model.ProvenanceSimulated, "source.provenance")
		check(src.TLSIdentity != "" || len(src.PeerCIDRs) > 0, "source selectors")
		check(len(src.PeerCIDRs) <= 32 && validCIDRs(src.PeerCIDRs), "source.peer_cidrs")
		check(src.TLSIdentity == "" || validTLSIdentity(src.TLSIdentity), "source.tls_identity")
		if s.Enabled && src.TLSIdentity != "" {
			check(s.TLS.Enabled && s.TLS.ClientAuth == "require_and_verify", "source TLS identity requires mTLS")
		}
		check(len(src.AuditApps) <= 16, "source.audit_apps count")
		for _, app := range src.AuditApps {
			check(validToken(app, 48), "source.audit_apps")
		}
	}
	return errors.Join(problems...)
}

func validCIDRs(values []string) bool {
	for _, value := range values {
		if _, err := netip.ParsePrefix(value); err != nil {
			return false
		}
	}
	return true
}

func validToken(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, ch := range value {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func validTLSIdentity(v string) bool {
	if !validToken(v, 1024) {
		return false
	}
	if strings.HasPrefix(v, "dns:") {
		name := strings.TrimPrefix(v, "dns:")
		if name == "" || len(name) > 253 || strings.ContainsAny(name, "*/\\:@") {
			return false
		}
		for _, part := range strings.Split(name, ".") {
			if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
				return false
			}
			for _, ch := range part {
				if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
					return false
				}
			}
		}
		return true
	}
	if strings.HasPrefix(v, "uri:") {
		u, err := url.Parse(strings.TrimPrefix(v, "uri:"))
		return err == nil && u.Scheme != "" && u.User == nil && u.Fragment == "" && u.RawQuery == ""
	}
	return false
}

func validListen(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return false
	}
	for _, ch := range port {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func listenConflict(a, b string) bool {
	ha, pa, ea := net.SplitHostPort(a)
	hb, pb, eb := net.SplitHostPort(b)
	portA, errA := strconv.Atoi(pa)
	portB, errB := strconv.Atoi(pb)
	if ea != nil || eb != nil || errA != nil || errB != nil || portA != portB {
		return false
	}
	ia, _ := netip.ParseAddr(ha)
	ib, _ := netip.ParseAddr(hb)
	return ia == ib || ia.IsUnspecified() || ib.IsUnspecified()
}

func (s Source) Target(enabled bool) model.Target {
	endpoint := s.TLSIdentity
	if endpoint == "" {
		endpoint = strings.Join(s.PeerCIDRs, ",")
	}
	return model.Target{ID: "syslog:" + s.ID, Type: model.SourceSyslog, Protocol: "syslog", Endpoint: endpoint, Enabled: enabled}
}
