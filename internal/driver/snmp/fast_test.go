package snmp

import (
	"context"
	"errors"
	"testing"

	"InfraPulseAgent/internal/driver"
	protocol "InfraPulseAgent/internal/protocol/snmp"
)

func TestFastUsesBoundedGetAndSlowInventoryDiscoversInterfaces(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	d := newFixtureDriver(f)
	first, err := d.Collect(context.Background())
	if err != nil || len(first.Interfaces) != 1 {
		t.Fatalf("initial: %+v %v", first, err)
	}
	walks := f.walkCalls
	// Add a second index between polls. It is discovered only by slow inventory,
	// while a known interface's state/identity/counters are read by bounded Get.
	f.walk[ifRoot] = append(f.walk[ifRoot], cell(ifRoot, 1, 32, protocol.Integer, 32), cell(ifRoot, 2, 32, protocol.OctetString, []byte("new interface")))
	f.walk[ifXRoot] = append(f.walk[ifXRoot], cell(ifXRoot, 1, 32, protocol.OctetString, []byte("ether32")), cell(ifXRoot, 6, 31, protocol.Counter64, uint64(1<<42)+300), cell(ifXRoot, 1, 31, protocol.OctetString, []byte("renamed31")), cell(ifXRoot, 19, 31, protocol.TimeTicks, uint32(50)))
	f.walk[ifRoot] = append(f.walk[ifRoot], cell(ifRoot, 6, 31, protocol.OctetString, []byte{0, 1, 2, 3, 4, 6}), cell(ifRoot, 7, 31, protocol.Integer, 2), cell(ifRoot, 8, 31, protocol.Integer, 2), cell(ifRoot, 4, 31, protocol.Integer, 9000))
	second, err := d.Collect(context.Background())
	if err != nil || len(second.Interfaces) != 1 || f.walkCalls != walks {
		t.Fatalf("fast unexpectedly walked inventory: %+v walks=%d %v", second, f.walkCalls, err)
	}
	i := second.Interfaces[0]
	if i.Name != "renamed31" || i.MAC != "00:01:02:03:04:06" || i.AdminUp || i.OperState != "down" || i.Counters.RXBytes != (1<<42)+300 || *i.CounterMetadata.DiscontinuityTicks != 50 || i.MTU != 1500 {
		t.Fatalf("dynamic identity/state or slow metadata: %+v", i)
	}
	if _, err = d.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	third, err := d.Collect(context.Background())
	if err != nil || len(third.Interfaces) != 2 || third.Interfaces[0].MTU != 9000 {
		t.Fatalf("inventory did not refresh metadata/indexes: %+v %v", third, err)
	}
	if f.maxGetBatch > 60 {
		t.Fatalf("unbounded Get: %d", f.maxGetBatch)
	}
}

func TestFastMissingInstancesPruneWithoutStaleCounters(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	d := newFixtureDriver(f)
	if _, err := d.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Explicitly missing HC and fallback byte counters remove availability.
	f.walk[ifRoot] = filterColumn(f.walk[ifRoot], ifRoot, 10)
	f.walk[ifXRoot] = filterColumn(f.walk[ifXRoot], ifXRoot, 6)
	s, err := d.Collect(context.Background())
	if err != nil || s.Interfaces[0].CounterMetadata.Available.RXBytes || s.Interfaces[0].Counters.RXBytes != 0 {
		t.Fatalf("stale counter retained: %+v %v", s, err)
	}
	f.walk[ifRoot] = nil
	f.walk[ifXRoot] = nil
	s, err = d.Collect(context.Background())
	if err != nil || len(s.Interfaces) != 0 {
		t.Fatalf("vanished interface retained: %+v %v", s, err)
	}
	walks := f.walkCalls
	s, err = d.Collect(context.Background())
	if err != nil || len(s.Interfaces) != 0 || f.walkCalls != walks {
		t.Fatalf("empty cached indexes forced a fast walk: %+v %v", s, err)
	}
	interfaceFixture(f)
	if _, err = d.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err = d.Collect(context.Background())
	if err != nil || len(s.Interfaces) != 1 {
		t.Fatalf("recreation not discovered by inventory: %+v %v", s, err)
	}
}
func filterColumn(values []protocol.Value, root string, column uint32) []protocol.Value {
	var out []protocol.Value
	for _, v := range values {
		c, _, _ := tableCell(v, root)
		if c != column {
			out = append(out, v)
		}
	}
	return out
}

func TestFastBatchesAndAggregateResponseBound(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	for index := uint32(32); index <= 40; index++ {
		f.walk[ifRoot] = append(f.walk[ifRoot], cell(ifRoot, 1, index, protocol.Integer, int(index)), cell(ifRoot, 2, index, protocol.OctetString, []byte("port")))
	}
	d := newFixtureDriver(f)
	if _, err := d.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	walks := f.walkCalls
	gets := f.getCalls
	s, err := d.Collect(context.Background())
	if err != nil || len(s.Interfaces) != 10 || f.maxGetBatch != 60 || f.getCalls-gets < 5 || f.walkCalls != walks {
		t.Fatalf("batching: interfaces=%d batch=%d gets=%d %v", len(s.Interfaces), f.maxGetBatch, f.getCalls-gets, err)
	}
	d.opts.MaxResponseBytes = 256
	if _, err = d.Collect(context.Background()); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("aggregate response exceeded bound: %v", err)
	}
}

func Test32BitPacketComponentsAndInterfaceRowLimit(t *testing.T) {
	f := fixture()
	interfaceFixture(f)
	f.walk[ifXRoot] = []protocol.Value{cell(ifXRoot, 2, 31, protocol.Counter32, uint32(4)), cell(ifXRoot, 3, 31, protocol.Counter32, uint32(5)), cell(ifXRoot, 4, 31, protocol.Counter32, uint32(6)), cell(ifXRoot, 5, 31, protocol.Counter32, uint32(7))}
	f.walk[ifRoot] = filterColumn(filterColumn(f.walk[ifRoot], ifRoot, 12), ifRoot, 18)
	s, err := newFixtureDriver(f).Collect(context.Background())
	if err != nil || s.Interfaces[0].Counters.RXPackets != 19 || s.Interfaces[0].Counters.TXPackets != 33 || s.Interfaces[0].CounterMetadata.Widths.RXPackets != 0 {
		t.Fatalf("32-bit components: %+v %v", s, err)
	}
	d := newFixtureDriver(f)
	d.opts.MaxRows = 1
	f.walk[ifRoot] = append(f.walk[ifRoot], cell(ifRoot, 1, 32, protocol.Integer, 32))
	if _, err = d.Collect(context.Background()); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("interface count limit: %v", err)
	}
}
