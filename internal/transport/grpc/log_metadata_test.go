package grpc

import (
	"bytes"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func syslogEnvelope() model.Envelope {
	at := testTime.UTC()
	source := at.Add(-time.Second)
	zero, offset := time.Duration(0), -25*time.Millisecond
	o := model.NewObservation(model.SourceRef{Type: model.SourceSyslog, CollectorID: "collector", TargetID: "server-a",
		InstanceID: "server-a", Protocol: "tls", Provenance: model.ProvenanceReal}, "syslog", at, at, &source)
	o.Time.ClockOffset, o.Time.Uncertainty = &offset, &zero
	e := testEnvelope()
	e.Logs = &model.LogBatch{Source: "syslog", ObservedAt: at, Observation: model.CloneObservation(o), Records: []model.LogRecord{{
		ID: "record", DeviceID: "syslog:server-a", Source: "syslog", SourceID: "server-a", ObservedAt: at,
		OriginalTimestamp: &source, Observation: o, Message: "message", Raw: "raw",
		Syslog: &model.SyslogMetadata{Version: 1, MessageID: "ID", StructuredData: `[example@32473 key="value"]`,
			RawTimestamp: "2026-10-08T12:00:00+03:00", Format: "rfc5424", ParseStatus: "parsed", PeerIP: "192.0.2.1",
			Transport: "tls", TLSIdentity: "dns:server-a.example", Authenticated: true, Managed: true},
	}}}
	return e
}

func TestSyslogSourceAndTimeWireRoundtrip(t *testing.T) {
	for _, raw := range [][]byte{nil, {0xff, ' ', 'm', 's', 'g'}} {
		e := syslogEnvelope()
		e.Logs.Records[0].Syslog.RawPayload = raw
		p, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		data, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		wire := new(pb.AgentEnvelope)
		if err := proto.Unmarshal(data, wire); err != nil {
			t.Fatal(err)
		}
		back, err := DecodeEnvelope(wire)
		if err != nil || !reflect.DeepEqual(e, back) {
			t.Fatalf("source/time/syslog presence lost: %v\n%#v\n%#v", err, e.Logs, back.Logs)
		}
		if raw != nil {
			p.GetLogs().Records[0].Syslog.RawPayload[0] = 0xfe
			if raw[0] != 0xff {
				t.Fatal("encoder aliases source raw bytes")
			}
			wire.GetLogs().Records[0].Syslog.RawPayload[0] = 0xfd
			if !bytes.Equal(back.Logs.Records[0].Syslog.RawPayload, raw) {
				t.Fatal("decoder aliases wire raw bytes")
			}
		}
	}
	for name, number := range map[string]protoreflect.FieldNumber{"syslog": 18, "observation": 19, "audit": 17, "raw": 13} {
		if field := (&pb.LogRecord{}).ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(name)); field == nil || field.Number() != number {
			t.Fatal("log field number changed", name)
		}
	}
	if (&pb.LogBatch{}).ProtoReflect().Descriptor().Fields().ByName("observation").Number() != 4 {
		t.Fatal("batch observation field number changed")
	}
}

func TestSyslogMissingOptionalEvidenceRemainsMissing(t *testing.T) {
	e := syslogEnvelope()
	e.Logs.Observation = nil
	r := &e.Logs.Records[0]
	r.OriginalTimestamp = nil
	r.Observation.Time.SourceTimestamp, r.Observation.Time.ClockOffset, r.Observation.Time.Uncertainty = nil, nil, nil
	r.Syslog = &model.SyslogMetadata{Format: "rfc3164", ParseStatus: "parsed", RawTimestamp: "Oct  8 12:00:00", Transport: "tcp"}
	p, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEnvelope(p)
	if err != nil || !reflect.DeepEqual(e, back) {
		t.Fatal("nil source/time evidence fabricated", err)
	}
	r.Observation = nil
	r.Syslog = nil
	p, err = Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	back, err = DecodeEnvelope(p)
	if err != nil || back.Logs.Observation != nil || back.Logs.Records[0].Observation != nil || back.Logs.Records[0].Syslog != nil {
		t.Fatal("legacy wire fabricated metadata", err)
	}
}

func TestSyslogRejectsMalformedNormalizedMetadata(t *testing.T) {
	cases := map[string]func(*model.LogBatch){
		"oversized SD":                 func(b *model.LogBatch) { b.Records[0].Syslog.StructuredData = strings.Repeat("x", 65537) },
		"invalid UTF8":                 func(b *model.LogBatch) { b.Records[0].Syslog.MessageID = string([]byte{0xff}) },
		"invalid message UTF8":         func(b *model.LogBatch) { b.Records[0].Message = string([]byte{0xff}) },
		"bad peer":                     func(b *model.LogBatch) { b.Records[0].Syslog.PeerIP = "claimed.host" },
		"peer zone":                    func(b *model.LogBatch) { b.Records[0].Syslog.PeerIP = "fe80::1%eth0" },
		"unknown transport":            func(b *model.LogBatch) { b.Records[0].Syslog.Transport = "udp" },
		"tcp authenticated":            func(b *model.LogBatch) { b.Records[0].Syslog.Transport = "tcp" },
		"empty authenticated identity": func(b *model.LogBatch) { b.Records[0].Syslog.TLSIdentity = "" },
		"unauthenticated identity":     func(b *model.LogBatch) { b.Records[0].Syslog.Authenticated = false },
		"unknown format":               func(b *model.LogBatch) { b.Records[0].Syslog.Format = "json" },
		"unknown parse status":         func(b *model.LogBatch) { b.Records[0].Syslog.ParseStatus = "complete" },
		"version overflow":             func(b *model.LogBatch) { b.Records[0].Syslog.Version = 1000 },
		"valid UTF8 raw bytes":         func(b *model.LogBatch) { b.Records[0].Syslog.RawPayload = []byte("use raw string") },
		"oversized raw bytes":          func(b *model.LogBatch) { b.Records[0].Syslog.RawPayload = bytes.Repeat([]byte{0xff}, 65537) },
		"unknown source type":          func(b *model.LogBatch) { b.Records[0].Observation.Source.Type = "unknown" },
		"bad source id":                func(b *model.LogBatch) { b.Records[0].Observation.Source.TargetID = "line\nbreak" },
		"source UTF8":                  func(b *model.LogBatch) { b.Records[0].Observation.Source.TargetID = string([]byte{0xff}) },
		"unknown provenance":           func(b *model.LogBatch) { b.Observation.Source.Provenance = "trusted" },
		"missing collected":            func(b *model.LogBatch) { b.Records[0].Observation.Time.CollectedAt = time.Time{} },
		"negative uncertainty":         func(b *model.LogBatch) { b.Records[0].Observation.Time.Uncertainty = ptr(-time.Second) },
		"unknown clock quality":        func(b *model.LogBatch) { b.Records[0].Observation.Time.Quality = "guaranteed" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := syslogEnvelope()
			change(e.Logs)
			if _, err := Encode(e); err == nil {
				t.Fatal("malformed normalized metadata accepted")
			}
		})
	}
}

func TestSyslogRejectsMalformedWireMetadata(t *testing.T) {
	cases := map[string]func(*pb.LogRecord){
		"missing source":                   func(r *pb.LogRecord) { r.Observation.Source = nil },
		"missing time":                     func(r *pb.LogRecord) { r.Observation.Time = nil },
		"missing collected":                func(r *pb.LogRecord) { r.Observation.Time.CollectedAt = nil },
		"bad source timestamp":             func(r *pb.LogRecord) { r.Observation.Time.SourceTimestamp = &timestamppb.Timestamp{Nanos: 1000000000} },
		"overflow clock duration":          func(r *pb.LogRecord) { r.Observation.Time.ClockOffset = &durationpb.Duration{Seconds: 315576000000} },
		"negative overflow clock duration": func(r *pb.LogRecord) { r.Observation.Time.ClockOffset = &durationpb.Duration{Seconds: -315576000000} },
		"bad duration nanos":               func(r *pb.LogRecord) { r.Observation.Time.ClockOffset = &durationpb.Duration{Nanos: 1000000000} },
		"negative uncertainty":             func(r *pb.LogRecord) { r.Observation.Time.Uncertainty = durationpb.New(-time.Second) },
		"tcp authentication":               func(r *pb.LogRecord) { r.Syslog.Transport = "tcp" },
		"huge raw":                         func(r *pb.LogRecord) { r.Syslog.RawPayload = bytes.Repeat([]byte{0xff}, 65537) },
		"UTF8 invariant":                   func(r *pb.LogRecord) { r.Raw = string([]byte{0xff}) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := Encode(syslogEnvelope())
			if err != nil {
				t.Fatal(err)
			}
			change(p.GetLogs().Records[0])
			if _, err := DecodeEnvelope(p); err == nil {
				t.Fatal("malformed wire metadata accepted")
			}
		})
	}
}

func TestLogClockDurationPreservesSignedExtremes(t *testing.T) {
	for _, value := range []time.Duration{time.Duration(math.MinInt64), -1, 0, 1, time.Duration(math.MaxInt64)} {
		back, err := observationDurationFromProto(durationpb.New(value))
		if err != nil || back == nil || *back != value {
			t.Fatal("valid duration changed", value, back, err)
		}
	}
}
