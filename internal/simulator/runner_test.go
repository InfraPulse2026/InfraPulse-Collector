package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/transport"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
)

func TestSimulatorMockControllerGRPCE2E(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverOptions, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: listener.Addr().String(), Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(serverOptions...)
	observed := make(chan model.Envelope, 512)
	// Deliberately consume slower than generation. Immediate Session.Close
	// after queue Pop/Send previously canceled low-priority buffered frames.
	pb.RegisterAgentServiceServer(server, mockcontroller.New(io.Discard, nil, func(e model.Envelope) { time.Sleep(5 * time.Millisecond); observed <- e }))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer listener.Close()
	client, err := transportgrpc.New(transportgrpc.Options{Address: listener.Addr().String(), Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	generator, _ := NewGenerator(Options{Scenario: "all", Duration: 90 * time.Millisecond, Interval: 10 * time.Millisecond, Seed: 42, Devices: 2})
	runner, err := NewRunner(generator, identity.Identity{AgentID: "simulated-test-agent", SessionID: "simulated-test-session"}, client, RuntimeOptions{DrainTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err = runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	last := uint64(0)
	for len(observed) > 0 {
		e := <-observed
		assertMarkers(t, e)
		if e.Sequence <= last {
			t.Fatal("sequence", e.Sequence, last)
		}
		last = e.Sequence
		if e.Hello != nil {
			seen["hello"] = true
		}
		if e.Inventory != nil {
			seen["inventory"] = true
		}
		if e.Telemetry != nil {
			seen["telemetry"] = true
		}
		if e.Topology != nil {
			seen["topology"] = true
		}
		if e.Logs != nil {
			seen["logs"] = true
			for _, record := range e.Logs.Records {
				if record.Audit != nil {
					seen["audit"] = true
				}
			}
		}
		if e.Telephony != nil {
			seen["telephony"] = true
		}
		if len(e.Events) > 0 {
			seen["events"] = true
		}
	}
	expected := 1 // handshake hello
	for step := uint64(0); step < generator.Steps(); step++ {
		expected += len(generator.Tick(step))
	}
	if int(last) != expected {
		t.Fatalf("graceful stream truncated: received=%d expected=%d", last, expected)
	}
	for _, kind := range []string{"hello", "inventory", "telemetry", "topology", "logs", "audit", "telephony", "events"} {
		if !seen[kind] {
			t.Fatal("missing", kind)
		}
	}
	if runner.QueueState().Messages != 0 {
		t.Fatal(runner.QueueState())
	}
}

type fakeTransport struct {
	connects  atomic.Int32
	failFirst bool
	sent      atomic.Int32
}

func (t *fakeTransport) Connect(ctx context.Context) (transport.Session, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	number := t.connects.Add(1)
	return &fakeSession{closed: make(chan struct{}), fail: t.failFirst && number == 1, transport: t}, nil
}

type fakeSession struct {
	closed    chan struct{}
	once      sync.Once
	ack       atomic.Bool
	fail      bool
	transport *fakeTransport
}

func (s *fakeSession) Send(e model.Envelope) error {
	select {
	case <-s.closed:
		return io.EOF
	default:
	}
	if e.Hello == nil && s.fail {
		return errors.New("fixture send failure")
	}
	s.transport.sent.Add(1)
	return nil
}
func (s *fakeSession) Receive() (model.ControllerMessage, error) {
	if s.ack.CompareAndSwap(false, true) {
		return model.ControllerMessage{HelloAccepted: true, ProtocolVersion: 1}, nil
	}
	<-s.closed
	return model.ControllerMessage{}, io.EOF
}
func (s *fakeSession) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func TestReconnectAndCancellation(t *testing.T) {
	g, _ := NewGenerator(Options{Duration: 50 * time.Millisecond, Interval: 10 * time.Millisecond, Devices: 1})
	tpt := &fakeTransport{failFirst: true}
	r, err := NewRunner(g, identity.Identity{AgentID: "simulated", SessionID: "session"}, tpt, RuntimeOptions{ReconnectInitial: 5 * time.Millisecond, ReconnectMax: 10 * time.Millisecond, DrainTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Run(context.Background()); err != nil || tpt.connects.Load() < 2 || tpt.sent.Load() < 3 {
		t.Fatal(err, tpt.connects.Load(), tpt.sent.Load())
	}
	g, _ = NewGenerator(Options{Duration: time.Hour, Devices: 1})
	r, _ = NewRunner(g, identity.Identity{AgentID: "simulated", SessionID: "session"}, &fakeTransport{}, RuntimeOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()
	if err = r.Run(ctx); err != nil || time.Since(started) > time.Second {
		t.Fatal(err, time.Since(started))
	}
}

func TestJSONOutputAndCancellation(t *testing.T) {
	g, _ := NewGenerator(Options{Scenario: "all", Duration: 20 * time.Millisecond, Interval: 10 * time.Millisecond, Devices: 1})
	var output bytes.Buffer
	if err := WriteJSON(context.Background(), g, identity.Identity{AgentID: "simulated", SessionID: "session"}, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	count := 0
	for {
		var e model.Envelope
		err := decoder.Decode(&e)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		assertMarkers(t, e)
		count++
		if e.Sequence != uint64(count) {
			t.Fatal(e.Sequence, count)
		}
	}
	if count < 5 {
		t.Fatal(count)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteJSON(ctx, g, identity.Identity{AgentID: "simulated", SessionID: "session"}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type silentHalfCloseTransport struct{}

func (silentHalfCloseTransport) Connect(context.Context) (transport.Session, error) {
	return &silentHalfCloseSession{fakeSession: &fakeSession{closed: make(chan struct{}), transport: &fakeTransport{}}}, nil
}

type silentHalfCloseSession struct{ *fakeSession }

func (*silentHalfCloseSession) CloseSend() error { return nil }

func TestGracefulHalfCloseDoesNotWaitForeverForPeerEOF(t *testing.T) {
	g, _ := NewGenerator(Options{Duration: 10 * time.Millisecond, Interval: 10 * time.Millisecond, Devices: 1})
	r, err := NewRunner(g, identity.Identity{AgentID: "simulated", SessionID: "session"}, silentHalfCloseTransport{}, RuntimeOptions{DrainTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = r.Run(context.Background())
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("unbounded peer drain", err, time.Since(started))
	}
}
