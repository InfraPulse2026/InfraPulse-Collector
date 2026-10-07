package platform

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"InfraPulseAgent/internal/platform/linux"
	"InfraPulseAgent/internal/platform/openwrt"
	"InfraPulseAgent/internal/platform/timesync"
)

// NewTimeSync is independent of local collection, which can be disabled on an
// edge Collector. Setup is deliberately separate from the read-only Platform.
func NewTimeSync(mode string) (timesync.Provider, error) {
	return newTimeSync(mode, runtime.GOOS, "/etc")
}

func newTimeSync(mode, goos, etcRoot string) (timesync.Provider, error) {
	if mode != "" && mode != "auto" && mode != "linux" && mode != "openwrt" {
		return nil, fmt.Errorf("unknown local platform mode: %s", mode)
	}
	if goos != "linux" {
		return nil, fmt.Errorf("%w: time synchronization setup on %s", ErrUnsupported, goos)
	}
	// OpenWrt evidence always wins for OS setup, even if collection uses the
	// generic Linux reader: sysntpd must not be treated as a systemd host.
	if openwrt.Detect(etcRoot) {
		return openwrt.NewTimeSync(), nil
	}
	if mode == "openwrt" {
		return nil, fmt.Errorf("%w: OpenWrt distribution evidence unavailable", ErrUnsupported)
	}
	return linux.NewTimeSync(), nil
}

// TimeSyncStatus performs one bounded read, without waiting for NTP convergence.
// A diagnostic failure must not stop the Collector's monitoring lifecycle.
func TimeSyncStatus(ctx context.Context, mode string, settings timesync.Settings) timesync.Status {
	if !settings.Enabled {
		return timesync.Disabled()
	}
	s := timesync.Status{Enabled: true, DesiredServers: append([]string(nil), settings.Servers...), CheckedAt: time.Now().UTC()}
	provider, err := NewTimeSync(mode)
	if err != nil {
		s.Diagnostic = err.Error()
		return s
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err = provider.Status(checkCtx, settings)
	if err != nil && s.Diagnostic == "" {
		s.Diagnostic = err.Error()
	}
	return s
}
