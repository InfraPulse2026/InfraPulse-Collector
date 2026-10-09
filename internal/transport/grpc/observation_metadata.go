package grpc

import (
	"errors"
	"time"
	"unicode/utf8"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/types/known/durationpb"
)

func observationStringsBound(limit int, fields ...string) error {
	for _, value := range fields {
		if len(value) > limit || !utf8.ValidString(value) {
			return errors.New("invalid or oversized observation text")
		}
	}
	return nil
}
func validateObservation(o *model.ObservationMetadata) error {
	if o == nil {
		return nil
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if err := observationStringsBound(2048, o.Source.ResourceID); err != nil {
		return err
	}
	return observationStringsBound(256, string(o.Source.Type), o.Source.CollectorID, o.Source.TargetID,
		o.Source.Protocol, o.Source.InstanceID, string(o.Source.Provenance), string(o.Time.Quality))
}

func observationToProto(o *model.ObservationMetadata) *pb.ObservationMetadata {
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
		TargetId: s.TargetID, Protocol: s.Protocol, InstanceId: s.InstanceID, Provenance: string(s.Provenance), ResourceId: s.ResourceID}, Time: t}
}

func observationFromProto(p *pb.ObservationMetadata) (*model.ObservationMetadata, error) {
	if p == nil {
		return nil, nil
	}
	if p.Source == nil || p.Time == nil {
		return nil, errors.New("incomplete observation metadata")
	}
	s, at := p.Source, p.Time
	o := &model.ObservationMetadata{Source: model.SourceRef{Type: model.SourceType(s.Type), CollectorID: s.CollectorId,
		TargetID: s.TargetId, Protocol: s.Protocol, InstanceID: s.InstanceId, Provenance: model.Provenance(s.Provenance), ResourceID: s.ResourceId},
		Time: model.ObservationTime{Quality: model.ClockQuality(at.ClockQuality)}}
	var err error
	o.Time.ObservedAt, err = requiredTime(at.ObservedAt, "observation.observed_at")
	if err != nil {
		return nil, err
	}
	o.Time.CollectedAt, err = requiredTime(at.CollectedAt, "observation.collected_at")
	if err != nil {
		return nil, err
	}
	if at.SourceTimestamp != nil {
		value, err := requiredTime(at.SourceTimestamp, "observation.source_timestamp")
		if err != nil {
			return nil, err
		}
		o.Time.SourceTimestamp = &value
	}
	o.Time.ClockOffset, err = observationDurationFromProto(at.ClockOffset)
	if err != nil {
		return nil, err
	}
	o.Time.Uncertainty, err = observationDurationFromProto(at.Uncertainty)
	if err != nil {
		return nil, err
	}
	return o, validateObservation(o)
}

func observationDurationFromProto(p *durationpb.Duration) (*time.Duration, error) {
	if p == nil {
		return nil, nil
	}
	if err := p.CheckValid(); err != nil {
		return nil, errors.New("invalid observation clock duration")
	}
	value := p.AsDuration()
	roundtrip := durationpb.New(value)
	if roundtrip.Seconds != p.Seconds || roundtrip.Nanos != p.Nanos {
		return nil, errors.New("observation clock duration overflows Go duration")
	}
	return &value, nil
}
