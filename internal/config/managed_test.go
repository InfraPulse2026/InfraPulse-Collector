package config

import (
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
)

func TestRemoteManagementOptInAndStrictLocalOwnership(t *testing.T) {
	c := Defaults()
	c.Security.Mode = "development"
	if c.RemoteManagement.Enabled {
		t.Fatal("managed mode enabled by default")
	}
	if c.Validate() != nil {
		t.Fatal("old defaults broken")
	}
	for _, field := range []string{"pki", "private_key", "backend_endpoint", "time_sync", "syslog_receiver", "outbox_path", "state_dir", "commands", "shell", "insecure_skip_verify"} {
		if _, e := Parse([]byte("remote_management:\n  " + field + ": unsafe\n")); e == nil {
			t.Fatal("unknown administrative field accepted", field)
		}
	}
	c.RemoteManagement.Enabled = true
	if c.Validate() == nil {
		t.Fatal("plaintext management permitted")
	}
	c.Security.Mode = "production"
	c.Security.CAFile = "ca"
	c.Security.CertFile = "cert"
	c.Security.KeyFile = "key"
	if c.Validate() != nil {
		t.Fatal("production opt-in failed")
	}
	for _, tc := range []struct {
		name   string
		update func(*Config)
	}{
		{"targets", func(c *Config) { c.RemoteManagement.MaxManagedTargets = 1025 }},
		{"payload", func(c *Config) { c.RemoteManagement.MaxConfigBytes = 2 << 20 }},
		{"interval", func(c *Config) { c.RemoteManagement.MinPollingInterval = time.Millisecond }},
		{"apply", func(c *Config) { c.RemoteManagement.ApplyTimeout = 2 * time.Minute }},
		{"protocol", func(c *Config) { c.RemoteManagement.AllowedProtocols = []string{"shell"} }},
		{"cidr", func(c *Config) { c.RemoteManagement.AllowedDestinations = []string{"0.0.0.0/0; rm"} }},
		{"credential", func(c *Config) { c.RemoteManagement.AllowedCredentialRefs = []string{"unconfigured"} }},
		{"ca", func(c *Config) { c.RemoteManagement.TLSCARefs = map[string]string{"key": "\x00"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := c
			tc.update(&copy)
			if copy.Validate() == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	c.Credentials = map[string]credentials.Reference{"read": {CommunityEnv: "READ"}}
	c.RemoteManagement.AllowedCredentialRefs = []string{"read"}
	if c.Validate() != nil {
		t.Fatal("existing credential ref rejected")
	}
	c.RemoteManagement.TLSCARefs = map[string]string{"bmc": "certs/bmc.pem"}
	ResolveManagedPaths(&c, t.TempDir())
	if strings.HasPrefix(c.RemoteManagement.TLSCARefs["bmc"], "certs") {
		t.Fatal("local CA relative path not resolved")
	}
}
