package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"InfraPulseAgent/internal/syslogreceiver"
)

func syslogConfig() Config {
	c := Defaults()
	c.Local.Enabled = false
	c.Security.Mode = "development"
	c.Queue.Mode = "durable"
	c.SyslogReceiver.Enabled = true
	c.SyslogReceiver.TLS.Enabled = false
	c.SyslogReceiver.TCP.Enabled = true
	c.SyslogReceiver.Sources = []syslogreceiver.Source{{ID: "server-a", PeerCIDRs: []string{"127.0.0.1/32"}, AuditApps: []string{"audispd"}}}
	return c
}

func TestSyslogReceiverStrictConfiguration(t *testing.T) {
	c := syslogConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"memory":          func(c *Config) { c.Queue.Mode = "memory" },
		"send fallback":   func(c *Config) { c.Queue.ACKFallback = "send" },
		"invalid port":    func(c *Config) { c.SyslogReceiver.TCP.Listen = "127.0.0.1:0" },
		"invalid address": func(c *Config) { c.SyslogReceiver.TCP.Listen = "collector.example:1514" },
		"public insecure": func(c *Config) { c.SyslogReceiver.TCP.Listen = "0.0.0.0:1514" },
		"public no ACL": func(c *Config) {
			c.SyslogReceiver.TCP.Listen = "0.0.0.0:1514"
			c.SyslogReceiver.TCP.AllowNonLoopback = true
		},
		"invalid framing":     func(c *Config) { c.SyslogReceiver.TCP.Framing = "auto" },
		"invalid CIDR":        func(c *Config) { c.SyslogReceiver.AllowedPeers = []string{"all"} },
		"no listeners":        func(c *Config) { c.SyslogReceiver.TCP.Enabled = false },
		"message size":        func(c *Config) { c.SyslogReceiver.Limits.MaxMessageBytes = 1 << 30 },
		"batch budget":        func(c *Config) { c.SyslogReceiver.Limits.MaxMessageBytes = 65536 },
		"outbox budget":       func(c *Config) { c.Queue.MaxMessageBytes = 4096 },
		"zero connections":    func(c *Config) { c.SyslogReceiver.Limits.MaxConnections = 0 },
		"source connections":  func(c *Config) { c.SyslogReceiver.Limits.MaxConnectionsPerSource = 65 },
		"timeout":             func(c *Config) { c.SyslogReceiver.Limits.ReadTimeout = 24 * time.Hour },
		"unknown policy":      func(c *Config) { c.SyslogReceiver.UnknownSourcePolicy = "allow" },
		"fake TLS selector":   func(c *Config) { c.SyslogReceiver.Sources[0].TLSIdentity = "dns:host.example" },
		"hostname selector":   func(c *Config) { c.SyslogReceiver.Sources[0].PeerCIDRs = nil },
		"source control":      func(c *Config) { c.SyslogReceiver.Sources[0].ID = "injected\nline" },
		"audit app injection": func(c *Config) { c.SyslogReceiver.Sources[0].AuditApps = []string{"bad\napp"} },
		"duplicate IDs": func(c *Config) {
			c.SyslogReceiver.Sources = append(c.SyslogReceiver.Sources, c.SyslogReceiver.Sources[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := syslogConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid receiver configuration accepted")
			}
		})
	}
	for _, input := range []string{
		"syslog_receiver:\n  unknown_field: true\n",
		"syslog_receiver:\n  tls:\n    insecure_skip_verify: true\n",
		"syslog_receiver:\n  udp:\n    enabled: true\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatal("unknown YAML accepted")
		}
	}
}

func TestSyslogReceiverDisabledAndExplicitPublicCompatibility(t *testing.T) {
	c := syslogConfig()
	c.SyslogReceiver.Enabled = false
	c.Queue.Mode = "memory"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c = syslogConfig()
	c.SyslogReceiver.TCP.Listen = "0.0.0.0:1514"
	c.SyslogReceiver.TCP.AllowNonLoopback = true
	c.SyslogReceiver.AllowedPeers = []string{"192.0.2.0/24"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SyslogReceiver.Limits.MaxMessageBytes = 65536
	c.Logs.MaxBatchBytes = 1 << 20
	c.Queue.MaxMessageBytes = 2 << 20
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSyslogReceiverRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "local:\n  enabled: false\nsecurity:\n  mode: development\nsyslog_receiver:\n  tls:\n    cert_file: syslog/server.crt\n    key_file: syslog/server.key\n    client_ca_file: syslog/ca.crt\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.SyslogReceiver.Enabled || c.SyslogReceiver.TLS.CertFile != filepath.Join(dir, "syslog", "server.crt") || c.SyslogReceiver.TLS.KeyFile != filepath.Join(dir, "syslog", "server.key") || c.SyslogReceiver.TLS.ClientCAFile != filepath.Join(dir, "syslog", "ca.crt") {
		t.Fatalf("unexpected receiver defaults/paths: %+v", c.SyslogReceiver)
	}
}
