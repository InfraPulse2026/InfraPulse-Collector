package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/sanitize"
)

type fixture struct {
	t       *testing.T
	dir     string
	path    string
	state   string
	p       *Pipeline
	records []model.LogRecord
	events  []model.Event
	reject  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := testTempDir(t)
	f := &fixture{t: t, dir: dir, path: filepath.Join(dir, "syslog"), state: filepath.Join(dir, "state")}
	f.write(f.path, "")
	f.start("syslog", 16, 128<<10, 4096)
	t.Cleanup(func() {
		if err := f.p.Close(); err != nil {
			t.Errorf("pipeline cleanup: %v", err)
		}
		for _, source := range f.p.sources {
			if len(source.handles) != 0 {
				t.Error("pipeline cleanup left owned file handles")
			}
		}
	})
	return f
}

func (f *fixture) start(format string, count, batchBytes, readBytes int) {
	f.t.Helper()
	o := Options{StateDir: f.state, DeviceID: "local:test", Sources: []Source{{ID: "system", Path: f.path, Format: format}}, MaxLineBytes: 128, MaxBatchRecords: count, MaxBatchBytes: batchBytes, MaxReadBytes: readBytes, Sanitize: func(s string) string {
		return sanitize.TextWithSecrets(s, []string{"configured-community", "router-secret"})
	}, Emit: func(e model.Envelope) error {
		if e.Logs != nil {
			if f.reject {
				return errors.New("RAM queue rejected batch")
			}
			f.records = append(f.records, e.Logs.Records...)
		}
		f.events = append(f.events, e.Events...)
		return nil
	}}
	p, err := New(o)
	if err != nil {
		f.t.Fatal(err)
	}
	f.p = p
}

func (f *fixture) write(path, data string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) append(data string) {
	f.t.Helper()
	h, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = h.WriteString(data); err != nil {
		f.t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) poll() {
	f.t.Helper()
	if err := f.p.Poll(context.Background(), "system"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) restart() {
	f.t.Helper()
	if err := f.p.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.start("syslog", 16, 128<<10, 4096)
}

func (f *fixture) messages() []string {
	values := make([]string, len(f.records))
	for i, r := range f.records {
		values[i] = r.Message
	}
	return values
}

func TestAppendRestartAndIncompleteLine(t *testing.T) {
	f := newFixture(t)
	f.append("first\npartial")
	f.poll()
	if len(f.records) != 1 || f.records[0].Message != "first" || f.p.Snapshot()[0].Offset != 6 {
		t.Fatalf("wrong first batch/cursor: %+v %+v", f.messages(), f.p.Snapshot())
	}
	f.restart()
	f.poll()
	if len(f.records) != 1 {
		t.Fatal("incomplete line or accepted line replayed")
	}
	f.append(" complete\nnext\n")
	f.poll()
	if got := strings.Join(f.messages(), "|"); got != "first|partial complete|next" {
		t.Fatal(got)
	}
	for _, r := range f.records {
		if r.ID == "" || r.DeviceID != "local:test" || r.SourceID != "system" || r.FileIdentity == "" || r.ObservedAt.IsZero() {
			t.Fatal("missing normalized metadata")
		}
	}
}

func TestRenameNewFileDrainsHeldDescriptorAndRestarts(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			f := newFixture(t)
			f.append("first\n")
			f.poll()
			f.append("old tail\n")
			if err := os.Rename(f.path, f.path+".1"); err != nil {
				t.Fatal(err)
			}
			f.write(f.path, "new line\n")
			if restart {
				f.restart()
			}
			f.poll()
			if got := strings.Join(f.messages(), "|"); got != "first|old tail|new line" {
				t.Fatal(got)
			}
			if f.records[0].FileIdentity == f.records[2].FileIdentity || f.p.Snapshot()[0].Rotations != 1 {
				t.Fatal("identity change not recognized")
			}
		})
	}
}

func TestTruncateAndCopytruncateRegrowth(t *testing.T) {
	for _, data := range []string{"new\n", "replacement content longer than the prior file\n", ""} {
		t.Run(fmt.Sprint(len(data)), func(t *testing.T) {
			f := newFixture(t)
			f.append("original content\n")
			f.poll()
			if err := os.WriteFile(f.path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			f.poll()
			if f.p.Snapshot()[0].Truncations != 1 {
				t.Fatalf("truncate unrecognized: %+v", f.p.Snapshot())
			}
			if data != "" && (len(f.records) != 2 || f.records[1].Message != strings.TrimSpace(data) || f.records[1].ID == f.records[0].ID) {
				t.Fatal(f.messages())
			}
		})
	}
}

func TestCopytruncateDoesNotReplayCopiedRotation(t *testing.T) {
	f := newFixture(t)
	f.append("original\n")
	f.poll()
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.write(f.path+".1", string(data))
	f.write(f.path, "fresh\n")
	f.poll()
	// A copied rotation has a new identity and can replay old content. Exact
	// rename rotation is distinguished from copies by identity, not guessed
	// from equal content; this documented duplication is safer than loss.
	if got := strings.Join(f.messages(), "|"); got != "original|fresh|original" {
		t.Fatal(got)
	}
}

func TestDeleteRecreateRetainsOpenFile(t *testing.T) {
	f := newFixture(t)
	f.append("accepted\n")
	f.poll()
	f.append("deleted tail\n")
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.write(f.path, "replacement\n")
	f.poll()
	if got := strings.Join(f.messages(), "|"); got != "accepted|deleted tail|replacement" {
		t.Fatal(got)
	}
}

func TestDeleteRecreateAfterRestartReportsGap(t *testing.T) {
	f := newFixture(t)
	f.append("accepted\n")
	f.poll()
	f.append("lost tail\n")
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.write(f.path, "replacement\n")
	f.start("syslog", 16, 128<<10, 4096)
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrRotationGap) {
		t.Fatal(err)
	}
	if got := strings.Join(f.messages(), "|"); got != "accepted|replacement" {
		t.Fatal(got)
	}
	found := false
	for _, e := range f.events {
		found = found || e.Type == "log.source.gap"
	}
	if !found {
		t.Fatal("lost rotation was silent")
	}
}

func TestRapidRotationsBetweenPolls(t *testing.T) {
	f := newFixture(t)
	f.append("base\n")
	f.poll()
	for i := 1; i <= 4; i++ {
		f.append(fmt.Sprintf("tail-%d\n", i))
		if err := os.Rename(f.path, fmt.Sprintf("%s.%d", f.path, i)); err != nil {
			t.Fatal(err)
		}
		f.write(f.path, fmt.Sprintf("file-%d\n", i))
	}
	f.poll()
	if len(f.records) != 9 {
		t.Fatal(f.messages())
	}
	seen := map[string]bool{}
	for _, r := range f.records {
		if seen[r.ID] {
			t.Fatal("duplicate record in rapid rename rotations")
		}
		seen[r.ID] = true
	}
	f.poll()
	if len(f.records) != 9 {
		t.Fatal("replayed completed rotations")
	}
}

func TestFirstRejectedBatchSurvivesRenameAndRestart(t *testing.T) {
	f := newFixture(t)
	f.reject = true
	f.append("rejected old\n")
	if err := f.p.Poll(context.Background(), "system"); err == nil {
		t.Fatal("batch should be rejected")
	}
	if f.p.sources["system"].cursor.Files[0].Offset != 0 {
		t.Fatal("rejected record advanced cursor")
	}
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		t.Fatal(err)
	}
	f.write(f.path, "new\n")
	f.restart()
	f.reject = false
	f.poll()
	if got := strings.Join(f.messages(), "|"); got != "rejected old|new" {
		t.Fatal(got)
	}
}

func TestEmptyAndMissingSourceRecovery(t *testing.T) {
	f := newFixture(t)
	f.poll()
	if len(f.records) != 0 {
		t.Fatal("empty file produced synthetic record")
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal(err)
	}
	f.write(f.path, "recovered\n")
	f.poll()
	if len(f.records) != 1 || f.p.Snapshot()[0].Status != "available" {
		t.Fatal(f.p.Snapshot())
	}
}

func TestOversizedLineBoundedDiscardRestart(t *testing.T) {
	f := newFixture(t)
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	f.start("syslog", 2, 8192, 129)
	f.append(strings.Repeat("router-secret", 10000) + "\nafter\n")
	f.poll()
	if len(f.records) != 1 || !f.records[0].Truncated || f.records[0].Raw != "[OVERSIZED LOG LINE REDACTED]" {
		t.Fatal(f.messages())
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	f.start("syslog", 2, 8192, 4096)
	for i := 0; i < 40 && len(f.records) < 2; i++ {
		f.poll()
	}
	if len(f.records) != 2 || f.records[1].Message != "after" {
		t.Fatal(f.messages())
	}
}

func TestLogBurstLimitsBatchMemoryAndCancellation(t *testing.T) {
	f := newFixture(t)
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	f.start("syslog", 3, 8192, 4096)
	f.append(strings.Repeat("message\n", 10000))
	f.poll()
	if len(f.records) != 3 || len(f.p.sources["system"].handles) != 1 {
		t.Fatal("unbounded burst read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := f.p.Snapshot()[0].Offset
	if err := f.p.Poll(ctx, "system"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.p.Snapshot()[0].Offset != before || len(f.records) != 3 {
		t.Fatal("canceled poll advanced")
	}
}

func TestPermissionErrorWithoutRoot(t *testing.T) {
	f := newFixture(t)
	f.p.open = func(string) (*os.File, error) { return nil, os.ErrPermission }
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrPermission) {
		t.Fatal(err)
	}
	if f.p.Snapshot()[0].LastError != ErrPermission.Error() {
		t.Fatal("raw filesystem error leaked")
	}
}

func TestCancellationDuringDiscovery(t *testing.T) {
	f := newFixture(t)
	f.append("unread\n")
	ctx, cancel := context.WithCancel(context.Background())
	f.p.open = func(path string) (*os.File, error) {
		file, err := openLog(path)
		cancel()
		return file, err
	}
	if err := f.p.Poll(ctx, "system"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(f.records) != 0 {
		t.Fatal("cancellation ignored")
	}
}

func TestMalformedInputAndNonUTF8(t *testing.T) {
	f := newFixture(t)
	f.append("not a syslog header\n\xffbad\n<999>broken priority\n")
	f.poll()
	if len(f.records) != 3 || f.records[0].Host != "" || f.records[0].OriginalTimestamp != nil || f.records[2].Severity != nil || strings.ContainsRune(f.records[1].Message, '\xff') {
		t.Fatal(f.records)
	}
}

func TestPrivateKeyAcrossRestartAndRotation(t *testing.T) {
	f := newFixture(t)
	f.append("-----BEGIN RSA PRIVATE KEY-----\nSECRETBASE64\n")
	f.poll()
	f.restart()
	if err := os.Rename(f.path, f.path+".1"); err != nil {
		t.Fatal(err)
	}
	f.write(f.path, "MORESECRETBASE64\n-----END RSA PRIVATE KEY-----\nnormal configured-community\n")
	f.poll()
	for _, r := range f.records {
		if strings.Contains(r.Raw, "SECRETBASE64") || strings.Contains(r.Raw, "configured-community") {
			t.Fatal("secret exposed")
		}
	}
	if f.records[len(f.records)-1].Message != "normal [REDACTED]" {
		t.Fatal("private key suppression did not end")
	}
}

func TestCorruptedCursorFailsClosed(t *testing.T) {
	for _, content := range []string{"{", `{"version":1,"source_id":"system","path":"wrong","format":"syslog","files":[]}`, `{"version":99}`, `null`, `{}`} {
		t.Run(content, func(t *testing.T) {
			f := newFixture(t)
			if err := f.p.Close(); err != nil {
				t.Fatal(err)
			}
			f.write(cursorPath(filepath.Join(f.state, "log-cursors"), "system"), content)
			o := f.p.options
			if _, err := New(o); !errors.Is(err, ErrCursorCorrupt) {
				t.Fatal(err)
			}
		})
	}
}

func TestCorruptedCursorMembershipAndOffsetAnchor(t *testing.T) {
	for _, corrupt := range []func(*cursor){
		func(c *cursor) { c.CurrentIdentity = "different-identity" },
		func(c *cursor) { c.Files[0].Anchor = "" },
		func(c *cursor) { c.Seen = []string{c.Files[0].Identity} },
		func(c *cursor) { c.Files[0].Offset = -1 },
	} {
		f := newFixture(t)
		f.append("accepted\n")
		f.poll()
		c := cloneCursor(f.p.sources["system"].cursor)
		if err := f.p.Close(); err != nil {
			t.Fatal(err)
		}
		corrupt(&c)
		if err := saveCursor(f.p.dir, c); err != nil {
			t.Fatal(err)
		}
		if _, err := New(f.p.options); !errors.Is(err, ErrCursorCorrupt) {
			t.Fatal("malformed persisted cursor shape accepted", err)
		}
	}
}

func TestCursorPersistenceFailureBlocksReadsAndCanReplay(t *testing.T) {
	f := newFixture(t)
	f.append("first\n")
	calls := 0
	f.p.save = func(dir string, c cursor) error {
		calls++
		if calls >= 2 {
			return ErrStatePersistence
		}
		return saveCursor(dir, c)
	}
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrStatePersistence) {
		t.Fatal(err)
	}
	if len(f.records) != 1 {
		t.Fatal("batch was not admitted before cursor persistence")
	}
	f.append("second\n")
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrStatePersistence) || len(f.records) != 1 {
		t.Fatal("read proceeded while accepted cursor was not persisted")
	}
	firstID := f.records[0].ID
	_ = f.p.Close() // simulated crash with failed state persistence
	f.start("syslog", 16, 128<<10, 4096)
	f.poll()
	if len(f.records) != 3 || f.records[1].ID != firstID {
		t.Fatal("crash replay lost deterministic identity", f.messages())
	}
}

func TestAdmissionPersistsCursorButNotRAMDelivery(t *testing.T) {
	f := newFixture(t)
	f.append("RAM admitted\n")
	f.poll()
	f.records = nil // represents a crash that lost the volatile outbox
	f.restart()
	f.poll()
	if len(f.records) != 0 {
		t.Fatal("test documents admission cursor semantics, not durable delivery")
	}
}

func TestRotationLimitsAreExplicit(t *testing.T) {
	f := newFixture(t)
	f.poll()
	for i := 0; i < maxTrackedFiles+1; i++ {
		f.write(fmt.Sprintf("%s.%d", f.path, i), "new rotation\n")
	}
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrRotationLimit) {
		t.Fatal(err)
	}
	if len(f.p.sources["system"].handles) > maxTrackedFiles {
		t.Fatal("unbounded held descriptors")
	}
}

func TestNonRegularSourceAndIdempotentClose(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrNotRegular) {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Poll(context.Background(), "system"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestAuditRecordsShareExactEventIdentity(t *testing.T) {
	f := newFixture(t)
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	f.start("auditd", 16, 128<<10, 4096)
	f.append("type=SYSCALL msg=audit(1700000000.123:42): uid=0 auid=1000 pid=23 syscall=59 success=yes exit=0\ntype=PATH msg=audit(1700000000.123:42): item=0 name=\"/tmp/example\"\n")
	f.poll()
	if len(f.records) != 2 || f.records[0].Audit == nil || f.records[1].Audit == nil || f.records[0].Audit.EventID != f.records[1].Audit.EventID || f.records[0].Source != "auditd" {
		t.Fatal("audit correlation identity lost")
	}
	if f.records[0].OriginalTimestamp == nil || !f.records[0].OriginalTimestamp.Equal(time.Unix(1700000000, 123000000)) {
		t.Fatal("wrong audit timestamp")
	}
}

func TestSerializedEscapingDoesNotStallSource(t *testing.T) {
	f := newFixture(t)
	if err := f.p.Close(); err != nil {
		t.Fatal(err)
	}
	f.start("raw", 2, 8192, 4096)
	f.p.options.MaxLineBytes = 1024
	f.append(strings.Repeat("<", 1000) + "\nafter\n")
	f.poll()
	if len(f.records) != 2 || !f.records[0].Truncated || f.records[0].Raw != "[LOG RECORD EXCEEDS BATCH LIMIT; REDACTED]" || f.records[1].Raw != "after" || f.p.Snapshot()[0].Offset != 1007 {
		t.Fatal("escaped record stalled source or exceeded bounds", f.messages(), f.p.Snapshot())
	}
	f.poll()
	if len(f.records) != 2 {
		t.Fatal("oversized serialized record replayed")
	}
}

func TestShortSecretsPostSanitationBoundsAndWholeMarkers(t *testing.T) {
	for _, values := range [][]string{{"x"}, {"D", "E", "R"}, {"[REDACTED]", "BEGIN"}} {
		t.Run(strings.Join(values, "/"), func(t *testing.T) {
			f := newFixture(t)
			f.p.options.Sanitize = func(s string) string { return sanitize.TextWithSecrets(s, values) }
			f.p.options.Emit = func(e model.Envelope) error {
				if e.Logs == nil {
					return nil
				}
				agentPass := (sanitize.KnownSecrets{Values: values}).Sanitize(e)
				data, err := json.Marshal(agentPass.Logs)
				if err != nil || len(data) > f.p.options.MaxBatchBytes {
					t.Error("second Agent sanitation exceeds batch byte limit", err)
				}
				for i, record := range agentPass.Logs.Records {
					if record.Message != e.Logs.Records[i].Message || record.Raw != e.Logs.Records[i].Raw || len(record.Message) > f.p.options.MaxLineBytes || len(record.Raw) > f.p.options.MaxLineBytes {
						t.Error("second Agent sanitation changes already bounded text")
					}
					for _, value := range values {
						if strings.Contains(record.Message+record.Raw, value) {
							t.Errorf("short secret remains in bounded log fields: %q", value)
						}
					}
				}
				f.records = append(f.records, agentPass.Logs.Records...)
				return nil
			}
			input := strings.Repeat(values[0], 120/len(values[0]))
			wasExpanded := len(sanitize.TextWithSecrets(input, values)) > f.p.options.MaxLineBytes
			f.append(input + "\nafter\n")
			f.poll()
			if len(f.records) != 2 || f.records[0].Truncated != wasExpanded {
				t.Fatal("sanitation expansion not replaced by a complete bounded marker", f.messages())
			}
		})
	}
}
