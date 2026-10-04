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
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, runtime.GOOS)
	}
	return linux.New("/proc", "/sys"), nil
}
