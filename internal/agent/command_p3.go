package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"InfraPulseAgent/internal/commands"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/sanitize"
)

// initCommandStore is independent of the telemetry queue mode. Commands enabled
// with a RAM outbox still need a durable execution/deduplication boundary.
func (a *Agent) initCommandStore() error {
	if !a.cfg.Commands.Enabled {
		// Disabling future mutations does not discard the delivery obligation
		// for results persisted by an earlier enabled launch.
		if _, err := os.Lstat(filepath.Join(a.cfg.Agent.StateDir, "commands", "safety-state.json")); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return commands.ErrStorageUnavailable
		}
	}
	store, err := commands.OpenStore(filepath.Join(a.cfg.Agent.StateDir, "commands"), a.cfg.CommandStoreOptions())
	a.commandStore = store
	return err
}

func (a *Agent) executePersistentCommand(ctx context.Context, c model.Command) error {
	now := time.Now()
	policy := commands.Policy{Enabled: a.cfg.Commands.Enabled, ProtectedDevices: a.cfg.Commands.ProtectedDevices, ProtectedInterfaces: a.cfg.Commands.ProtectedInterfaces, ProtectedServices: a.cfg.Commands.ProtectedServices}
	reservation, err := a.queue.ReserveResult(4096)
	if err != nil {
		return err
	}
	defer reservation.Release()
	result := model.CommandResult{CommandID: c.ID, StartedAt: now.UTC(), FinishedAt: now.UTC(), Status: "rejected"}
	admitted := false
	historical := false
	// Retained outcomes describe a past execution. They remain queryable when
	// current policy disables new mutations or the original request has aged.
	// Shape validation with its own requested_at preserves all typed checks.
	if commands.Validate(c, c.RequestedAt) == nil && a.commandStore != nil && a.commandStore.Health().Available {
		record, lookupErr := a.commandStore.LookupCommand(c)
		if lookupErr != nil && !errors.Is(lookupErr, commands.ErrStorageUnavailable) {
			result.ErrorCode, result.ErrorMessage, historical = "duplicate_conflict", "duplicate command identity has conflicting parameters", true
		} else if record != nil && record.Envelope != nil && record.Result != nil {
			needsBinding := !slices.Contains(record.CommandIDs, c.ID) || c.IdempotencyKey != "" && !slices.Contains(record.IdempotencyKeys, c.IdempotencyKey)
			if needsBinding {
				bound, duplicate, bindErr := a.commandStore.Begin(c, now)
				if bindErr != nil || !duplicate || bound == nil || bound.Result == nil || bound.Envelope == nil {
					result.ErrorCode, result.ErrorMessage, historical = "storage_unavailable", "historical identity binding could not be persisted; mutation was not started", true
					record = nil
				} else {
					record = bound
				}
			}
			if record != nil {
				if c.ID == record.Command.ID {
					return reservation.Commit(a.bindResultEnvelope(*record.Envelope))
				}
				result, historical = *record.Result, true
				result.CommandID = c.ID
			}
		}
	}
	if historical {
		// The result is delivered below; no new execution reservation exists.
	} else if err = commands.Validate(c, now); err != nil {
		result.ErrorCode, result.ErrorMessage = "invalid_command", err.Error()
	} else if a.storageError != "" || a.queue.State().StorageError != "" {
		result.ErrorCode, result.ErrorMessage = "storage_unavailable", "durable outbox is degraded; mutation was not started"
	} else if !policy.Enabled {
		result.ErrorCode, result.ErrorMessage = "policy_denied", "commands are disabled by policy"
	} else if a.commandStore == nil || !a.commandStore.Health().Available {
		result.ErrorCode, result.ErrorMessage = "storage_unavailable", "durable execution state unavailable; mutation was not started"
	} else {
		record, duplicate, beginErr := a.commandStore.Begin(c, now)
		if beginErr != nil {
			result.ErrorCode, result.ErrorMessage = "command_admission", "durable command admission failed; mutation was not started"
			if !errors.Is(beginErr, commands.ErrStorageUnavailable) && !errors.Is(beginErr, commands.ErrStoreFull) {
				result.ErrorCode, result.ErrorMessage = "duplicate_conflict", "duplicate command identity has conflicting parameters"
			}
		} else if duplicate {
			if record.Envelope != nil && record.Result != nil {
				if c.ID == record.Command.ID {
					e := a.bindResultEnvelope(*record.Envelope)
					return reservation.Commit(e)
				}
				result = *record.Result
				result.CommandID = c.ID
			} else {
				result.ErrorCode, result.ErrorMessage = "command_in_progress", "command has already crossed durable admission; mutation was not restarted"
			}
		} else {
			admitted = true
			devices := append([]model.Device{a.deviceSnapshot()}, a.remote.Devices()...)
			var target *model.Device
			for i := range devices {
				if devices[i].ID == c.TargetDeviceID {
					target = &devices[i]
					break
				}
			}
			if target == nil || c.TargetDeviceID == a.deviceSnapshot().ID {
				result = commands.RejectUnsupported(c, devices, policy, now)
			} else {
				deadline := c.RequestedAt.Add(c.Timeout)
				if limit := now.Add(c.Timeout); limit.Before(deadline) {
					deadline = limit
				}
				commandCtx, cancel := context.WithDeadline(ctx, deadline)
				result, err = a.remote.WithDevice(commandCtx, c.TargetDeviceID, func(d driver.Driver) model.CommandResult {
					return commands.ExecuteDurable(commandCtx, c, *target, d, policy, time.Now(), a.commandStore)
				})
				cancel()
				if err != nil {
					result = model.CommandResult{CommandID: c.ID, StartedAt: now.UTC(), FinishedAt: time.Now().UTC(), Status: "timeout", ErrorCode: "command_timeout", ErrorMessage: "command expired while waiting for device"}
				}
			}
		}
	}
	e, err := a.envelope(model.Envelope{CommandResult: &result})
	if err != nil {
		return err
	}
	e = (sanitize.KnownSecrets{Values: a.secretValues()}).Sanitize(e)
	if admitted {
		if err := a.commandStore.Complete(c, e, time.Now()); err != nil {
			return err
		}
	}
	return reservation.Commit(e)
}

func (a *Agent) bindResultEnvelope(e model.Envelope) model.Envelope {
	// Recovery-created results retain their persisted message ID. Original
	// admission metadata survives ordinary result replay.
	if e.AgentID == "" {
		e.AgentID = a.id.AgentID
	}
	if e.SessionID == "" {
		e.SessionID = a.id.SessionID
	}
	return e
}

func (a *Agent) recoverCommandResults() error {
	if a.commandStore == nil {
		return nil
	}
	for _, e := range a.commandStore.Pending() {
		if a.queue.Contains(e.MessageID) {
			continue
		}
		if err := a.queue.Push(a.bindResultEnvelope(e)); err != nil {
			if errors.Is(err, queue.ErrFull) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (a *Agent) acknowledgeCommandResult(messageID string) error {
	if a.commandStore == nil || !a.commandStore.Health().Available {
		return nil
	}
	return a.commandStore.Acknowledge(messageID, time.Now())
}
