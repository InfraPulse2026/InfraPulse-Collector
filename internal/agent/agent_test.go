package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	grpcgo "google.golang.org/grpc"
)

// Synthetic values belong only to this test fixture; production collectors are
// never replaced by a successful synthetic fallback.
type fixturePlatform struct {
	started    time.Time
	samples    atomic.Uint64
	active     atomic.Int32
	metricName string
}

func newFixturePlatform() *fixturePlatform { return &fixturePlatform{started: time.Now()} }

func (p *fixturePlatform) Capabilities() []model.Capability {
	return []model.Capability{{Name: "system.read", Supported: true}, {Name: "inventory.read", Supported: true}, {Name: "interfaces.read", Supported: true}}
}

func (p *fixturePlatform) Identity(ctx context.Context) (model.Device, error) {
	if err := ctx.Err(); err != nil {
		return model.Device{}, err
	}
	return model.Device{Hostname: "fixture-host", Vendor: "fixture", Model: "fixture-server", Type: "server", OS: "fixture-os", Architecture: "amd64", Driver: "fixture", BootID: "fixture-boot", UptimeSeconds: 100 + time.Since(p.started).Seconds(), Online: true, LastSeen: time.Now().UTC(), Capabilities: p.Capabilities()}, nil
}

func (p *fixturePlatform) Collect(ctx context.Context) (model.TelemetrySample, error) {
	if err := ctx.Err(); err != nil {
		return model.TelemetrySample{}, err
	}
	p.active.Add(1)
	defer p.active.Add(-1)
	n := p.samples.Add(1) * 120
	usage := 25.0
	at := time.Now().UTC()
	s := model.TelemetrySample{ObservedAt: at, CollectedAt: at, Source: "simulated", BootID: "fixture-boot", UptimeSeconds: 100 + time.Since(p.started).Seconds(),
		CPU:        &model.CPUStats{LogicalCPUs: 2, UsagePercent: &usage, Load1: .25, Load5: .2, Load15: .1},
		Memory:     &model.MemoryStats{TotalBytes: 1024, UsedBytes: 256, AvailableBytes: 768, FreeBytes: 512},
		Interfaces: []model.Interface{{ID: "fixture-interface", Index: 1, Name: "test0", MAC: "02:00:00:00:00:01", MTU: 1500, AdminUp: true, OperState: "up", Addresses: []string{"192.0.2.10/24"}, Counters: model.Counters{RXBytes: n, TXBytes: n * 2, RXPackets: n / 10, TXPackets: n / 5, RXErrors: n / 40, TXErrors: n / 60, RXDropped: n / 30, TXDropped: n / 60}}},
	}
	if p.metricName != "" {
		s.Metrics = []model.Metric{{Name: p.metricName, Value: 1, Unit: "count"}}
	}
	return s, nil
}

type tcpController struct {
	server   *grpcgo.Server
	listener net.Listener
	finished chan error
}

func startTCPController(t *testing.T, address string, controller *mockcontroller.Controller) *tcpController {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	options, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: listener.Addr().String(), Mode: "development"})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := grpcgo.NewServer(options...)
	pb.RegisterAgentServiceServer(server, controller)
	running := &tcpController{server: server, listener: listener, finished: make(chan error, 1)}
	go func() { running.finished <- server.Serve(listener) }()
	t.Cleanup(func() { running.stop() })
	return running
}

func (s *tcpController) stop() { s.server.Stop(); _ = s.listener.Close() }

func testConfig(t *testing.T, address string) config.Config {
	t.Helper()
	c := config.Defaults()
	c.Agent.StateDir = t.TempDir()
	c.Agent.Name = "e2e-agent"
	c.Controller.Address = address
	c.Controller.ConnectTimeout = 200 * time.Millisecond
	c.Controller.SendTimeout = 200 * time.Millisecond
	c.Controller.ReconnectInitial = 10 * time.Millisecond
	c.Controller.ReconnectMax = 40 * time.Millisecond
	c.Security.Mode = "development"
	c.Telemetry.FastInterval = 20 * time.Millisecond
	c.Telemetry.InventoryInterval = 40 * time.Millisecond
	c.Telemetry.HeartbeatInterval = 30 * time.Millisecond
	c.Telemetry.CollectTimeout = 100 * time.Millisecond
	c.Queue.MaxMessages = 16
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestAgent(t *testing.T, c config.Config, platform *fixturePlatform) *Agent {
	t.Helper()
	transport, err := transportgrpc.New(transportgrpc.Options{Address: c.Controller.Address, Mode: c.Security.Mode, MaxMessageBytes: c.Queue.MaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), c, platform, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func observeChannel(ch chan<- model.Envelope) func(model.Envelope) {
	return func(e model.Envelope) {
		select {
		case ch <- e:
		default:
		}
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true within 3s")
		case <-ticker.C:
		}
	}
}

func waitEnvelope(t *testing.T, ch <-chan model.Envelope, accept func(model.Envelope) bool) model.Envelope {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case e := <-ch:
			if accept(e) {
				return e
			}
		case <-deadline.C:
			t.Fatal("expected controller message did not arrive within 3s")
		}
	}
}

func runTestAgent(t *testing.T, a *Agent) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- a.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Errorf("agent graceful shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("agent did not shut down within 3s")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestAgentTCPStreamingTypedRejectionAndGracefulShutdown(t *testing.T) {
	observed := make(chan model.Envelope, 256)
	command := &model.Command{TargetDeviceID: "auto-local", Timeout: 100 * time.Millisecond, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "fixture-interface", Operation: "disable"}}
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, command, observeChannel(observed)))
	c := testConfig(t, controller.listener.Addr().String())
	platform := newFixturePlatform()
	a := newTestAgent(t, c, platform)
	stop := runTestAgent(t, a)
	hello := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Hello != nil })
	if hello.AgentID != a.Snapshot().AgentID || hello.Hello.Device.Hostname != "fixture-host" || hello.Hello.ProtocolVersion != 1 {
		t.Fatalf("incorrect hello: %+v", hello.Hello)
	}
	var previous *model.TelemetrySample
	var inventory, heartbeat, rates, result bool
	lastSequence := hello.Sequence
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.AgentID != hello.AgentID || e.SessionID != hello.SessionID || e.Sequence <= lastSequence {
			t.Fatalf("stream identity/sequence changed: %+v", e)
		}
		lastSequence = e.Sequence
		if e.Inventory != nil {
			inventory = true
			if len(e.Inventory.Devices) != 1 || e.Inventory.Devices[0].ID != hello.Hello.Device.ID {
				t.Fatal("inventory target identity is incorrect")
			}
		}
		if e.Heartbeat != nil {
			heartbeat = true
		}
		if s := e.Telemetry; s != nil {
			if s.DeviceID != hello.Hello.Device.ID || s.Source != "simulated" || s.CPU == nil || s.Memory == nil || len(s.Interfaces) != 1 {
				t.Fatalf("wrong normalized telemetry: %+v", s)
			}
			if previous == nil && s.Interfaces[0].Counters.RXBytes == 120 && s.Interfaces[0].Rates != nil {
				t.Fatal("first sample contains fabricated rates")
			}
			if previous != nil && s.Interfaces[0].Rates != nil {
				// Rates use adjacent collected samples. Queue eviction may omit a
				// sample from the wire, so compare adjacent received samples only
				// when the fixture counter proves no sample was skipped.
				delta := s.Interfaces[0].Counters.RXBytes - previous.Interfaces[0].Counters.RXBytes
				if delta == 120 {
					want := float64(delta) / s.ObservedAt.Sub(previous.ObservedAt).Seconds()
					if math.Abs(s.Interfaces[0].Rates.RXBytesPerSecond-want) > 1e-6 {
						t.Fatalf("wrong actual-elapsed traffic rate: got %f want %f", s.Interfaces[0].Rates.RXBytesPerSecond, want)
					}
					rates = true
				}
			}
			previous = s
		}
		if e.CommandResult != nil {
			result = true
			if e.CommandResult.Status != "rejected" || e.CommandResult.ErrorCode != "policy_denied" || e.CommandResult.Changed {
				t.Fatalf("default-deny command was not rejected: %+v", e.CommandResult)
			}
		}
		return inventory && heartbeat && rates && result
	})
	stop()
	if a.Snapshot().Connected || platform.active.Load() != 0 {
		t.Fatal("agent left connection/collection active after shutdown")
	}
	for _, state := range a.Snapshot().Collectors {
		if state.Running {
			t.Fatalf("collector still running: %+v", state)
		}
	}
	data, err := os.ReadFile(filepath.Join(c.Agent.StateDir, "diagnostics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Diagnostics
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.AgentID != hello.AgentID {
		t.Fatalf("wrong persisted diagnostics: %+v %v", persisted, err)
	}
}

func TestAgentReconnectRetainsPersistentIdentityAndSession(t *testing.T) {
	firstObserved := make(chan model.Envelope, 128)
	first := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, nil, observeChannel(firstObserved)))
	address := first.listener.Addr().String()
	c := testConfig(t, address)
	platform := newFixturePlatform()
	a := newTestAgent(t, c, platform)
	stop := runTestAgent(t, a)
	original := waitEnvelope(t, firstObserved, func(e model.Envelope) bool { return e.Hello != nil })
	waitEnvelope(t, firstObserved, func(e model.Envelope) bool { return e.Telemetry != nil })
	first.stop()
	before := platform.samples.Load()
	eventually(t, func() bool { return !a.Snapshot().Connected && platform.samples.Load() >= before+3 })
	secondObserved := make(chan model.Envelope, 128)
	second := startTCPController(t, address, mockcontroller.New(nil, nil, observeChannel(secondObserved)))
	_ = second
	reconnected := waitEnvelope(t, secondObserved, func(e model.Envelope) bool { return e.Hello != nil })
	if reconnected.AgentID != original.AgentID || reconnected.SessionID != original.SessionID || !reconnected.Hello.StartedAt.Equal(original.Hello.StartedAt) || reconnected.Sequence <= original.Sequence {
		t.Fatalf("identity/session changed after reconnect: before=%+v after=%+v", original, reconnected)
	}
	waitEnvelope(t, secondObserved, func(e model.Envelope) bool { return e.Telemetry != nil })
	stop()
	restarted := newTestAgent(t, c, newFixturePlatform())
	if restarted.Snapshot().AgentID != original.AgentID || restarted.Snapshot().SessionID == original.SessionID {
		t.Fatal("restart did not retain agent ID and replace session ID")
	}
}

func TestAgentOfflineQueueIsBoundedAndCancellationIsGraceful(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, address)
	c.Queue = config.Queue{MaxMessages: 4, MaxBytes: 4096, MaxMessageBytes: 2048}
	platform := newFixturePlatform()
	a := newTestAgent(t, c, platform)
	stop := runTestAgent(t, a)
	eventually(t, func() bool { return a.Snapshot().Queue.DroppedTelemetry >= 3 })
	snapshot := a.Snapshot()
	if snapshot.Connected || snapshot.Queue.Messages > c.Queue.MaxMessages || snapshot.Queue.Bytes > c.Queue.MaxBytes || platform.samples.Load() < 3 {
		t.Fatalf("offline collector/queue is incorrect: %+v", snapshot)
	}
	stop()
	if platform.active.Load() != 0 {
		t.Fatal("offline shutdown left collection active")
	}
}

func TestAgentBackpressureCancelsSessionAndReconnects(t *testing.T) {
	hellos := make(chan model.Envelope, 16)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	// Blocking is intentional here to model a controller that stops reading.
	// Cleanup releases the callback before stopping the server, avoiding a test
	// goroutine that outlives the stalled-reader scenario.
	observe := func(e model.Envelope) {
		if e.Hello != nil {
			select {
			case hellos <- e:
			default:
			}
		}
		if e.Telemetry != nil {
			blockOnce.Do(func() { close(blocked); <-release })
		}
	}
	controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, nil, observe))
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	c := testConfig(t, controller.listener.Addr().String())
	c.Telemetry.FastInterval = 10 * time.Millisecond
	platform := newFixturePlatform()
	platform.metricName = strings.Repeat("load-payload", 24<<10)
	a := newTestAgent(t, c, platform)
	stop := runTestAgent(t, a)
	original := waitEnvelope(t, hellos, func(e model.Envelope) bool { return e.Hello != nil })
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not stop reading")
	}
	reconnected := waitEnvelope(t, hellos, func(e model.Envelope) bool { return e.Hello != nil && e.Sequence > original.Sequence })
	if reconnected.AgentID != original.AgentID || reconnected.SessionID != original.SessionID {
		t.Fatal("backpressure reconnect changed identity")
	}
	snapshot := a.Snapshot()
	if snapshot.Queue.Messages > c.Queue.MaxMessages || snapshot.Queue.Bytes > c.Queue.MaxBytes {
		t.Fatalf("backpressure exceeded queue bound: %+v", snapshot.Queue)
	}
	releaseOnce.Do(func() { close(release) })
	stop()
}
