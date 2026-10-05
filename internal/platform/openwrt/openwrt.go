package openwrt

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/linux"
)

type Options struct {
	ProcRoot    string
	SysRoot     string
	EtcRoot     string
	Runner      Runner
	CallTimeout time.Duration
}

type Platform struct {
	linux       *linux.Platform
	etcRoot     string
	runner      Runner
	callTimeout time.Duration
	mu          sync.Mutex
	services    bool
	firewall    bool
}

func New(procRoot, sysRoot string) *Platform {
	return NewWithOptions(Options{ProcRoot: procRoot, SysRoot: sysRoot})
}

// NewWithOptions supports isolated roots and typed schema fixtures. Production
// construction supplies no Runner and always uses the fixed local ubus binary.
func NewWithOptions(options Options) *Platform {
	if options.ProcRoot == "" {
		options.ProcRoot = "/proc"
	}
	if options.SysRoot == "" {
		options.SysRoot = "/sys"
	}
	if options.EtcRoot == "" {
		options.EtcRoot = filepath.Join(filepath.Dir(options.ProcRoot), "etc")
		if options.ProcRoot == "/proc" {
			options.EtcRoot = "/etc"
		}
	}
	if options.Runner == nil {
		options.Runner = execRunner{}
	}
	if options.CallTimeout <= 0 || options.CallTimeout > 30*time.Second {
		options.CallTimeout = defaultTimeout
	}
	return &Platform{linux: linux.New(options.ProcRoot, options.SysRoot),
		etcRoot: options.EtcRoot, runner: options.Runner, callTimeout: options.CallTimeout}
}

func (p *Platform) Capabilities() []model.Capability {
	p.mu.Lock()
	services, firewall := p.services, p.firewall
	p.mu.Unlock()
	caps := p.linux.Capabilities()
	for _, capability := range []struct {
		name      string
		available bool
	}{
		{"services.read", services}, {"firewall.read", firewall},
	} {
		c := model.Capability{Name: capability.name, Supported: capability.available}
		if !c.Supported {
			c.Reason = "OpenWrt source unavailable or not yet observed"
		}
		caps = append(caps, c)
	}
	return caps
}

// Collect reuses procfs/sysfs measurements and Linux CPU baselines. Expensive
// OpenWrt-specific reads run only with inventory, never in the fast job.
func (p *Platform) Collect(ctx context.Context) (model.TelemetrySample, error) {
	return p.linux.Collect(ctx)
}

func (p *Platform) Identity(ctx context.Context) (model.Device, error) {
	d, err := p.linux.Identity(ctx)
	if err != nil {
		return model.Device{}, err
	}
	d.Driver, d.Source, d.Type = "local-openwrt", "local", "unknown"
	release, evidence := readRelease(ctx, p.etcRoot)
	d.PlatformEvidence = evidence
	if len(evidence) == 0 {
		// This constructor may be used directly by a fixture or the file may
		// disappear after selection. Do not fabricate a confirmed release.
		d.PlatformEvidence = append(d.PlatformEvidence, model.Evidence{Source: "OpenWrt.release", Value: "distribution evidence unavailable", Confidence: "unresolved"})
	}
	applyRelease(&d, release)
	observed := time.Now().UTC()
	data, boardErr := p.read(ctx, Board)
	if boardErr == nil {
		var b board
		b, boardErr = parseBoard(data)
		if boardErr == nil {
			applyBoard(&d, b)
		}
	}
	d.PlatformEvidence = append(d.PlatformEvidence, sourceEvidence("ubus.system.board", boardErr))
	data, networkErr := p.read(ctx, Networks)
	if networkErr == nil {
		d.Networks, networkErr = parseNetworks(data, observed)
	}
	d.PlatformEvidence = append(d.PlatformEvidence, sourceEvidence("ubus.network.interface.dump", networkErr))
	data, serviceErr := p.read(ctx, Services)
	if serviceErr == nil {
		d.Services, serviceErr = parseServices(data, observed)
		if serviceErr == nil {
			p.bootEnablement(ctx, d.Services)
		}
	}
	d.PlatformEvidence = append(d.PlatformEvidence, sourceEvidence("ubus.service.list", serviceErr))
	data, firewallErr := p.read(ctx, Firewall)
	if firewallErr == nil {
		d.Firewall, firewallErr = parseFirewall(data, observed)
	}
	if firewallErr != nil {
		d.Firewall = &model.FirewallSnapshot{ObservedAt: observed, Source: "ubus.uci.firewall.configuration", Completeness: "failed"}
	}
	d.PlatformEvidence = append(d.PlatformEvidence, sourceEvidence("ubus.uci.firewall.configuration", firewallErr))
	if err := ctx.Err(); err != nil {
		return model.Device{}, err
	}
	p.mu.Lock()
	p.services, p.firewall = serviceErr == nil, firewallErr == nil
	p.mu.Unlock()
	d.Capabilities = p.Capabilities()
	return d, nil
}

func (p *Platform) read(ctx context.Context, op Operation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout := p.callTimeout
	if deadline, ok := ctx.Deadline(); ok {
		// Leave budget for the remaining optional reads and publication. A
		// missing/hung ubus must not consume the entire inventory/startup job.
		if share := time.Until(deadline) / 5; share < timeout {
			timeout = share
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := p.runner.Run(callCtx, op)
	if err != nil {
		return nil, err
	}
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, errOutputLimit
	}
	return data, nil
}

func sourceEvidence(source string, err error) model.Evidence {
	e := model.Evidence{Source: source, Value: "read-only schema observed", Confidence: "high"}
	if err != nil {
		e.Confidence, e.Value = "unresolved", "source unavailable: read failed or malformed output"
		if errors.Is(err, context.DeadlineExceeded) {
			e.Value = "read timeout"
		}
		// Errors from runners can carry credentials. Only a fixed category is
		// stored in inventory/diagnostics, never arbitrary error text.
	}
	return e
}

func applyRelease(d *model.Device, release map[string]string) {
	for _, key := range []string{"VERSION_ID", "DISTRIB_RELEASE"} {
		if release[key] != "" {
			d.SoftwareVersion = release[key]
			break
		}
	}
	for _, key := range []string{"OPENWRT_RELEASE", "DISTRIB_DESCRIPTION", "PRETTY_NAME"} {
		if release[key] != "" {
			d.Distribution = release[key]
			break
		}
	}
	for _, key := range []string{"BUILD_ID", "DISTRIB_REVISION"} {
		if release[key] != "" {
			d.FirmwareVersion = release[key]
			break
		}
	}
	// Distribution target and package architecture differ from Go architecture.
	// Preserve their evidence without relabeling the binary architecture.
	for _, key := range []string{"DISTRIB_TARGET", "DISTRIB_ARCH", "OPENWRT_BOARD", "OPENWRT_ARCH"} {
		if release[key] != "" {
			d.PlatformEvidence = append(d.PlatformEvidence, model.Evidence{Source: "OpenWrt.release." + key, Value: release[key], Confidence: "high"})
		}
	}
}

func applyBoard(d *model.Device, b board) {
	for _, field := range []struct {
		value string
		dest  *string
	}{
		{b.hostname, &d.Hostname}, {b.kernel, &d.Kernel}, {b.model, &d.Model},
		{b.name, &d.BoardName}, {b.version, &d.SoftwareVersion}, {b.revision, &d.FirmwareVersion},
		{b.description, &d.Distribution}, {b.system, &d.SystemDescription},
	} {
		if field.value != "" {
			*field.dest = field.value
		}
	}
	if b.hostname != "" {
		d.Name = b.hostname
	}
	for _, value := range []struct{ source, value string }{
		{"release.target", b.target}, {"rootfs_type", b.rootfs},
	} {
		if value.value != "" {
			d.PlatformEvidence = append(d.PlatformEvidence, model.Evidence{Source: "ubus.system.board." + value.source, Value: value.value, Confidence: "high"})
		}
	}
}

func (p *Platform) bootEnablement(ctx context.Context, services []model.Service) {
	if ctx.Err() != nil {
		return
	}
	// A positive S?? symlink proves the boot start link exists. Its absence is
	// inconclusive without executing/sourcing START/STOP definitions, which P2
	// does not do; Enabled therefore remains nil for unknown/disabled services.
	dir, err := os.Open(filepath.Join(p.etcRoot, "rc.d"))
	if err != nil {
		return
	}
	defer dir.Close()
	entries, err := dir.ReadDir(maxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > maxEntries {
		return
	}
	enabled := make(map[string]bool)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		name := entry.Name()
		if len(name) < 4 || name[0] != 'S' || name[1] < '0' || name[1] > '9' || name[2] < '0' || name[2] > '9' || entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		service := name[3:]
		target, err := os.Readlink(filepath.Join(p.etcRoot, "rc.d", name))
		if err != nil {
			continue
		}
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(p.etcRoot, "rc.d", resolved)
		}
		if filepath.Clean(resolved) != filepath.Clean(filepath.Join(p.etcRoot, "init.d", service)) {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && !strings.ContainsAny(service, "/\\") {
			enabled[service] = true
		}
	}
	for i := range services {
		if enabled[services[i].Name] {
			value := true
			services[i].Enabled = &value
		}
	}
}
