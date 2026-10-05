package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"InfraPulseAgent/internal/telephony"
)

func TestP2ConfigBoundsAndTypedOperations(t *testing.T) {
	for _, change := range []func(*Config){func(c *Config) { c.Local.Platform = "shell" }, func(c *Config) { c.Logs.MaxReadBytes = c.Logs.MaxLineBytes }, func(c *Config) {
		c.Logs.Sources = []LogSource{{ID: "x", Path: "/var/log/syslog", Format: "syslog"}, {ID: "x", Path: "/var/log/messages", Format: "raw"}}
	}, func(c *Config) {
		c.Logs.Sources = []LogSource{{ID: "x", Path: "/var/log/syslog", Format: "syslog"}}
		c.Queue.MaxMessageBytes = 4096
	}, func(c *Config) { c.Logs.Sources = []LogSource{{ID: "x", Path: "/var/log/syslog", Format: "command"}} }, func(c *Config) {
		c.Telephony.Endpoints = []telephony.Endpoint{{ID: "pbx", Address: "host\r\nVia: injected"}}
	}, func(c *Config) {
		c.Telephony.Endpoints = []telephony.Endpoint{{ID: "pbx", Address: "localhost", Transport: "ws"}}
	}, func(c *Config) {
		c.Telephony.Endpoints = []telephony.Endpoint{{ID: "pbx", Address: "localhost"}, {ID: "pbx", Address: "localhost"}}
	}, func(c *Config) { c.Telephony.Concurrency = 100 }} {
		c := Defaults()
		c.Security.Mode = "development"
		change(&c)
		if c.Validate() == nil {
			t.Fatal("invalid P2 config accepted")
		}
	}
	if _, err := Parse([]byte("security:\n  mode: development\nlogs:\n  shell: true\n")); err == nil {
		t.Fatal("arbitrary shell config")
	}
}
func TestP2SourcePathsAndDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("security:\n  mode: development\nlocal:\n  platform: auto\nlogs:\n  sources:\n    - id: audit\n      path: audit.log\n      format: auditd\ntelephony:\n  endpoints:\n    - id: pbx\n      address: localhost\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Logs.Sources[0].Path != filepath.Join(dir, "audit.log") {
		t.Fatal("relative log path")
	}
	ep := c.Telephony.Endpoints[0].WithDefaults()
	if ep.Port != 5060 || ep.Transport != "udp" || ep.Timeout != 2*time.Second {
		t.Fatal("SIP defaults")
	}
}
