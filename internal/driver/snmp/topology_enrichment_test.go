package snmp

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/snmp"
)

func managementCell(column uint32, family uint32, raw []byte, kind protocol.Type, value any) protocol.Value {
	parts := []string{strconv.FormatUint(uint64(column), 10), "100", "7", "1", strconv.FormatUint(uint64(family), 10), strconv.Itoa(len(raw))}
	for _, octet := range raw {
		parts = append(parts, strconv.Itoa(int(octet)))
	}
	return val(remoteManagementRoot+"."+strings.Join(parts, "."), kind, value)
}

func TestLLDPEnrichmentPreservesManagementCapabilitiesAndLocalInterface(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	ipv6 := netip.MustParseAddr("2001:db8::10").AsSlice()
	f.walk[remoteManagementRoot] = []protocol.Value{
		managementCell(3, 1, []byte{192, 0, 2, 10}, protocol.Integer, 2),
		managementCell(4, 1, []byte{192, 0, 2, 10}, protocol.Integer, 17),
		managementCell(3, 2, ipv6, protocol.Integer, 1),
		managementCell(4, 2, ipv6, protocol.Integer, 0),
		managementCell(3, 99, []byte{0, 255}, protocol.Integer, 1),
	}
	f.walk[remoteRoot] = append(f.walk[remoteRoot],
		val(remoteRoot+".10.100.7.1", protocol.OctetString, []byte("advertised switch")),
		val(remoteRoot+".11.100.7.1", protocol.OctetString, []byte{0x28}),
		val(remoteRoot+".12.100.7.1", protocol.OctetString, []byte{0x20}))
	d := newFixtureDriver(f)
	update, err := d.Topology(context.Background())
	if err != nil || update.Completeness != "complete" || len(update.Observations) != 1 {
		t.Fatal(update, err)
	}
	n := update.Observations[0]
	if n.LocalInterfaceIndex == nil || *n.LocalInterfaceIndex != 31 || n.LocalPortNumber != 7 || n.LocalInterfaceMAC != "00:01:02:03:04:05" || n.LocalInterfaceDescription != "Gigabit port 31" {
		t.Fatal("local interface facts lost", n)
	}
	if n.RemoteSystemDescription != "advertised switch" || !reflect.DeepEqual(n.RemoteCapabilitiesSupported, []string{"bridge", "router"}) || !reflect.DeepEqual(n.RemoteCapabilitiesEnabled, []string{"bridge"}) {
		t.Fatal("LLDP facts lost", n)
	}
	if len(n.RemoteManagementAddresses) != 3 {
		t.Fatal("management addresses lost", n.RemoteManagementAddresses)
	}
	for _, a := range n.RemoteManagementAddresses {
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	if n.RemoteManagementAddresses[0].AddressFamily != 1 || n.RemoteManagementAddresses[0].Address != "192.0.2.10" || n.RemoteManagementAddresses[0].InterfaceID == nil || *n.RemoteManagementAddresses[0].InterfaceID != 17 {
		t.Fatal(n.RemoteManagementAddresses)
	}
	if n.RemoteManagementAddresses[1].Address != "2001:db8::10" || n.RemoteManagementAddresses[2].Address != "" {
		t.Fatal("unknown family invented address", n.RemoteManagementAddresses)
	}
	if err := model.ValidateNeighborEnrichment(n); err != nil {
		t.Fatal(err)
	}
	// After the initial IF-MIB cache fill, only the three slow LLDP tables are read.
	walks := f.walkCalls
	if _, err := d.Topology(context.Background()); err != nil || f.walkCalls-walks != 3 {
		t.Fatal("topology duplicated full interface inventory polling", f.walkCalls-walks, err)
	}
}

func TestLLDPOptionalManagementFailureAndMalformedFactsRemainPartial(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	f.errors[remoteManagementRoot] = protocol.ErrUnavailable
	u, err := newFixtureDriver(f).Topology(context.Background())
	if err != nil || u.Completeness != "complete" || u.Observations[0].RemoteManagementAddresses != nil {
		t.Fatal("optional absence fabricated or rejected", u, err)
	}
	f.errors[remoteManagementRoot] = protocol.ErrConnectivity
	u, err = newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(err, driver.ErrConnectivity) || u.Completeness != "partial" || len(u.Observations) != 1 {
		t.Fatal("optional transport failure erased neighbor", u, err)
	}
	f.errors[remoteManagementRoot] = nil
	f.walk[remoteManagementRoot] = []protocol.Value{managementCell(3, 1, []byte{192, 0, 2}, protocol.Integer, 2)}
	f.walk[remoteRoot] = append(f.walk[remoteRoot], val(remoteRoot+".10.100.7.1", protocol.OctetString, []byte("malicious\nname")), val(remoteRoot+".11.100.7.1", protocol.OctetString, []byte{1, 2, 3}))
	u, err = newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(err, driver.ErrMalformed) || !errors.Is(err, driver.ErrLimit) || u.Completeness != "partial" || len(u.Observations) != 1 || u.Observations[0].RemoteSystemDescription != "" || u.Observations[0].RemoteCapabilitiesSupported != nil {
		t.Fatal("malformed facts not isolated", u, err)
	}
}

func TestLLDPManagementAddressBoundsAndNoExtraTargets(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	for i := 0; i < 17; i++ {
		f.walk[remoteManagementRoot] = append(f.walk[remoteManagementRoot], managementCell(3, 1, []byte{192, 0, 2, byte(i + 1)}, protocol.Integer, 1))
	}
	u, err := newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(err, driver.ErrLimit) || u.Completeness != "partial" || len(u.Observations) != 1 || len(u.Observations[0].RemoteManagementAddresses) != 16 {
		t.Fatal("management list not bounded", u, err)
	}
	// Fixture Get/Walk are the only operations: advertised addresses remain data.
	if f.getCalls != 2 {
		t.Fatal("management addresses caused extra network probes", f.getCalls)
	}
}

func TestLLDPUnsafeCachedInterfaceDoesNotRejectNeighborSnapshot(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	d := newFixtureDriver(f)
	d.interfaces = []model.Interface{{ID: "ifindex:31", Index: 31, Name: "unsafe\nname", Description: "Gigabit port 31"}}
	u, err := d.Topology(context.Background())
	if !errors.Is(err, driver.ErrMalformed) || u.Completeness != "partial" || len(u.Observations) != 1 {
		t.Fatal("unsafe local mapping discarded snapshot", u, err)
	}
	n := u.Observations[0]
	if n.LocalInterfaceID != "" || n.LocalInterfaceName != "" || len(n.RemoteChassisID) == 0 || n.Confidence != "unresolved" {
		t.Fatal("unsafe interface identity retained or neighbor erased", n)
	}
	if err := model.ValidateNeighborEnrichment(n); err != nil {
		t.Fatal("retained raw neighbor cannot be delivered", err)
	}
}
