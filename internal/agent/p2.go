package agent

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"

	"InfraPulseAgent/internal/logs"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/sanitize"
	"InfraPulseAgent/internal/scheduler"
	"InfraPulseAgent/internal/telephony"
	"github.com/google/uuid"
)

func (a *Agent) secretValues() []string {
	// These are values of explicitly configured references, never arbitrary env.
	var values []string
	if name := a.cfg.Security.Enrollment.TokenEnv; name != "" {
		if value := os.Getenv(name); value != "" {
			values = append(values, value)
		}
	}
	for _, ref := range a.cfg.Credentials {
		for _, name := range []string{ref.CommunityEnv, ref.PasswordEnv, ref.AuthPasswordEnv, ref.PrivacyPasswordEnv} {
			if name != "" {
				if value := os.Getenv(name); value != "" {
					values = append(values, value)
				}
			}
		}
	}
	return values
}
func (a *Agent) sanitizeText(s string) string { return sanitize.TextWithSecrets(s, a.secretValues()) }

func (a *Agent) p2CapabilitiesLocked() []model.Capability {
	var caps []model.Capability
	if len(a.cfg.Logs.Sources) > 0 {
		caps = append(caps, model.Capability{Name: "logs.read", Supported: runtime.GOOS == "linux", Reason: platformReason()})
		for _, s := range a.cfg.Logs.Sources {
			if s.Format == "auditd" {
				caps = append(caps, model.Capability{Name: "audit.read", Supported: runtime.GOOS == "linux", Reason: platformReason()})
				break
			}
		}
	}
	if len(a.cfg.Telephony.Endpoints) > 0 {
		caps = append(caps, model.Capability{Name: "telephony.options", Supported: true}, model.Capability{Name: "telephony.health", Supported: true})
		for _, ep := range a.cfg.Telephony.Endpoints {
			if ep.Transport == "tls" {
				caps = append(caps, model.Capability{Name: "telephony.options.tls", Supported: true})
				break
			}
		}
	}
	return caps
}
func platformReason() string {
	if runtime.GOOS != "linux" {
		return "Linux log collection; file pipeline fixtures do not establish Windows LocalPlatform support"
	}
	return ""
}

func (a *Agent) p2Jobs() error {
	a.logScheduler = scheduler.New(1, a.collectorError)
	a.sipScheduler = scheduler.New(a.cfg.Telephony.Concurrency, a.collectorError)
	if len(a.cfg.Logs.Sources) > 0 {
		sources := make([]logs.Source, 0, len(a.cfg.Logs.Sources))
		for _, s := range a.cfg.Logs.Sources {
			sources = append(sources, logs.Source{ID: s.ID, Path: s.Path, Format: s.Format})
		}
		var err error
		a.logs, err = logs.New(logs.Options{StateDir: a.cfg.Agent.StateDir, CollectorID: a.id.AgentID, DeviceID: a.deviceSnapshot().ID, Sources: sources, MaxLineBytes: a.cfg.Logs.MaxLineBytes, MaxBatchRecords: a.cfg.Logs.MaxBatchRecords, MaxBatchBytes: a.cfg.Logs.MaxBatchBytes, MaxReadBytes: a.cfg.Logs.MaxReadBytes, Sanitize: a.sanitizeText, Emit: a.push})
		if err != nil {
			return err
		}
		for _, s := range sources {
			if err = a.logScheduler.Add(scheduler.Job{Name: "logs:" + s.ID, Interval: a.cfg.Logs.Interval, Timeout: a.cfg.Logs.Timeout, Collect: func(ctx context.Context) error { return a.logs.Poll(ctx, s.ID) }}); err != nil {
				return err
			}
		}
	}
	for _, ep := range a.cfg.Telephony.Endpoints {
		ep = ep.WithDefaults()
		target := ep.Target()
		ref := model.SourceRef{Type: target.Type, CollectorID: a.id.AgentID, TargetID: target.ID, Protocol: target.Protocol, InstanceID: ep.ID, Provenance: model.ProvenanceReal}
		c, err := telephony.New(ep, ref)
		if err != nil {
			return err
		}
		a.sip = append(a.sip, c)
		if err = a.sipScheduler.Add(scheduler.Job{Name: "telephony:" + ep.ID, Interval: ep.Interval, Timeout: ep.Timeout + 250*time.Millisecond, Collect: func(ctx context.Context) error {
			h, events, err := c.Collect(ctx)
			if err != nil {
				return err
			}
			u := model.TelephonyUpdate{ObservedAt: h.ObservedAt, Source: "sip.options", Targets: []model.TelephonyHealth{h}}
			err = a.emitTarget(target, model.Envelope{Telephony: &u})
			if len(events) > 0 {
				err = errors.Join(err, a.emitTarget(target, model.Envelope{Events: events}))
			}
			return err
		}}); err != nil {
			return err
		}
	}
	// Collector-only hosting still advertises configured, implemented probes.
	if !a.cfg.Local.Enabled {
		a.mu.Lock()
		a.device.Capabilities = append(a.device.Capabilities, a.p2CapabilitiesLocked()...)
		a.mu.Unlock()
	}
	return nil
}

// Caller holds a.mu; records refer only to confirmed running/stopped changes.
func (a *Agent) serviceTransitions(old, next model.Device) {
	previous := make(map[string]string, len(old.Services))
	for _, s := range old.Services {
		previous[s.Name] = s.State
	}
	for _, s := range next.Services {
		state, ok := previous[s.Name]
		if !ok || state == s.State || state == "unknown" || s.State == "unknown" {
			continue
		}
		kind := "service.stopped"
		if s.State == "running" {
			kind = "service.started"
		}
		id, err := uuid.NewRandom()
		if err != nil {
			continue
		}
		_ = a.emitTarget(a.localTarget, model.Envelope{Events: []model.Event{{ID: id.String(), DeviceID: next.ID, Type: kind, Timestamp: s.ObservedAt, Severity: "info", Source: s.Source, Message: "service " + s.Name + " is " + s.State}}})
	}
}
