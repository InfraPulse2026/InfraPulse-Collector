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
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/logs"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/pki"
	"InfraPulseAgent/internal/platform"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/remote"
	"InfraPulseAgent/internal/sanitize"
	"InfraPulseAgent/internal/scheduler"
	"InfraPulseAgent/internal/telemetry"
	"InfraPulseAgent/internal/telephony"
	"InfraPulseAgent/internal/transport"
	"InfraPulseAgent/internal/version"
	"github.com/google/uuid"
)

type Agent struct {
	commandStore *commands.Store
	certificates *pki.Manager
	activeCancel context.CancelFunc
	storageError string
	ackFallback  bool
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
	remote       *remote.Registry
	commands     *commands.Deduplicator
	logs         *logs.Pipeline
	logScheduler *scheduler.Scheduler
	sipScheduler *scheduler.Scheduler
	sip          []*telephony.Collector
}

type Diagnostics struct {
	SNMP              []SNMPSecurityStatus    `json:"snmp,omitempty"`
	CommandState      *commands.StoreHealth   `json:"command_state,omitempty"`
	Certificate       *pki.Status             `json:"certificate,omitempty"`
	ACKFallback       bool                    `json:"ack_fallback,omitempty"`
	AgentID           string                  `json:"agent_id"`
	SessionID         string                  `json:"session_id"`
	Build             model.BuildInfo         `json:"build"`
	Platform          string                  `json:"platform"`
	ControllerAddress string                  `json:"controller_address"`
	TLSMode           string                  `json:"tls_mode"`
	Connected         bool                    `json:"connected"`
	ObservedAt        time.Time               `json:"observed_at"`
	Queue             model.QueueState        `json:"queue"`
	Collectors        []scheduler.State       `json:"collectors"`
	Devices           []model.Device          `json:"devices"`
	RecentErrors      []string                `json:"recent_errors"`
	RemoteDevices     []remote.State          `json:"remote_devices,omitempty"`
	LogSources        []logs.State            `json:"log_sources,omitempty"`
	Telephony         []model.TelephonyHealth `json:"telephony,omitempty"`
}

func New(ctx context.Context, cfg config.Config, p platform.Platform, t transport.Transport, logger *slog.Logger) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("transport is required")
	}
	if cfg.Local.Enabled && p == nil {
		return nil, errors.New("local collection enabled but platform unavailable")
	}
	id, err := identity.LoadOrCreate(ctx, cfg.Agent.StateDir)
	if err != nil {
		return nil, err
	}
	var q *queue.Queue
	var storageError string
	if cfg.Queue.WithDefaults().Mode == "durable" {
		q, err = queue.NewDurable(cfg.Queue.DurableOptions(cfg.Agent.StateDir))
		if err != nil {
			storageError = "durable_outbox_unavailable"
			cfg.Commands.Enabled = false
			q, err = queue.New(cfg.Queue.MaxMessages, cfg.Queue.MaxBytes, cfg.Queue.MaxMessageBytes)
		}
	} else {
		q, err = queue.New(cfg.Queue.MaxMessages, cfg.Queue.MaxBytes, cfg.Queue.MaxMessageBytes)
	}
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Agent{cfg: cfg, id: id, started: time.Now(), platform: p, transport: t, queue: q, storageError: storageError, rates: telemetry.NewRateTracker(), logger: logger.With("component", "agent", "agent_id", id.AgentID)}
	initialized := false
	defer func() {
		if !initialized {
			_ = q.Close()
		}
	}()
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
	a.commands = commands.NewDeduplicator(256)
	a.remote, err = remote.New(cfg, RemoteFactory(cfg, credentials.NewEnvironment(cfg.Credentials)), a.push)
	if err != nil {
		return nil, err
	}
	if err = a.jobs(); err != nil {
		_ = a.remote.Close()
		return nil, err
	}
	if err = a.p2Jobs(); err != nil {
		_ = a.remote.Close()
		if a.logs != nil {
			_ = a.logs.Close()
		}
		return nil, err
	}
	if e := a.initCommandStore(); e != nil {
		a.recordError("commands.storage: " + e.Error())
		a.logger.Warn("durable command storage unavailable; mutations blocked")
	}
	if storageError != "" {
		a.recordError(storageError)
		a.event("durable.outbox.degraded", "error", storageError)
	}
	initialized = true
	return a, nil
}

func (a *Agent) setDevice(d model.Device) {
	d = model.CloneDevice(d)
	d.ID = "local:" + a.id.AgentID
	d.Location = a.cfg.Location
	if d.Name == "" {
		d.Name = a.cfg.Agent.Name
	}
	a.mu.Lock()
	a.serviceTransitions(a.device, d)
	d.Capabilities = append(d.Capabilities, a.p2CapabilitiesLocked()...)
	a.device = d
	a.mu.Unlock()
}
func (a *Agent) deviceSnapshot() model.Device {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.device
	return model.CloneDevice(d)
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
		h := model.Heartbeat{Timestamp: time.Now().UTC(), UptimeSeconds: time.Since(a.started).Seconds(), Build: version.Info(), Health: a.health(), Queue: a.queue.State(), RemoteDeviceCount: len(a.cfg.Devices)}
		if a.storageError != "" {
			h.Queue.StorageError = a.storageError
		}
		if a.certificates != nil {
			s := a.certificates.Status()
			h.Certificate = &model.CertificateStatus{ExpiresAt: s.ExpiresAt, RemainingSeconds: s.RemainingSeconds, EnrollmentState: s.EnrollmentState, RenewalState: s.RenewalState, LastSuccessfulRenewal: s.LastSuccessfulRenewal, LastError: s.LastError}
		}
		err := a.push(model.Envelope{Heartbeat: &h})
		return errors.Join(err, a.saveDiagnostics())
	}})
}

func (a *Agent) health() string {
	if a.certificates != nil {
		s := a.certificates.Status()
		if s.RemainingSeconds <= 0 || s.RenewalState == "failed" {
			return "degraded"
		}
	}
	if a.storageError != "" || a.queue.State().StorageError != "" || a.commandStore != nil && !a.commandStore.Health().Available {
		return "degraded"
	}
	for _, c := range a.sip {
		if state := c.State().State; state == "down" || state == "degraded" {
			return "degraded"
		}
	}
	for _, s := range append(a.logScheduler.Stats(), a.sipScheduler.Stats()...) {
		if s.LastError != "" {
			return "degraded"
		}
	}
	for _, s := range a.scheduler.Stats() {
		if s.LastError != "" {
			return "degraded"
		}
	}
	for _, s := range a.remote.Stats() {
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
	if e.MessageID == "" {
		e.MessageID = id.String()
	}
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
	return a.queue.Push((sanitize.KnownSecrets{Values: a.secretValues()}).Sanitize(e))
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
	message := a.sanitizeText(err.Error())
	if len(message) > 1024 {
		message = strings.Clone(message[:1024])
	}
	a.recordError(name + ": " + message)
	a.logger.Warn("collector failed", "collector", name, "error", message)
	a.event("collector.error", "warn", name+": "+message)
}
func (a *Agent) recordError(message string) {
	message = a.sanitizeText(message)
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
func (a *Agent) Run(ctx context.Context) (resultErr error) {
	if a.commandStore != nil {
		defer func() { resultErr = errors.Join(resultErr, a.commandStore.Close()) }()
	}
	defer func() { resultErr = errors.Join(resultErr, a.queue.Close()) }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer a.remote.Close()
	if a.logs != nil {
		defer func() { resultErr = errors.Join(resultErr, a.logs.Close()) }()
	}
	a.event("agent.started", "info", "agent started")
	done := make(chan error, 6)
	go func() { done <- a.scheduler.Run(runCtx) }()
	go func() { done <- a.remote.Run(runCtx) }()
	go func() { done <- a.supervise(runCtx) }()
	go func() { done <- a.logScheduler.Run(runCtx) }()
	go func() { done <- a.sipScheduler.Run(runCtx) }()
	go func() { done <- a.certificateLoop(runCtx) }()
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
	for finished < 6 {
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
		a.queue.ResetInflight()
		sessionCtx, cancel := context.WithCancel(ctx)
		applicationACK := false
		watchdog := time.AfterFunc(a.cfg.Controller.ConnectTimeout, cancel)
		session, err := a.transport.Connect(sessionCtx)
		if err == nil {
			hello := model.AgentHello{AgentID: a.id.AgentID, SessionID: a.id.SessionID, Name: a.cfg.Agent.Name, Build: version.Info(), StartedAt: a.started.UTC(), Device: a.deviceSnapshot(), ProtocolVersion: 1, ApplicationACK: true}
			e, eErr := a.envelope(model.Envelope{Hello: &hello})
			err = eErr
			if err == nil {
				err = a.send(sessionCtx, cancel, session, e)
			}
			if err == nil {
				var ack model.ControllerMessage
				ack, err = session.Receive()
				applicationACK = ack.ApplicationACK
				if err == nil && (!ack.HelloAccepted || ack.ProtocolVersion != 1 || ack.Command != nil) {
					err = errors.New("controller rejected protocol handshake")
				}
			}
		}
		watchdog.Stop()
		if err == nil && a.queue.IsDurable() && !applicationACK && a.cfg.Queue.WithDefaults().ACKFallback == "reject" {
			err = errors.New("controller lacks required application ACK; durable records retained within configured bounds")
		}
		if err == nil && sessionCtx.Err() != nil {
			err = sessionCtx.Err()
		}
		if err == nil {
			err = a.recoverCommandResults()
		}
		if err == nil {
			backoff.Reset()
			a.mu.Lock()
			a.connected = true
			a.activeCancel = cancel
			a.ackFallback = a.queue.IsDurable() && !applicationACK
			a.mu.Unlock()
			a.logger.Info("controller connected", "controller", a.cfg.Controller.Address)
			a.event("agent.connected", "info", "controller connected")
			if !applicationACK && a.queue.IsDurable() {
				a.logger.Warn("application ACK unavailable; explicit best-effort send fallback active")
			}
			err = a.exchange(sessionCtx, cancel, session, applicationACK)
		}
		cancel()
		if session != nil {
			_ = session.Close()
		}
		a.mu.Lock()
		a.connected = false
		a.activeCancel = nil
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
	e.SessionID = a.id.SessionID
	e.Sequence = a.sequence.Add(1)
	timer := time.AfterFunc(a.cfg.Controller.SendTimeout, cancel)
	err := s.Send(e)
	timer.Stop()
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func (a *Agent) exchange(ctx context.Context, cancel context.CancelFunc, s transport.Session, applicationACK bool) error {
	received := make(chan error, 1)
	commandsDone := make(chan error, 1)
	commandWork := make(chan model.Command, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				commandsDone <- ctx.Err()
				return
			case c := <-commandWork:
				if err := a.handleCommand(ctx, c); err != nil {
					commandsDone <- err
					cancel()
					return
				}
			}
		}
	}()
	go func() {
		for ctx.Err() == nil {
			message, err := s.Receive()
			if err != nil {
				received <- err
				cancel()
				return
			}
			if message.Command == nil {
				if applicationACK && message.ACKMessageID != "" {
					_, err = a.queue.Acknowledge(message.ACKMessageID)
					if err == nil {
						err = a.acknowledgeCommandResult(message.ACKMessageID)
					}
					if err == nil {
						err = a.recoverCommandResults()
					}
					if err != nil {
						received <- err
						cancel()
						return
					}
					continue
				}
				received <- errors.New("unexpected controller message after handshake")
				cancel()
				return
			}
			if ctx.Err() != nil {
				break
			}
			select {
			case commandWork <- *message.Command:
			case <-ctx.Done():
				received <- ctx.Err()
				return
			default:
				received <- errors.New("controller command backlog exceeds bounded worker capacity")
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
		if !applicationACK {
			if _, err = a.queue.Acknowledge(e.MessageID); err != nil {
				sendErr = err
				break
			}
			if e.CommandResult != nil {
				if err = a.acknowledgeCommandResult(e.MessageID); err != nil {
					sendErr = err
					break
				}
			}
			if err = a.recoverCommandResults(); err != nil {
				sendErr = err
				break
			}
		}
	}
	cancel()
	_ = s.Close()
	receiveErr := <-received
	commandErr := <-commandsDone
	return errors.Join(sendErr, receiveErr, commandErr)
}

func (a *Agent) Snapshot() Diagnostics {
	a.mu.Lock()
	d := Diagnostics{AgentID: a.id.AgentID, SessionID: a.id.SessionID, Build: version.Info(), Platform: runtime.GOOS + "/" + runtime.GOARCH, ControllerAddress: a.cfg.Controller.Address, TLSMode: a.cfg.Security.Mode, Connected: a.connected, ObservedAt: time.Now().UTC(), RecentErrors: append([]string(nil), a.recentErrors...)}
	a.mu.Unlock()
	d.Queue = a.queue.State()
	d.SNMP = a.snmpSecurityStatus()
	if a.storageError != "" {
		d.Queue.StorageError = a.storageError
	}
	a.mu.Lock()
	d.ACKFallback = a.ackFallback
	a.mu.Unlock()
	if a.commandStore != nil {
		s := a.commandStore.Health()
		d.CommandState = &s
	}
	if a.certificates != nil {
		s := a.certificates.Status()
		d.Certificate = &s
	}
	d.Collectors = a.scheduler.Stats()
	d.Collectors = append(d.Collectors, a.remote.Stats()...)
	d.Collectors = append(d.Collectors, a.logScheduler.Stats()...)
	d.Collectors = append(d.Collectors, a.sipScheduler.Stats()...)
	for i := range d.Collectors {
		d.Collectors[i].LastError = a.sanitizeText(d.Collectors[i].LastError)
		if len(d.Collectors[i].LastError) > 1024 {
			d.Collectors[i].LastError = d.Collectors[i].LastError[:1024]
		}
	}
	d.Devices = []model.Device{a.deviceSnapshot()}
	d.RemoteDevices = a.remote.Snapshot()
	d.Devices = append(d.Devices, a.remote.Devices()...)
	if a.logs != nil {
		d.LogSources = a.logs.Snapshot()
	}
	for _, c := range a.sip {
		d.Telephony = append(d.Telephony, c.State())
	}
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
