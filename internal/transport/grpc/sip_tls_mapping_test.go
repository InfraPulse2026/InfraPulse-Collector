package grpc

import (
	"reflect"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
)

func TestSIPTLSHealthWireRoundtrip(t *testing.T) {
	now := time.Now().UTC()
	code := uint32(200)
	e := model.Envelope{MessageID: "message", AgentID: "agent", SessionID: "session", SentAt: now, Telephony: &model.TelephonyUpdate{ObservedAt: now, Source: "sip.options", Targets: []model.TelephonyHealth{{TargetID: "pbx", Address: "pbx.example.net:5061", Transport: "tls", Source: "sip.options", ObservedAt: now, State: "up", Reachable: true, ResponseCode: &code}}}}
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
		t.Fatal(back, err)
	}
}
