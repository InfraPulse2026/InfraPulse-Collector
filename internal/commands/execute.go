package commands

import (
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"context"
	"errors"
	"time"
)

// Execute is called under the registry device exclusion with result admission
// already reserved by Agent. Every initiator follows the same checks.
func Execute(ctx context.Context, c model.Command, target model.Device, d driver.Driver, policy Policy, now time.Time) model.CommandResult {
	r := model.CommandResult{CommandID: c.ID, StartedAt: now.UTC(), Status: "rejected"}
	finish := func(status, code, message string) model.CommandResult {
		r.Status = status
		r.ErrorCode = code
		r.ErrorMessage = message
		r.FinishedAt = time.Now().UTC()
		return r
	}
	if err := Validate(c, now); err != nil {
		return finish("rejected", "invalid_command", err.Error())
	}
	if target.ID != c.TargetDeviceID {
		return finish("rejected", "unknown_target", "target device is not managed by this agent")
	}
	deadline := c.RequestedAt.Add(c.Timeout)
	if limit := now.Add(c.Timeout); limit.Before(deadline) {
		deadline = limit
	}
	if !deadline.After(now) {
		return finish("timeout", "command_expired", "command deadline expired before execution")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if !Supported(d.Capabilities(), RequiredCapability(c)) {
		return finish("unsupported", "capability_unsupported", "target capability is unsupported")
	}
	if err := policy.Check(c); err != nil {
		return finish("rejected", policyErrorCode(err), err.Error())
	}
	manager, ok := d.(driver.InterfaceManager)
	if !ok || c.Interface == nil {
		return finish("unsupported", "executor_unsupported", "typed operation has no executor")
	}
	resolved, err := manager.ResolveInterface(ctx, c.Interface.InterfaceID)
	if err != nil {
		return executionError(finish, err, "interface resolution failed")
	}
	if err = policy.Check(c, resolved.Aliases...); err != nil {
		return finish("rejected", policyErrorCode(err), err.Error())
	}
	if err = ctx.Err(); err != nil {
		return finish("timeout", "command_timeout", "command expired before mutation")
	}
	// Capabilities may have changed while resolution read the current records.
	if !Supported(d.Capabilities(), RequiredCapability(c)) {
		return finish("unsupported", "capability_unsupported", "target capability changed before mutation")
	}
	verification, err := manager.SetInterface(ctx, resolved, c.Interface.Operation == "enable")
	r.Verification = &verification
	r.Changed = verification.Changed
	r.Uncertain = verification.Uncertain || errors.Is(err, driver.ErrUncertain)
	if r.Uncertain {
		return finish("timeout", "mutation_uncertain", "mutation outcome could not be verified")
	}
	if err != nil {
		return executionError(finish, err, "interface mutation or verification failed")
	}
	if !verification.Verified {
		return finish("failed", "verification_failed", "read-after-write did not confirm the requested state")
	}
	return finish("success", "", "")
}
func policyErrorCode(err error) string {
	if errors.Is(err, ErrProtected) {
		return "protected_resource"
	}
	return "policy_denied"
}
func executionError(finish func(string, string, string) model.CommandResult, err error, message string) model.CommandResult {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return finish("timeout", "command_timeout", message)
	}
	if errors.Is(err, driver.ErrUnavailable) {
		return finish("unsupported", "operation_unavailable", message)
	}
	return finish("failed", "operation_failed", message)
}
