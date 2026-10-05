package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSNMPv3StrictConfigAndCredentialKinds(t *testing.T) {
	text := "security:\n  mode: development\ncredentials:\n  secure-switch:\n    username_env: SNMP_USER\n    auth_password_env: SNMP_AUTH\n    privacy_password_env: SNMP_PRIV\ndevices:\n  - id: secure-switch\n    address: 192.0.2.10\n    driver: snmp\n    credential_ref: secure-switch\n    snmp:\n      version: v3\n"
	c, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if s := c.Devices[0].SNMP.Security.WithDefaults(); s.SecurityLevel != "authPriv" || s.AuthProtocol != "SHA256" || s.PrivacyProtocol != "AES" {
		t.Fatal(s)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Devices[0].SNMP.SecurityLevel = "noAuthNoPriv" },
		func(c *Config) { c.Devices[0].SNMP.AuthProtocol = "invented-secret-algorithm" },
		func(c *Config) {
			r := c.Credentials["secure-switch"]
			r.PrivacyPasswordEnv = ""
			c.Credentials["secure-switch"] = r
		},
		func(c *Config) {
			r := c.Credentials["secure-switch"]
			r.CommunityEnv = "SNMP_COMMUNITY"
			c.Credentials["secure-switch"] = r
		},
	} {
		bad, _ := Parse([]byte(text))
		mutate(&bad)
		if err := bad.Validate(); err == nil || strings.Contains(err.Error(), "invented-secret-algorithm") {
			t.Fatalf("accepted or unsafe config error: %v", err)
		}
	}
	c.Devices[0].SNMP.SecurityLevel = "authNoPriv"
	r := c.Credentials["secure-switch"]
	r.PrivacyPasswordEnv = ""
	c.Credentials["secure-switch"] = r
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(text + "      auth_password: plaintext-secret\n")); err == nil || strings.Contains(err.Error(), "plaintext-secret") {
		t.Fatal("plaintext secret accepted or leaked")
	}
}

func TestSIPTLSCAPathResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	text := "security:\n  mode: development\ntelephony:\n  endpoints:\n    - id: tls-pbx\n      address: pbx.example.net\n      transport: tls\n      tls:\n        ca_file: certs/ca.pem\n        server_name: pbx.example.net\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telephony.Endpoints[0].TLS.CAFile != filepath.Join(dir, "certs", "ca.pem") {
		t.Fatal(c.Telephony.Endpoints[0])
	}
}
