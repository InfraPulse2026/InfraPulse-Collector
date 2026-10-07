package snmp

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	protocol "InfraPulseAgent/internal/protocol/snmp"
)

type fixtureClient struct {
	get                 map[string]protocol.Value
	walk                map[string][]protocol.Value
	errors              map[string]error
	change              bool
	lastChangeCalls     int
	getCalls, walkCalls int
	maxGetBatch         int
}

func (f *fixtureClient) Get(ctx context.Context, oids []string) ([]protocol.Value, error) {
	f.getCalls++
	if len(oids) > f.maxGetBatch {
		f.maxGetBatch = len(oids)
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	var out []protocol.Value
	for _, oid := range oids {
		if err := f.errors[oid]; err != nil {
			return nil, err
		}
		v, ok := f.get[oid]
		if !ok {
			for root, values := range f.walk {
				if strings.HasPrefix(oid, root+".") {
					if err := f.errors[root]; err != nil && !errors.Is(err, protocol.ErrUnavailable) {
						return nil, err
					}
					for _, candidate := range values {
						if candidate.OID == oid {
							v = candidate
							ok = true
						}
					}
				}
			}
		}
		if !ok {
			v = protocol.Value{OID: oid, Type: protocol.NoSuchObject}
		}
		if oid == lastChangeOID && f.change {
			f.lastChangeCalls++
			v.Data = uint32(f.lastChangeCalls)
		}
		out = append(out, v)
	}
	return out, nil
}
func (f *fixtureClient) BulkWalk(ctx context.Context, root string) ([]protocol.Value, error) {
	f.walkCalls++
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return f.walk[root], f.errors[root]
}
func (f *fixtureClient) Close() error { return nil }
func val(oid string, kind protocol.Type, data any) protocol.Value {
	return protocol.Value{OID: oid, Type: kind, Data: data}
}
func cell(root string, column, index uint32, kind protocol.Type, data any) protocol.Value {
	return val(root+"."+strconv.FormatUint(uint64(column), 10)+"."+strconv.FormatUint(uint64(index), 10), kind, data)
}
func fixture() *fixtureClient {
	return &fixtureClient{get: map[string]protocol.Value{
		systemRoot + ".1.0": val(systemRoot+".1.0", protocol.OctetString, []byte("MikroTik fixture")),
		systemRoot + ".2.0": val(systemRoot+".2.0", protocol.ObjectIdentifier, ".1.3.6.1.4.1.14988.1.1.1"),
		systemRoot + ".3.0": val(systemRoot+".3.0", protocol.TimeTicks, uint32(123456)),
		systemRoot + ".4.0": val(systemRoot+".4.0", protocol.OctetString, []byte("operator")),
		systemRoot + ".5.0": val(systemRoot+".5.0", protocol.OctetString, []byte("fixture-switch")),
		systemRoot + ".6.0": val(systemRoot+".6.0", protocol.OctetString, []byte("some free text location")),
		lastChangeOID:       val(lastChangeOID, protocol.TimeTicks, uint32(10)),
	}, walk: map[string][]protocol.Value{}, errors: map[string]error{}}
}
func newFixtureDriver(f *fixtureClient) *Driver {
	return NewWithClient(driver.Options{ID: "switch-01", Name: "Switch", Address: "192.0.2.1", Location: model.PhysicalLocation{Datacenter: "dc", Rack: "rack1"}, MaxRows: 64, TopologyTTLSeconds: 900}, f)
}
func interfaceFixture(f *fixtureClient) {
	f.walk[ifRoot] = []protocol.Value{
		cell(ifRoot, 1, 31, protocol.Integer, 31), cell(ifRoot, 2, 31, protocol.OctetString, []byte("Gigabit port 31")), cell(ifRoot, 3, 31, protocol.Integer, 6), cell(ifRoot, 4, 31, protocol.Integer, 1500), cell(ifRoot, 5, 31, protocol.Gauge32, uint32(math.MaxUint32)), cell(ifRoot, 6, 31, protocol.OctetString, []byte{0, 1, 2, 3, 4, 5}), cell(ifRoot, 7, 31, protocol.Integer, 1), cell(ifRoot, 8, 31, protocol.Integer, 1),
		cell(ifRoot, 10, 31, protocol.Counter32, uint32(123)), cell(ifRoot, 16, 31, protocol.Counter32, uint32(456)), cell(ifRoot, 11, 31, protocol.Counter32, uint32(10)), cell(ifRoot, 12, 31, protocol.Counter32, uint32(2)), cell(ifRoot, 17, 31, protocol.Counter32, uint32(20)), cell(ifRoot, 18, 31, protocol.Counter32, uint32(3)), cell(ifRoot, 13, 31, protocol.Counter32, uint32(0)), cell(ifRoot, 19, 31, protocol.Counter32, uint32(4)), cell(ifRoot, 14, 31, protocol.Counter32, uint32(5)), cell(ifRoot, 20, 31, protocol.Counter32, uint32(0)),
	}
	f.walk[ifXRoot] = []protocol.Value{cell(ifXRoot, 1, 31, protocol.OctetString, []byte("ether31")), cell(ifXRoot, 18, 31, protocol.OctetString, []byte("management")), cell(ifXRoot, 15, 31, protocol.Gauge32, uint32(10000)), cell(ifXRoot, 19, 31, protocol.TimeTicks, uint32(0)), cell(ifXRoot, 6, 31, protocol.Counter64, uint64(1<<42)), cell(ifXRoot, 10, 31, protocol.Counter64, uint64(1<<43)), cell(ifXRoot, 7, 31, protocol.Counter64, uint64(10)), cell(ifXRoot, 8, 31, protocol.Counter64, uint64(2)), cell(ifXRoot, 9, 31, protocol.Counter64, uint64(3)), cell(ifXRoot, 11, 31, protocol.Counter64, uint64(20)), cell(ifXRoot, 12, 31, protocol.Counter64, uint64(4)), cell(ifXRoot, 13, 31, protocol.Counter64, uint64(5))}
}

func TestSYSTEMMappingAndPhysicalLocation(t *testing.T) {
	f := fixture()
	d := newFixtureDriver(f)
	dev, err := d.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dev.ID != "switch-01" || dev.Hostname != "fixture-switch" || dev.SysObjectID != ".1.3.6.1.4.1.14988.1.1.1" || dev.UptimeSeconds != 1234.56 || dev.UptimeAvailable == nil || !*dev.UptimeAvailable {
		t.Fatalf("device: %+v", dev)
	}
	if dev.SystemContact != "operator" || dev.SystemLocation != "some free text location" || dev.Location.Rack != "rack1" {
		t.Fatalf("location/contact: %+v", dev)
	}
	if dev.UptimeSource != "snmp.sysUpTime.management_subsystem" {
		t.Fatal(dev.UptimeSource)
	}
	if dev.Model != "" || dev.SerialNumber != "" {
		t.Fatal("invented inventory")
	}
}
func TestMissingOptionalSYSTEMAndMalformedValues(t *testing.T) {
	f := fixture()
	delete(f.get, systemRoot+".4.0")
	delete(f.get, systemRoot+".6.0")
	delete(f.get, systemRoot+".3.0")
	d := newFixtureDriver(f)
	dev, e := d.Probe(context.Background())
	if e != nil || dev.SystemContact != "" || dev.SystemLocation != "" || *dev.UptimeAvailable {
		t.Fatalf("optional: %+v %v", dev, e)
	}
	f.get[systemRoot+".3.0"] = val(systemRoot+".3.0", protocol.TimeTicks, "invalid")
	_, e = d.Probe(context.Background())
	if !errors.Is(e, driver.ErrMalformed) {
		t.Fatal(e)
	}
	f = fixture()
	for k := range f.get {
		delete(f.get, k)
	}
	_, e = newFixtureDriver(f).Probe(context.Background())
	if !errors.Is(e, driver.ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestInterfaceMappingHCCounters(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	d := newFixtureDriver(f)
	sample, err := d.Collect(context.Background())
	if err != nil || len(sample.Interfaces) != 1 {
		t.Fatalf("collect: %+v %v", sample, err)
	}
	i := sample.Interfaces[0]
	if i.Index != 31 || i.Name != "ether31" || i.Description != "Gigabit port 31" || i.Alias != "management" || i.MAC != "00:01:02:03:04:05" || i.MTU != 1500 || !i.AdminUp || i.OperState != "up" || *i.SpeedBitsPerSecond != 10_000_000_000 {
		t.Fatalf("interface: %+v", i)
	}
	if i.Counters.RXBytes != 1<<42 || i.Counters.TXBytes != 1<<43 || i.Counters.RXPackets != 15 || i.Counters.TXPackets != 29 || i.CounterMetadata.Widths.RXBytes != 64 || !i.CounterMetadata.Available.TXErrors || i.Counters.TXErrors != 0 {
		t.Fatalf("counters: %+v %+v", i.Counters, i.CounterMetadata)
	}
}
func TestFallbackCountersAndMissingOptionalIFX(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	f.walk[ifXRoot] = nil
	f.errors[ifXRoot] = protocol.ErrUnavailable
	d := newFixtureDriver(f)
	sample, e := d.Collect(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	i := sample.Interfaces[0]
	if i.Counters.RXBytes != 123 || i.CounterMetadata.Widths.RXBytes != 32 || !i.CounterMetadata.Available.RXPackets || i.Counters.RXPackets != 12 || i.CounterMetadata.Widths.RXPackets != 0 {
		t.Fatalf("fallback: %+v", i)
	}
	if i.SpeedBitsPerSecond != nil {
		t.Fatal("saturated ifSpeed treated as available")
	}
	if i.Name != "" {
		t.Fatal("ifDescr promoted to authoritative ifName")
	}
	f.walk[ifRoot] = []protocol.Value{cell(ifRoot, 1, 31, protocol.Integer, 31)}
	sample, e = newFixtureDriver(f).Collect(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	i = sample.Interfaces[0]
	if i.CounterMetadata.Available.RXBytes || i.Metadata.MTU || i.Metadata.AdminStatus || i.Type != nil {
		t.Fatalf("missing values represented as measurements: %+v", i)
	}
}
func TestMalformedInterfaceAndConnectivity(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	f.walk[ifRoot] = append(f.walk[ifRoot], cell(ifRoot, 10, 31, protocol.OctetString, []byte("bad counter")))
	d := newFixtureDriver(f)
	_, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal("HC fallback should ignore unused malformed legacy? ", err)
	}
	f.walk[ifXRoot] = nil
	_, err = d.Collect(context.Background())
	if !errors.Is(err, driver.ErrMalformed) {
		t.Fatal(err)
	}
	f.errors[ifRoot] = protocol.ErrConnectivity
	_, err = d.Collect(context.Background())
	if !errors.Is(err, driver.ErrConnectivity) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = d.Collect(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestENTITYInventoryAndAmbiguousChassis(t *testing.T) {
	f := fixture()
	f.walk[entityRoot] = []protocol.Value{cell(entityRoot, 5, 1, protocol.Integer, 3), cell(entityRoot, 4, 1, protocol.Integer, 0), cell(entityRoot, 7, 1, protocol.OctetString, []byte("chassis")), cell(entityRoot, 8, 1, protocol.OctetString, []byte("hw1")), cell(entityRoot, 9, 1, protocol.OctetString, []byte("fw1")), cell(entityRoot, 10, 1, protocol.OctetString, []byte("sw1")), cell(entityRoot, 11, 1, protocol.OctetString, []byte("SERIAL1")), cell(entityRoot, 12, 1, protocol.OctetString, []byte("MikroTik")), cell(entityRoot, 13, 1, protocol.OctetString, []byte("model1")), cell(entityRoot, 5, 2, protocol.Integer, 9), cell(entityRoot, 4, 2, protocol.Integer, 1), cell(entityRoot, 7, 2, protocol.OctetString, []byte("module"))}
	d := newFixtureDriver(f)
	dev, e := d.Inventory(context.Background())
	if e != nil || dev.Model != "model1" || dev.SerialNumber != "SERIAL1" || dev.HardwareRevision != "hw1" || dev.FirmwareVersion != "fw1" || len(dev.Components) != 2 || dev.Components[1].ParentID != "entity:1" {
		t.Fatalf("entity: %+v %v", dev, e)
	}
	f.walk[entityRoot] = append(f.walk[entityRoot], cell(entityRoot, 5, 3, protocol.Integer, 3))
	dev, e = d.Inventory(context.Background())
	if e != nil || dev.Model != "" || dev.SerialNumber != "" {
		t.Fatalf("ambiguous chassis picked: %+v %v", dev, e)
	}
	f.errors[entityRoot] = protocol.ErrUnavailable
	dev, e = d.Inventory(context.Background())
	if e != nil || len(dev.Components) != 0 || dev.Model != "" {
		t.Fatalf("optional MIB: %+v %v", dev, e)
	}
}
func TestVendorDetectionEvidence(t *testing.T) {
	for _, tc := range []struct{ oid, description, hint, want string }{{".1.3.6.1.4.1.14988.1", "", "Cisco", "MikroTik"}, {".1.3.6.1.4.1.149880.1", "", "MikroTik", ""}, {".1.3.6.1.4.1.999.1", "MikroTik RouterOS", "", "MikroTik"}, {"", "unclassified device", "MikroTik", ""}} {
		vendor, evidence := detectVendor(tc.oid, tc.description, tc.hint)
		if vendor != tc.want {
			t.Fatalf("%+v => %s", tc, vendor)
		}
		if tc.hint != "" && evidence[0].Source != "config.vendor_hint" {
			t.Fatal(evidence)
		}
	}
	f := fixture()
	f.get[systemRoot+".5.0"] = val(systemRoot+".5.0", protocol.OctetString, []byte("MikroTik-host"))
	f.get[systemRoot+".1.0"] = val(systemRoot+".1.0", protocol.OctetString, []byte("unknown"))
	delete(f.get, systemRoot+".2.0")
	dev, e := newFixtureDriver(f).Probe(context.Background())
	if e != nil || dev.Vendor != "" {
		t.Fatalf("hostname detection: %+v %v", dev, e)
	}
}

func topologyFixture(f *fixtureClient) {
	interfaceFixture(f)
	f.walk[localPortRoot] = []protocol.Value{cell(localPortRoot, 2, 7, protocol.Integer, 5), cell(localPortRoot, 3, 7, protocol.OctetString, []byte("ether31")), cell(localPortRoot, 4, 7, protocol.OctetString, []byte("Gigabit port 31"))}
	for c, v := range map[uint32]protocol.Value{4: val("", protocol.Integer, 4), 5: val("", protocol.OctetString, []byte{6, 7, 8, 9, 10, 11}), 6: val("", protocol.Integer, 5), 7: val("", protocol.OctetString, []byte("remote-port")), 8: val("", protocol.OctetString, []byte("uplink")), 9: val("", protocol.OctetString, []byte("unknown-remote"))} {
		v.OID = remoteRoot + "." + strconv.FormatUint(uint64(c), 10) + ".100.7.1"
		f.walk[remoteRoot] = append(f.walk[remoteRoot], v)
	}
}
func TestLLDPPortNumberIsNotIfIndex(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	d := newFixtureDriver(f)
	update, e := d.Topology(context.Background())
	if e != nil || update.Completeness != "complete" || len(update.Observations) != 1 {
		t.Fatalf("topology: %+v %v", update, e)
	}
	n := update.Observations[0]
	if n.LocalPortNumber != 7 || n.LocalInterfaceID != "ifindex:31" || n.LocalInterfaceName != "ether31" || n.Confidence != "high" || n.RemoteSystemName != "unknown-remote" || n.Protocol != "lldp" || !reflect.DeepEqual(n.RemoteChassisID, []byte{6, 7, 8, 9, 10, 11}) {
		t.Fatalf("mapping: %+v", n)
	}
	if n.ExpiresAt.Sub(n.ObservedAt) != 900*time.Second || update.ExpiryAssumption == "" {
		t.Fatal("missing explicit TTL assumption")
	}
}
func TestLLDPMACAndUnresolvedMapping(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	f.walk[localPortRoot] = []protocol.Value{cell(localPortRoot, 2, 7, protocol.Integer, 3), cell(localPortRoot, 3, 7, protocol.OctetString, []byte{0, 1, 2, 3, 4, 5})}
	d := newFixtureDriver(f)
	u, e := d.Topology(context.Background())
	if e != nil || u.Observations[0].LocalInterfaceID != "ifindex:31" {
		t.Fatalf("MAC: %+v %v", u, e)
	}
	f.walk[localPortRoot] = []protocol.Value{cell(localPortRoot, 2, 7, protocol.Integer, 5), cell(localPortRoot, 3, 7, protocol.OctetString, []byte("unresolved-port"))}
	u, e = d.Topology(context.Background())
	if e != nil || u.Observations[0].LocalInterfaceID != "" || u.Observations[0].Confidence != "unresolved" {
		t.Fatalf("invented port: %+v %v", u, e)
	}
	iface := model.Interface{ID: "one", Name: "name"}
	_, _, _, ok := resolveLocalPort(localPort{subtype: 5, id: []byte("name")}, []model.Interface{iface, {ID: "two", Name: "name"}})
	if ok {
		t.Fatal("ambiguous evidence resolved")
	}
}
func TestLLDPAllIdentifierSubtypesAndMultipleNeighbors(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	original := append([]protocol.Value(nil), f.walk[remoteRoot]...)
	f.walk[remoteRoot] = nil
	for subtype := 1; subtype <= 7; subtype++ {
		for _, v := range original {
			column := stringsColumn(v.OID)
			v.OID = remoteRoot + "." + strconv.Itoa(column) + ".100.7." + strconv.Itoa(subtype)
			if column == 4 || column == 6 {
				v.Data = subtype
			}
			f.walk[remoteRoot] = append(f.walk[remoteRoot], v)
		}
	}
	u, e := newFixtureDriver(f).Topology(context.Background())
	if e != nil || len(u.Observations) != 7 {
		t.Fatalf("neighbors: %+v %v", u, e)
	}
	for i, n := range u.Observations {
		if n.RemoteChassisSubtype != uint32(i+1) || n.RemotePortSubtype != uint32(i+1) {
			t.Fatal(n)
		}
	}
}
func stringsColumn(oid string) int {
	suffix := oid[len(remoteRoot)+1:]
	for i, c := range suffix {
		if c == '.' {
			n, _ := strconv.Atoi(suffix[:i])
			return n
		}
	}
	return 0
}
func TestLLDPPartialFailedAndCompleteEmpty(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	f.errors[remoteRoot] = protocol.ErrConnectivity
	u, e := newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(e, driver.ErrConnectivity) || u.Completeness != "partial" || len(u.Observations) != 1 {
		t.Fatalf("partial lost observations: %+v %v", u, e)
	}
	f.errors[remoteRoot] = nil
	f.walk[remoteRoot] = nil
	u, e = newFixtureDriver(f).Topology(context.Background())
	if e != nil || u.Completeness != "complete" || len(u.Observations) != 0 {
		t.Fatalf("empty complete: %+v %v", u, e)
	}
	f.walk[localPortRoot] = nil
	f.errors[localPortRoot] = protocol.ErrUnavailable
	f.errors[remoteRoot] = protocol.ErrUnavailable
	delete(f.get, lastChangeOID)
	u, e = newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(e, driver.ErrUnavailable) || u.Completeness != "failed" {
		t.Fatalf("unsupported not failed: %+v %v", u, e)
	}
}
func TestLLDPChurnTimeoutAndMalformed(t *testing.T) {
	f := fixture()
	topologyFixture(f)
	f.change = true
	u, e := newFixtureDriver(f).Topology(context.Background())
	if e == nil || u.Completeness != "partial" {
		t.Fatalf("table churn accepted complete: %+v %v", u, e)
	}
	f = fixture()
	topologyFixture(f)
	f.walk[remoteRoot] = append(f.walk[remoteRoot], val(remoteRoot+".4.invalid", protocol.Integer, 4))
	u, e = newFixtureDriver(f).Topology(context.Background())
	if !errors.Is(e, driver.ErrMalformed) || u.Completeness != "partial" {
		t.Fatalf("malformed: %+v %v", u, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	u, e = newFixtureDriver(f).Topology(ctx)
	if !errors.Is(e, driver.ErrConnectivity) || !errors.Is(e, context.DeadlineExceeded) || u.Completeness == "complete" {
		t.Fatalf("timeout: %+v %v", u, e)
	}
}
func TestARPDoesNotCreatePhysicalLink(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	f.walk[".1.3.6.1.2.1.4.22"] = []protocol.Value{val(".1.3.6.1.2.1.4.22.1.2.31.192.0.2.9", protocol.OctetString, []byte{6, 7, 8, 9, 10, 11})}
	u, e := newFixtureDriver(f).Topology(context.Background())
	if e != nil || len(u.Observations) != 0 {
		t.Fatalf("ARP leaked into topology: %+v %v", u, e)
	}
}
