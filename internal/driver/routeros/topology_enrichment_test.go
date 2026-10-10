package routeros

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/routeros"
)

func TestRouterOSNeighborEnrichmentPreservesProtocolOrigins(t *testing.T) {
	f := newFixture(t)
	f.bodies["ip/neighbor"] = `[{"interface":"uplink","mac-address":"02:10:00:00:00:01","identity":"shared-host","discovered-by":"mndp,lldp,cdp","interface-name":"GigabitEthernet0/1","address":"192.0.2.10","address4":"192.0.2.10","address6":"2001:0db8::10","age":"2s","platform":"MikroTik","board":"RB-test","version":"7.test","system-description":"LLDP description","system-caps":"router,bridge,wlan-ap","system-caps-enabled":"bridge"}]`
	d := setup(t, f, false, driver.Options{TopologyTTLSeconds: 120})
	u, err := d.Topology(context.Background())
	if err != nil || u.Completeness != "complete" || len(u.Observations) != 3 {
		t.Fatal(u, err)
	}
	for _, n := range u.Observations {
		if n.LocalInterfaceID != "*1" || n.LocalInterfaceIndex != nil || n.LocalInterfaceMAC == "" || len(n.RemoteManagementAddresses) != 2 || n.RemoteManagementAddresses[0].Address != "192.0.2.10" || n.RemoteManagementAddresses[1].Address != "2001:db8::10" {
			t.Fatal("local identity/address evidence lost or fake ifIndex", n)
		}
		if n.RemotePlatform != "MikroTik" || n.RemoteModel != "RB-test" || n.RemoteSoftwareVersion != "7.test" || n.RemoteChassisSubtype != 0 || n.AgeSeconds == nil || *n.AgeSeconds != 2 || n.ExpiresAt.Sub(n.ObservedAt) != 120*time.Second {
			t.Fatal("protocol facts/time confused", n)
		}
		if n.Protocol == "lldp" {
			if n.RemotePortID != nil || n.RemoteSystemDescription != "LLDP description" || !reflect.DeepEqual(n.RemoteCapabilitiesSupported, []string{"bridge", "router", "wlan_access_point"}) || !reflect.DeepEqual(n.RemoteCapabilitiesEnabled, []string{"bridge"}) {
				t.Fatal("LLDP facts lost or CDP port attributed", n)
			}
		} else if n.RemoteSystemDescription != "" || n.RemoteCapabilitiesSupported != nil || n.RemoteCapabilitiesEnabled != nil {
			t.Fatal("LLDP-only claim leaked across protocols", n)
		}
		if n.Protocol == "cdp" && string(n.RemotePortID) != "GigabitEthernet0/1" {
			t.Fatal("CDP port lost", n)
		}
		if n.Protocol == "mndp" && n.RemotePortID != nil {
			t.Fatal("MNDP port fabricated", n)
		}
		if err := model.ValidateNeighborEnrichment(n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRouterOSMalformedOptionalFactsDoNotBlockGoodNeighbors(t *testing.T) {
	f := newFixture(t)
	rows := []protocol.Record{
		{"interface": "uplink", "identity": "bad\nname", "discovered-by": "lldp", "interface-name": strings.Repeat("x", 1025), "address": "not-an-ip", "system-caps": strings.Repeat("cap,", 20)},
		{"interface": "uplink", "identity": "good", "discovered-by": "mndp", "address6": "::ffff:192.0.2.1"},
	}
	data, _ := json.Marshal(rows)
	f.bodies["ip/neighbor"] = string(data)
	u, err := setup(t, f, false, driver.Options{}).Topology(context.Background())
	if !errors.Is(err, driver.ErrMalformed) || !errors.Is(err, driver.ErrLimit) || u.Completeness != "partial" || len(u.Observations) != 2 {
		t.Fatal("malformed neighbor discarded whole snapshot", u, err)
	}
	if u.Observations[0].RemoteSystemName != "" || len(u.Observations[0].RemotePortID) != 0 || len(u.Observations[0].RemoteManagementAddresses) != 0 {
		t.Fatal("malformed identity retained", u.Observations[0])
	}
	good := u.Observations[1]
	if good.RemoteSystemName != "good" || len(good.RemoteManagementAddresses) != 1 || good.RemoteManagementAddresses[0].AddressFamily != 2 || good.RemoteManagementAddresses[0].Address != "::ffff:192.0.2.1" {
		t.Fatal("mapped IPv6 family collapsed or good neighbor lost", good)
	}
	for _, n := range u.Observations {
		if err := model.ValidateNeighborEnrichment(n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRouterOSCapabilitiesBoundedAndUnknownRetained(t *testing.T) {
	var values []string
	for i := 0; i < 20; i++ {
		values = append(values, "vendor-cap-"+strings.Repeat("x", i+1))
	}
	caps, err := neighborCapabilities(strings.Join(values, ","))
	if !errors.Is(err, driver.ErrLimit) || len(caps) != 16 || caps[0] != "vendor-cap-x" {
		t.Fatal("unknown capabilities lost or unbounded", caps, err)
	}
}
