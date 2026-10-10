// Package mockcontroller is a development-only observable gRPC endpoint.
package mockcontroller

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/sanitize"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Controller struct {
	// DesiredConfigs is an immutable development scenario, configured before
	// Serve. Terminal apply statuses advance snapshots; reconnect rebinds the
	// current snapshot to the new process session.
	DesiredConfigs              []model.DesiredCollectorConfig
	RepeatConfig                bool
	DisconnectAfterConfigStatus int
	configSessions              map[string]*configSession
	configProgress              map[string]int
	configRepeats               map[string]bool
	configDisconnects           map[string]bool
	configStatusCounts          map[string]int
	// DisableACK exercises explicit legacy-controller fallback in fixtures.
	DisableACK bool
	pb.UnimplementedAgentServiceServer
	Writer  io.Writer
	Observe func(model.Envelope)
	Command *model.Command
	mu      sync.Mutex
	streams chan struct{}
}

func New(writer io.Writer, command *model.Command, observe func(model.Envelope)) *Controller {
	return &Controller{Writer: writer, Command: command, Observe: observe, streams: make(chan struct{}, 32)}
}

func (c *Controller) Connect(stream grpc.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	select {
	case c.streams <- struct{}{}:
		defer func() { <-c.streams }()
	default:
		return status.Error(codes.ResourceExhausted, "mock stream limit reached")
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	e, err := transportgrpc.DecodeEnvelope(first)
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid first envelope")
	}
	if e.Hello == nil || e.Hello.ProtocolVersion != 1 || e.AgentID != e.Hello.AgentID || e.SessionID != e.Hello.SessionID {
		return status.Error(codes.FailedPrecondition, "first message must be a matching protocol-v1 hello")
	}
	if err = transportgrpc.VerifyPeerIdentity(stream.Context(), e.AgentID); err != nil {
		return status.Error(codes.Unauthenticated, "certificate identity does not match agent identity")
	}
	configSession := c.openConfigSession(e.Hello)
	defer c.closeConfigSession(configSession)
	if err = c.observe(e); err != nil {
		return err
	}
	applicationACK := e.Hello.ApplicationACK && !c.DisableACK
	if err = stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1, ApplicationAck: applicationACK, ManagedConfigV1: e.Hello.ManagedConfigV1}}}); err != nil {
		return err
	}
	if c.Command != nil {
		command := *c.Command
		if command.ID == "" {
			id, idErr := uuid.NewRandom()
			if idErr != nil {
				return status.Error(codes.Internal, "cannot generate command id")
			}
			command.ID = id.String()
		}
		command.RequestedAt = time.Now().UTC()
		if command.TargetDeviceID == "auto-local" {
			command.TargetDeviceID = e.Hello.Device.ID
		}
		wire, encodeErr := transportgrpc.EncodeCommand(command)
		if encodeErr != nil {
			return status.Error(codes.InvalidArgument, "invalid configured mock command")
		}
		if err = stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_Command{Command: wire}}); err != nil {
			return err
		}
	}
	if err = c.sendInitialConfig(stream, configSession); err != nil {
		return err
	}
	sequence := e.Sequence
	for {
		wire, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		next, err := transportgrpc.DecodeEnvelope(wire)
		if err != nil {
			return status.Error(codes.InvalidArgument, "invalid agent envelope")
		}
		if next.Hello != nil || next.AgentID != e.AgentID || next.SessionID != e.SessionID || next.Sequence <= sequence {
			return status.Error(codes.InvalidArgument, "invalid stream identity or sequence")
		}
		sequence = next.Sequence
		if err = c.observe(next); err != nil {
			return err
		}
		if applicationACK {
			if next.MessageID == "" || len(next.MessageID) > 128 {
				return status.Error(codes.InvalidArgument, "invalid message ID")
			}
			if err = stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_MessageAck{MessageAck: &pb.MessageACK{MessageId: next.MessageID}}}); err != nil {
				return err
			}
		}
		if err = c.handleConfigStatus(stream, configSession, next.ConfigApplyResult); err != nil {
			return err
		}
		if err = c.sendPendingConfigs(stream, configSession); err != nil {
			return err
		}
	}
}

func (c *Controller) observe(e model.Envelope) error {
	e = (sanitize.KnownSecrets{}).Sanitize(e)
	if c.Writer != nil {
		c.mu.Lock()
		err := json.NewEncoder(c.Writer).Encode(e)
		c.mu.Unlock()
		if err != nil {
			return status.Error(codes.Internal, "cannot write mock output")
		}
	}
	if c.Observe != nil {
		c.Observe(e)
	}
	return nil
}
