package logs

import (
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestSyslogKnownFormats(t *testing.T) {
	for _, test := range []struct {
		name, line, host, process, pid, message string
		timestamp                               bool
		priority                                *uint32
	}{
		{"rfc5424", `<165>1 2003-10-11T22:14:15.003Z mymachine su 123 ID47 - BOMsu root failed`, "mymachine", "su", "123", "BOMsu root failed", true, uintPointer(165)},
		{"structured", `<34>1 2003-10-11T22:14:15Z host app - ID [example@32473 key="a\"b"] hello`, "host", "app", "", "hello", true, uintPointer(34)},
		{"nil", `<0>1 - - - - - -`, "", "", "", "", false, uintPointer(0)},
		{"bsd", `Oct 11 22:14:15 host sshd[42]: failed password`, "host", "sshd", "42", "failed password", false, nil},
		{"bsd priority", `<34>Oct 11 22:14:15 host su: hello`, "host", "su", "", "hello", false, uintPointer(34)},
		{"rsyslog file", `2026-10-05T10:20:30.123+03:00 host process[3]: text`, "host", "process", "3", "text", true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Parse(test.line, "syslog", time.Now().UTC())
			if r.Host != test.host || r.Process != test.process || r.ProcessID != test.pid || r.Message != test.message || (r.OriginalTimestamp != nil) != test.timestamp {
				t.Fatalf("bad parsed record: %+v", r)
			}
			if test.priority == nil {
				if r.Severity != nil || r.Facility != nil {
					t.Fatal("unknown priority invented")
				}
			} else if r.Severity == nil || r.Facility == nil || *r.Severity != *test.priority%8 || *r.Facility != *test.priority/8 {
				t.Fatal("priority mapping failed")
			}
		})
	}
}

func uintPointer(v uint32) *uint32 { return &v }

func TestSyslogUncertainInputPreserved(t *testing.T) {
	for _, line := range []string{"garbage", "<999>1 malformed", "<034>1 malformed", "<1>1 badtime host app 1 ID - hello", "Oct 40 99:00:00 host app: wrong", `<34>1 2026-10-05T10:00:00Z host app 1 ID [notclosed`} {
		r := Parse(line, "syslog", time.Now().UTC())
		if r.Message != line || r.OriginalTimestamp != nil || r.Host != "" || r.Process != "" {
			t.Fatalf("uncertain fields invented: %+v", r)
		}
	}
}

func TestAuditWellDefinedFields(t *testing.T) {
	r := Parse(`node=host type=SYSCALL msg=audit(1700000000.123:42): uid=0 auid=4294967295 pid=25 exe="/usr/bin/example" syscall=59 success=no exit=-13`, "auditd", time.Now().UTC())
	a := r.Audit
	if a == nil || a.EventID != "1700000000.123:42" || a.RecordType != "SYSCALL" || a.UID == nil || *a.UID != 0 || a.AUID == nil || *a.AUID != 4294967295 || a.PID == nil || *a.PID != 25 || a.Executable != "/usr/bin/example" || a.Syscall != "59" || a.Success == nil || *a.Success || a.Result != "-13" || a.Host != "host" {
		t.Fatalf("bad audit fields: %+v", a)
	}
}

func TestAuditMalformedMissingAndConflictingFields(t *testing.T) {
	for _, line := range []string{"garbage", "type=SYSCALL msg=audit(no:42): uid=0", "type=SYSCALL msg=audit(1700000000.1234567890:42): uid=0"} {
		if r := Parse(line, "auditd", time.Now().UTC()); r.Audit != nil || r.OriginalTimestamp != nil {
			t.Fatal("malformed audit parsed", r)
		}
	}
	r := Parse(`type=SYSCALL msg=audit(1700000000.123:42): uid=0 uid=1 auid=invalid pid=-1 exe=414243 syscall=notanumber success=perhaps exit=bad msg='uid=3'`, "auditd", time.Now().UTC())
	if a := r.Audit; a == nil || a.UID != nil || a.AUID != nil || a.PID != nil || a.Executable != "" || a.Syscall != "" || a.Success != nil || a.Result != "" {
		t.Fatal("uncertain audit values invented", a)
	}
}

func TestAuditCorrelationIsExactAndBoundedByBatch(t *testing.T) {
	records := []model.LogRecord{
		{ID: "A1", DeviceID: "local:test", SourceID: "audit", Audit: &model.AuditRecord{EventID: "1.1:42", Host: "host"}},
		{ID: "B", DeviceID: "local:test", SourceID: "audit", Audit: &model.AuditRecord{EventID: "1.1:43", Host: "host"}},
		{ID: "A2", DeviceID: "local:test", SourceID: "audit", Audit: &model.AuditRecord{EventID: "1.1:42", Host: "host"}},
		{ID: "other-host", DeviceID: "local:test", SourceID: "audit", Audit: &model.AuditRecord{EventID: "1.1:42", Host: "other"}},
		{ID: "malformed"},
	}
	groups := correlateAuditBatch(records)
	if len(groups) != len(records) || groups[0].ID != "A1" || groups[1].ID != "A2" || groups[2].ID != "B" || groups[3].ID != "other-host" || groups[4].ID != "malformed" || records[1].ID != "B" {
		t.Fatal("audit correlation merged unrelated records or mutated source")
	}
}

func TestNormalizedFieldBoundsPreserveRaw(t *testing.T) {
	for _, line := range []string{
		"Oct 11 22:14:15 " + strings.Repeat("h", 2048) + " " + strings.Repeat("p", 2048) + ": message",
		`type=SYSCALL msg=audit(1700000000.123:42): exe="` + strings.Repeat("e", 5000) + `"`,
		"type=" + strings.Repeat("A", 5000) + " msg=audit(1700000000.123:42): uid=0",
		"type=SYSCALL msg=audit(1700000000.123:" + strings.Repeat("9", 5000) + "): uid=0",
	} {
		format := "auditd"
		if strings.HasPrefix(line, "Oct") {
			format = "syslog"
		}
		r := Parse(line, format, time.Now().UTC())
		if r.Raw != line || len(r.Host) > 1024 || len(r.Process) > 1024 || len(r.ProcessID) > 1024 {
			t.Fatal("normalized fields exceed wire bounds or raw changed")
		}
		if r.Audit != nil && (len(r.Audit.EventID) > 4096 || len(r.Audit.RecordType) > 4096 || len(r.Audit.Executable) > 4096) {
			t.Fatal("audit field exceeds wire bound")
		}
	}
}

func TestTimestampYearZeroIsUnavailable(t *testing.T) {
	for _, line := range []string{"<14>1 0000-01-01T00:00:00Z host app 1 ID - message", "0000-01-01T00:00:00Z host app: message"} {
		if r := Parse(line, "syslog", time.Now().UTC()); r.OriginalTimestamp != nil || r.Raw != line {
			t.Fatal("invalid protobuf timestamp invented")
		}
	}
}
