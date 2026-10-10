package queue

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestManagedStatusPayloadRemainsImmutableAndSeparateFromACK(t *testing.T) {
	q, err := New(4, 4096, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	r := model.ConfigApplyResult{CollectorID: "collector", Revision: 1, Status: model.ConfigStatusApplied}
	e := model.Envelope{MessageID: "status-id", ConfigApplyResult: &r}
	if q.Push(e) != nil {
		t.Fatal("status not admitted")
	}
	r.Status = model.ConfigStatusFailed
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := q.Pop(ctx)
	if err != nil || got.ConfigApplyResult == nil || got.ConfigApplyResult.Status != model.ConfigStatusApplied {
		t.Fatal("status admission aliases producer", err)
	}
	e.Telemetry = &model.TelemetrySample{}
	if q.Push(e) == nil {
		t.Fatal("mixed config/telemetry payload accepted")
	}
}
