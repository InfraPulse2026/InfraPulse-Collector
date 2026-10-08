package agent

import (
	"context"
	"errors"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/syslogreceiver"
)

func (a *Agent) initSyslog() error {
	if !a.cfg.SyslogReceiver.Enabled {
		return nil
	}
	// Existing monitoring can degrade to RAM after storage failure, but network
	// syslog must never silently treat that fallback as durable admission.
	if a.storageError != "" || !a.queue.IsDurable() {
		return errors.New("syslog_receiver requires available durable outbox")
	}
	r, err := syslogreceiver.New(a.cfg.SyslogReceiver, syslogreceiver.Options{
		CollectorID: a.id.AgentID, MaxBatchBytes: a.cfg.Logs.MaxBatchBytes,
		Sanitize: a.sanitizeText,
		Emit: func(ctx context.Context, envelope model.Envelope) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return a.push(envelope)
		},
	})
	if err != nil {
		return err
	}
	a.syslog = r
	return nil
}
