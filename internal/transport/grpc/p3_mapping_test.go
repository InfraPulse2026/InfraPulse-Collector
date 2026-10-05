package grpc

import (
	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"reflect"
	"testing"
	"time"
)

func TestP3WireBackwardCompatibilityAndPresence(t *testing.T) {
	d := (&pb.ControllerEnvelope{}).ProtoReflect().Descriptor()
	for name, number := range map[string]int{"hello_ack": 1, "command": 2, "message_ack": 3} {
		if int(d.Fields().ByName(protoreflect.Name(name)).Number()) != number {
			t.Fatal(name)
		}
	}
	at := time.Now().UTC()
	c := model.Command{ID: "rollback", TargetDeviceID: "router", Initiator: "human", RequestedAt: at, Timeout: time.Second, Rollback: &model.RollbackAction{SnapshotID: "snapshot"}}
	wire, err := EncodeCommand(c)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := proto.Marshal(wire)
	decoded := new(pb.Command)
	_ = proto.Unmarshal(data, decoded)
	back, err := DecodeCommand(decoded)
	if err != nil || !reflect.DeepEqual(c, back) {
		t.Fatalf("rollback: %+v %v", back, err)
	}
	e := model.Envelope{MessageID: "id", AgentID: "agent", SessionID: "session", SentAt: at, Heartbeat: &model.Heartbeat{Timestamp: at, Queue: model.QueueState{Mode: "durable", StorageBytes: 1234, Messages: 3, Bytes: 300, Unacked: 2, PendingResults: 1, OldestPending: at, StorageError: "storage_unavailable"}, Certificate: &model.CertificateStatus{ExpiresAt: at.Add(time.Hour), RemainingSeconds: 3600, EnrollmentState: "enrolled", RenewalState: "idle"}}}
	w, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = proto.Marshal(w)
	parsed := new(pb.AgentEnvelope)
	_ = proto.Unmarshal(data, parsed)
	round, err := DecodeEnvelope(parsed)
	if err != nil || !reflect.DeepEqual(e, round) {
		t.Fatalf("heartbeat roundtrip: %+v %v", round, err)
	}
	e.Heartbeat = nil
	e.CommandResult = &model.CommandResult{CommandID: "rollback", Status: "timeout", Uncertain: true, SnapshotID: "snapshot"}
	w, err = Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	round, err = DecodeEnvelope(w)
	if err != nil || round.CommandResult.SnapshotID != "snapshot" || !round.CommandResult.Uncertain {
		t.Fatalf("result roundtrip: %+v %v", round, err)
	}
}
