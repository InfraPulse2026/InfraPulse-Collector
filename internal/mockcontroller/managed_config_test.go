package mockcontroller

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func managedMockStream(t *testing.T, c *Controller) (context.Context, *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.MaxRecvMsgSize(1 << 20))
	pb.RegisterAgentServiceServer(s, c)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { s.Stop(); _ = l.Close() })
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ctx, conn
}
func managedMockHello(t *testing.T, ctx context.Context, conn *grpc.ClientConn, id, session string, capable bool) grpc.BidiStreamingClient[pb.AgentEnvelope, pb.ControllerEnvelope] {
	t.Helper()
	s, err := pb.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := model.Envelope{MessageID: "hello", AgentID: id, SessionID: session, Sequence: 1, SentAt: time.Now().UTC(), Hello: &model.AgentHello{AgentID: id, SessionID: session, StartedAt: time.Now().UTC(), ProtocolVersion: 1, ApplicationACK: true, ManagedConfigV1: capable, Device: model.Device{ID: "local:" + id, Type: "unknown", Driver: "none"}}}
	w, err := transportgrpc.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Send(w); err != nil {
		t.Fatal(err)
	}
	ack, err := s.Recv()
	if err != nil || ack.GetHelloAck() == nil || ack.GetHelloAck().ManagedConfigV1 != capable {
		t.Fatal("mock capability handshake", err)
	}
	return s
}
func mockConfig(revision uint64) model.DesiredCollectorConfig {
	return model.DesiredCollectorConfig{Revision: revision, ConfigurationDigest: strings.Repeat("a", 64), Targets: []model.ManagedTargetSpec{{ID: "switch", Protocol: "snmp", Endpoint: "192.0.2.1", Enabled: true, CredentialRef: "snmp", SNMP: &model.ManagedSNMPOptions{Version: "v2c"}}}}
}
func managedMockStatus(t *testing.T, s grpc.BidiStreamingClient[pb.AgentEnvelope, pb.ControllerEnvelope], id, session string, sequence uint64, c model.DesiredCollectorConfig) {
	t.Helper()
	e := model.Envelope{MessageID: "status", AgentID: id, SessionID: session, Sequence: sequence, SentAt: time.Now().UTC(), ConfigApplyResult: &model.ConfigApplyResult{CollectorID: id, Revision: c.Revision, ConfigurationDigest: c.ConfigurationDigest, Status: model.ConfigStatusApplied, AppliedAt: time.Now().UTC(), AppliedRevision: c.Revision, AppliedDigest: c.ConfigurationDigest}}
	w, err := transportgrpc.Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Send(w); err != nil {
		t.Fatal(err)
	}
	ack, err := s.Recv()
	if err != nil || ack.GetMessageAck() == nil || ack.GetMessageAck().MessageId != "status" {
		t.Fatal("status envelope receipt ACK distinct from apply", err)
	}
}
func managedMockConfig(t *testing.T, s grpc.BidiStreamingClient[pb.AgentEnvelope, pb.ControllerEnvelope]) model.DesiredCollectorConfig {
	t.Helper()
	w, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if w.GetDesiredConfig() == nil {
		t.Fatal("expected typed desired config")
	}
	c, err := transportgrpc.DecodeDesiredConfig(w.GetDesiredConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMockManagedSnapshotsRepeatStaleAndReconnect(t *testing.T) {
	c := New(nil, nil, nil)
	c.RepeatConfig = true
	c.DisconnectAfterConfigStatus = 1
	if err := c.SetDesiredConfigs([]model.DesiredCollectorConfig{mockConfig(1), mockConfig(2), mockConfig(1)}); err != nil {
		t.Fatal(err)
	}
	ctx, conn := managedMockStream(t, c)
	s := managedMockHello(t, ctx, conn, "a", "session-1", true)
	first := managedMockConfig(t, s)
	if first.CollectorID != "a" || first.SessionID != "session-1" || first.Revision != 1 {
		t.Fatal("mock initial binding")
	}
	managedMockStatus(t, s, "a", "session-1", 2, first)
	if _, err := s.Recv(); err == nil {
		t.Fatal("mock did not disconnect")
	}
	s = managedMockHello(t, ctx, conn, "a", "session-2", true)
	repeat := managedMockConfig(t, s)
	if repeat.Revision != 1 || repeat.SessionID != "session-2" {
		t.Fatal("reconnect did not rebind pending snapshot")
	}
	managedMockStatus(t, s, "a", "session-2", 2, repeat)
	next := managedMockConfig(t, s)
	if next.Revision != 2 {
		t.Fatal("next snapshot not sent")
	}
	managedMockStatus(t, s, "a", "session-2", 3, next)
	repeat = managedMockConfig(t, s)
	if repeat.Revision != 2 {
		t.Fatal("idempotency repeat not sent")
	}
	managedMockStatus(t, s, "a", "session-2", 4, repeat)
	stale := managedMockConfig(t, s)
	if stale.Revision != 1 {
		t.Fatal("stale fixture not sent")
	}
}

func TestMockManagedInboxBoundedAndCollectorScoped(t *testing.T) {
	c := New(nil, nil, nil)
	ctx, conn := managedMockStream(t, c)
	a := managedMockHello(t, ctx, conn, "a", "session-a", true)
	b := managedMockHello(t, ctx, conn, "b", "session-b", true)
	for range 8 {
		if err := c.SendConfig("a", mockConfig(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SendConfig("a", mockConfig(2)); err == nil {
		t.Fatal("unbounded mock inbox")
	}
	wrong := mockConfig(2)
	wrong.CollectorID = "a"
	if err := c.SendConfig("b", wrong); err != nil {
		t.Fatal(err)
	}
	heartbeat := func(s grpc.BidiStreamingClient[pb.AgentEnvelope, pb.ControllerEnvelope], id, session string) {
		e := model.Envelope{MessageID: "heartbeat", AgentID: id, SessionID: session, Sequence: 2, SentAt: time.Now().UTC(), Heartbeat: &model.Heartbeat{Timestamp: time.Now().UTC()}}
		w, err := transportgrpc.Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Send(w); err != nil {
			t.Fatal(err)
		}
		ack, err := s.Recv()
		if err != nil || ack.GetMessageAck() == nil {
			t.Fatal(err)
		}
	}
	heartbeat(a, "a", "session-a")
	for range 8 {
		if got := managedMockConfig(t, a); got.CollectorID != "a" || got.SessionID != "session-a" {
			t.Fatal("cross-collector queue")
		}
	}
	heartbeat(b, "b", "session-b")
	if got := managedMockConfig(t, b); got.CollectorID != "a" || got.SessionID != "session-b" {
		t.Fatal("negative identity claim was silently repaired")
	}
	legacy := managedMockHello(t, ctx, conn, "legacy", "session-legacy", false)
	defer legacy.CloseSend()
	if err := c.SendConfig("legacy", mockConfig(1)); err == nil {
		t.Fatal("old collector received unsupported configuration")
	}
}
