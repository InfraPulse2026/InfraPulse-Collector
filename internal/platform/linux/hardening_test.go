package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingOptionalSysfsAndProcFields(t *testing.T) {
	root := copyFixture(t)
	// A virtual/disappearing interface can have counters while its sysfs
	// metadata has vanished. Optional sources must not destroy real counters.
	for _, path := range []string{"sys/class/net/eth0", "proc/cpuinfo", "proc/pressure", "proc/sys/kernel/random/boot_id"} {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	p := New(filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	sample, err := p.Collect(context.Background())
	if err != nil || sample.CPU == nil || sample.Memory == nil || len(sample.Interfaces) != 2 || sample.BootID != "" {
		t.Fatal(sample, err)
	}
	for _, iface := range sample.Interfaces {
		if iface.Name == "eth0" && (iface.Counters.RXBytes == 0 || iface.Index != 0 || iface.MTU != 0 || iface.MAC != "" || iface.OperState != "unknown" || iface.SpeedBitsPerSecond != nil) {
			t.Fatal(iface)
		}
		if iface.Name == "eth0" && (iface.Metadata == nil || iface.Metadata.Index || iface.Metadata.MTU || iface.Metadata.AdminStatus) {
			t.Fatal("vanished sysfs metadata was marked available", iface.Metadata)
		}
	}
	if sample.CPU.Model != "" || sample.CPU.PhysicalCores != 0 || sample.Memory.PressureSomeAvg10 != nil {
		t.Fatal("optional values fabricated")
	}
}

func TestInvalidSysfsMetadataDoesNotPublishNegativeValues(t *testing.T) {
	root := copyFixture(t)
	for name, value := range map[string]string{"ifindex": "-2", "mtu": "99999999999999999", "operstate": "arbitrary-state", "address": "not-a-MAC", "speed": "-1", "flags": "malformed"} {
		if err := os.WriteFile(filepath.Join(root, "sys", "class", "net", "eth0", name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := New(filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	sample, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range sample.Interfaces {
		if iface.Name == "eth0" && (iface.Index != 0 || iface.MTU != 0 || iface.OperState != "unknown" || iface.MAC != "" || iface.SpeedBitsPerSecond != nil) {
			t.Fatal(iface)
		}
		if iface.Name == "eth0" && (iface.Metadata == nil || iface.Metadata.Index || iface.Metadata.MTU || iface.Metadata.AdminStatus) {
			t.Fatal("invalid sysfs metadata was marked available", iface.Metadata)
		}
	}
}

func TestSysfsAvailabilityDistinguishesObservedDownFromUnavailable(t *testing.T) {
	root := copyFixture(t)
	if err := os.WriteFile(filepath.Join(root, "sys", "class", "net", "eth0", "flags"), []byte("0x0"), 0600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	sample, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range sample.Interfaces {
		if iface.Name == "eth0" && (iface.AdminUp || iface.Metadata == nil || !iface.Metadata.Index || !iface.Metadata.MTU || !iface.Metadata.AdminStatus) {
			t.Fatal("observed down state lost availability", iface)
		}
	}
}

func TestMandatorySourceFailureDoesNotCommitCPUBaseline(t *testing.T) {
	root := copyFixture(t)
	p := New(filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	path := filepath.Join(root, "proc", "meminfo")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Collect(context.Background()); err == nil {
		t.Fatal("missing mandatory source returned fabricated sample")
	}
	if p.previous != nil {
		t.Fatal("failed sample committed CPU baseline")
	}
	// A source which cannot be read as a file represents a restricted or
	// unavailable mount independently of platform-specific permission rules.
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Collect(context.Background()); err == nil {
		t.Fatal("unreadable source returned sample")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sample, err := p.Collect(context.Background())
	if err != nil || sample.CPU.UsagePercent != nil {
		t.Fatal(sample, err)
	}
}

func TestVeryLargeInterfaceTableRemainsBounded(t *testing.T) {
	var source strings.Builder
	for i := range maxInterfaces {
		fmt.Fprintf(&source, "veth%d: 1 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n", i)
	}
	interfaces, err := parseInterfaces(source.String())
	if err != nil || len(interfaces) != maxInterfaces {
		t.Fatal(len(interfaces), err)
	}
	source.WriteString("overflow: 1 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n")
	if _, err := parseInterfaces(source.String()); err == nil {
		t.Fatal("unbounded interface table")
	}
}
