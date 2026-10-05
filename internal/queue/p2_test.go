package queue

import (
	"context"
	"errors"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestLogAdmissionCannotEvictTelemetryOrCommandResults(t *testing.T) {
	q, _ := New(2, 8192, 4096)
	if err := q.Push(model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "real"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(model.Envelope{CommandResult: &model.CommandResult{CommandID: "critical"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(model.Envelope{Logs: &model.LogBatch{Source: "storm"}}); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	if q.State().DroppedLogs != 1 {
		t.Fatal("log drops not observable")
	}
	result, _ := q.Pop(context.Background())
	if result.CommandResult == nil {
		t.Fatal("result lost")
	}
	sample, _ := q.Pop(context.Background())
	if sample.Telemetry == nil {
		t.Fatal("telemetry lost")
	}
}
func TestTelemetryEvictsAdmittedLogsWithObservableLoss(t *testing.T) {
	q, _ := New(1, 8192, 4096)
	if err := q.Push(model.Envelope{Logs: &model.LogBatch{Source: "linux"}}); err != nil {
		t.Fatal(err)
	}
	if err := q.Push(model.Envelope{Telemetry: &model.TelemetrySample{DeviceID: "real"}}); err != nil {
		t.Fatal(err)
	}
	if q.State().DroppedLogs != 1 {
		t.Fatal("eviction not counted")
	}
	e, _ := q.Pop(context.Background())
	if e.Telemetry == nil {
		t.Fatal("priority")
	}
}
