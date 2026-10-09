package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const redfishConfig = `security:
  mode: development
credentials:
  bmc:
    username_env: REDFISH_USER
    password_env: REDFISH_PASSWORD
devices:
  - id: bmc-01
    driver: redfish
    address: https://bmc.example.test
    credential_ref: bmc
`

func TestRedfishConfigDefaultsAndTarget(t *testing.T) {
	c, err := Parse([]byte(redfishConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	d := c.Devices[0]
	r := d.Redfish.WithDefaults()
	if r.Auth != "session" || r.Provenance != "real" || r.MaxResources != 128 || r.MaxPages != 8 || r.MaxRequests != 512 || r.RequestTimeout != 5*time.Second {
		t.Fatal("Redfish defaults changed", r)
	}
	target := d.Target(Polling{FastInterval: time.Second, MediumInterval: time.Second, InventoryInterval: time.Second, TopologyInterval: time.Second, Timeout: time.Second})
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	if target.ID != "remote:bmc-01" || target.ID == d.ID || string(target.Type) != "redfish" || target.Protocol != "redfish" || target.CredentialRef != "bmc" {
		t.Fatal("Redfish source identity/config projection", target)
	}
	if target.Polling.FastInterval != 0 || target.Polling.TopologyInterval != 0 || target.Polling.MediumInterval != 30*time.Second || target.Polling.InventoryInterval != 5*time.Minute || target.Polling.Timeout != 30*time.Second {
		t.Fatal("Redfish inherited legacy fast cadence/timeout", target.Polling)
	}
	d.Polling = Polling{MediumInterval: time.Minute, InventoryInterval: time.Hour, Timeout: time.Minute}
	target = d.Target(Polling{})
	if target.Polling.MediumInterval != time.Minute || target.Polling.InventoryInterval != time.Hour || target.Polling.Timeout != time.Minute {
		t.Fatal("Redfish polling overrides lost")
	}
}

func TestRedfishConfigurationRejectsUnsafeOrUnboundedInput(t *testing.T) {
	changes := map[string]func(*Config){
		"http":                   func(c *Config) { c.Devices[0].Address = "http://bmc.example.test" },
		"userinfo":               func(c *Config) { c.Devices[0].Address = "https://SECRET:bmc@bmc.example.test" },
		"path":                   func(c *Config) { c.Devices[0].Address += "/redfish/v1" },
		"query":                  func(c *Config) { c.Devices[0].Address += "?password=SECRET" },
		"empty query":            func(c *Config) { c.Devices[0].Address += "?" },
		"fragment":               func(c *Config) { c.Devices[0].Address += "#SECRET" },
		"escaped path":           func(c *Config) { c.Devices[0].Address += "/%2f" },
		"zero port":              func(c *Config) { c.Devices[0].Address = "https://bmc.example.test:0" },
		"oversized port":         func(c *Config) { c.Devices[0].Address = "https://bmc.example.test:65536" },
		"oversized address":      func(c *Config) { c.Devices[0].Address = "https://" + strings.Repeat("a", 4096) },
		"mutations":              func(c *Config) { c.Devices[0].EnableMutations = true },
		"missing credential":     func(c *Config) { c.Devices[0].CredentialRef = "missing" },
		"community":              func(c *Config) { r := c.Credentials["bmc"]; r.CommunityEnv = "COMMUNITY"; c.Credentials["bmc"] = r },
		"auth":                   func(c *Config) { c.Devices[0].Redfish.Auth = "auto" },
		"provenance":             func(c *Config) { c.Devices[0].Redfish.Provenance = "test" },
		"resources":              func(c *Config) { c.Devices[0].Redfish.MaxResources = 1025 },
		"resources remote bound": func(c *Config) { c.Remote.MaxRows = 64 },
		"pages":                  func(c *Config) { c.Devices[0].Redfish.MaxPages = 65 },
		"requests":               func(c *Config) { c.Devices[0].Redfish.MaxRequests = 4097 },
		"request timeout":        func(c *Config) { c.Devices[0].Redfish.RequestTimeout = 31 * time.Second },
		"negative resources":     func(c *Config) { c.Devices[0].Redfish.MaxResources = -1 },
		"negative pages":         func(c *Config) { c.Devices[0].Redfish.MaxPages = -1 },
		"negative requests":      func(c *Config) { c.Devices[0].Redfish.MaxRequests = -1 },
		"fast polling":           func(c *Config) { c.Devices[0].Polling.FastInterval = time.Second },
		"topology polling":       func(c *Config) { c.Devices[0].Polling.TopologyInterval = time.Second },
		"poll timeout":           func(c *Config) { c.Devices[0].Polling.Timeout = 6 * time.Minute },
		"foreign driver":         func(c *Config) { c.Devices[0].Driver = "routeros"; c.Devices[0].Redfish.Auth = "session" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(redfishConfig))
			if err != nil {
				t.Fatal(err)
			}
			change(&c)
			if err := c.Validate(); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("invalid config accepted or configuration value leaked", err)
			}
		})
	}
	if _, err := Parse([]byte(redfishConfig + "    redfish:\n      insecure_skip_verify: true\n")); err == nil {
		t.Fatal("accepted insecure TLS option")
	}
}

func TestRedfishBasicAuthenticationAndSimulatedProvenanceAreExplicit(t *testing.T) {
	c, _ := Parse([]byte(redfishConfig))
	c.Devices[0].Redfish.Auth = "basic"
	c.Devices[0].Redfish.Provenance = "simulated"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Security.Mode = "production"
	c.Security.CAFile, c.Security.CertFile, c.Security.KeyFile = "ca.pem", "cert.pem", "key.pem"
	if err := c.Validate(); err == nil {
		t.Fatal("production simulated provenance accepted")
	}
	c.Devices[0].Redfish.Provenance = "real"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRedfishCustomCAPathResolvesFromYAMLDirectory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "collector.yaml")
	if err := os.WriteFile(path, []byte(redfishConfig+"    tls:\n      ca_file: certs/bmc-ca.pem\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Devices[0].TLS.CAFile != filepath.Join(directory, "certs", "bmc-ca.pem") {
		t.Fatal("CA path escaped YAML directory convention")
	}
}
