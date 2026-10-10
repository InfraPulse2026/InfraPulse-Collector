package config

import (
	"strings"
	"testing"
	"time"
)

const ipmiConfig = `security:
  mode: development
credentials:
  bmc:
    username_env: IPMI_USER
    password_env: IPMI_PASSWORD
devices:
  - id: legacy-01
    driver: ipmi
    address: bmc.example.test
    credential_ref: bmc
`

func TestIPMIConfigDefaultsAndExplicitCipher(t *testing.T) {
	c, err := Parse([]byte(ipmiConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	i := c.Devices[0].IPMI.WithDefaults()
	if *i.Port != 623 || *i.CipherSuite != 17 || i.Privilege != "user" || i.Retries != 0 || i.MaxOperations != 1024 || i.MaxSDRRecords != 128 || i.MaxFRURecords != 16 {
		t.Fatal("unsafe IPMI defaults", i)
	}
	target := c.Devices[0].Target(Polling{FastInterval: 3 * time.Second, Timeout: 2 * time.Second})
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	if target.ID != "remote:legacy-01" || target.Protocol != "ipmi" || target.Polling.FastInterval != 0 || target.Polling.TopologyInterval != 0 || target.Polling.MediumInterval != 30*time.Second || target.Polling.InventoryInterval != 5*time.Minute || target.Polling.Timeout != 30*time.Second {
		t.Fatal("incorrect hardware target", target)
	}
	for _, host := range []string{"192.0.2.1", "2001:db8::1", "bmc.example.test."} {
		c.Devices[0].Address = host
		suite, port := 3, 1623
		c.Devices[0].IPMI.CipherSuite, c.Devices[0].IPMI.Port = &suite, &port
		c.Devices[0].IPMI.Privilege = "operator"
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIPMIConfigRejectsInvalidAndUnsafeSettings(t *testing.T) {
	changes := map[string]func(*Config){
		"url":                   func(c *Config) { c.Devices[0].Address = "https://bmc.test" },
		"port-in-host":          func(c *Config) { c.Devices[0].Address = "bmc.test:623" },
		"bad-host":              func(c *Config) { c.Devices[0].Address = "bad@bmc" },
		"empty-host":            func(c *Config) { c.Devices[0].Address = "" },
		"host-controls":         func(c *Config) { c.Devices[0].Address = "bmc\x01" },
		"port-zero":             func(c *Config) { v := 0; c.Devices[0].IPMI.Port = &v },
		"port-large":            func(c *Config) { v := 65536; c.Devices[0].IPMI.Port = &v },
		"cipher-zero":           func(c *Config) { v := 0; c.Devices[0].IPMI.CipherSuite = &v },
		"cipher-no-encryption":  func(c *Config) { v := 16; c.Devices[0].IPMI.CipherSuite = &v },
		"cipher-md5":            func(c *Config) { v := 8; c.Devices[0].IPMI.CipherSuite = &v },
		"null-auth":             func(c *Config) { c.Devices[0].IPMI.Privilege = "null" },
		"admin":                 func(c *Config) { c.Devices[0].IPMI.Privilege = "administrator" },
		"writes":                func(c *Config) { c.Devices[0].EnableMutations = true },
		"https-options":         func(c *Config) { c.Devices[0].TLS.CAFile = "ca.pem" },
		"bad-credential":        func(c *Config) { c.Devices[0].CredentialRef = "missing" },
		"community":             func(c *Config) { r := c.Credentials["bmc"]; r.CommunityEnv = "COMMUNITY"; c.Credentials["bmc"] = r },
		"no-password":           func(c *Config) { r := c.Credentials["bmc"]; r.PasswordEnv = ""; c.Credentials["bmc"] = r },
		"retry":                 func(c *Config) { c.Devices[0].IPMI.Retries = 4 },
		"negative-retry":        func(c *Config) { c.Devices[0].IPMI.Retries = -1 },
		"timeout":               func(c *Config) { c.Devices[0].IPMI.RequestTimeout = time.Hour },
		"session":               func(c *Config) { c.Devices[0].IPMI.MaxSessionDuration = time.Hour },
		"operations":            func(c *Config) { c.Devices[0].IPMI.MaxOperations = 8193 },
		"sdr-records":           func(c *Config) { c.Devices[0].IPMI.MaxSDRRecords = 1025 },
		"sdr-bytes":             func(c *Config) { c.Devices[0].IPMI.MaxSDRBytes = 2 << 20 },
		"fru-records":           func(c *Config) { c.Devices[0].IPMI.MaxFRURecords = 129 },
		"fru-bytes":             func(c *Config) { c.Devices[0].IPMI.MaxFRUBytes = 65537 },
		"poll-bytes":            func(c *Config) { c.Devices[0].IPMI.MaxPollBytes = 9 << 20 },
		"cache":                 func(c *Config) { c.Devices[0].IPMI.SDRCacheTTL = 24 * time.Hour },
		"fast":                  func(c *Config) { c.Devices[0].Polling.FastInterval = 3 * time.Second },
		"topology":              func(c *Config) { c.Devices[0].Polling.TopologyInterval = time.Minute },
		"negative-interval":     func(c *Config) { c.Devices[0].Polling.MediumInterval = -time.Second },
		"duplicate":             func(c *Config) { c.Devices = append(c.Devices, c.Devices[0]) },
		"bad-id":                func(c *Config) { c.Devices[0].ID = "local:reserved" },
		"wrong-driver":          func(c *Config) { v := 17; c.Devices[0].IPMI.CipherSuite = &v; c.Devices[0].Driver = "snmp" },
		"production-simulation": func(c *Config) { c.Security.Mode = "production"; c.Devices[0].IPMI.Provenance = "simulated" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c, _ := Parse([]byte(ipmiConfig))
			change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	for _, extra := range []string{"    ipmi:\n      cipher_suite: 0\n", "    ipmi:\n      version: v1.5\n", "    ipmi:\n      port: 0\n", "    ipmi:\n      unknown: true\n"} {
		c, e := Parse([]byte(ipmiConfig + extra))
		if e == nil && c.Validate() == nil {
			t.Fatal("unsafe/unknown YAML accepted", strings.TrimSpace(extra))
		}
	}
}

func TestIPMIPollBudgetCoversEffectiveResponseLimit(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		responseBytes, budget int
		valid                 bool
	}{
		{"default_response_small_budget", 1 << 20, 260, false},
		{"default_response_below_boundary", 1 << 20, 4095, false},
		{"default_response_boundary", 1 << 20, 4096, true},
		{"reduced_response_below_boundary", 512, 511, false},
		{"reduced_response_boundary", 512, 512, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(ipmiConfig))
			if err != nil {
				t.Fatal(err)
			}
			c.Remote.MaxResponseBytes = tt.responseBytes
			c.Devices[0].IPMI.MaxFRUBytes = 8
			c.Devices[0].IPMI.MaxSDRBytes = 260
			c.Devices[0].IPMI.MaxPollBytes = tt.budget
			err = c.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("response=%d poll=%d: valid=%v error=%v", tt.responseBytes, tt.budget, tt.valid, err)
			}
			if !tt.valid && !strings.Contains(err.Error(), "invalid ipmi max_poll_bytes") {
				t.Fatalf("wrong budget validation error: %v", err)
			}
		})
	}
}
