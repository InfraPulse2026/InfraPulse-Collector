package grpc

import (
	"errors"
	"net/netip"
	"slices"
	"time"
	"unicode/utf8"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/types/known/durationpb"
)

func logStringsBound(limit int, fields ...string) error {
	for _, value := range fields {
		if len(value) > limit || !utf8.ValidString(value) {
			return errors.New("invalid or oversized log text")
		}
	}
	return nil
}

func validateLogObservation(o *model.ObservationMetadata) error {
	if o == nil {
		return nil
	}
	if err := o.Validate(); err != nil {
		return err
	}
	return logStringsBound(256, string(o.Source.Type), o.Source.CollectorID, o.Source.TargetID,
		o.Source.Protocol, o.Source.InstanceID, string(o.Source.Provenance), string(o.Time.Quality))
}

func validateSyslogMetadata(s *model.SyslogMetadata) error {
	if s == nil {
		return nil
	}
	if err := logStringsBound(65536, s.StructuredData); err != nil {
		return err
	}
	if err := logStringsBound(1024, s.MessageID, s.RawTimestamp, s.TLSIdentity); err != nil {
		return err
	}
	if err := logStringsBound(128, s.PeerIP, s.Format, s.ParseStatus, s.Transport); err != nil {
		return err
	}
	if s.Format != "rfc5424" && s.Format != "rfc3164" && s.Format != "unknown" ||
		s.ParseStatus != "parsed" && s.ParseStatus != "fallback" || s.Version > 999 {
		return errors.New("invalid syslog parsing metadata")
	}
	if s.Transport != "" && s.Transport != "tls" && s.Transport != "tcp" {
		return errors.New("invalid syslog transport")
	}
	if s.PeerIP != "" {
		peer, err := netip.ParseAddr(s.PeerIP)
		if err != nil || peer.Zone() != "" {
			return errors.New("invalid syslog peer IP")
		}
	}
	if s.Authenticated && (s.Transport != "tls" || s.TLSIdentity == "") ||
		s.TLSIdentity != "" && (!s.Authenticated || s.Transport != "tls") {
		return errors.New("invalid syslog authentication evidence")
	}
	if len(s.RawPayload) > 65536 || len(s.RawPayload) > 0 && utf8.Valid(s.RawPayload) {
		return errors.New("invalid syslog raw octets")
	}
	return nil
}

func syslogToProto(s *model.SyslogMetadata) *pb.SyslogMetadata {
	if s == nil {
		return nil
	}
	return &pb.SyslogMetadata{Version: s.Version, MessageId: s.MessageID, StructuredData: s.StructuredData,
		RawTimestamp: s.RawTimestamp, Format: s.Format, ParseStatus: s.ParseStatus, PeerIp: s.PeerIP,
		Transport: s.Transport, TlsIdentity: s.TLSIdentity, Authenticated: s.Authenticated,
		Managed: s.Managed, RawPayload: slices.Clone(s.RawPayload)}
}

func syslogFromProto(s *pb.SyslogMetadata) (*model.SyslogMetadata, error) {
	if s == nil {
		return nil, nil
	}
	metadata := &model.SyslogMetadata{Version: s.Version, MessageID: s.MessageId, StructuredData: s.StructuredData,
		RawTimestamp: s.RawTimestamp, Format: s.Format, ParseStatus: s.ParseStatus, PeerIP: s.PeerIp,
		Transport: s.Transport, TLSIdentity: s.TlsIdentity, Authenticated: s.Authenticated,
		Managed: s.Managed, RawPayload: s.RawPayload}
	if err := validateSyslogMetadata(metadata); err != nil {
		return nil, err
	}
	metadata.RawPayload = slices.Clone(s.RawPayload)
	return metadata, nil
}

func logObservationToProto(o *model.ObservationMetadata) *pb.ObservationMetadata {
	if o == nil {
		return nil
	}
	s, at := o.Source, o.Time
	t := &pb.ObservationTime{ObservedAt: timestamp(at.ObservedAt), CollectedAt: timestamp(at.CollectedAt), ClockQuality: string(at.Quality)}
	if at.SourceTimestamp != nil {
		t.SourceTimestamp = timestamp(*at.SourceTimestamp)
	}
	if at.ClockOffset != nil {
		t.ClockOffset = durationpb.New(*at.ClockOffset)
	}
	if at.Uncertainty != nil {
		t.Uncertainty = durationpb.New(*at.Uncertainty)
	}
	return &pb.ObservationMetadata{Source: &pb.SourceRef{Type: string(s.Type), CollectorId: s.CollectorID,
		TargetId: s.TargetID, Protocol: s.Protocol, InstanceId: s.InstanceID, Provenance: string(s.Provenance)}, Time: t}
}

func logObservationFromProto(p *pb.ObservationMetadata) (*model.ObservationMetadata, error) {
	if p == nil {
		return nil, nil
	}
	if p.Source == nil || p.Time == nil {
		return nil, errors.New("incomplete log observation metadata")
	}
	s, at := p.Source, p.Time
	o := &model.ObservationMetadata{Source: model.SourceRef{Type: model.SourceType(s.Type), CollectorID: s.CollectorId,
		TargetID: s.TargetId, Protocol: s.Protocol, InstanceID: s.InstanceId, Provenance: model.Provenance(s.Provenance)},
		Time: model.ObservationTime{Quality: model.ClockQuality(at.ClockQuality)}}
	var err error
	o.Time.ObservedAt, err = requiredTime(at.ObservedAt, "log.observation.observed_at")
	if err != nil {
		return nil, err
	}
	o.Time.CollectedAt, err = requiredTime(at.CollectedAt, "log.observation.collected_at")
	if err != nil {
		return nil, err
	}
	if at.SourceTimestamp != nil {
		value, err := requiredTime(at.SourceTimestamp, "log.observation.source_timestamp")
		if err != nil {
			return nil, err
		}
		o.Time.SourceTimestamp = &value
	}
	o.Time.ClockOffset, err = logDurationFromProto(at.ClockOffset)
	if err != nil {
		return nil, err
	}
	o.Time.Uncertainty, err = logDurationFromProto(at.Uncertainty)
	if err != nil {
		return nil, err
	}
	return o, validateLogObservation(o)
}

func logDurationFromProto(p *durationpb.Duration) (*time.Duration, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.CheckValid(); err != nil {
		return nil, errors.New("invalid log clock duration")
	}
	value := p.AsDuration()
	roundtrip := durationpb.New(value)
	if roundtrip.Seconds != p.Seconds || roundtrip.Nanos != p.Nanos {
		return nil, errors.New("log clock duration overflows Go duration")
	}
	return &value, nil
}
