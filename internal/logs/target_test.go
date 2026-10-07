package logs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestFileSourceTargetAndTemporalMetadata(t *testing.T) {
	f := newFixture(t)
	f.p.options.CollectorID = "collector-test"
	f.write(f.path, "2026-10-07T10:01:02Z fixture daemon: timed record\n")
	if err := f.p.Poll(context.Background(), "system"); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 {
		t.Fatalf("records: %d", len(f.records))
	}
	r := f.records[0]
	o := r.Observation
	if o == nil || o.Source.Type != model.SourceSyslog || o.Source.Protocol != "file" || o.Source.CollectorID != "collector-test" || o.Source.TargetID != "logs:system" || o.Source.InstanceID != r.SourceID || o.Source.TargetID == r.DeviceID {
		t.Fatalf("file source binding: %+v", o)
	}
	if r.OriginalTimestamp == nil || o.Time.SourceTimestamp == nil || !o.Time.SourceTimestamp.Equal(*r.OriginalTimestamp) || !o.Time.ObservedAt.Equal(r.ObservedAt) || o.Time.CollectedAt.IsZero() || o.Time.ClockOffset != nil || o.Time.Uncertainty != nil || o.Time.Quality != model.ClockUnknown {
		t.Fatalf("file time metadata: %+v", o.Time)
	}
	if err := o.Time.Validate(); err != nil {
		t.Fatal(err)
	}
	target := (Source{ID: "system", Format: "syslog", Path: f.path}).Target(time.Second, time.Second)
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataIsIncludedInLogBatchBudget(t *testing.T) {
	f := newFixture(t)
	f.p.options.CollectorID = "collector-test"
	f.p.options.MaxBatchBytes = 4608
	var sizes []int
	f.p.options.Emit = func(e model.Envelope) error {
		if e.Logs != nil {
			data, err := json.Marshal(e.Logs)
			if err != nil {
				return err
			}
			sizes = append(sizes, len(data))
		}
		return nil
	}
	f.write(f.path, "Oct  7 10:01:02 host daemon: one\nOct  7 10:01:02 host daemon: two\nOct  7 10:01:02 host daemon: three\nOct  7 10:01:02 host daemon: four\nOct  7 10:01:02 host daemon: five\nOct  7 10:01:02 host daemon: six\n")
	for i := 0; i < 3; i++ {
		if err := f.p.Poll(context.Background(), "system"); err != nil {
			t.Fatal(err)
		}
	}
	if len(sizes) == 0 {
		t.Fatal("no log batches")
	}
	for _, size := range sizes {
		if size > f.p.options.MaxBatchBytes {
			t.Fatalf("metadata exceeded batch limit: %d", size)
		}
	}
}

func TestCollectionTimestampPrecisionCannotOverflowLogBatch(t *testing.T) {
	f := newFixture(t)
	f.start("syslog", 1024, 30<<10, 256<<10)
	f.p.options.CollectorID = "collector-test"
	observed := time.Date(2026, 10, 7, 10, 1, 2, 0, time.UTC)
	completed := observed.Add(999999999 * time.Nanosecond)
	calls := 0
	f.p.now = func() time.Time {
		calls++
		if calls%2 == 1 {
			return observed
		}
		return completed
	}
	var count int
	f.p.options.Emit = func(e model.Envelope) error {
		if b := e.Logs; b != nil {
			data, err := json.Marshal(b)
			if err != nil {
				return err
			}
			if len(data) > f.p.options.MaxBatchBytes {
				t.Fatalf("completion timestamp precision exceeded batch: %d", len(data))
			}
			for _, r := range b.Records {
				if !r.Observation.Time.CollectedAt.Equal(completed) {
					t.Fatal("collection completion not recorded")
				}
			}
			count += len(b.Records)
		}
		return nil
	}
	f.write(f.path, strings.Repeat("Oct  7 10:01:02 host daemon: record\n", 100))
	for count < 100 {
		before := count
		if err := f.p.Poll(context.Background(), "system"); err != nil {
			t.Fatal(err)
		}
		if count == before {
			t.Fatal("batch budget stalled log cursor")
		}
	}
}
