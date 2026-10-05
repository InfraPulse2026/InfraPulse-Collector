// Package linux collects read-only Linux data from procfs, sysfs and OS APIs.
package linux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/model"
)

const (
	maxFileBytes          = 4 << 20
	maxInterfaces         = 4096
	maxInterfaceAddresses = 65536
)

type Platform struct {
	procRoot       string
	sysRoot        string
	etcRoot        string
	actualRoots    bool
	mu             sync.Mutex
	previous       *cpuTimes
	previousBoot   string
	previousUptime float64
}

// New supports isolated fixture roots as well as actual procfs/sysfs. Fixture
// collectors never read interface addresses or hostname from the host OS.
func New(procRoot, sysRoot string) *Platform {
	procRoot, sysRoot = filepath.Clean(procRoot), filepath.Clean(sysRoot)
	actual := procRoot == "/proc" && sysRoot == "/sys"
	etcRoot := filepath.Join(filepath.Dir(procRoot), "etc")
	if actual {
		etcRoot = "/etc"
	}
	return &Platform{procRoot: procRoot, sysRoot: sysRoot, etcRoot: etcRoot, actualRoots: actual}
}

func (p *Platform) Capabilities() []model.Capability {
	return []model.Capability{
		{Name: "system.read", Supported: true},
		{Name: "inventory.read", Supported: true},
		{Name: "cpu.read", Supported: true},
		{Name: "memory.read", Supported: true},
		{Name: "interfaces.read", Supported: true},
		{Name: "telemetry.read", Supported: true},
	}
}

func (p *Platform) Identity(ctx context.Context) (model.Device, error) {
	if err := ctx.Err(); err != nil {
		return model.Device{}, err
	}
	hostname := strings.TrimSpace(p.optional(ctx, filepath.Join(p.procRoot, "sys/kernel/hostname")))
	if hostname == "" && p.actualRoots {
		var err error
		hostname, err = os.Hostname()
		if err != nil {
			return model.Device{}, fmt.Errorf("local hostname: %w", err)
		}
	}
	uptime, err := p.uptime(ctx)
	if err != nil {
		return model.Device{}, err
	}
	release := parseOSRelease(p.optional(ctx, filepath.Join(p.etcRoot, "os-release")))
	if len(release) == 0 {
		release = parseOSRelease(p.optional(ctx, filepath.Join(p.etcRoot, "openwrt_release")))
	}
	device := model.Device{
		Name: hostname, Hostname: hostname, Type: "unknown", OS: "linux",
		Driver: "local-linux", Architecture: runtime.GOARCH,
		Kernel:       strings.TrimSpace(p.optional(ctx, filepath.Join(p.procRoot, "sys/kernel/osrelease"))),
		Distribution: release["PRETTY_NAME"], SoftwareVersion: release["VERSION_ID"],
		UptimeSeconds: uptime, BootID: p.bootID(ctx), Online: true,
		LastSeen: time.Now().UTC(), Capabilities: p.Capabilities(),
	}
	if device.Distribution == "" {
		device.Distribution = release["NAME"]
	}
	if device.Distribution == "" {
		device.Distribution = release["DISTRIB_DESCRIPTION"]
	}
	if device.SoftwareVersion == "" {
		device.SoftwareVersion = release["DISTRIB_RELEASE"]
	}
	// DMI metadata is optional, and absent on many ARM/OpenWrt systems.
	device.Vendor = strings.TrimSpace(p.optional(ctx, filepath.Join(p.sysRoot, "class/dmi/id/sys_vendor")))
	device.Model = strings.TrimSpace(p.optional(ctx, filepath.Join(p.sysRoot, "class/dmi/id/product_name")))
	device.SerialNumber = strings.TrimSpace(p.optional(ctx, filepath.Join(p.sysRoot, "class/dmi/id/product_serial")))
	device.HardwareRevision = strings.TrimSpace(p.optional(ctx, filepath.Join(p.sysRoot, "class/dmi/id/product_version")))
	if err := ctx.Err(); err != nil {
		return model.Device{}, err
	}
	return device, nil
}

// Collect serializes samples so CPU baselines remain consistent if callers
// invoke this collector concurrently. A baseline is committed only after all
// mandatory measurements were successfully collected.
func (p *Platform) Collect(ctx context.Context) (model.TelemetrySample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	observed := time.Now().UTC()
	stat, err := readBounded(ctx, filepath.Join(p.procRoot, "stat"))
	if err != nil {
		return model.TelemetrySample{}, err
	}
	times, logical, err := parseCPUStat(string(stat))
	if err != nil {
		return model.TelemetrySample{}, fmt.Errorf("parse proc/stat: %w", err)
	}
	loadData, err := readBounded(ctx, filepath.Join(p.procRoot, "loadavg"))
	if err != nil {
		return model.TelemetrySample{}, err
	}
	loads, err := parseLoad(string(loadData))
	if err != nil {
		return model.TelemetrySample{}, fmt.Errorf("parse proc/loadavg: %w", err)
	}
	memData, err := readBounded(ctx, filepath.Join(p.procRoot, "meminfo"))
	if err != nil {
		return model.TelemetrySample{}, err
	}
	memory, err := parseMemory(string(memData))
	if err != nil {
		return model.TelemetrySample{}, fmt.Errorf("parse proc/meminfo: %w", err)
	}
	netData, err := readBounded(ctx, filepath.Join(p.procRoot, "net/dev"))
	if err != nil {
		return model.TelemetrySample{}, err
	}
	interfaces, err := parseInterfaces(string(netData))
	if err != nil {
		return model.TelemetrySample{}, fmt.Errorf("parse proc/net/dev: %w", err)
	}
	if err = p.interfaceMetadata(ctx, interfaces); err != nil {
		return model.TelemetrySample{}, err
	}
	uptime, err := p.uptime(ctx)
	if err != nil {
		return model.TelemetrySample{}, err
	}
	boot := p.bootID(ctx)
	cores, cpuModel := parseCPUInfo(p.optional(ctx, filepath.Join(p.procRoot, "cpuinfo")))
	cpu := &model.CPUStats{LogicalCPUs: logical, PhysicalCores: cores, Model: cpuModel,
		Load1: loads[0], Load5: loads[1], Load15: loads[2]}
	if p.previous != nil && boot == p.previousBoot && uptime >= p.previousUptime {
		cpu.UsagePercent = cpuUsage(*p.previous, times)
	}
	var metrics []model.Metric
	if v := parsePressure(p.optional(ctx, filepath.Join(p.procRoot, "pressure/memory"))); v != nil {
		memory.PressureSomeAvg10 = v
	}
	for _, pressure := range []string{"cpu", "io"} {
		if v := parsePressure(p.optional(ctx, filepath.Join(p.procRoot, "pressure", pressure))); v != nil {
			metrics = append(metrics, model.Metric{Name: pressure + ".pressure.some.avg10", Value: *v, Unit: "percent"})
		}
	}
	if err := ctx.Err(); err != nil {
		return model.TelemetrySample{}, err
	}
	p.previous, p.previousBoot, p.previousUptime = &times, boot, uptime
	return model.TelemetrySample{ObservedAt: observed, CollectedAt: time.Now().UTC(), Source: "local",
		BootID: boot, UptimeSeconds: uptime, CPU: cpu, Memory: &memory,
		Interfaces: interfaces, Metrics: metrics}, nil
}

func readBounded(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("read %s: file exceeds %d bytes", path, maxFileBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func (p *Platform) optional(ctx context.Context, path string) string {
	data, err := readBounded(ctx, path)
	if err != nil {
		return ""
	}
	return string(data)
}

func (p *Platform) bootID(ctx context.Context) string {
	return strings.TrimSpace(p.optional(ctx, filepath.Join(p.procRoot, "sys/kernel/random/boot_id")))
}

func (p *Platform) uptime(ctx context.Context) (float64, error) {
	data, err := readBounded(ctx, filepath.Join(p.procRoot, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("parse proc/uptime: missing uptime")
	}
	v, err := finiteNonnegative(fields[0])
	if err != nil {
		return 0, fmt.Errorf("parse proc/uptime: %w", err)
	}
	return v, nil
}

func (p *Platform) interfaceMetadata(ctx context.Context, interfaces []model.Interface) error {
	// OS address discovery applies only to the actual host, never to fixtures.
	addresses := map[string][]string{}
	if p.actualRoots {
		addressCount := 0
		actual, err := net.Interfaces()
		if err != nil {
			return fmt.Errorf("list local interfaces: %w", err)
		}
		if len(actual) > maxInterfaces {
			return errors.New("local interface count exceeds limit")
		}
		for _, iface := range actual {
			if err := ctx.Err(); err != nil {
				return err
			}
			addrs, err := iface.Addrs()
			if err != nil {
				// An interface can disappear between the list and address read.
				continue
			}
			for _, address := range addrs {
				addressCount++
				if addressCount > maxInterfaceAddresses {
					return errors.New("interface address count exceeds limit")
				}
				addresses[iface.Name] = append(addresses[iface.Name], address.String())
			}
		}
	}
	for i := range interfaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		iface := &interfaces[i]
		iface.Metadata = &model.InterfaceMetadata{}
		root := filepath.Join(p.sysRoot, "class/net", iface.Name)
		if index, err := strconv.ParseUint(strings.TrimSpace(p.optional(ctx, filepath.Join(root, "ifindex"))), 10, 31); err == nil {
			iface.Index = int(index)
			iface.Metadata.Index = index > 0
		}
		if mtu, err := strconv.ParseUint(strings.TrimSpace(p.optional(ctx, filepath.Join(root, "mtu"))), 10, 31); err == nil {
			iface.MTU = int(mtu)
			iface.Metadata.MTU = mtu > 0
		}
		iface.MAC = strings.TrimSpace(p.optional(ctx, filepath.Join(root, "address")))
		if iface.MAC != "" {
			if _, err := net.ParseMAC(iface.MAC); err != nil {
				iface.MAC = ""
			}
		}
		iface.OperState = strings.TrimSpace(p.optional(ctx, filepath.Join(root, "operstate")))
		switch iface.OperState {
		case "unknown", "notpresent", "down", "lowerlayerdown", "testing", "dormant", "up":
		default:
			iface.OperState = "unknown"
		}
		flags, err := strconv.ParseUint(strings.TrimSpace(p.optional(ctx, filepath.Join(root, "flags"))), 0, 64)
		if err == nil {
			iface.AdminUp = flags&1 != 0 // IFF_UP is the administrative state.
			iface.Metadata.AdminStatus = true
		}
		speed, err := strconv.ParseUint(strings.TrimSpace(p.optional(ctx, filepath.Join(root, "speed"))), 10, 64)
		if err == nil && speed > 0 && speed <= math.MaxUint64/1000000 {
			bits := speed * 1000000 // sysfs reports megabits per second.
			iface.SpeedBitsPerSecond = &bits
		}
		iface.ID = fmt.Sprintf("%d:%s:%s", iface.Index, iface.Name, iface.MAC)
		iface.Addresses = addresses[iface.Name]
	}
	return nil
}
