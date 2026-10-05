package grpc

import (
	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"errors"
	"math"
)

func queueFromProto(q *pb.QueueState) (model.QueueState, error) {
	if q.Unacked > uint64(math.MaxInt) || q.PendingResults > uint64(math.MaxInt) || q.StorageBytes > math.MaxInt64 {
		return model.QueueState{}, errors.New("queue counts overflow native integer")
	}
	oldest, err := optionalTime(q.OldestPending, "queue.oldest_pending")
	if err != nil {
		return model.QueueState{}, err
	}
	return model.QueueState{Messages: int(q.Messages), Bytes: int(q.Bytes), DroppedTelemetry: q.DroppedTelemetry, DroppedOther: q.DroppedOther, DroppedLogs: q.DroppedLogs, Mode: q.Mode, StorageBytes: int64(q.StorageBytes), OldestPending: oldest, Unacked: int(q.Unacked), PendingResults: int(q.PendingResults), StorageError: q.StorageError}, nil
}
func certificateToProto(c *model.CertificateStatus) *pb.CertificateStatus {
	if c == nil {
		return nil
	}
	return &pb.CertificateStatus{ExpiresAt: optionalTimestamp(c.ExpiresAt), RemainingSeconds: c.RemainingSeconds, EnrollmentState: c.EnrollmentState, RenewalState: c.RenewalState, LastSuccessfulRenewal: optionalTimestamp(c.LastSuccessfulRenewal), LastError: c.LastError}
}
func certificateFromProto(c *pb.CertificateStatus) (*model.CertificateStatus, error) {
	if c == nil {
		return nil, nil
	}
	expires, err := requiredTime(c.ExpiresAt, "certificate.expires_at")
	if err != nil {
		return nil, err
	}
	renewed, err := optionalTime(c.LastSuccessfulRenewal, "certificate.last_successful_renewal")
	if err != nil {
		return nil, err
	}
	if len(c.LastError) > 256 || len(c.EnrollmentState) > 64 || len(c.RenewalState) > 64 {
		return nil, errors.New("certificate status exceeds limit")
	}
	return &model.CertificateStatus{ExpiresAt: expires, RemainingSeconds: c.RemainingSeconds, EnrollmentState: c.EnrollmentState, RenewalState: c.RenewalState, LastSuccessfulRenewal: renewed, LastError: c.LastError}, nil
}
