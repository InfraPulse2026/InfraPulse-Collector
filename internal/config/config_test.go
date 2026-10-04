package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndValidation(t *testing.T) {
	c, e := Parse([]byte("security:\n  mode: development\n"))
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Validate(); e != nil {
		t.Fatal(e)
	}
	if c.Telemetry.FastInterval != 3*time.Second || !c.Local.Enabled {
		t.Fatal("defaults lost")
	}
}
func TestStrictYAML(t *testing.T) {
	for _, s := range []string{"unknown: true", "telemetry:\n  fast_interval: nope", "security: {}\n---\nsecurity: {}", "agent:\n  name: a\n  name: b"} {
		if _, e := Parse([]byte(s)); e == nil {
			t.Fatalf("accepted invalid YAML %q", s)
		}
	}
}
func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{{"production TLS", func(c *Config) { c.Security.Mode = "production" }}, {"remote plaintext", func(c *Config) { c.Controller.Address = "192.0.2.1:9443" }}, {"fast interval", func(c *Config) { c.Telemetry.FastInterval = 0 }}, {"queue", func(c *Config) { c.Queue.MaxBytes = 0 }}, {"backoff", func(c *Config) { c.Controller.ReconnectMax = time.Millisecond }}, {"workers", func(c *Config) { c.Scheduler.Concurrency = 0 }}, {"duplicate", func(c *Config) {
		c.Devices = []Device{{ID: "x", Driver: "snmp", Address: "localhost", CredentialRef: "ref"}, {ID: "x", Driver: "snmp", Address: "localhost", CredentialRef: "ref"}}
	}}, {"embedded secret", func(c *Config) {
		c.Devices = []Device{{ID: "x", Driver: "routeros", Address: "https://user:secret@localhost", CredentialRef: "ref"}}
	}}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c := Defaults()
			c.Security.Mode = "development"
			tt.change(&c)
			e := c.Validate()
			if e == nil {
				t.Fatal("accepted invalid config")
			}
			if strings.Contains(e.Error(), "secret") {
				t.Fatal("secret echoed")
			}
		})
	}
}
func TestLoadPathsAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if e := os.WriteFile(path, []byte("security:\n  mode: development\nagent:\n  state_dir: data\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := Load(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.Agent.StateDir != filepath.Join(dir, "data") {
		t.Fatal(c.Agent.StateDir)
	}
	t.Setenv("INFRAPULSE_STATE_DIR", filepath.Join(dir, "override"))
	c, e = Load(path)
	if e != nil || c.Agent.StateDir != filepath.Join(dir, "override") {
		t.Fatal(c, e)
	}
}
