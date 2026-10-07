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
func Execute(ctx context.Context, c model.Command, target model.Device, d driver.Adapter, policy Policy, now time.Time) model.CommandResult {
	return execute(ctx, c, target, d, policy, now, nil)
}

// ExecuteDurable uses the same validation/protection/verification path, with a
// synced journal and typed snapshot immediately before an external write.
func ExecuteDurable(ctx context.Context, c model.Command, target model.Device, d driver.Adapter, policy Policy, now time.Time, store *Store) model.CommandResult {
	if store == nil || !store.Health().Available {
		return model.CommandResult{CommandID: c.ID, StartedAt: now.UTC(), FinishedAt: now.UTC(), Status: "rejected", ErrorCode: "storage_unavailable", ErrorMessage: "durable command state unavailable; mutation was not started"}
	}
	return execute(ctx, c, target, d, policy, now, store)
}

func execute(ctx context.Context, c model.Command, target model.Device, d driver.Adapter, policy Policy, now time.Time, store *Store) model.CommandResult {
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
	if !ok || (c.Interface == nil && c.Rollback == nil) {
		return finish("unsupported", "executor_unsupported", "typed operation has no executor")
	}
	if store != nil {
		if reader, supported := d.(driver.SnapshotIdentityReader); supported {
			current, err := reader.SnapshotIdentity(ctx)
			if err != nil {
				return executionError(finish, err, "snapshot compatibility could not be observed")
			}
			if current.ID != c.TargetDeviceID {
				return finish("rejected", "snapshot_conflict", "snapshot compatibility identity differs from target")
			}
			target = current
		}
	}
	var snapshot *model.ConfigurationSnapshot
	var reference string
	expected := false
	if c.Interface != nil {
		reference, expected = c.Interface.InterfaceID, c.Interface.Operation == "enable"
	} else {
		if store == nil {
			return finish("unsupported", "executor_unsupported", "rollback requires persistent snapshots")
		}
		value, err := store.Snapshot(c.Rollback.SnapshotID, time.Now())
		if err != nil {
			return snapshotError(finish, err)
		}
		if value.DeviceID != c.TargetDeviceID {
			return finish("rejected", "snapshot_ownership", "snapshot belongs to a different target")
		}
		snapshot, reference, expected = &value, value.CanonicalID, value.PreviousAdminUp
	}
	resolved, err := manager.ResolveInterface(ctx, reference)
	if err != nil {
		return executionError(finish, err, "interface resolution failed")
	}
	if err = policy.Check(c, resolved.Aliases...); err != nil {
		return finish("rejected", policyErrorCode(err), err.Error())
	}
	if snapshot != nil {
		current, e := store.Snapshot(snapshot.ID, time.Now())
		if e != nil {
			return snapshotError(finish, e)
		}
		if !snapshotMatches(current, target, resolved) {
			return finish("rejected", "snapshot_conflict", "snapshot identity, version or expected state differs from current resource")
		}
	}
	if err = ctx.Err(); err != nil {
		return finish("timeout", "command_timeout", "command expired before mutation")
	}
	// Capabilities may have changed while resolution read the current records.
	if !Supported(d.Capabilities(), RequiredCapability(c)) {
		return finish("unsupported", "capability_unsupported", "target capability changed before mutation")
	}
	if store != nil {
		id, e := store.Executing(c, target, resolved, expected, time.Now())
		if e != nil {
			return snapshotError(finish, e)
		}
		r.SnapshotID = id
	}
	verification, err := manager.SetInterface(ctx, resolved, expected)
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

func snapshotError(finish func(string, string, string) model.CommandResult, err error) model.CommandResult {
	if errors.Is(err, ErrSnapshotExpired) {
		return finish("rejected", "snapshot_expired", "snapshot expired or unavailable")
	}
	if errors.Is(err, ErrSnapshotConflict) {
		return finish("rejected", "snapshot_conflict", "snapshot is stale, incompatible or not verified")
	}
	return finish("rejected", "storage_unavailable", "durable command or snapshot storage unavailable; mutation was not started")
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
