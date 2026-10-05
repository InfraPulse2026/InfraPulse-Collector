// Package commands enforces typed command validation and default-deny policy.
// P0 has no mutation executor; a valid permitted command is still unsupported.
package commands

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"time"
	"unicode"

	"InfraPulseAgent/internal/model"
)

var ErrPolicy = errors.New("commands are disabled by policy")
var ErrProtected = errors.New("target resource is protected")

func identifier(s string) bool {
	return s != "" && len(s) <= 128 && strings.IndexFunc(s, unicode.IsControl) < 0
}

func Validate(c model.Command, now time.Time) error {
	if !identifier(c.ID) || !identifier(c.TargetDeviceID) {
		return errors.New("invalid command or target identifier")
	}
	if c.Timeout < 10*time.Millisecond || c.Timeout > time.Minute {
		return errors.New("command timeout must be between 10ms and 1m")
	}
	if c.RequestedAt.IsZero() || c.RequestedAt.After(now.Add(time.Minute)) || c.RequestedAt.Before(now.Add(-5*time.Minute)) {
		return errors.New("command requested_at outside allowed time window")
	}
	if c.Initiator != "human" && c.Initiator != "ai_suggested" && c.Initiator != "automation" {
		return errors.New("invalid command initiator")
	}
	if len(c.IdempotencyKey) > 128 {
		return errors.New("invalid idempotency key")
	}
	actions := 0
	for _, present := range []bool{c.Interface != nil, c.Service != nil, c.Rollback != nil} {
		if present {
			actions++
		}
	}
	if actions != 1 {
		return errors.New("command requires exactly one typed action")
	}
	if c.Interface != nil {
		if !identifier(c.Interface.InterfaceID) || (c.Interface.Operation != "enable" && c.Interface.Operation != "disable") {
			return errors.New("invalid interface action")
		}
	}
	if c.Service != nil {
		if !identifier(c.Service.Service) || (c.Service.Operation != "start" && c.Service.Operation != "stop" && c.Service.Operation != "restart") {
			return errors.New("invalid service action")
		}
	}
	if c.Rollback != nil && !identifier(c.Rollback.SnapshotID) {
		return errors.New("invalid rollback snapshot identifier")
	}
	return nil
}

func RequiredCapability(c model.Command) string {
	if c.Rollback != nil {
		return "resource.rollback"
	}
	if c.Interface != nil {
		return "interfaces." + c.Interface.Operation
	}
	if c.Service != nil {
		return "services." + c.Service.Operation
	}
	return ""
}
func Supported(capabilities []model.Capability, name string) bool {
	for _, c := range capabilities {
		if c.Name == name && c.Supported {
			return true
		}
	}
	return false
}

type Policy struct {
	Enabled                                                  bool
	ProtectedDevices, ProtectedInterfaces, ProtectedServices []string
}

// aliases must include current driver-resolved IDs/names before a future Apply.
// P0 only rejects commands and never acts on unresolved identifiers.
func (p Policy) Check(c model.Command, aliases ...string) error {
	if !p.Enabled {
		return ErrPolicy
	}
	if contains(p.ProtectedDevices, c.TargetDeviceID) {
		return ErrProtected
	}
	if c.Interface != nil || c.Rollback != nil {
		if c.Interface != nil && containsInterface(p.ProtectedInterfaces, c.Interface.InterfaceID) {
			return ErrProtected
		}
		for _, a := range aliases {
			if containsInterface(p.ProtectedInterfaces, a) {
				return ErrProtected
			}
		}
	}
	if c.Service != nil && contains(p.ProtectedServices, c.Service.Service) {
		return ErrProtected
	}
	return nil
}
func contains(values []string, v string) bool {
	for _, s := range values {
		if s == v {
			return true
		}
	}
	return false
}
func containsInterface(values []string, v string) bool {
	if contains(values, v) {
		return true
	}
	mac, err := net.ParseMAC(v)
	if err != nil {
		return false
	}
	for _, candidate := range values {
		other, e := net.ParseMAC(candidate)
		if e == nil && bytes.Equal(mac, other) {
			return true
		}
	}
	return false
}

func RejectUnsupported(c model.Command, devices []model.Device, policy Policy, now time.Time) model.CommandResult {
	r := model.CommandResult{CommandID: c.ID, StartedAt: now.UTC(), FinishedAt: now.UTC(), Status: "unsupported", ErrorCode: "unsupported", ErrorMessage: "no mutation executor is implemented in P0"}
	if err := Validate(c, now); err != nil {
		r.Status = "rejected"
		r.ErrorCode = "invalid_command"
		r.ErrorMessage = err.Error()
		return r
	}
	var target *model.Device
	for i := range devices {
		if devices[i].ID == c.TargetDeviceID {
			target = &devices[i]
			break
		}
	}
	if target == nil {
		r.Status = "rejected"
		r.ErrorCode = "unknown_target"
		r.ErrorMessage = "target device is not managed by this agent"
		return r
	}
	if err := policy.Check(c); err != nil {
		r.Status = "rejected"
		r.ErrorCode = "policy_denied"
		if errors.Is(err, ErrProtected) {
			r.ErrorCode = "protected_resource"
		}
		r.ErrorMessage = err.Error()
		return r
	}
	if !Supported(target.Capabilities, RequiredCapability(c)) {
		r.ErrorMessage = "target capability is unsupported"
	}
	return r
}
