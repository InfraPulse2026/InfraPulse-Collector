package logs

import (
	"time"

	"InfraPulseAgent/internal/model"
)

// A file source is a served Target. It is not a remote Syslog receiver and
// its ID is independent of the host Device ID carried by each record.
func (s Source) Target(interval, timeout time.Duration) model.Target {
	kind := model.SourceLocal
	if s.Format == "syslog" {
		kind = model.SourceSyslog
	} else if s.Format == "auditd" {
		kind = model.SourceAuditd
	}
	return model.Target{ID: "logs:" + s.ID, Type: kind, Protocol: "file", Endpoint: s.Path, Enabled: true, Polling: model.TargetPolling{FastInterval: interval, Timeout: timeout}}
}

func (p *Pipeline) sourceRef(s *sourceState) model.SourceRef {
	target := s.source.Target(0, 0)
	return model.SourceRef{Type: target.Type, CollectorID: p.options.CollectorID, TargetID: target.ID, Protocol: target.Protocol, InstanceID: s.source.ID, Provenance: model.ProvenanceReal}
}
