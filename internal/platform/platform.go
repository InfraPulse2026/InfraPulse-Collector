// Package platform describes the OS hosting the agent, independently of remote
// network devices and their protocol drivers.
package platform

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/linux"
	"InfraPulseAgent/internal/platform/openwrt"
)

var ErrUnsupported = errors.New("local platform unsupported")

type Platform interface {
	Identity(context.Context) (model.Device, error)
	Collect(context.Context) (model.TelemetrySample, error)
	Capabilities() []model.Capability
}

// New selects a real local implementation. Unsupported hosts may still run the
// agent in collector-only mode; no synthetic local measurements are returned.
func New() (Platform, error) {
	return NewMode("auto")
}

// NewMode selects auto/linux/openwrt without introducing another Agent. Explicit
// OpenWrt mode still requires distribution evidence; optional ubus sources may
// be unavailable while Linux measurements remain usable.
func NewMode(mode string) (Platform, error) {
	return newMode(mode, runtime.GOOS, "/proc", "/sys", "/etc")
}

func newMode(mode, goos, procRoot, sysRoot, etcRoot string) (Platform, error) {
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "linux" && mode != "openwrt" {
		return nil, fmt.Errorf("unknown local platform mode: %s", mode)
	}
	if goos != "linux" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, goos)
	}
	if mode != "linux" && openwrt.Detect(etcRoot) {
		return openwrt.NewWithOptions(openwrt.Options{ProcRoot: procRoot, SysRoot: sysRoot, EtcRoot: etcRoot}), nil
	}
	if mode == "openwrt" {
		return nil, fmt.Errorf("%w: OpenWrt distribution evidence unavailable", ErrUnsupported)
	}
	return linux.New(procRoot, sysRoot), nil
}
