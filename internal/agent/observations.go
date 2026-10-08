package agent

import (
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/logs"
	"InfraPulseAgent/internal/model"
)

func (a *Agent) localSource() model.SourceRef {
	return model.SourceRef{Type: model.SourceLocal, CollectorID: a.id.AgentID, TargetID: a.localTarget.ID, Protocol: a.localTarget.Protocol, Provenance: model.ProvenanceReal}
}

// emitTarget is the common observation boundary. Adapters never send gRPC;
// delivery still owns sanitization, immutable admission, message IDs and replay.
func (a *Agent) emitTarget(target model.Target, e model.Envelope) error {
	ref := model.SourceRef{Type: target.Type, CollectorID: a.id.AgentID, TargetID: target.ID, Protocol: target.Protocol, Provenance: model.ProvenanceReal}
	return a.push(model.ObserveEnvelope(e, ref, time.Now().UTC()))
}

// configuredTargets projects existing YAML sections into one domain view. The
// projection contains no credentials and creates no jobs or additional workers.
func (a *Agent) configuredTargets() []model.Target {
	result := make([]model.Target, 0, 1+len(a.cfg.Devices)+len(a.cfg.Telephony.Endpoints)+len(a.cfg.Logs.Sources))
	result = append(result, a.localTarget)
	defaults := config.Polling{FastInterval: a.cfg.Telemetry.FastInterval, MediumInterval: a.cfg.Telemetry.MediumInterval, InventoryInterval: a.cfg.Telemetry.InventoryInterval, TopologyInterval: a.cfg.Telemetry.TopologyInterval, Timeout: a.cfg.Telemetry.CollectTimeout}
	for _, d := range a.cfg.Devices {
		result = append(result, d.Target(defaults))
	}
	for _, ep := range a.cfg.Telephony.Endpoints {
		result = append(result, ep.Target())
	}
	for _, s := range a.cfg.Logs.Sources {
		result = append(result, (logs.Source{ID: s.ID, Path: s.Path, Format: s.Format}).Target(a.cfg.Logs.Interval, a.cfg.Logs.Timeout))
	}
	for _, s := range a.cfg.SyslogReceiver.Sources {
		result = append(result, s.Target(a.cfg.SyslogReceiver.Enabled))
	}
	return result
}
