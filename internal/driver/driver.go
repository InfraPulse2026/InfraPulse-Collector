// Package driver defines small transport-independent remote device contracts.
package driver

import (
	"InfraPulseAgent/internal/model"
	"context"
	"errors"
)

var (
	ErrConnectivity = errors.New("remote connectivity failure")
	ErrUnavailable  = errors.New("remote operation unavailable")
	ErrMalformed    = errors.New("malformed remote response")
	ErrLimit        = errors.New("remote response exceeds limit")
	ErrUncertain    = errors.New("mutation outcome uncertain")
)

type Driver interface {
	Probe(context.Context) (model.Device, error)
	Collect(context.Context) (model.TelemetrySample, error)
	Inventory(context.Context) (model.Device, error)
	Topology(context.Context) (model.TopologyUpdate, error)
	Capabilities() []model.Capability
	Close() error
}
type Options struct {
	ID, Name, Address, VendorHint string
	Location                      model.PhysicalLocation
	MaxRows, MaxResponseBytes     int
	TopologyTTLSeconds            uint32
	EnableMutations               bool
}
type ResolvedInterface struct {
	Interface model.Interface
	Aliases   []string
}
type InterfaceManager interface {
	ResolveInterface(context.Context, string) (ResolvedInterface, error)
	SetInterface(context.Context, ResolvedInterface, bool) (model.InterfaceVerification, error)
}
