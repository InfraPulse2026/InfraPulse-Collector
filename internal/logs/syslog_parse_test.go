package logs

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestRFC5424MetadataAndOpaqueMessage(t *testing.T) {
	observed := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, timestamp, sd, msg string
	}{
		{"timezone", "2003-08-24T05:14:15.000003-07:00", "-", "%% sample"},
		{"nil timestamp", "-", "-", ""},
		{"multiple SD", "2026-10-08T10:00:00Z", `[example@32473 key="a b"][second@32473 n="2"]`, "hello"},
		{"escaped SD", "2026-10-08T10:00:00Z", `[example@32473 key="a\"b\\c\]d"]`, "message"},
		{"unknown escape kept", "2026-10-08T10:00:00Z", `[example@32473 key="a\qb"]`, "message"},
		{"repeated param", "2026-10-08T10:00:00Z", `[example@32473 key="one" key="two"]`, "message"},
		{"UTF8 SD", "2026-10-08T10:00:00Z", `[example@32473 key="привет"]`, "message"},
		{"opaque MSG", "2026-10-08T10:00:00Z", "-", "\ufeff leading\nnewline\x00  "},
		{"MSG looks like SD", "2026-10-08T10:00:00Z", `[example@32473]`, `[second@32473 key="part of MSG"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := "<165>1 " + tc.timestamp + " claimed-host app 42 ID47 " + tc.sd
			if tc.msg != "" {
				line += " " + tc.msg
			}
			r := Parse(line, "syslog", observed)
			if r.Syslog == nil || r.Syslog.Format != "rfc5424" || r.Syslog.ParseStatus != "parsed" || r.Syslog.Version != 1 || r.Syslog.MessageID != "ID47" || r.Syslog.StructuredData != tc.sd || r.Syslog.RawTimestamp != tc.timestamp {
				t.Fatalf("metadata missing or altered: %+v", r.Syslog)
			}
			if r.Raw != line || r.Message != tc.msg || r.Host != "claimed-host" || r.Process != "app" || r.ProcessID != "42" || !r.ObservedAt.Equal(observed) || r.Audit != nil {
				t.Fatalf("raw/MSG/time changed or audit invented: %+v", r)
			}
			if tc.timestamp == "-" {
				if r.OriginalTimestamp != nil {
					t.Fatal("NILVALUE timestamp replaced")
				}
			} else {
				expected, err := time.Parse(time.RFC3339Nano, tc.timestamp)
				if err != nil || r.OriginalTimestamp == nil || !r.OriginalTimestamp.Equal(expected) || r.OriginalTimestamp.Location() != time.UTC {
					t.Fatal("absolute source timestamp changed", r.OriginalTimestamp)
				}
			}
			if r.Syslog.Authenticated || r.Syslog.Managed || r.Syslog.TLSIdentity != "" || r.Observation != nil {
				t.Fatal("parser invented transport identity or observation metadata")
			}
		})
	}
}

func TestRFC5424NILVALUEAndEmptyMessage(t *testing.T) {
	for _, line := range []string{"<0>1 - - - - - -", "<0>1 - - - - - - ", "<0>1 - - - - - [example@32473]"} {
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Raw != line || r.Message != "" || r.Host != "" || r.Process != "" || r.ProcessID != "" || r.OriginalTimestamp != nil || r.Syslog.MessageID != "" || r.Syslog.RawTimestamp != "-" || r.Syslog.ParseStatus != "parsed" || r.Severity == nil || *r.Severity != 0 || r.Facility == nil || *r.Facility != 0 {
			t.Fatalf("incorrect NILVALUE: %+v", r)
		}
	}
}

func TestSyslogInvalidPriorityNeverNormalized(t *testing.T) {
	for _, pri := range []string{"<192>", "<999>", "<-1>", "<+1>", "<00>", "<01>", "<034>", "<>", "<1a>", "<1", ""} {
		line := pri + "1 2026-10-08T10:00:00Z host app 42 ID - hello"
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Raw != line || r.Message != line || r.Host != "" || r.Process != "" || r.OriginalTimestamp != nil || r.Facility != nil || r.Severity != nil || r.Syslog.ParseStatus != "fallback" {
			t.Fatalf("invalid PRI normalized %q: %+v", pri, r)
		}
	}
}

func TestRFC5424MalformedTimestampRawFallback(t *testing.T) {
	for _, ts := range []string{"", "2026-10-08", "2026-10-08T10:00:00", "2026-10-08t10:00:00Z", "2026-10-08T10:00:00z", "2026-10-08T10:00:60Z", "2026-10-08T10:00:00.1234567Z", "2026-10-08T10:00:00,123Z", "2026-10-08T10:00:00+24:00", "2026-10-08T10:00:00+00:60", "0000-01-01T00:00:00Z", "2026-02-30T10:00:00Z"} {
		line := "<14>1 " + ts + " host app 42 ID - hello"
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Raw != line || r.Message != line || r.Host != "" || r.Process != "" || r.OriginalTimestamp != nil || r.Syslog.ParseStatus != "fallback" || r.Syslog.RawTimestamp != "" {
			t.Fatalf("invalid timestamp normalized %q: %+v", ts, r)
		}
	}
}

func TestRFC5424MalformedStructuredDataRawFallback(t *testing.T) {
	for _, sd := range []string{"", "[", "[]", "[ x]", "[x ]", "[x key=plain]", `[x key="unterminated]`, `[x key="unescaped ]"]`, `[x key="v"extra]`, `[x][x]`, "[" + strings.Repeat("x", 33) + "]", `[x ` + strings.Repeat("p", 33) + `="v"]`, "[x key=\"\xff\"]", "-without-space", "[x]without-space"} {
		line := "<14>1 2026-10-08T10:00:00Z host app 42 ID " + sd
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Raw != line || r.Message != line || r.Host != "" || r.OriginalTimestamp != nil || r.Syslog.ParseStatus != "fallback" || r.Syslog.StructuredData != "" {
			t.Fatalf("malformed SD normalized %q: %+v", sd, r)
		}
	}
}

func TestRFC5424UnsupportedVersionPreserved(t *testing.T) {
	for _, version := range []string{"0", "2", "01", "1000"} {
		line := "<14>" + version + " 2026-10-08T10:00:00Z host app 42 ID - hello"
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Raw != line || r.Message != line || r.Syslog.ParseStatus != "fallback" || r.Syslog.Version != 0 || r.Host != "" || r.OriginalTimestamp != nil {
			t.Fatalf("unsupported version interpreted %q: %+v", version, r)
		}
	}
}

func TestRFC3164NoYearOrTimezoneInvented(t *testing.T) {
	observed := time.Date(2026, 10, 8, 10, 0, 0, 0, time.FixedZone("collector", 3*3600))
	for _, ts := range []string{"Oct 11 22:14:15", "Jan  7 00:00:01", "Jan 07 00:00:01", "Feb 29 12:00:00"} {
		line := "<34>" + ts + " claimed-host audispd[42]: message"
		r := Parse(line, "syslog", observed)
		if r.Raw != line || r.Syslog.RawTimestamp != ts || r.Syslog.Format != "rfc3164" || r.Syslog.ParseStatus != "parsed" || r.OriginalTimestamp != nil || !r.ObservedAt.Equal(observed) || r.Host != "claimed-host" || r.Process != "audispd" || r.ProcessID != "42" || r.Message != "message" {
			t.Fatalf("legacy timestamp confidence fabricated: %+v", r)
		}
	}
	for _, ts := range []string{"Oct 40 22:14:15", "Oct 11 24:00:00", "Oct 11 22:60:15", "Oct 11 22:14:60", "Feb 30 12:00:00"} {
		line := "<34>" + ts + " host app: message"
		r := Parse(line, "syslog", observed)
		if r.Raw != line || r.Message != line || r.Syslog.RawTimestamp != "" || r.Syslog.ParseStatus != "fallback" || r.OriginalTimestamp != nil || r.Host != "" {
			t.Fatalf("malformed legacy timestamp interpreted: %+v", r)
		}
	}
}

func TestAttachAuditPreservesEnclosingSyslog(t *testing.T) {
	for _, recordType := range []string{"SYSCALL", "EXECVE", "PATH", "USER_AUTH", "USER_LOGIN", "EOE", "UNKNOWN_FUTURE_TYPE"} {
		line := "<14>1 2026-10-08T10:00:00Z claimed-host audispd 42 AUDIT - \ufeffnode=audit-node type=" + recordType + " msg=audit(1700000000.123:42): uid=0 pid=25"
		r := Parse(line, "syslog", time.Now().UTC())
		r.SourceID = "managed-sender"
		r.Observation = model.NewObservation(model.SourceRef{Type: model.SourceSyslog, TargetID: "configured-target", Provenance: model.ProvenanceReal}, "syslog-tls", r.ObservedAt, r.ObservedAt, r.OriginalTimestamp)
		before := r
		if r.Audit != nil {
			t.Fatal("syslog parser implicitly detected audit")
		}
		if !AttachAudit(&r) || r.Audit == nil || r.Audit.RecordType != recordType || r.Audit.EventID != "1700000000.123:42" || r.Audit.Host != "audit-node" {
			t.Fatalf("audit grammar not attached for %s: %+v", recordType, r.Audit)
		}
		r.Audit = nil
		if !reflect.DeepEqual(r, before) {
			t.Fatal("audit wrapper changed enclosing syslog identity, message or time")
		}
	}
}

func TestAttachAuditMalformedFallback(t *testing.T) {
	for _, msg := range []string{"type=SYSCALL message=no-audit-header", "ordinary type=arbitrary", "ordinary type=SYSCALL msg=audit(1700000000:42): uid=0", `message="some type=SYSCALL msg=audit(1700000000:42): uid=0"`, "type=SYSCALL msg=audit(no:42): uid=0", "type=SYSCALL msg=audit(1700000000.1234567890:42): uid=0", "type=SYSCALL msg=audit(1700000000:18446744073709551616): uid=0", "type=SYSCALL msg=audit(999999999999999999999999:42): uid=0"} {
		line := "<14>1 - host audispd - - - " + msg
		r := Parse(line, "syslog", time.Now().UTC())
		before := r
		if AttachAudit(&r) || r.Audit != nil || !reflect.DeepEqual(before, r) {
			t.Fatalf("malformed audit changed original syslog: %+v", r)
		}
	}
	if AttachAudit(nil) {
		t.Fatal("nil audit wrapper succeeded")
	}
}

func TestAuditEventIDWithoutFractionIsExact(t *testing.T) {
	r := Parse("type=SYSCALL msg=audit(1700000000:42): uid=0", "auditd", time.Now().UTC())
	if r.Audit == nil || r.Audit.EventID != "1700000000:42" || r.OriginalTimestamp == nil || !r.OriginalTimestamp.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("audit identity fabricated: %+v", r)
	}
}

func FuzzSyslogParse(f *testing.F) {
	for _, line := range []string{"<0>1 - - - - - -", `<165>1 2003-10-11T22:14:15.003Z host app 42 ID [x value="a\"b\]c"] hello`, "<34>Oct 11 22:14:15 host su: hello", "<+1>1 malformed", "garbage", "type=SYSCALL msg=audit(1700000000:42): uid=0"} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 64<<10 {
			t.Skip()
		}
		at := time.Unix(1700000000, 0).UTC()
		for _, format := range []string{"syslog", "auditd", "raw"} {
			r := Parse(line, format, at)
			if r.Raw != line || !r.ObservedAt.Equal(at) || len(r.Message) > len(line) || r.Observation != nil {
				t.Fatal("parser rewrote raw input or observation time")
			}
			if r.OriginalTimestamp != nil && (r.OriginalTimestamp.Year() < 1 || r.OriginalTimestamp.Year() > 9999) {
				t.Fatal("source timestamp outside wire range")
			}
			AttachAudit(&r)
			if r.Raw != line {
				t.Fatal("audit wrapper rewrote raw input")
			}
		}
	})
}

func FuzzStructuredDataBoundary(f *testing.F) {
	for _, line := range []string{"-", "- msg", `[x p="a\"b\]c"] body`, `[x][y p="a b"] \ufeffbody`, "[broken"} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 64<<10 {
			t.Skip()
		}
		sd, msg, ok := structuredMessage(line)
		if ok && line != sd && line != sd+" "+msg {
			t.Fatal("SD boundary changed source bytes")
		}
	})
}
