package agent

import (
	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	grpcgo "google.golang.org/grpc"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type noACKController struct {
	pb.UnimplementedAgentServiceServer
	observed chan model.Envelope
}

func (c *noACKController) Connect(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	w, err := s.Recv()
	if err != nil {
		return err
	}
	hello, err := transportgrpc.DecodeEnvelope(w)
	if err != nil {
		return err
	}
	if !hello.Hello.ApplicationACK {
		return io.ErrUnexpectedEOF
	}
	if err = s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1, ApplicationAck: true}}}); err != nil {
		return err
	}
	for {
		w, err = s.Recv()
		if err != nil {
			return err
		}
		e, err := transportgrpc.DecodeEnvelope(w)
		if err != nil {
			return err
		}
		select {
		case c.observed <- e:
		default:
		}
	}
}
func startACKBlackhole(t *testing.T) *tcpController {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpcgo.NewServer()
	running := &tcpController{server: s, listener: ln, finished: make(chan error, 1)}
	t.Cleanup(running.stop)
	return running
}
func configureDurable(c *config.Config) {
	c.Queue.Mode = "durable"
	c.Queue.MaxMessages = 64
	c.Queue.BatchDelay = 2 * time.Millisecond
	c.Queue.ACKTimeout = time.Second
}
func TestDurableAgentRestartControllerACKAndStableReplay(t *testing.T) {
	blackhole := startACKBlackhole(t)
	observed := make(chan model.Envelope, 512)
	pb.RegisterAgentServiceServer(blackhole.server, &noACKController{observed: observed})
	go func() { blackhole.finished <- blackhole.server.Serve(blackhole.listener) }()
	c := testConfig(t, blackhole.listener.Addr().String())
	configureDurable(&c)
	c.Local.Enabled = false
	a := newTestAgent(t, c, nil)
	at := time.Now().UTC()
	marker, err := a.envelope(model.Envelope{CommandResult: &model.CommandResult{CommandID: "already-executed", Status: "success", StartedAt: at, FinishedAt: at}})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.queue.Push(marker); err != nil {
		t.Fatal(err)
	}
	stop := runTestAgent(t, a)
	first := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == marker.MessageID })
	if !a.queue.Contains(marker.MessageID) {
		t.Fatal("Send deleted unACKed record")
	}
	stop()
	address := blackhole.listener.Addr().String()
	blackhole.stop()
	replayed := make(chan model.Envelope, 512)
	_ = startTCPController(t, address, mockcontroller.New(io.Discard, nil, observeChannel(replayed)))
	b := newTestAgent(t, c, nil)
	stopB := runTestAgent(t, b)
	defer stopB()
	second := waitEnvelope(t, replayed, func(e model.Envelope) bool { return e.MessageID == marker.MessageID })
	if first.AgentID != second.AgentID || first.SessionID == second.SessionID || second.CommandResult.CommandID != "already-executed" {
		t.Fatal("replay identity changed")
	}
	awaitP2(t, func() bool { return !b.queue.Contains(marker.MessageID) })
}
func TestDurableControllerRestartReplaysWithoutAgentRestart(t *testing.T) {
	blackhole := startACKBlackhole(t)
	observed := make(chan model.Envelope, 512)
	pb.RegisterAgentServiceServer(blackhole.server, &noACKController{observed: observed})
	go func() { blackhole.finished <- blackhole.server.Serve(blackhole.listener) }()
	c := testConfig(t, blackhole.listener.Addr().String())
	configureDurable(&c)
	a := newTestAgent(t, c, newFixturePlatform())
	marker, _ := a.envelope(model.Envelope{CommandResult: &model.CommandResult{CommandID: "pending-result", Status: "rejected"}})
	if err := a.queue.Push(marker); err != nil {
		t.Fatal(err)
	}
	stop := runTestAgent(t, a)
	defer stop()
	first := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == marker.MessageID })
	address := blackhole.listener.Addr().String()
	blackhole.stop()
	replayed := make(chan model.Envelope, 512)
	_ = startTCPController(t, address, mockcontroller.New(io.Discard, nil, observeChannel(replayed)))
	second := waitEnvelope(t, replayed, func(e model.Envelope) bool { return e.MessageID == marker.MessageID })
	if first.SessionID != second.SessionID {
		t.Fatal("reconnect changed runtime session")
	}
	awaitP2(t, func() bool { return !a.queue.Contains(marker.MessageID) })
}
func TestDurableLegacyFallbackAndCorruptionFailClosed(t *testing.T) {
	for _, fallback := range []string{"reject", "send"} {
		t.Run(fallback, func(t *testing.T) {
			observed := make(chan model.Envelope, 512)
			m := mockcontroller.New(io.Discard, nil, observeChannel(observed))
			m.DisableACK = true
			s := startTCPController(t, "127.0.0.1:0", m)
			c := testConfig(t, s.listener.Addr().String())
			configureDurable(&c)
			c.Queue.ACKFallback = fallback
			a := newTestAgent(t, c, newFixturePlatform())
			marker, _ := a.envelope(model.Envelope{CommandResult: &model.CommandResult{CommandID: "legacy", Status: "rejected"}})
			_ = a.queue.Push(marker)
			stop := runTestAgent(t, a)
			defer stop()
			if fallback == "send" {
				_ = waitEnvelope(t, observed, func(e model.Envelope) bool { return e.MessageID == marker.MessageID })
				awaitP2(t, func() bool { return !a.queue.Contains(marker.MessageID) })
				if !a.Snapshot().ACKFallback {
					t.Fatal("fallback invisible")
				}
			} else {
				_ = waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Hello != nil })
				awaitP2(t, func() bool { return len(a.Snapshot().RecentErrors) > 0 })
				if !a.queue.Contains(marker.MessageID) {
					t.Fatal("legacy rejection lost durable result")
				}
			}
		})
	}
	c := testConfig(t, "127.0.0.1:9443")
	configureDurable(&c)
	c.Commands.Enabled = true
	dir := filepath.Join(c.Agent.StateDir, "outbox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "outbox.db")
	if err := os.WriteFile(path, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, c, newFixturePlatform())
	if a.queue.IsDurable() || a.cfg.Commands.Enabled || a.Snapshot().Queue.StorageError == "" {
		t.Fatal("storage failure did not disable mutations")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "corrupted" {
		t.Fatal("corrupted state destroyed")
	}
}
