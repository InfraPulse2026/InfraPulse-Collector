package sanitize

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestSyslogSanitizesSenderPayloadAndOwnsMutableMetadata(t *testing.T) {
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	source := at.Add(-time.Hour)
	zero := time.Duration(0)
	o := model.NewObservation(model.SourceRef{Type: model.SourceSyslog, TargetID: "server-a", Protocol: "tls", Provenance: model.ProvenanceReal}, "syslog", at, at, &source)
	o.Time.ClockOffset, o.Time.Uncertainty = &zero, &zero
	payload := append([]byte{0xff}, []byte(" password=value configured-value")...)
	input := model.Envelope{Logs: &model.LogBatch{Observation: o, Records: []model.LogRecord{{
		Observation: o, OriginalTimestamp: &source, Severity: ptr(uint32(1)), Facility: ptr(uint32(2)),
		Syslog: &model.SyslogMetadata{MessageID: "configured-value", StructuredData: `[id token="value"]`, RawTimestamp: "configured-value",
			RawPayload: payload, PeerIP: "192.0.2.1", Transport: "tls", TLSIdentity: "dns:server-a", Authenticated: true, Managed: true},
		Audit: &model.AuditRecord{UID: ptr(uint32(3)), AUID: ptr(uint32(4)), PID: ptr(uint32(5)), Success: ptr(true)},
	}}}}
	result := (KnownSecrets{Values: []string{"configured-value"}}).Sanitize(input)
	r := &result.Logs.Records[0]
	if strings.Contains(r.Syslog.MessageID+r.Syslog.StructuredData+r.Syslog.RawTimestamp+string(r.Syslog.RawPayload), "configured-value") ||
		strings.Contains(r.Syslog.StructuredData+string(r.Syslog.RawPayload), "value") {
		t.Fatal("syslog payload secret retained")
	}
	if r.Syslog.PeerIP != "192.0.2.1" || r.Syslog.Transport != "tls" || r.Syslog.TLSIdentity != "dns:server-a" || !r.Syslog.Authenticated || !r.Syslog.Managed {
		t.Fatal("sanitizer rewrote transport identity evidence")
	}
	if len(r.Syslog.RawPayload) == 0 || r.Syslog.RawPayload[0] != 0xff {
		t.Fatal("non-UTF8 sanitized octets lost")
	}
	r.Syslog.RawPayload[0] = 0xfe
	r.Syslog.TLSIdentity = "other"
	r.Observation.Source.TargetID = "other"
	*r.Observation.Time.SourceTimestamp = at
	*r.Observation.Time.ClockOffset = time.Second
	*r.Observation.Time.Uncertainty = time.Second
	*result.Logs.Observation.Time.ClockOffset = time.Hour
	*r.OriginalTimestamp = at
	*r.Severity, *r.Facility = 7, 23
	*r.Audit.UID, *r.Audit.AUID, *r.Audit.PID, *r.Audit.Success = 0, 0, 0, false
	original := &input.Logs.Records[0]
	if !bytes.Equal(original.Syslog.RawPayload, payload) || payload[0] != 0xff || original.Syslog.MessageID != "configured-value" || original.Syslog.TLSIdentity != "dns:server-a" ||
		original.Observation.Source.TargetID != "server-a" || *o.Time.ClockOffset != 0 || *o.Time.Uncertainty != 0 || !o.Time.SourceTimestamp.Equal(source) ||
		!original.OriginalTimestamp.Equal(source) || *original.Severity != 1 || *original.Facility != 2 || *original.Audit.UID != 3 || *original.Audit.AUID != 4 || *original.Audit.PID != 5 || !*original.Audit.Success {
		t.Fatal("sanitizer result aliases source metadata")
	}
}

func TestSyslogRawOctetsBecomeAbsentWhenSanitizedTextIsUTF8(t *testing.T) {
	input := model.Envelope{Logs: &model.LogBatch{Records: []model.LogRecord{{Syslog: &model.SyslogMetadata{RawPayload: []byte{0xff}}}}}}
	result := (KnownSecrets{Values: []string{string([]byte{0xff})}}).Sanitize(input)
	if result.Logs.Records[0].Syslog.RawPayload != nil || input.Logs.Records[0].Syslog.RawPayload[0] != 0xff {
		t.Fatal("UTF8 bytes retained or source mutated")
	}
}

func ptr[T any](value T) *T { return &value }
