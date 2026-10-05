package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestServiceDisappearanceAndFirewallRefreshDoNotCacheStaleState(t *testing.T) {
	good := fixtureRunner(t)
	serviceData := []byte(`{"dnsmasq":{"instances":{"main":{"running":true,"pid":100}}},"dropbear":{"instances":{"main":{"running":true,"pid":200}}}}`)
	firewallData := []byte(`{"values":{"lan":{".type":"zone","name":"lan"},"allow":{".type":"rule","name":"allow","dest_port":"443","enabled":"1"}}}`)
	p := fixturePlatform(t, runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		switch op {
		case Services:
			return serviceData, nil
		case Firewall:
			return firewallData, nil
		default:
			return good.Run(ctx, op)
		}
	}))
	first, err := p.Identity(context.Background())
	if err != nil || len(first.Services) != 2 || len(first.Firewall.Rules) != 1 || first.Firewall.Rules[0].DestinationPort != "443" {
		t.Fatal(first, err)
	}
	serviceData = []byte(`{"dnsmasq":{"instances":{"main":{"running":false}}}}`)
	firewallData = []byte(`{"values":{"lan":{".type":"zone","name":"lan"},"allow":{".type":"rule","name":"allow","dest_port":"8443","enabled":"0"}}}`)
	second, err := p.Identity(context.Background())
	if err != nil || len(second.Services) != 1 || second.Services[0].Name != "dnsmasq" || second.Services[0].State != "stopped" || second.Firewall.Rules[0].DestinationPort != "8443" || second.Firewall.Rules[0].Enabled == nil || *second.Firewall.Rules[0].Enabled {
		t.Fatal(second, err)
	}
	if len(first.Services) != 2 || first.Firewall.Rules[0].DestinationPort != "443" || !*first.Firewall.Rules[0].Enabled {
		t.Fatal("later read mutated prior normalized snapshot")
	}
	serviceData = []byte(`{}`)
	firewallData = []byte(`{"values":{}}`)
	empty, err := p.Identity(context.Background())
	if err != nil || len(empty.Services) != 0 || empty.Firewall.Completeness != "complete" || len(empty.Firewall.Rules) != 0 || !capability(empty, "services.read") || !capability(empty, "firewall.read") {
		t.Fatal(empty, err)
	}
}

func TestRPCFailureAndPartialConfigurationPreserveUnavailableEvidence(t *testing.T) {
	good := fixtureRunner(t)
	failure := false
	firewallData := fixture(t, "firewall.json")
	p := fixturePlatform(t, runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		if failure && (op == Services || op == Firewall) {
			return nil, errors.New("RPC permission denied token=do-not-forward")
		}
		if op == Firewall {
			return firewallData, nil
		}
		return good.Run(ctx, op)
	}))
	baseline, err := p.Identity(context.Background())
	if err != nil || !capability(baseline, "services.read") || !capability(baseline, "firewall.read") {
		t.Fatal(err)
	}
	baselineTelemetry, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failure = true
	failed, err := p.Identity(context.Background())
	if err != nil || failed.Firewall.Completeness != "failed" || capability(failed, "services.read") || capability(failed, "firewall.read") || !capability(failed, "interfaces.read") {
		t.Fatal(failed, err)
	}
	encoded, _ := json.Marshal(failed)
	if strings.Contains(string(encoded), "do-not-forward") {
		t.Fatal("RPC secret leaked")
	}
	failure = false
	firewallData = []byte(`{"values":{"unsupported":{".type":"include","path":"secret-path"},"lan":{".type":"zone","name":"lan"}}}`)
	partial, err := p.Identity(context.Background())
	if err != nil || partial.Firewall.Completeness != "partial" || len(partial.Firewall.Zones) != 1 || !capability(partial, "firewall.read") {
		t.Fatal(partial, err)
	}
	encoded, _ = json.Marshal(partial)
	if strings.Contains(string(encoded), "secret-path") {
		t.Fatal("unsupported raw UCI option escaped")
	}
	firewallData = []byte(`{"values":{"bad":{".type":"rule","enabled":["1"]}}}`)
	malformed, err := p.Identity(context.Background())
	if err != nil || malformed.Firewall.Completeness != "failed" || capability(malformed, "firewall.read") {
		t.Fatal(malformed, err)
	}
	telemetry, err := p.Collect(context.Background())
	if err != nil || !reflect.DeepEqual(telemetry.Interfaces, baselineTelemetry.Interfaces) {
		t.Fatal("optional RPC failure destroyed Linux interfaces")
	}
	for _, value := range []model.Device{failed, partial, malformed} {
		if value.Driver != "local-openwrt" || value.UptimeSeconds <= 0 {
			t.Fatal("Linux fallback lost")
		}
	}
}
