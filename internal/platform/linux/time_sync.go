package linux

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/platform/timesync"
)

const ntpManagedMarker = "# InfraPulse-managed time synchronization; edit local Collector config, not this file.\n"
const ntpManagedTimesyncd = "/etc/systemd/timesyncd.conf.d/90-infrapulse.conf"

// TimeSync configures an existing, active systemd-managed OS NTP client. It
// never implements NTP, starts a competing client, or adjusts timestamps.
type TimeSync struct {
	root   string
	runner ntpRunner
	mu     sync.Mutex
}

func NewTimeSync() *TimeSync { return &TimeSync{root: "/", runner: ntpExecRunner{}} }

type ntpService struct {
	id        string
	available bool
	active    bool
	pid       int
	op        ntpOperation
}

func (p *TimeSync) Status(ctx context.Context, settings timesync.Settings) (timesync.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status(ctx, settings)
}

func (p *TimeSync) status(ctx context.Context, settings timesync.Settings) (timesync.Status, error) {
	settings, err := timesync.Normalize(settings)
	s := timesync.Status{Enabled: settings.Enabled, DesiredServers: slices.Clone(settings.Servers), CheckedAt: time.Now().UTC()}
	if err != nil {
		return ntpFailure(s, err)
	}
	if !settings.Enabled {
		return s, nil
	}
	service, err := p.selectService(ctx)
	s.Provider, s.Available, s.Active = ntpProviderName(service), service.available, service.active
	if err != nil {
		return ntpFailure(s, err)
	}
	if !service.active {
		return ntpFailure(s, errors.New("time synchronization service is inactive; administrator must enable and start the selected OS service"))
	}
	if s.Provider == "chrony" {
		layout, err := p.chronyLayout(ctx, service, settings.Servers)
		s.ManagedFile, s.ConfiguredServers = layout.managed, layout.servers
		if err != nil {
			return ntpFailure(s, err)
		}
		s.Configured = ntpSameServers(s.ConfiguredServers, settings.Servers)
		tracking, err := p.run(ctx, ntpTracking)
		if err != nil {
			return ntpFailure(s, err)
		}
		s.Synchronized, err = ntpParseTracking(tracking)
		if err != nil {
			return ntpFailure(s, err)
		}
		sources, err := p.run(ctx, ntpSources)
		if err != nil {
			return ntpFailure(s, err)
		}
		if err := ntpCheckSources(sources, settings.Servers); err != nil {
			s.Configured = false
			return ntpFailure(s, err)
		}
	} else {
		s.ManagedFile = ntpManagedTimesyncd
		runtime, err := p.timesyncStatus(ctx)
		if err != nil {
			return ntpFailure(s, err)
		}
		s.ConfiguredServers = ntpUnique(append(slices.Clone(runtime.system), runtime.link...))
		s.Configured = ntpSameServers(runtime.system, settings.Servers) && ntpSubset(runtime.link, settings.Servers) && ntpSubset(runtime.fallback, settings.Servers)
		clock, err := p.run(ctx, ntpClockStatus)
		if err != nil {
			return ntpFailure(s, err)
		}
		props, err := ntpProperties(clock)
		if err != nil {
			return ntpFailure(s, err)
		}
		value, ok := props["NTPSynchronized"]
		if !ok || (value != "yes" && value != "no") {
			return ntpFailure(s, errors.New("OS synchronization state is unavailable"))
		}
		v := value == "yes"
		s.Synchronized = &v
	}
	if !s.Configured {
		s.Diagnostic = "corporate NTP configuration differs from desired servers; use the explicit time-sync apply deployment helper"
	} else if s.Synchronized != nil && !*s.Synchronized {
		s.Diagnostic = "OS time synchronization has not been achieved yet; monitoring continues"
	}
	return s, nil
}

func (p *TimeSync) Apply(ctx context.Context, settings timesync.Settings) (timesync.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	settings, err := timesync.Normalize(settings)
	s := timesync.Status{Enabled: settings.Enabled, DesiredServers: slices.Clone(settings.Servers), CheckedAt: time.Now().UTC()}
	if err != nil {
		return ntpFailure(s, err)
	}
	if !settings.Enabled {
		return s, nil // disabling does not remove configuration or stop an OS client
	}
	service, err := p.selectService(ctx)
	s.Provider, s.Available, s.Active = ntpProviderName(service), service.available, service.active
	if err != nil {
		return ntpFailure(s, err)
	}
	if !service.active {
		return ntpFailure(s, errors.New("time synchronization service is inactive; administrator must enable and start it before apply"))
	}
	var managed string
	var desired []byte
	if s.Provider == "chrony" {
		layout, err := p.chronyLayout(ctx, service, settings.Servers)
		s.ManagedFile, s.ConfiguredServers = layout.managed, layout.servers
		if err != nil {
			return ntpFailure(s, err)
		}
		if ntpSameServers(layout.servers, settings.Servers) {
			return p.status(ctx, settings)
		}
		managed = layout.managed
		if managed == "" {
			return ntpFailure(s, errors.New("setup_required: chrony has no safe persistent confdir/sourcedir/include fragment; administrator must configure an include mechanism"))
		}
		var b strings.Builder
		b.WriteString(ntpManagedMarker)
		for _, server := range settings.Servers {
			fmt.Fprintf(&b, "server %s iburst\n", server)
		}
		desired = []byte(b.String())
	} else {
		managed, s.ManagedFile = ntpManagedTimesyncd, ntpManagedTimesyncd
		// Empty FallbackNTP alone can restore distribution-compiled defaults on
		// some systemd releases. Explicit corporate fallback servers avoid this.
		desired = []byte(ntpManagedMarker + "[Time]\nNTP=\nNTP=" + strings.Join(settings.Servers, " ") + "\nFallbackNTP=\nFallbackNTP=" + strings.Join(settings.Servers, " ") + "\n")
		runtime, err := p.timesyncStatus(ctx)
		if err != nil {
			return ntpFailure(s, err)
		}
		if !ntpSubset(runtime.link, settings.Servers) {
			return ntpFailure(s, errors.New("setup_required: systemd-timesyncd has other per-link/DHCP NTP servers; administrator must disable or configure them"))
		}
		if ntpSameServers(runtime.system, settings.Servers) && ntpSubset(runtime.fallback, settings.Servers) {
			old, exists, err := p.ownedFile(ctx, managed)
			if err != nil {
				return ntpFailure(s, err)
			}
			if !exists || bytes.Equal(old, desired) {
				return p.status(ctx, settings)
			}
		}
	}
	old, exists, err := p.ownedFile(ctx, managed)
	if err != nil {
		return ntpFailure(s, err)
	}
	if exists && bytes.Equal(old, desired) {
		// An unchanged file never causes repeated restarts. An interrupted
		// deployment or administrator override is reported for manual recovery.
		result, statusErr := p.status(ctx, settings)
		if statusErr == nil && !result.Configured {
			return ntpFailure(result, errors.New("setup_required: managed file is unchanged but OS runtime differs; administrator must check overrides and restart the selected service"))
		}
		return result, statusErr
	} else {
		if err := p.writeOwned(ctx, managed, desired); err != nil {
			return ntpFailure(s, err)
		}
		if err := p.restart(ctx, service); err != nil {
			// Cleanup must still restore ownership after an expired apply deadline.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			rollbackErr := p.restoreOwned(cleanup, managed, desired, old, exists)
			if rollbackErr != nil {
				return ntpFailure(s, fmt.Errorf("OS service restart failed (%w); managed-file rollback also failed: %v", err, rollbackErr))
			}
			// Restore the prior service configuration, without starting another
			// client. A failed recovery is explicitly reported to the operator.
			if recoveryErr := p.restart(cleanup, service); recoveryErr != nil {
				return ntpFailure(s, fmt.Errorf("OS service restart failed (%w); previous file restored but service recovery failed: %v", err, recoveryErr))
			}
			return ntpFailure(s, fmt.Errorf("OS service restart failed; previous managed configuration restored: %w", err))
		}
	}
	result, err := p.status(ctx, settings)
	result.Changed = true
	if err == nil && !result.Configured {
		return ntpFailure(result, errors.New("setup_required: OS service did not adopt the desired corporate NTP servers; check administrator overrides"))
	}
	return result, err
}

func ntpFailure(s timesync.Status, err error) (timesync.Status, error) {
	s.Diagnostic = err.Error()
	return s, err
}

func (p *TimeSync) run(ctx context.Context, op ntpOperation) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := p.runner.Run(ctx, op)
	if len(data) > 64<<10 {
		return nil, errors.New("time synchronization OS response exceeds limit")
	}
	return data, err
}

func ntpProperties(data []byte) (map[string]string, error) {
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t\r") {
			return nil, errors.New("invalid structured OS service properties")
		}
		if _, exists := result[key]; exists {
			return nil, errors.New("duplicate structured OS service property")
		}
		result[key] = value
	}
	return result, nil
}

func (p *TimeSync) selectService(ctx context.Context) (ntpService, error) {
	var units []ntpService
	for _, op := range []ntpOperation{ntpChronyService, ntpChronydService, ntpTimesyncdService, ntpOtherService, ntpNtpdService, ntpOpenntpdService, ntpNtpsecService} {
		data, err := p.run(ctx, op)
		if err != nil {
			return ntpService{}, fmt.Errorf("OS provider detection requires working systemd service properties: %w", err)
		}
		props, err := ntpProperties(data)
		if err != nil {
			return ntpService{}, err
		}
		load, ok := props["LoadState"]
		if !ok || props["ActiveState"] == "" || props["Id"] == "" {
			return ntpService{}, errors.New("OS provider detection properties are incomplete")
		}
		pid, err := strconv.Atoi(props["MainPID"])
		if err != nil || pid < 0 {
			return ntpService{}, errors.New("OS provider detection MainPID is invalid")
		}
		active := props["ActiveState"] != "inactive" && props["ActiveState"] != "failed"
		units = append(units, ntpService{id: props["Id"], available: load == "loaded", active: active, pid: pid, op: op})
	}
	// Aliases such as chrony.service and chronyd.service may name one unit.
	var selected ntpService
	for _, u := range units[:2] {
		if u.available && u.active {
			selected = u
			break
		}
	}
	if selected.id == "" && units[2].available && units[2].active {
		selected = units[2]
	}
	if selected.id == "" {
		for _, u := range units[:3] {
			if u.available {
				selected = u
				break
			}
		}
	}
	for _, u := range units {
		if !u.active || (u.id == selected.id && u.pid == selected.pid) {
			continue
		}
		// Two loaded chrony aliases with a shared process are the same client.
		if ntpProviderName(selected) == "chrony" && ntpProviderName(u) == "chrony" && selected.pid > 0 && u.pid == selected.pid {
			continue
		}
		return selected, errors.New("conflicting active NTP client detected; administrator must select a single OS time synchronization service")
	}
	if selected.id == "" {
		return selected, errors.New("unsupported: no chrony or systemd-timesyncd service is available; install/configure an OS NTP client manually")
	}
	return selected, nil
}

func ntpProviderName(s ntpService) string {
	switch s.op {
	case ntpChronyService, ntpChronydService:
		return "chrony"
	case ntpTimesyncdService:
		return "systemd-timesyncd"
	default:
		return ""
	}
}

func (p *TimeSync) restart(ctx context.Context, s ntpService) error {
	if p.root == "/" && os.Geteuid() != 0 {
		return fmt.Errorf("time synchronization service restart requires elevated root privileges: %w", os.ErrPermission)
	}
	op := ntpRestartTimesyncd
	if s.op == ntpChronyService {
		op = ntpRestartChrony
	} else if s.op == ntpChronydService {
		op = ntpRestartChronyd
	}
	_, err := p.run(ctx, op)
	return err
}

type ntpTimesyncState struct{ system, link, fallback []string }

func (p *TimeSync) timesyncStatus(ctx context.Context) (ntpTimesyncState, error) {
	data, err := p.run(ctx, ntpTimesyncStatus)
	if err != nil {
		return ntpTimesyncState{}, err
	}
	props, err := ntpProperties(data)
	if err != nil {
		return ntpTimesyncState{}, err
	}
	for _, required := range []string{"SystemNTPServers", "LinkNTPServers", "FallbackNTPServers"} {
		if _, ok := props[required]; !ok {
			return ntpTimesyncState{}, errors.New("OS NTP server properties unavailable; automatic apply requires show-timesync support")
		}
	}
	return ntpTimesyncState{strings.Fields(props["SystemNTPServers"]), strings.Fields(props["LinkNTPServers"]), strings.Fields(props["FallbackNTPServers"])}, nil
}

func ntpSameServers(a, b []string) bool { return ntpSubset(a, b) && ntpSubset(b, a) }
func ntpSubset(a, b []string) bool {
	for _, value := range a {
		if !slices.Contains(b, value) {
			return false
		}
	}
	return true
}
func ntpUnique(a []string) []string {
	var result []string
	for _, value := range a {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func ntpParseTracking(data []byte) (*bool, error) {
	r := csv.NewReader(bytes.NewReader(data))
	record, err := r.Read()
	if err != nil || len(record) != 14 {
		return nil, errors.New("invalid chronyc CSV tracking response")
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected extra chronyc tracking records")
	}
	var synchronized bool
	switch record[13] {
	case "Normal", "Insert second", "Delete second":
		// chrony's local mode can report normal leap state without NTP.
		synchronized = record[0] != "7F7F0101" && record[0] != "00000000" && record[1] != ""
	case "Not synchronised":
	default:
		return nil, errors.New("unknown chronyc leap status")
	}
	return &synchronized, nil
}

func ntpCheckSources(data []byte, servers []string) error {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	for count := 0; ; count++ {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || len(record) != 10 || count >= 1024 {
			return errors.New("invalid or oversized chronyc CSV sources response")
		}
		if (record[0] != "^" && record[0] != "=") || !slices.Contains(servers, record[2]) {
			return errors.New("setup_required: chrony has a runtime source outside corporate NTP configuration")
		}
	}
}

func (p *TimeSync) physical(logical string) string {
	return filepath.Join(p.root, filepath.FromSlash(strings.TrimPrefix(logical, "/")))
}

func (p *TimeSync) safePath(logical string, missing bool) error {
	if !strings.HasPrefix(logical, "/") || path.Clean(logical) != logical {
		return errors.New("setup_required: OS configuration path must be absolute and normalized")
	}
	current := p.root
	for _, component := range strings.Split(strings.TrimPrefix(logical, "/"), "/") {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && missing {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("setup_required: symlink in OS time synchronization configuration path")
		}
		if current != p.physical(logical) && !info.IsDir() {
			return errors.New("setup_required: OS configuration parent is not a directory")
		}
	}
	return nil
}

func (p *TimeSync) ownedFile(ctx context.Context, logical string) ([]byte, bool, error) {
	if !strings.HasPrefix(logical, "/etc/") {
		return nil, false, errors.New("managed time synchronization files must reside under /etc")
	}
	if err := p.safePath(logical, true); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(p.physical(logical))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("setup_required: managed OS configuration is not a regular file")
	}
	data, err := readBounded(ctx, p.physical(logical))
	if err != nil {
		return nil, false, err
	}
	if !bytes.HasPrefix(data, []byte(ntpManagedMarker)) {
		return nil, false, errors.New("refusing to overwrite administrator-owned time synchronization file")
	}
	return data, true, nil
}

func (p *TimeSync) writeOwned(ctx context.Context, logical string, data []byte) error {
	if p.root == "/" && os.Geteuid() != 0 {
		return fmt.Errorf("time synchronization configuration could not be applied without elevated root privileges: %w", os.ErrPermission)
	}
	previous, existed, err := p.ownedFile(ctx, logical)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(p.physical(logical))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".infrapulse-ntp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	current, exists, err := p.ownedFile(ctx, logical)
	if err != nil {
		return err
	}
	if exists != existed || !bytes.Equal(previous, current) {
		return errors.New("managed file changed concurrently; publication refused")
	}
	if !existed {
		// A hard-link publication cannot replace an administrator file created
		// between the ownership check and publication.
		return os.Link(tmp, p.physical(logical))
	}
	return os.Rename(tmp, p.physical(logical))
}

func (p *TimeSync) restoreOwned(ctx context.Context, logical string, desired, old []byte, existed bool) error {
	current, exists, err := p.ownedFile(ctx, logical)
	if err != nil {
		return err
	}
	if !exists || !bytes.Equal(current, desired) {
		return errors.New("managed file changed concurrently; rollback refused")
	}
	if existed {
		return p.writeOwned(ctx, logical, old)
	}
	return os.Remove(p.physical(logical))
}
