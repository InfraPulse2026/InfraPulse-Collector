package linux

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFixtureIdentityAndTelemetry(t *testing.T) {
	p := New("testdata/proc", "testdata/sys")
	identity, err := p.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if identity.Hostname != "fixture-node" || identity.Distribution != "Fixture Linux 12" ||
		identity.SoftwareVersion != "12" || identity.Kernel != "6.6.12-fixture" ||
		identity.UptimeSeconds != 1234.5 || identity.BootID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("identity = %+v", identity)
	}
	if identity.SerialNumber != "" || identity.Type != "unknown" {
		t.Fatalf("unavailable inventory was fabricated: %+v", identity)
	}
	sample, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPU.UsagePercent != nil || sample.CPU.LogicalCPUs != 2 || sample.CPU.PhysicalCores != 1 ||
		sample.CPU.Model != "Fixture CPU" || sample.CPU.Load1 != 0.25 {
		t.Fatalf("CPU = %+v", sample.CPU)
	}
	if sample.Memory.TotalBytes != 1000000*1024 || sample.Memory.UsedBytes != 300000*1024 ||
		sample.Memory.SwapUsedBytes != 50000*1024 || sample.Memory.PressureSomeAvg10 == nil ||
		*sample.Memory.PressureSomeAvg10 != 2.5 {
		t.Fatalf("memory = %+v", sample.Memory)
	}
	if sample.Source != "local" || sample.CollectedAt.Before(sample.ObservedAt) ||
		sample.ObservedAt.Location().String() != "UTC" {
		t.Fatalf("timestamp/source = %+v", sample)
	}
	if len(sample.Interfaces) != 2 {
		t.Fatalf("interfaces = %+v", sample.Interfaces)
	}
	iface := sample.Interfaces[0]
	if iface.Name != "eth0" || iface.ID != "2:eth0:02:11:22:33:44:55" || iface.Index != 2 ||
		!iface.AdminUp || iface.OperState != "up" || iface.MTU != 1500 ||
		iface.SpeedBitsPerSecond == nil || *iface.SpeedBitsPerSecond != 1000000000 || len(iface.Addresses) != 0 ||
		iface.Counters.RXBytes != 10000 || iface.Counters.TXBytes != 20000 || iface.Counters.RXErrors != 2 ||
		iface.Counters.TXErrors != 4 || iface.Counters.RXDropped != 3 || iface.Counters.TXDropped != 5 {
		t.Fatalf("eth0 = %+v", iface)
	}
	if sample.Interfaces[1].OperState != "unknown" || sample.Interfaces[1].SpeedBitsPerSecond != nil {
		t.Fatalf("missing sysfs metadata should remain unavailable: %+v", sample.Interfaces[1])
	}
}

func TestCPUCounterDeltaAndReset(t *testing.T) {
	previous, _, err := parseCPUStat("cpu 100 20 30 850 10 4 6 0 25 5\ncpu0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := parseCPUStat("cpu 125 25 40 900 15 8 7 0 40 10\ncpu0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	// Delta=100, idle+iowait delta=55. Guest deltas must not add 20.
	if previous.total != 1020 || current.total != 1120 {
		t.Fatalf("guest time counted twice: %+v %+v", previous, current)
	}
	usage := cpuUsage(previous, current)
	if usage == nil || math.Abs(*usage-45) > 1e-9 {
		t.Fatalf("usage = %v", usage)
	}
	if cpuUsage(current, previous) != nil {
		t.Fatal("reset produced usage")
	}
	if cpuUsage(current, current) != nil {
		t.Fatal("zero interval produced usage")
	}
	decreasedField := current
	decreasedField.fields[0] = previous.fields[0] - 1
	if cpuUsage(previous, decreasedField) != nil {
		t.Fatal("per-counter reset ignored")
	}
}

func TestCollectRebootAndFailedBaseline(t *testing.T) {
	root := copyFixture(t)
	p := New(filepath.Join(root, "proc"), filepath.Join(root, "sys"))
	collect := func() {
		t.Helper()
		if _, err := p.Collect(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	collect()
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("proc/stat", "cpu 125 25 40 900 15 8 7 0 40 10\ncpu0 0 0 0 0\n")
	write("proc/meminfo", "invalid")
	if _, err := p.Collect(context.Background()); err == nil {
		t.Fatal("malformed meminfo accepted")
	}
	if p.previous.total != 1020 {
		t.Fatal("failed collection changed CPU baseline")
	}
	original, err := os.ReadFile("testdata/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	write("proc/meminfo", string(original))
	sample, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPU.UsagePercent == nil || *sample.CPU.UsagePercent != 45 {
		t.Fatalf("usage = %+v", sample.CPU)
	}
	write("proc/sys/kernel/random/boot_id", "different-boot")
	write("proc/stat", "cpu 135 25 50 910 15 8 7 0 40 10\ncpu0 0 0 0 0\n")
	sample, err = p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPU.UsagePercent != nil {
		t.Fatal("reboot baseline reused")
	}
	write("proc/uptime", "2.00 1.00")
	sample, err = p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPU.UsagePercent != nil {
		t.Fatal("decreased uptime baseline reused")
	}
}

func TestMemoryAvailableAndFallback(t *testing.T) {
	base := "MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 20 kB\nCached: 300 kB\nSReclaimable: 40 kB\nShmem: 10 kB\nSwapTotal: 200 kB\nSwapFree: 150 kB\n"
	fallback, err := parseMemory(base)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.AvailableBytes != 450*1024 || fallback.UsedBytes != 550*1024 || fallback.SwapUsedBytes != 50*1024 {
		t.Fatalf("fallback = %+v", fallback)
	}
	available, err := parseMemory(base + "MemAvailable: 800 kB\n")
	if err != nil {
		t.Fatal(err)
	}
	if available.UsedBytes != 200*1024 || available.FreeBytes != 100*1024 {
		t.Fatalf("available = %+v", available)
	}
	for _, malformed := range []string{"MemTotal: 1000 GB\nMemFree: 1 kB", "MemFree: 1 kB",
		"MemTotal: 100 kB\nMemFree: 200 kB", base + "MemAvailable: 1001 kB", base + "SwapFree: 300 kB",
		"MemTotal: 18446744073709551615 kB\nMemFree: 1 kB"} {
		if _, err := parseMemory(malformed); err == nil {
			t.Errorf("accepted invalid memory: %s", malformed)
		}
	}
}

func TestMalformedSourcesAndLimits(t *testing.T) {
	for _, text := range []string{"", "cpu 1 2 bad 4\ncpu0 0 0 0 0", "cpu 1 2 3 4", "cpu 18446744073709551615 1 1 1\ncpu0 0 0 0 0"} {
		if _, _, err := parseCPUStat(text); err == nil {
			t.Errorf("accepted CPU: %s", text)
		}
	}
	for _, text := range []string{"1 NaN 2", "1 +Inf 2", "1 -1 2", "1"} {
		if _, err := parseLoad(text); err == nil {
			t.Errorf("accepted load: %s", text)
		}
	}
	for _, text := range []string{"", "eth0: 1 2", "../escape: " + strings.Repeat("0 ", 16),
		"eth0: " + strings.Repeat("-1 ", 16)} {
		if _, err := parseInterfaces(text); err == nil {
			t.Errorf("accepted interfaces: %s", text)
		}
	}
	var many strings.Builder
	for i := 0; i <= maxInterfaces; i++ {
		many.WriteString(fmt.Sprintf("eth%d", i))
		many.WriteString(": " + strings.Repeat("0 ", 16) + "\n")
	}
	if _, err := parseInterfaces(many.String()); err == nil {
		t.Fatal("interface limit ignored")
	}
	root := t.TempDir()
	path := filepath.Join(root, "oversized")
	if err := os.WriteFile(path, make([]byte, maxFileBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(context.Background(), path); err == nil {
		t.Fatal("file size limit ignored")
	}
}

func TestCancellationAndConcurrentCollect(t *testing.T) {
	p := New("testdata/proc", "testdata/sys")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Identity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("identity error = %v", err)
	}
	if _, err := p.Collect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("collect error = %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := p.Collect(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestOptionalParsers(t *testing.T) {
	for _, text := range []string{"", "full avg10=4", "some avg10=NaN", "some avg10=101", "some avg10=-1"} {
		if parsePressure(text) != nil {
			t.Errorf("invalid pressure accepted: %s", text)
		}
	}
	if got := parsePressure("some avg10=0.25 avg60=0.50"); got == nil || *got != 0.25 {
		t.Fatalf("pressure = %v", got)
	}
	if got := parseOSRelease("# comment\nNAME='Linux'\nPRETTY_NAME=\"A \\\"quoted\\\" Linux\"\n"); got["NAME"] != "Linux" || got["PRETTY_NAME"] != "A \"quoted\" Linux" {
		t.Fatalf("release = %v", got)
	}
	if cores, model := parseCPUInfo("processor : 0\nHardware : ARM board\n"); cores != 0 || model != "ARM board" {
		t.Fatalf("CPU info = %d %q", cores, model)
	}
}

func copyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir("testdata", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("testdata", path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}
