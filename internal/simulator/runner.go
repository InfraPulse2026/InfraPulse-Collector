package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/sanitize"
	"InfraPulseAgent/internal/transport"
	"InfraPulseAgent/internal/version"
	"github.com/google/uuid"
)

type RuntimeOptions struct {
	Name                                                                      string
	ConnectTimeout, SendTimeout, ReconnectInitial, ReconnectMax, DrainTimeout time.Duration
	MaxMessages, MaxBytes, MaxMessageBytes                                    int
}

func (o RuntimeOptions) defaults() RuntimeOptions {
	if o.Name == "" {
		o.Name = "infrapulse-simulator"
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 5 * time.Second
	}
	if o.SendTimeout == 0 {
		o.SendTimeout = 10 * time.Second
	}
	if o.ReconnectInitial == 0 {
		o.ReconnectInitial = time.Second
	}
	if o.ReconnectMax == 0 {
		o.ReconnectMax = 30 * time.Second
	}
	if o.DrainTimeout == 0 {
		o.DrainTimeout = 5 * time.Second
	}
	if o.MaxMessages == 0 {
		o.MaxMessages = 256
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 8 << 20
	}
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = 1 << 20
	}
	return o
}

type Runner struct {
	generator      *Generator
	options        RuntimeOptions
	identity       identity.Identity
	transport      transport.Transport
	queue          *queue.Queue
	sequence       atomic.Uint64
	runStarted     atomic.Bool
	started        time.Time
	productionDone chan struct{}
}

func NewRunner(generator *Generator, id identity.Identity, t transport.Transport, options RuntimeOptions) (*Runner, error) {
	options = options.defaults()
	if generator == nil || t == nil || id.AgentID == "" || id.SessionID == "" || len(options.Name) > 128 || options.ConnectTimeout <= 0 || options.ConnectTimeout > time.Minute || options.SendTimeout <= 0 || options.SendTimeout > time.Minute || options.ReconnectInitial <= 0 || options.ReconnectMax < options.ReconnectInitial || options.ReconnectMax > time.Minute || options.DrainTimeout < time.Millisecond || options.DrainTimeout > time.Minute || options.MaxMessages > 65536 || options.MaxBytes > 256<<20 || options.MaxMessageBytes > 4<<20 {
		return nil, errors.New("invalid simulator runtime configuration")
	}
	q, err := queue.New(options.MaxMessages, options.MaxBytes, options.MaxMessageBytes)
	if err != nil {
		return nil, err
	}
	return &Runner{generator: generator, identity: id, transport: t, options: options, queue: q, started: time.Now().UTC(), productionDone: make(chan struct{})}, nil
}
func (r *Runner) QueueState() model.QueueState { return r.queue.State() }

func metadata(e model.Envelope, id identity.Identity) model.Envelope {
	// The generator has explicit target bindings and logical observation times.
	// Runtime binds the persistent Collector identity without restamping them.
	e = model.ObserveEnvelope(e, model.SourceRef{Type: model.SourceLocal, CollectorID: id.AgentID, TargetID: "simulated:runtime", Protocol: "simulator", Provenance: model.ProvenanceSimulated}, time.Time{})
	e.MessageID = uuid.NewString()
	e.AgentID = id.AgentID
	e.SessionID = id.SessionID
	e.SentAt = time.Now().UTC()
	return e
}
func hello(generator *Generator, id identity.Identity, name string, started time.Time) model.Envelope {
	device := model.Device{ID: "simulated:agent:" + id.AgentID, Name: name, Source: "simulated", Type: "switch", Driver: "simulated", Online: true, LastSeen: generator.options.Start, OS: "simulated"}
	device = model.ObserveDevice(device, model.SourceRef{Type: model.SourceLocal, CollectorID: id.AgentID, TargetID: "simulated:runtime", Protocol: "simulator", Provenance: model.ProvenanceSimulated}, generator.options.Start, generator.options.Start)
	build := version.Info()
	build.Version = "simulated:" + build.Version
	return metadata(model.Envelope{Hello: &model.AgentHello{AgentID: id.AgentID, SessionID: id.SessionID, Name: name, Build: build, StartedAt: started, Device: device, ProtocolVersion: 1}}, id)
}

// produce keeps real-time generation bounded by Interval. Logical timestamps
// are independent of scheduling jitter, reconnections and the runtime clock.
func produce(ctx context.Context, generator *Generator, emit func(model.Envelope) error) error {
	started := time.Now()
	for step := uint64(0); step < generator.Steps(); step++ {
		if step > 0 {
			delay := time.Until(started.Add(time.Duration(step) * generator.options.Interval))
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, e := range generator.Tick(step) {
			if err := emit(e); err != nil {
				return err
			}
		}
	}
	// Honor the requested duration even when it is shorter than one interval.
	if delay := time.Until(started.Add(generator.options.Duration)); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// Run uses the production wire contract and bounded RAM outbox. It reconnects
// with the shared transport backoff. Accepted messages can still be evicted or
// lost after Pop/Send failure; the simulator does not promise durable delivery.
func (r *Runner) Run(parent context.Context) error {
	if !r.runStarted.CompareAndSwap(false, true) {
		return errors.New("simulator runner can only run once")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- r.supervise(ctx) }()
	productionErr := produce(ctx, r.generator, func(e model.Envelope) error {
		err := r.queue.Push((sanitize.KnownSecrets{}).Sanitize(metadata(e, r.identity)))
		if errors.Is(err, queue.ErrFull) || errors.Is(err, queue.ErrMessageTooLarge) {
			return nil
		}
		return err
	})
	close(r.productionDone)
	if productionErr != nil {
		cancel()
		<-completed
		if parent.Err() != nil {
			return nil
		}
		return productionErr
	}
	timer := time.NewTimer(r.options.DrainTimeout)
	defer timer.Stop()
	select {
	case err := <-completed:
		return err
	case <-parent.Done():
		cancel()
		<-completed
		return nil
	case <-timer.C:
		cancel()
		<-completed
		return errors.New("simulator final RAM outbox drain timed out")
	}
}

var errComplete = errors.New("simulator production complete")

func (r *Runner) supervise(ctx context.Context) error {
	backoff := transport.NewBackoff(r.options.ReconnectInitial, r.options.ReconnectMax)
	for ctx.Err() == nil {
		sessionCtx, cancel := context.WithCancel(ctx)
		watchdog := time.AfterFunc(r.options.ConnectTimeout, cancel)
		session, err := r.transport.Connect(sessionCtx)
		var stopClose func() bool
		if err == nil {
			stopClose = context.AfterFunc(sessionCtx, func() { _ = session.Close() })
			err = r.send(sessionCtx, cancel, session, hello(r.generator, r.identity, r.options.Name, r.started))
			if err == nil {
				var ack model.ControllerMessage
				ack, err = session.Receive()
				if err == nil && (!ack.HelloAccepted || ack.ProtocolVersion != 1 || ack.Command != nil) {
					err = errors.New("controller rejected simulator protocol handshake")
				}
			}
		}
		watchdog.Stop()
		if err == nil && sessionCtx.Err() != nil {
			err = sessionCtx.Err()
		}
		if err == nil {
			backoff.Reset()
			err = r.exchange(sessionCtx, cancel, session)
		}
		cancel()
		if session != nil {
			_ = session.Close()
		}
		if stopClose != nil {
			stopClose()
		}
		if errors.Is(err, errComplete) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		timer := time.NewTimer(backoff.Next())
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func (r *Runner) send(ctx context.Context, cancel context.CancelFunc, session transport.Session, e model.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.Sequence = r.sequence.Add(1)
	e.SentAt = time.Now().UTC()
	timer := time.AfterFunc(r.options.SendTimeout, cancel)
	defer timer.Stop()
	if err := session.Send(e); err != nil {
		return err
	}
	return ctx.Err()
}

func (r *Runner) exchange(ctx context.Context, cancel context.CancelFunc, session transport.Session) error {
	received := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			message, err := session.Receive()
			if err != nil {
				received <- err
				cancel()
				return
			}
			if message.Command == nil {
				received <- errors.New("unexpected simulator controller message")
				cancel()
				return
			}
			now := time.Now().UTC()
			result := model.CommandResult{CommandID: message.Command.ID, Status: "unsupported", StartedAt: now, FinishedAt: now, ErrorCode: "simulator_unsupported", ErrorMessage: "simulated development agent does not execute commands"}
			if err = r.queue.Push(metadata(model.Envelope{CommandResult: &result}, r.identity)); err != nil {
				received <- err
				cancel()
				return
			}
		}
		received <- ctx.Err()
	}()
	popCtx, popCancel := context.WithCancel(ctx)
	wakeFinished := make(chan struct{})
	go func() {
		defer close(wakeFinished)
		select {
		case <-r.productionDone:
			popCancel()
		case <-ctx.Done():
			popCancel()
		}
	}()
	var sendErr, receiveErr error
	receiveFinished := false
	for ctx.Err() == nil {
		productionEnded := false
		select {
		case <-r.productionDone:
			productionEnded = true
		default:
		}
		if productionEnded && r.queue.State().Messages == 0 {
			if closer, ok := session.(transport.SendCloser); ok {
				sendErr = closer.CloseSend()
				if sendErr == nil {
					// gRPC Send can return before queued frames reach the peer.
					// Half-close preserves those frames; Receive EOF follows
					// the Mock's consumption of the complete outbound stream.
					// Run's existing drain deadline cancels an unresponsive peer.
					receiveErr = <-received
					receiveFinished = true
					if errors.Is(receiveErr, io.EOF) {
						sendErr = errComplete
					} else {
						sendErr = receiveErr
					}
				}
			} else {
				sendErr = errComplete
			}
			break
		}
		currentCtx := popCtx
		if productionEnded {
			currentCtx = ctx
		}
		e, err := r.queue.Pop(currentCtx)
		if err != nil {
			if ctx.Err() == nil && productionEnded {
				continue
			}
			select {
			case <-r.productionDone:
				continue
			default:
			}
			sendErr = err
			break
		}
		if err = r.send(ctx, cancel, session, e); err != nil {
			sendErr = err
			break
		}
	}
	cancel()
	popCancel()
	_ = session.Close()
	<-wakeFinished
	if !receiveFinished {
		receiveErr = <-received
	}
	if errors.Is(sendErr, errComplete) {
		return errComplete
	}
	return errors.Join(sendErr, receiveErr)
}

// WriteJSON emits the same normalized envelopes with real session metadata.
// The payload sequence is deterministic; transport UUIDs and SentAt are not.
func WriteJSON(ctx context.Context, generator *Generator, id identity.Identity, writer io.Writer) error {
	if generator == nil || writer == nil || id.AgentID == "" || id.SessionID == "" {
		return errors.New("invalid simulator JSON output configuration")
	}
	encoder := json.NewEncoder(writer)
	sequence := uint64(1)
	e := hello(generator, id, "infrapulse-simulator", time.Now().UTC())
	e.Sequence = sequence
	if err := encoder.Encode(e); err != nil {
		return err
	}
	return produce(ctx, generator, func(e model.Envelope) error {
		sequence++
		e = metadata(e, id)
		e.Sequence = sequence
		return encoder.Encode((sanitize.KnownSecrets{}).Sanitize(e))
	})
}
