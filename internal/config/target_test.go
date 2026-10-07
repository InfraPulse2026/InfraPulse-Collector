package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestConfiguredDeviceTargetPreservesLegacyConfiguration(t *testing.T) {
	cfg := Defaults()
	d := Device{ID: "switch", Name: "Switch", Driver: "snmp", Address: "192.0.2.10", CredentialRef: "credential-reference",
		Polling: Polling{FastInterval: 5 * time.Second}, Location: model.PhysicalLocation{Site: "site-a"}}
	defaults := Polling{FastInterval: cfg.Telemetry.FastInterval, MediumInterval: cfg.Telemetry.MediumInterval,
		InventoryInterval: cfg.Telemetry.InventoryInterval, TopologyInterval: cfg.Telemetry.TopologyInterval, Timeout: cfg.Telemetry.CollectTimeout}
	target := d.Target(defaults)
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	if target.ID != "remote:switch" || target.ID == d.ID || target.Name != d.Name || target.Type != model.SourceSNMP || target.Protocol != "snmp" || target.Endpoint != d.Address || target.CredentialRef != d.CredentialRef || !target.Enabled || target.Location.Site != d.Location.Site {
		t.Fatalf("target adaptation lost configured connection metadata: %+v", target)
	}
	if target.Polling.FastInterval != 5*time.Second || target.Polling.MediumInterval != defaults.MediumInterval || target.Polling.InventoryInterval != defaults.InventoryInterval || target.Polling.TopologyInterval != defaults.TopologyInterval || target.Polling.Timeout != defaults.Timeout {
		t.Fatal("target overrides or defaults changed")
	}
	if d.ID != "switch" || d.Polling.Timeout != 0 {
		t.Fatal("target adaptation mutated legacy configuration")
	}
	data, err := json.Marshal(target)
	if err != nil || strings.Contains(string(data), d.CredentialRef) {
		t.Fatal("target diagnostics leaked credential reference")
	}
}

func TestConfiguredTargetRetainsThreeSecondDefault(t *testing.T) {
	c := Defaults()
	d := Device{ID: "router", Driver: "routeros", Address: "https://192.0.2.20", CredentialRef: "router"}
	target := d.Target(Polling{FastInterval: c.Telemetry.FastInterval, Timeout: c.Telemetry.CollectTimeout})
	if target.Polling.FastInterval != 3*time.Second || target.Type != model.SourceRouterOS || target.ID != "remote:router" {
		t.Fatal("collector target changed legacy fast interval or source domain")
	}
}
