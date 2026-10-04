// Package agent orchestrates collectors and the outbound controller session.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"InfraPulseAgent/internal/commands"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/sanitize"
	"InfraPulseAgent/internal/scheduler"
	"InfraPulseAgent/internal/telemetry"
	"InfraPulseAgent/internal/transport"
	"InfraPulseAgent/internal/version"
	"github.com/google/uuid"
)

type Agent struct {
	cfg          config.Config
	id           identity.Identity
	started      time.Time
	platform     platform.Platform
	transport    transport.Transport
	queue        *queue.Queue
	scheduler    *scheduler.Scheduler
	rates        *telemetry.RateTracker
	logger       *slog.Logger
	sequence     atomic.Uint64
	mu           sync.Mutex
	device       model.Device
	connected    bool
	recentErrors []string
}

type Diagnostics struct {
	AgentID           string            `json:"agent_id"`
	SessionID         string            `json:"session_id"`
	Build             model.BuildInfo   `json:"build"`
	Platform          string            `json:"platform"`
	ControllerAddress string            `json:"controller_address"`
	TLSMode           string            `json:"tls_mode"`
	Connected         bool              `json:"connected"`
	ObservedAt        time.Time         `json:"observed_at"`
	Queue             model.QueueState  `json:"queue"`
	Collectors        []scheduler.State `json:"collectors"`
	Devices           []model.Device    `json:"devices"`
	RecentErrors      []string          `json:"recent_errors"`
}

func New(ctx context.Context, cfg config.Config, p platform.Platform, t transport.Transport, logger *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("transport is required")
	}
	if len(cfg.Devices) > 0 {
		return nil, errors.New("remote device drivers are not implemented in P0; use devices: [] until P1")
	}
	if cfg.Local.Enabled && p == nil {
		return nil, errors.New("local collection enabled but platform unavailable")
	}
	id, err := identity.LoadOrCreate(ctx, cfg.Agent.StateDir)
	if err != nil {
		return nil, err
	}
	q, err := queue.New(cfg.Queue.MaxMessages, cfg.Queue.MaxBytes, cfg.Queue.MaxMessageBytes)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Agent{cfg: cfg, id: id, started: time.Now(), platform: p, transport: t, queue: q, rates: telemetry.NewRateTracker(), logger: logger.With("component", "agent", "agent_id", id.AgentID)}
	host, _ := os.Hostname()
	a.device = model.Device{ID: "local:" + id.AgentID, Name: cfg.Agent.Name, Hostname: host, Type: "unknown", OS: runtime.GOOS, Architecture: runtime.GOARCH, Driver: "none", Online: true, LastSeen: time.Now().UTC(), Location: cfg.Location}
	if cfg.Local.Enabled {
		collectCtx, cancel := context.WithTimeout(ctx, cfg.Telemetry.CollectTimeout)
		d, e := p.Identity(collectCtx)
		cancel()
		if e != nil {
			return nil, fmt.Errorf("local identity: %w", e)
		}
		a.setDevice(d)
	}
	a.scheduler = scheduler.New(cfg.Scheduler.Concurrency, a.collectorError)
	if err = a.jobs(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Agent) setDevice(d model.Device) {
	d.ID = "local:" + a.id.AgentID
	d.Location = a.cfg.Location
	if d.Name == "" {
		d.Name = a.cfg.Agent.Name
	}
	a.mu.Lock()
	a.device = d
	a.mu.Unlock()
}
func (a *Agent) deviceSnapshot() model.Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.device
	d.Capabilities = append([]model.Capability(nil), d.Capabilities...)
	return d
}

func (a *Agent) jobs() error {
	if a.cfg.Local.Enabled {
		if err := a.scheduler.Add(scheduler.Job{Name: "local.fast", Interval: a.cfg.Telemetry.FastInterval, Timeout: a.cfg.Telemetry.CollectTimeout, Collect: func(ctx context.Context) error {
			s, err := a.platform.Collect(ctx)
			if err != nil {
				return err
			}
			s.DeviceID = "local:" + a.id.AgentID
			a.rates.Apply(&s)
			return a.push(model.Envelope{Telemetry: &s})
		}}); err != nil {
			return err
		}
		if err := a.scheduler.Add(scheduler.Job{Name: "local.inventory", Interval: a.cfg.Telemetry.InventoryInterval, Timeout: a.cfg.Telemetry.CollectTimeout, Collect: func(ctx context.Context) error {
			d, err := a.platform.Identity(ctx)
			if err != nil {
				return err
			}
			a.setDevice(d)
			return a.push(model.Envelope{Inventory: &model.InventoryUpdate{ObservedAt: time.Now().UTC(), Devices: []model.Device{a.deviceSnapshot()}}})
		}}); err != nil {
			return err
		}
	}
	return a.scheduler.Add(scheduler.Job{Name: "agent.heartbeat", Interval: a.cfg.Telemetry.HeartbeatInterval, Timeout: a.cfg.Telemetry.CollectTimeout, Collect: func(ctx context.Context) error {
		h := model.Heartbeat{Timestamp: time.Now().UTC(), UptimeSeconds: time.Since(a.started).Seconds(), Build: version.Info(), Health: a.health(), Queue: a.queue.State()}
		err := a.push(model.Envelope{Heartbeat: &h})
		return errors.Join(err, a.saveDiagnostics())
	}})
}

func (a *Agent) health() string {
	for _, s := range a.scheduler.Stats() {
		if s.LastError != "" {
			return "degraded"
		}
	}
	return "ok"
}
func (a *Agent) envelope(e model.Envelope) (model.Envelope, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return e, err
	}
	e.MessageID = id.String()
	e.AgentID = a.id.AgentID
	e.SessionID = a.id.SessionID
	e.SentAt = time.Now().UTC()
	return e, nil
}
func (a *Agent) push(e model.Envelope) error {
	e, err := a.envelope(e)
	if err != nil {
		return err
	}
	return a.queue.Push((sanitize.KnownSecrets{}).Sanitize(e))
}

func (a *Agent) event(kind, severity, message string) {
	id, err := uuid.NewRandom()
	if err != nil {
		return
	}
	e := model.Event{ID: id.String(), Type: kind, DeviceID: a.deviceSnapshot().ID, Timestamp: time.Now().UTC(), Severity: severity, Message: message, Source: "agent"}
	if err = a.push(model.Envelope{Events: []model.Event{e}}); err != nil {
		a.logger.Debug("event dropped", "event_type", kind, "error", err)
	}
}

func (a *Agent) collectorError(name string, err error) {
	if errors.Is(err, queue.ErrFull) || errors.Is(err, queue.ErrMessageTooLarge) {
		a.logger.Debug("outbox dropped sample", "collector", name, "error", err)
		return
	}
	message := sanitize.Text(err.Error())
	if len(message) > 1024 {
		message = strings.Clone(message[:1024])
	}
	a.recordError(name + ": " + message)
	a.logger.Warn("collector failed", "collector", name, "error", message)
	a.event("collector.error", "warn", name+": "+message)
}
func (a *Agent) recordError(message string) {
	message = sanitize.Text(message)
	if len(message) > 1024 {
		message = strings.Clone(message[:1024])
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.recentErrors) >= 20 {
		copy(a.recentErrors, a.recentErrors[1:])
		a.recentErrors = a.recentErrors[:19]
	}
	a.recentErrors = append(a.recentErrors, message)
}

// Run cancels network IO and collectors together and bounds shutdown waiting.
func (a *Agent) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.event("agent.started", "info", "agent started")
	done := make(chan error, 2)
	go func() { done <- a.scheduler.Run(runCtx) }()
	go func() { done <- a.supervise(runCtx) }()
	var err error
	finished := 0
	select {
	case <-ctx.Done():
	case err = <-done:
		finished = 1
	}
	cancel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for finished < 2 {
		select {
		case e := <-done:
			err = errors.Join(err, e)
			finished++
		case <-timer.C:
			return errors.Join(err, errors.New("graceful shutdown exceeded 5s"))
		}
	}
	return errors.Join(err, a.saveDiagnostics())
}

func (a *Agent) supervise(ctx context.Context) error {
	backoff := transport.NewBackoff(a.cfg.Controller.ReconnectInitial, a.cfg.Controller.ReconnectMax)
	for ctx.Err() == nil {
		sessionCtx, cancel := context.WithCancel(ctx)
		watchdog := time.AfterFunc(a.cfg.Controller.ConnectTimeout, cancel)
		session, err := a.transport.Connect(sessionCtx)
		if err == nil {
			hello := model.AgentHello{AgentID: a.id.AgentID, SessionID: a.id.SessionID, Name: a.cfg.Agent.Name, Build: version.Info(), StartedAt: a.started.UTC(), Device: a.deviceSnapshot(), ProtocolVersion: 1}
			e, eErr := a.envelope(model.Envelope{Hello: &hello})
			err = eErr
			if err == nil {
				err = a.send(sessionCtx, cancel, session, e)
			}
			if err == nil {
				var ack model.ControllerMessage
				ack, err = session.Receive()
				if err == nil && (!ack.HelloAccepted || ack.ProtocolVersion != 1 || ack.Command != nil) {
					err = errors.New("controller rejected protocol handshake")
				}
			}
		}
		watchdog.Stop()
		if err == nil && sessionCtx.Err() != nil {
			err = sessionCtx.Err()
		}
		if err == nil {
			backoff.Reset()
			a.mu.Lock()
			a.connected = true
			a.mu.Unlock()
			a.logger.Info("controller connected", "controller", a.cfg.Controller.Address)
			a.event("agent.connected", "info", "controller connected")
			err = a.exchange(sessionCtx, cancel, session)
		}
		cancel()
		if session != nil {
			_ = session.Close()
		}
		a.mu.Lock()
		a.connected = false
		a.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			a.recordError("transport: " + err.Error())
		}
		delay := backoff.Next()
		a.logger.Warn("controller disconnected; reconnect scheduled", "retry_in", delay)
		a.event("agent.disconnected", "warn", "controller disconnected")
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (a *Agent) send(ctx context.Context, cancel context.CancelFunc, s transport.Session, e model.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.SentAt = time.Now().UTC()
	e.Sequence = a.sequence.Add(1)
	timer := time.AfterFunc(a.cfg.Controller.SendTimeout, cancel)
	err := s.Send(e)
	timer.Stop()
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (a *Agent) exchange(ctx context.Context, cancel context.CancelFunc, s transport.Session) error {
	received := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			message, err := s.Receive()
			if err != nil {
				received <- err
				cancel()
				return
			}
			if message.Command == nil {
				received <- errors.New("unexpected controller message after handshake")
				cancel()
				return
			}
			if ctx.Err() != nil {
				break
			}
			policy := commands.Policy{Enabled: a.cfg.Commands.Enabled, ProtectedDevices: a.cfg.Commands.ProtectedDevices, ProtectedInterfaces: a.cfg.Commands.ProtectedInterfaces, ProtectedServices: a.cfg.Commands.ProtectedServices}
			result := commands.RejectUnsupported(*message.Command, []model.Device{a.deviceSnapshot()}, policy, time.Now())
			if err = a.push(model.Envelope{CommandResult: &result}); err != nil {
				received <- err
				cancel()
				return
			}
		}
		received <- ctx.Err()
	}()
	var sendErr error
	for ctx.Err() == nil {
		e, err := a.queue.Pop(ctx)
		if err != nil {
			sendErr = err
			break
		}
		if err = a.send(ctx, cancel, s, e); err != nil {
			sendErr = err
			break
		}
	}
	cancel()
	_ = s.Close()
	receiveErr := <-received
	return errors.Join(sendErr, receiveErr)
}

func (a *Agent) Snapshot() Diagnostics {
	a.mu.Lock()
	d := Diagnostics{AgentID: a.id.AgentID, SessionID: a.id.SessionID, Build: version.Info(), Platform: runtime.GOOS + "/" + runtime.GOARCH, ControllerAddress: a.cfg.Controller.Address, TLSMode: a.cfg.Security.Mode, Connected: a.connected, ObservedAt: time.Now().UTC(), RecentErrors: append([]string(nil), a.recentErrors...)}
	a.mu.Unlock()
	d.Queue = a.queue.State()
	d.Collectors = a.scheduler.Stats()
	for i := range d.Collectors {
		d.Collectors[i].LastError = sanitize.Text(d.Collectors[i].LastError)
		if len(d.Collectors[i].LastError) > 1024 {
			d.Collectors[i].LastError = d.Collectors[i].LastError[:1024]
		}
	}
	d.Devices = []model.Device{a.deviceSnapshot()}
	return d
}

func (a *Agent) saveDiagnostics() error {
	data, err := json.MarshalIndent(a.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(a.cfg.Agent.StateDir, ".diagnostics-*")
	if err != nil {
		return fmt.Errorf("create diagnostics snapshot: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(a.cfg.Agent.StateDir, "diagnostics.json"))
}
