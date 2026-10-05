package queue

import (
	"InfraPulseAgent/internal/model"
	"context"
	"errors"
	"testing"
)

func TestResultReservationPreventsOverflowAndEviction(t *testing.T) {
	q, _ := New(2, 8192, 4096)
	r, err := q.ReserveResult(4096)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Push(model.Envelope{CommandResult: &model.CommandResult{CommandID: "first"}}); err != nil {
		t.Fatal(err)
	}
	if err = q.Push(model.Envelope{CommandResult: &model.CommandResult{CommandID: "extra"}}); !errors.Is(err, ErrFull) {
		t.Fatal("reservation consumed", err)
	}
	if err = r.Commit(model.Envelope{CommandResult: &model.CommandResult{CommandID: "reserved"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "reserved"} {
		e, err := q.Pop(context.Background())
		if err != nil || e.CommandResult.CommandID != id {
			t.Fatal(e, err)
		}
	}
	r.Release()
	if q.reservedMessages != 0 || q.reservedBytes != 0 {
		t.Fatal("reservation leaked")
	}
}
