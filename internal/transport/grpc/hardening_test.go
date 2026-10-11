package grpc

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestPhase8HalfOpenHTTP2CancellationReleasesSockets(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var opened, closed atomic.Int64
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			opened.Add(1)
			workers.Go(func() {
				defer closed.Add(1)
				defer connection.Close()
				_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
				// Accept the connection but never send the HTTP/2 server preface.
				// Cancellation must close this half-open socket.
				_, _ = io.Copy(io.Discard, connection)
			})
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-acceptDone; workers.Wait() })
	client, err := New(Options{Address: listener.Addr().String(), Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	const cycles = 20
	started := time.Now()
	for range cycles {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		s, err := client.Connect(ctx)
		if s != nil {
			_ = s.Close()
		}
		cancel()
		if err == nil {
			t.Fatal("half-open HTTP/2 session unexpectedly connected")
		}
	}
	deadline := time.Now().Add(time.Second)
	for closed.Load() < opened.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if opened.Load() != cycles || closed.Load() != opened.Load() {
		t.Fatalf("half-open sockets did not stabilize: opened=%d closed=%d", opened.Load(), closed.Load())
	}
	t.Logf("%d half-open connection cancellations in %s; sockets=%d opened/%d closed", cycles, time.Since(started), opened.Load(), closed.Load())
}

func TestPhase8CloseUnblocksConcurrentSendAndReceive(t *testing.T) {
	connected := make(chan struct{}, 1)
	address := startController(t, Options{Mode: "development"}, func(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		connected <- struct{}{}
		<-s.Context().Done()
		return s.Context().Err()
	})
	c, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Send(helloEnvelope()); err != nil {
		t.Fatal(err)
	}
	<-connected
	e := testEnvelope()
	e.Events = []model.Event{{ID: "fixture", Timestamp: testTime, Source: "simulated", Message: strings.Repeat("x", 1<<20)}}
	completed := make(chan error, 2)
	var sent atomic.Int64
	go func() {
		for range 64 {
			if err := s.Send(e); err != nil {
				completed <- err
				return
			}
			sent.Add(1)
		}
		completed <- errors.New("all sends unexpectedly completed without receiver flow control")
	}()
	go func() { _, err := s.Receive(); completed <- err }()
	// A stalled peer never consumes the second envelope; repeated large sends
	// fill the bounded HTTP/2 window. Both directions must remain interruptible.
	time.Sleep(30 * time.Millisecond)
	started := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case err := <-completed:
			if err == nil {
				t.Fatal("stream operation succeeded after close")
			}
		case <-time.After(time.Second):
			t.Fatal("Close did not cancel concurrent stream operation")
		}
	}
	if sent.Load() == 64 {
		t.Fatal("fixture did not produce sender backpressure")
	}
	t.Logf("stalled peer: %d large sends completed before cancellation; Close+Send/Recv=%s", sent.Load(), time.Since(started))
}

func TestPhase8RepeatedStreamLifecyclesStabilize(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var opened, closed atomic.Int64
	address := startController(t, Options{Mode: "development"}, func(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		opened.Add(1)
		defer closed.Add(1)
		if _, err := s.Recv(); err != nil {
			return err
		}
		if err := s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1}}}); err != nil {
			return err
		}
		<-s.Context().Done()
		return s.Context().Err()
	})
	c, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		s, err := c.Connect(ctx)
		if err == nil {
			err = s.Send(helloEnvelope())
			if err == nil {
				_, err = s.Receive()
			}
			_ = s.Close()
		}
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for (closed.Load() != 50 || runtime.NumGoroutine() > baseline+8) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if opened.Load() != 50 || closed.Load() != 50 || runtime.NumGoroutine() > baseline+8 {
		t.Fatalf("stream resources did not stabilize: opened=%d closed=%d goroutines=%d->%d", opened.Load(), closed.Load(), baseline, runtime.NumGoroutine())
	}
	t.Logf("50 real TCP/gRPC stream lifecycles: opened=%d closed=%d goroutines=%d->%d (shared server remains running)", opened.Load(), closed.Load(), baseline, runtime.NumGoroutine())
}

func FuzzPhase8AgentEnvelopeDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff})
	for _, e := range []model.Envelope{helloEnvelope(), {MessageID: "m", AgentID: "a", SessionID: "s", SentAt: testTime, Telemetry: func() *model.TelemetrySample { s := testSample(); s.Source = "simulated"; return &s }()}} {
		p, err := Encode(e)
		if err != nil {
			f.Fatal(err)
		}
		data, err := proto.Marshal(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		var wire pb.AgentEnvelope
		if proto.Unmarshal(data, &wire) != nil {
			return
		}
		e, err := DecodeEnvelope(&wire)
		if err != nil {
			return
		}
		reencoded, err := Encode(e)
		if err != nil {
			t.Fatal("accepted normalized envelope cannot encode", err)
		}
		if _, err := proto.Marshal(reencoded); err != nil {
			t.Fatal("accepted normalized envelope cannot serialize", err)
		}
		if reencoded.MessageId != wire.MessageId || reencoded.AgentId != wire.AgentId || reencoded.SessionId != wire.SessionId {
			t.Fatal("decode/encode changed delivery identity")
		}
		if e.Telemetry != nil && reencoded.GetTelemetry().Samples[0].Source != wire.GetTelemetry().Samples[0].Source {
			t.Fatal("decode/encode changed telemetry provenance")
		}
	})
}
