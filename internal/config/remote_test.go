package config

import (
	"strings"
	"testing"
)

func TestRemoteConfigReferencesAndLimits(t *testing.T) {
	text := "security:\n  mode: development\ncredentials:\n  switch:\n    community_env: SWITCH_COMMUNITY\ndevices:\n  - id: stable-switch\n    name: Switch\n    address: 192.0.2.10\n    driver: snmp\n    credential_ref: switch\n    location:\n      rack: R1\n    polling:\n      fast_interval: 5s\n"
	c, err := Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Devices[0].Location.Rack != "R1" || c.Remote.Concurrency != 4 {
		t.Fatal("remote defaults/mapping")
	}
	for _, change := range []func(*Config){func(c *Config) { c.Remote.Concurrency = 0 }, func(c *Config) { c.Devices[0].CredentialRef = "missing" }, func(c *Config) { c.Devices[0].ID = "local:reserved" }, func(c *Config) { c.Devices[0].EnableMutations = true }, func(c *Config) { c.Devices[0].Address = "https://secret@example" }} {
		bad, _ := Parse([]byte(text))
		change(&bad)
		if e := bad.Validate(); e == nil || strings.Contains(e.Error(), "secret") {
			t.Fatal("accepted invalid config or leaked value")
		}
	}
}
