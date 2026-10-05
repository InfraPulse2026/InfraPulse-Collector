package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDistributionDetection(t *testing.T) {
	for _, tc := range []struct {
		name, file, contents string
		expected             bool
	}{
		{"release", "openwrt_release", "DISTRIB_ID='OpenWrt'\nDISTRIB_RELEASE='24.10.0'\n", true},
		{"os release", "os-release", "ID=openwrt\nNAME=OpenWrt\n", true},
		{"hostname insufficient", "os-release", "ID=linux\nNAME=OpenWrt-router\n", false},
		{"missing ID", "openwrt_release", "DISTRIB_DESCRIPTION='OpenWrt 24.10.0'", false},
		{"wrong distribution", "openwrt_release", "DISTRIB_ID='Other'", false},
		{"malformed quote", "openwrt_release", "DISTRIB_ID='OpenWrt", false},
		{"malformed input", "openwrt_release", "DISTRIB_ID='OpenWrt'\ninvalid-line", false},
		{"oversized", "openwrt_release", "DISTRIB_ID='OpenWrt'\n" + strings.Repeat("#", maxReleaseBytes), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.contents), 0600); err != nil {
				t.Fatal(err)
			}
			if got := Detect(dir); got != tc.expected {
				t.Fatalf("Detect = %v, want %v", got, tc.expected)
			}
		})
	}
	if Detect(t.TempDir()) {
		t.Fatal("missing source detected as OpenWrt")
	}
}

func TestReleaseIsDataAndPreservesMissingFields(t *testing.T) {
	release, err := parseRelease("DISTRIB_ID='OpenWrt'\nDISTRIB_DESCRIPTION=\"router \\\"fixture\\\"\"\nUNKNOWN='$(touch never-executed)'\n")
	if err != nil || release["DISTRIB_DESCRIPTION"] != `router "fixture"` || release["UNKNOWN"] != "$(touch never-executed)" || release["DISTRIB_RELEASE"] != "" {
		t.Fatalf("release = %+v, %v", release, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readFile(ctx, "testdata/openwrt_release", maxReleaseBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestBoardSchemaAndMissingOptionalData(t *testing.T) {
	b, err := parseBoard(fixture(t, "board.json"))
	if err != nil || b.model != "Schema fixture board" || b.version != "24.10.0" || b.name != "fixture,schema-board" {
		t.Fatalf("board = %+v, %v", b, err)
	}
	missing, err := parseBoard([]byte(`{"hostname":"host","release":{"distribution":"OpenWrt"}}`))
	if err != nil || missing.model != "" || missing.version != "" || missing.hostname != "host" {
		t.Fatalf("missing values fabricated: %+v, %v", missing, err)
	}
	for _, value := range []string{
		`[]`, `null`, `{"hostname":32}`, `{"model":null}`, `{"release":false}`,
		`{"release":{"version":true}}`, `{"release":{"distribution":"Other"}}`,
		`{"hostname":"` + strings.Repeat("x", maxFieldBytes+1) + `"}`,
		`{"hostname":"line\nline"}`, `{} trailing`,
	} {
		if _, err := parseBoard([]byte(value)); err == nil {
			t.Fatalf("malformed board accepted: %s", value)
		}
	}
}

func TestNetworkSchema(t *testing.T) {
	observed := time.Unix(100, 0).UTC()
	networks, err := parseNetworks(fixture(t, "networks.json"), observed)
	if err != nil || len(networks) != 2 {
		t.Fatalf("networks = %+v, %v", networks, err)
	}
	lan, wan := networks[0], networks[1]
	if lan.Name != "lan" || lan.Device != "br-lan" || lan.Protocol != "static" || lan.Up == nil || !*lan.Up ||
		lan.Pending == nil || *lan.Pending || !reflect.DeepEqual(lan.Addresses, []string{"192.0.2.1/24", "2001:db8::1/64"}) ||
		lan.Source != "ubus.network.interface.dump" || !lan.ObservedAt.Equal(observed) {
		t.Fatalf("lan = %+v", lan)
	}
	if wan.Up == nil || *wan.Up || wan.Pending == nil || !*wan.Pending || len(wan.Addresses) != 0 {
		t.Fatalf("wan = %+v", wan)
	}
	missing, err := parseNetworks([]byte(`{"interface":[{"interface":"minimal","l3_device":"eth0"}]}`), observed)
	if err != nil || missing[0].Up != nil || missing[0].Pending != nil || missing[0].Device != "eth0" {
		t.Fatalf("missing network values = %+v, %v", missing, err)
	}
	for _, value := range []string{
		`{}`, `{"interface":null}`, `{"interface":{}}`, `{"interface":[{}]}`,
		`{"interface":[{"interface":"lan","up":"true"}]}`,
		`{"interface":[{"interface":"lan","ipv4-address":[{"address":"garbage","mask":24}]}]}`,
		`{"interface":[{"interface":"lan","ipv4-address":[{"address":"192.0.2.1","mask":33}]}]}`,
		`{"interface":[{"interface":"lan","ipv4-address":[{"address":"192.0.2.1"}]}]}`,
		`{"interface":[{"interface":"lan","ipv6-address":[{"address":"192.0.2.1","mask":24}]}]}`,
	} {
		if _, err := parseNetworks([]byte(value), observed); err == nil {
			t.Fatalf("malformed network accepted: %s", value)
		}
	}
}

func TestProcdServicesStatusIsEvidenceBased(t *testing.T) {
	observed := time.Unix(101, 0).UTC()
	services, err := parseServices(fixture(t, "services.json"), observed)
	if err != nil || len(services) != 4 {
		t.Fatalf("services = %+v, %v", services, err)
	}
	if services[0].Name != "dnsmasq" || services[0].State != "running" || services[0].Instances[0].PID == nil ||
		*services[0].Instances[0].PID != 734 || services[0].Enabled != nil || !services[0].ObservedAt.Equal(observed) ||
		services[1].Name != "dropbear" || services[1].State != "stopped" || services[1].Enabled != nil ||
		services[2].Name != "firewall" || services[2].State != "unknown" || services[3].State != "unknown" {
		t.Fatalf("service semantics = %+v", services)
	}
	for _, value := range []string{
		`[]`, `{"service":null}`, `{"service":{"instances":false}}`,
		`{"service":{"instances":{"first":{"running":"yes"}}}}`,
		`{"service":{"instances":{"first":{"running":true,"pid":-1}}}}`,
		`{"service":{"instances":{"first":{"running":true,"pid":0}}}}`,
		`{"service/unsafe":{}}`,
	} {
		if _, err := parseServices([]byte(value), observed); err == nil {
			t.Fatalf("malformed service accepted: %s", value)
		}
	}
	for _, tc := range []struct{ instances, state string }{
		{`{}`, "unknown"}, {`{"a":{"running":false},"b":{}}`, "unknown"},
		{`{"a":{"running":true},"b":{}}`, "running"}, {`{"a":{"running":false},"b":{"running":false}}`, "stopped"},
	} {
		got, err := parseServices([]byte(`{"service":{"instances":`+tc.instances+`}}`), observed)
		if err != nil || got[0].State != tc.state {
			t.Fatalf("instances=%s result=%+v err=%v", tc.instances, got, err)
		}
	}
}

func TestFirewallConfigurationSchema(t *testing.T) {
	observed := time.Unix(102, 0).UTC()
	f, err := parseFirewall(fixture(t, "firewall.json"), observed)
	if err != nil || len(f.Zones) != 2 || len(f.Forwardings) != 1 || len(f.Rules) != 2 || len(f.Redirects) != 1 ||
		f.Completeness != "complete" || f.Enabled != nil || f.Source != "ubus.uci.firewall.configuration" || !f.ObservedAt.Equal(observed) {
		t.Fatalf("firewall = %+v, %v", f, err)
	}
	if f.Zones[0].Name != "lan" || f.Zones[0].Enabled == nil || !*f.Zones[0].Enabled || f.Zones[0].Output != "ACCEPT" ||
		!reflect.DeepEqual(f.Zones[0].Networks, []string{"lan", "guest"}) || f.Zones[1].Input != "REJECT" ||
		!reflect.DeepEqual(f.Zones[1].Networks, []string{"wan", "wan6"}) {
		t.Fatalf("zones = %+v", f.Zones)
	}
	if f.Forwardings[0].SourceZone != "lan" || f.Forwardings[0].DestinationZone != "wan" ||
		f.Rules[0].Protocol != "tcp udp" || f.Rules[0].DestinationPort != "53" || f.Rules[1].Enabled == nil || *f.Rules[1].Enabled ||
		f.Redirects[0].ExternalPort != "8443" || f.Redirects[0].InternalIP != "192.0.2.20" ||
		f.Redirects[0].InternalPort != "443" || f.Redirects[0].Target != "DNAT" {
		t.Fatalf("firewall mapping = %+v", f)
	}
	partial, err := parseFirewall([]byte(`{"values":{"include":{".type":"include","path":"never-executed"}}}`), observed)
	if err != nil || partial.Completeness != "partial" || len(partial.Rules) != 0 {
		t.Fatalf("unsupported includes = %+v, %v", partial, err)
	}
	for _, value := range []string{
		`{}`, `{"values":null}`, `{"values":{"a":{}}}`, `{"values":{"a":{".type":3}}}`,
		`{"values":{"a":{".type":"zone"}}}`, `{"values":{"a":{".type":"zone","name":"lan","network":42}}}`,
		`{"values":{"a":{".type":"rule","enabled":"sometimes"}}}`,
		`{"values":{"a":{".type":"rule","enabled":true}}}`,
		`{"values":{"a":{".type":"forwarding","src":"lan"}}}`,
	} {
		if _, err := parseFirewall([]byte(value), observed); err == nil {
			t.Fatalf("malformed firewall accepted: %s", value)
		}
	}
}

func TestBoundedJSONObjectsAndLists(t *testing.T) {
	services := make(map[string]any, maxEntries+1)
	for i := range maxEntries + 1 {
		services["s"+strings.Repeat("x", i+1)] = map[string]any{}
	}
	data, _ := json.Marshal(services)
	if _, err := parseServices(data, time.Now()); err == nil {
		t.Fatal("excessive services accepted")
	}
	entries := make([]any, maxEntries+1)
	for i := range entries {
		entries[i] = map[string]any{"interface": "lan"}
	}
	data, _ = json.Marshal(map[string]any{"interface": entries})
	if _, err := parseNetworks(data, time.Now()); err == nil {
		t.Fatal("excessive networks accepted")
	}
	if _, err := decodeObject(make([]byte, maxResponseBytes+1)); !errors.Is(err, errOutputLimit) {
		t.Fatalf("response limit = %v", err)
	}
}
