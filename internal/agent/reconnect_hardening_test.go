package agent

import (
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	grpcgo "google.golang.org/grpc"
)

type phase8HandshakeThenClose struct {
	pb.UnimplementedAgentServiceServer
	hellos chan time.Time
}

func (c *phase8HandshakeThenClose) Connect(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	if _, err := s.Recv(); err != nil {
		return err
	}
	select {
	case c.hellos <- time.Now():
	default:
	}
	return s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1, ApplicationAck: true}}})
}

// This real loopback gRPC fixture establishes Hello and immediately closes the
// stream. It records the existing retry policy instead of claiming that a
// successful Hello is proof of sustained Backend service.
func TestPhase8HandshakeThenCloseReconnectIsBounded(t *testing.T) {
	running := startACKBlackhole(t)
	hellos := make(chan time.Time, 64)
	pb.RegisterAgentServiceServer(running.server, &phase8HandshakeThenClose{hellos: hellos})
	go func() { running.finished <- running.server.Serve(running.listener) }()
	c := testConfig(t, running.listener.Addr().String())
	c.Local.Enabled = false
	c.Controller.ReconnectInitial = 80 * time.Millisecond
	c.Controller.ReconnectMax = 640 * time.Millisecond
	configureDurable(&c)
	a := newTestAgent(t, c, nil)
	stop := runTestAgent(t, a)
	defer stop()
	var observed []time.Time
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
collect:
	for {
		select {
		case at := <-hellos:
			observed = append(observed, at)
		case <-timer.C:
			break collect
		}
	}
	if len(observed) < 2 || len(observed) > 27 {
		t.Fatalf("reconnect attempts do not match bounded positive backoff: %d", len(observed))
	}
	minimum, maximum := time.Second, time.Duration(0)
	for i := 1; i < len(observed); i++ {
		gap := observed[i].Sub(observed[i-1])
		minimum, maximum = min(minimum, gap), max(maximum, gap)
		if gap < c.Controller.ReconnectInitial/2-5*time.Millisecond {
			t.Fatal("Hello-close peer bypassed positive reconnect delay", gap)
		}
	}
	if s := a.queue.State(); s.Messages > c.Queue.MaxMessages || s.Bytes > c.Queue.MaxBytes {
		t.Fatal("reconnect lifecycle events grew queue beyond capacity", s)
	}
	t.Logf("Hello-then-close workload1s: %d attempts, gap=%s..%s, configured initial=%s maximum=%s; successful Hello resets existing backoff", len(observed), minimum, maximum, c.Controller.ReconnectInitial, c.Controller.ReconnectMax)
}
