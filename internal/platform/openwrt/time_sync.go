package openwrt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"InfraPulseAgent/internal/platform/timesync"
)

// sysntpd reads the global system.ntp UCI section, not a separate managed
// fragment. Preserve its administrator ownership: this provider only observes
// configuration and service state. Installation instructions perform setup.
type timeSyncProvider struct{ reader *Platform }

func NewTimeSync() timesync.Provider { return NewTimeSyncWithRunner(nil) }

// NewTimeSyncWithRunner reuses the closed set of bounded local ubus reads.
func NewTimeSyncWithRunner(runner Runner) timesync.Provider {
	return &timeSyncProvider{reader: NewWithOptions(Options{Runner: runner})}
}

func (p *timeSyncProvider) Status(ctx context.Context, settings timesync.Settings) (timesync.Status, error) {
	status := timesync.Status{Enabled: settings.Enabled, Provider: "openwrt-sysntpd", CheckedAt: time.Now().UTC()}
	settings, err := timesync.Normalize(settings)
	if err != nil {
		status.Diagnostic = "invalid time synchronization configuration"
		return status, err
	}
	status.DesiredServers = slices.Clone(settings.Servers)
	if !settings.Enabled {
		status.Diagnostic = "time synchronization management disabled"
		return status, nil
	}
	if err := ctx.Err(); err != nil {
		status.Diagnostic = "OpenWrt time synchronization status canceled"
		return status, err
	}
	data, err := p.reader.read(ctx, Services)
	if err != nil {
		return timeSyncReadFailure(status, "procd service state", err)
	}
	services, err := parseServices(data, status.CheckedAt)
	if err != nil {
		status.Diagnostic = "malformed OpenWrt procd service state"
		return status, errors.New(status.Diagnostic)
	}
	serviceState := "unknown"
	for _, service := range services {
		if service.Name == "sysntpd" {
			status.Available = true
			status.Active = service.State == "running"
			serviceState = service.State
			break
		}
	}
	if !status.Available {
		status.Diagnostic = "OpenWrt sysntpd service unavailable"
		return status, errors.New(status.Diagnostic)
	}
	data, err = p.reader.read(ctx, NTPConfig)
	if err != nil {
		return timeSyncReadFailure(status, "system.ntp UCI configuration", err)
	}
	servers, enabled, useDHCP, err := parseTimeSyncConfig(data)
	if err != nil {
		status.Diagnostic = "malformed OpenWrt system.ntp UCI configuration"
		return status, errors.New(status.Diagnostic)
	}
	status.ConfiguredServers = servers
	status.Configured = enabled && !useDHCP && equalTimeSyncServers(servers, settings.Servers)
	if !status.Configured {
		status.Diagnostic = "setup_required: administrator must configure system.ntp corporate servers, enabled=1 and use_dhcp=0"
	} else if serviceState == "unknown" {
		status.Diagnostic = "OpenWrt sysntpd service activity unconfirmed; synchronization status unavailable"
	} else if !status.Active {
		status.Diagnostic = "OpenWrt sysntpd service inactive; administrator must start the configured OS time synchronization service"
	} else {
		// procd running state does not prove NTP synchronization. sysntpd has
		// no synchronization-achieved property in this status interface.
		status.Diagnostic = "OpenWrt sysntpd active; synchronization status unavailable"
	}
	return status, nil
}

func (p *timeSyncProvider) Apply(ctx context.Context, settings timesync.Settings) (timesync.Status, error) {
	status, err := p.Status(ctx, settings)
	if err != nil || !settings.Enabled {
		return status, err
	}
	status.Diagnostic = "setup_required: OpenWrt NTP apply unsupported because system.ntp is administrator-owned; configure sysntpd through deployment UCI setup"
	return status, errors.New(status.Diagnostic)
}

func parseTimeSyncConfig(data []byte) ([]string, bool, bool, error) {
	obj, err := decodeObject(data)
	if err != nil {
		return nil, false, false, err
	}
	raw, ok := obj["values"]
	if !ok {
		return nil, false, false, errors.New("OpenWrt NTP configuration lacks UCI values")
	}
	entry, err := decodeObject(raw)
	if err != nil {
		return nil, false, false, err
	}
	kind, err := stringField(entry, ".type")
	if err != nil || kind != "timeserver" {
		return nil, false, false, errors.New("OpenWrt system.ntp is not a timeserver section")
	}
	name, err := stringField(entry, ".name")
	if err != nil || name != "ntp" {
		return nil, false, false, errors.New("OpenWrt NTP configuration is not the system.ntp section")
	}
	servers, err := stringList(entry, "server")
	if err != nil {
		return nil, false, false, err
	}
	// Validation is shared with trusted local config, but OS source data is
	// observation only: it can never become a new desired configuration.
	observed, err := timesync.Normalize(timesync.Settings{Servers: servers})
	if err != nil {
		return nil, false, false, errors.New("invalid OpenWrt NTP server list")
	}
	enabled, err := timeSyncFlag(entry, "enabled", true)
	if err != nil {
		return nil, false, false, err
	}
	useDHCP, err := timeSyncFlag(entry, "use_dhcp", true)
	return observed.Servers, enabled, useDHCP, err
}

func timeSyncFlag(entry object, key string, defaultValue bool) (bool, error) {
	if _, present := entry[key]; !present {
		return defaultValue, nil
	}
	value, err := stringField(entry, key)
	if err != nil {
		return false, err
	}
	switch value {
	case "1", "on", "true", "yes":
		return true, nil
	case "0", "off", "false", "no":
		return false, nil
	default:
		return false, errors.New("malformed OpenWrt NTP boolean option")
	}
}

func equalTimeSyncServers(actual, desired []string) bool {
	actual, desired = slices.Clone(actual), slices.Clone(desired)
	slices.Sort(actual)
	slices.Sort(desired)
	return slices.Equal(actual, desired)
}

func timeSyncReadFailure(status timesync.Status, source string, err error) (timesync.Status, error) {
	category := "unavailable"
	var exitError *exec.ExitError
	// UBUS_STATUS_PERMISSION_DENIED is 6; no human-readable stderr is parsed.
	permissionDenied := errors.Is(err, os.ErrPermission) || errors.As(err, &exitError) && exitError.ExitCode() == 6
	switch {
	case permissionDenied:
		category = "permission denied"
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.Is(err, context.Canceled):
		category = "canceled"
	case errors.Is(err, errOutputLimit):
		category = "response exceeds limit"
	}
	status.Diagnostic = fmt.Sprintf("OpenWrt NTP %s: %s", source, category)
	// Arbitrary fixture/process errors may contain credentials or raw config.
	// Only fixed categories escape into runtime diagnostics and returned errors.
	if permissionDenied {
		return status, fmt.Errorf("%s: %w", status.Diagnostic, os.ErrPermission)
	}
	if errors.Is(err, context.Canceled) {
		return status, fmt.Errorf("%s: %w", status.Diagnostic, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status, fmt.Errorf("%s: %w", status.Diagnostic, context.DeadlineExceeded)
	}
	return status, errors.New(status.Diagnostic)
}
