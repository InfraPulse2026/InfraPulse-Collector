// Package telephony performs fixed SIP OPTIONS health probes over UDP/TCP/TLS.
// It never accepts requests, headers or arbitrary payloads from a Controller.
package telephony

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

type Endpoint struct {
	// DialAddress is internal, policy-checked destination pinning. It cannot
	// be set by YAML and never replaces SIP headers or source address metadata.
	DialAddress      string        `yaml:"-" json:"-"`
	ID               string        `yaml:"id"`
	Address          string        `yaml:"address"`
	Port             uint16        `yaml:"port"`
	Transport        string        `yaml:"transport"`
	Timeout          time.Duration `yaml:"timeout"`
	Interval         time.Duration `yaml:"interval"`
	FromUser         string        `yaml:"from_user"`
	ToUser           string        `yaml:"to_user"`
	Domain           string        `yaml:"domain"`
	FailureThreshold uint32        `yaml:"failure_threshold"`
	LatencyThreshold time.Duration `yaml:"latency_threshold"`
	TLS              TLS           `yaml:"tls"`
}

type TLS struct {
	CAFile     string `yaml:"ca_file"`
	ServerName string `yaml:"server_name"`
}

func (e Endpoint) WithDefaults() Endpoint {
	if e.Port == 0 {
		e.Port = 5060
		if e.Transport == "tls" {
			e.Port = 5061
		}
	}
	if e.Transport == "" {
		e.Transport = "udp"
	}
	if e.Timeout == 0 {
		e.Timeout = 2 * time.Second
	}
	if e.Interval == 0 {
		e.Interval = 30 * time.Second
	}
	if e.FromUser == "" {
		e.FromUser = "infrapulse"
	}
	if e.FailureThreshold == 0 {
		e.FailureThreshold = 3
	}
	if e.LatencyThreshold == 0 {
		e.LatencyThreshold = 250 * time.Millisecond
	}
	return e
}

func (e Endpoint) Validate() error {
	e = e.WithDefaults()
	if e.DialAddress != "" {
		ip, err := netip.ParseAddr(e.DialAddress)
		if err != nil || ip.Zone() != "" {
			return errors.New("SIP dial address must be a numeric IP without a zone")
		}
	}
	if !token(e.ID, 128) || !host(e.Address) || e.Transport != "udp" && e.Transport != "tcp" && e.Transport != "tls" || e.Timeout < 10*time.Millisecond || e.Timeout > time.Minute || e.Interval < 10*time.Millisecond || e.Interval > 24*time.Hour || e.FailureThreshold > 100 || e.LatencyThreshold < time.Millisecond || e.LatencyThreshold > time.Minute || !token(e.FromUser, 128) || e.ToUser != "" && !token(e.ToUser, 128) || e.Domain != "" && !host(e.Domain) {
		return errors.New("invalid SIP endpoint: bounded ID, host, udp/tcp/tls transport and typed settings are required")
	}
	if e.Transport != "tls" && (e.TLS.CAFile != "" || e.TLS.ServerName != "") || len(e.TLS.CAFile) > 4096 || strings.ContainsAny(e.TLS.CAFile, "\r\n\x00") || e.TLS.ServerName != "" && !host(e.TLS.ServerName) {
		return errors.New("invalid SIP TLS settings")
	}
	return nil
}

func token(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}

func host(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	s = strings.TrimSuffix(s, ".")
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
