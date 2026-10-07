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

// Adapter owns a source's lifecycle and only advertises implemented capabilities.
// Optional readers below let a new protocol expose the observations it supports.
type Adapter interface {
	Capabilities() []model.Capability
	Close() error
}
type ProbeReader interface {
	Probe(context.Context) (model.Device, error)
}
type TelemetryReader interface {
	Collect(context.Context) (model.TelemetrySample, error)
}
type InventoryReader interface {
	Inventory(context.Context) (model.Device, error)
}
type TopologyReader interface {
	Topology(context.Context) (model.TopologyUpdate, error)
}

// Driver preserves the existing SNMP/RouterOS contract. Collector orchestration
// accepts Adapter and invokes only the small readers an adapter implements.
type Driver interface {
	Adapter
	ProbeReader
	TelemetryReader
	InventoryReader
	TopologyReader
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

// SnapshotIdentityReader refreshes compatibility metadata for a concrete
// snapshot-capable driver immediately before mutation or rollback.
type SnapshotIdentityReader interface {
	SnapshotIdentity(context.Context) (model.Device, error)
}
