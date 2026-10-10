// Package grpc maps normalized values to the versioned protobuf contract and
// opens the agent's outbound stream. It does not own polling or command policy.
package grpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/transport"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

const DefaultMaxMessageBytes = 4 << 20

type Options struct {
	// TLSConfigProvider supplies an immutable verified client TLS snapshot for
	// each connection after enrollment or certificate rotation.
	TLSConfigProvider func() (*tls.Config, error)
	Address           string
	Mode              string
	CAFile            string
	CertFile          string
	KeyFile           string
	ServerName        string
	MaxMessageBytes   int
}

type client struct {
	options Options
	creds   credentials.TransportCredentials
}

// New validates TLS credentials eagerly. Plaintext is available only for an
// explicit development mode on a loopback address.
func New(options Options) (transport.Transport, error) {
	if err := validateOptions(&options); err != nil {
		return nil, err
	}
	var creds credentials.TransportCredentials
	if options.Mode == "development" {
		creds = insecure.NewCredentials()
	} else {
		config, err := clientTLSConfig(options)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(config)
	}
	return &client{options: options, creds: creds}, nil
}

// ValidateEnvelope checks the same mapping used by Send before durable
// admission. Invalid identifiers or an oversized protobuf cannot poison replay.
func (c *client) ValidateEnvelope(envelope model.Envelope) error {
	message, err := Encode(envelope)
	if err != nil {
		return fmt.Errorf("validate agent message: %w", err)
	}
	if proto.Size(message) > c.options.MaxMessageBytes {
		return errors.New("encoded agent message exceeds transport size limit")
	}
	// Size does not validate UTF-8 strings. Verify serialization as well so a
	// legacy free-text field cannot repeatedly fail during retained delivery.
	if _, err := proto.Marshal(message); err != nil {
		return errors.New("agent message cannot be serialized as protobuf")
	}
	return nil
}

func (c *client) Connect(ctx context.Context) (transport.Session, error) {
	creds := c.creds
	verifiedHandshake := &atomic.Bool{}
	if c.options.Mode == "production" {
		config, err := clientTLSConfig(c.options)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(config)
		creds = &verifiedCredentials{TransportCredentials: creds, verified: verifiedHandshake}
	}
	conn, err := grpcgo.NewClient(c.options.Address,
		grpcgo.WithTransportCredentials(creds),
		grpcgo.WithDefaultCallOptions(
			grpcgo.MaxCallRecvMsgSize(c.options.MaxMessageBytes),
			grpcgo.MaxCallSendMsgSize(c.options.MaxMessageBytes),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create controller client: %w", err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := pb.NewAgentServiceClient(conn).Connect(streamCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, fmt.Errorf("open controller stream: %w", err)
	}
	return &session{conn: conn, stream: stream, cancel: cancel, verifiedHandshake: verifiedHandshake}, nil
}

// Stream call Peer metadata is finalized too late for bidirectional sessions.
// Attest the completed standard TLS handshake instead, without replacing any
// certificate verification or exposing transport credentials to the core.
type verifiedCredentials struct {
	credentials.TransportCredentials
	verified *atomic.Bool
}

func (c *verifiedCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, raw)
	if err == nil {
		if tlsInfo, ok := info.(credentials.TLSInfo); ok && len(tlsInfo.State.VerifiedChains) > 0 && len(tlsInfo.State.PeerCertificates) > 0 {
			c.verified.Store(true)
		}
	}
	return conn, info, err
}

func (c *verifiedCredentials) Clone() credentials.TransportCredentials {
	return &verifiedCredentials{TransportCredentials: c.TransportCredentials.Clone(), verified: c.verified}
}

func clientTLSConfig(options Options) (*tls.Config, error) {
	if options.TLSConfigProvider == nil {
		return TLSConfig(options, false)
	}
	c, err := options.TLSConfigProvider()
	if err != nil {
		return nil, errors.New("client certificate unavailable")
	}
	if c == nil || c.InsecureSkipVerify || c.MinVersion < tls.VersionTLS12 || c.RootCAs == nil || len(c.Certificates) == 0 || c.VerifyConnection != nil || c.VerifyPeerCertificate != nil {
		return nil, errors.New("unsafe dynamic TLS configuration")
	}
	return c.Clone(), nil
}

// A session supports one sender and one receiver concurrently, as gRPC does.
// The core serializes sends and owns the single receive loop.
type session struct {
	verifiedHandshake *atomic.Bool
	authenticated     atomic.Bool
	conn              *grpcgo.ClientConn
	stream            grpcgo.BidiStreamingClient[pb.AgentEnvelope, pb.ControllerEnvelope]
	cancel            context.CancelFunc
	closeOnce         sync.Once
	closeErr          error
}

func (s *session) Authenticated() bool { return s.authenticated.Load() }

func (s *session) Send(envelope model.Envelope) error {
	message, err := Encode(envelope)
	if err != nil {
		return fmt.Errorf("encode agent message: %w", err)
	}
	if err := s.stream.Send(message); err != nil {
		return fmt.Errorf("send agent message: %w", err)
	}
	return nil
}

func (s *session) Receive() (model.ControllerMessage, error) {
	message, err := s.stream.Recv()
	if err != nil {
		return model.ControllerMessage{}, fmt.Errorf("receive controller message: %w", err)
	}
	if s.verifiedHandshake != nil && s.verifiedHandshake.Load() {
		s.authenticated.Store(true)
	}
	if message == nil {
		return model.ControllerMessage{}, errors.New("controller message is nil")
	}
	switch payload := message.Payload.(type) {
	case *pb.ControllerEnvelope_HelloAck:
		if payload == nil || payload.HelloAck == nil {
			return model.ControllerMessage{}, errors.New("hello acknowledgement is nil")
		}
		if payload.HelloAck.ProtocolVersion == 0 {
			return model.ControllerMessage{}, errors.New("hello acknowledgement has no protocol version")
		}
		return model.ControllerMessage{HelloAccepted: payload.HelloAck.Accepted, ProtocolVersion: payload.HelloAck.ProtocolVersion, ApplicationACK: payload.HelloAck.ApplicationAck, ManagedConfigV1: payload.HelloAck.ManagedConfigV1}, nil
	case *pb.ControllerEnvelope_DesiredConfig:
		if payload == nil {
			return model.ControllerMessage{DesiredConfig: &model.DesiredCollectorConfig{}, ConfigError: "invalid_config_wire"}, nil
		}
		if len(message.ProtoReflect().GetUnknown()) > 0 {
			correlation := desiredConfigCorrelation(payload.DesiredConfig)
			return model.ControllerMessage{DesiredConfig: &correlation, ConfigError: "invalid_config_wire"}, nil
		}
		config, decodeErr := DecodeDesiredConfig(payload.DesiredConfig)
		if decodeErr != nil {
			correlation := desiredConfigCorrelation(payload.DesiredConfig)
			return model.ControllerMessage{DesiredConfig: &correlation, ConfigError: "invalid_config_wire"}, nil
		}
		return model.ControllerMessage{DesiredConfig: &config}, nil
	case *pb.ControllerEnvelope_MessageAck:
		if payload == nil || payload.MessageAck == nil || payload.MessageAck.MessageId == "" || len(payload.MessageAck.MessageId) > 128 {
			return model.ControllerMessage{}, errors.New("invalid message acknowledgement")
		}
		return model.ControllerMessage{ACKMessageID: payload.MessageAck.MessageId}, nil
	case *pb.ControllerEnvelope_Command:
		if payload == nil {
			return model.ControllerMessage{}, errors.New("command payload is nil")
		}
		command, err := DecodeCommand(payload.Command)
		if err != nil {
			return model.ControllerMessage{}, fmt.Errorf("decode controller command: %w", err)
		}
		return model.ControllerMessage{Command: &command}, nil
	default:
		return model.ControllerMessage{}, errors.New("controller payload is missing or unsupported")
	}
}

func (s *session) CloseSend() error { return s.stream.CloseSend() }

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

// ServerOptions is intended for the Mock Controller as well as integration
// tests. Production requires a trusted client certificate; development remains
// explicitly limited to loopback listeners.
func ServerOptions(options Options) ([]grpcgo.ServerOption, error) {
	if err := validateOptions(&options); err != nil {
		return nil, err
	}
	result := []grpcgo.ServerOption{
		grpcgo.MaxRecvMsgSize(options.MaxMessageBytes),
		grpcgo.MaxSendMsgSize(options.MaxMessageBytes),
	}
	if options.Mode == "production" {
		config, err := TLSConfig(options, true)
		if err != nil {
			return nil, err
		}
		result = append(result, grpcgo.Creds(credentials.NewTLS(config)))
	}
	return result, nil
}

// TLSConfig builds TLS with verification enabled. On the server, CAFile is the
// client trust store; on the agent, it is the controller trust store.
func TLSConfig(options Options, server bool) (*tls.Config, error) {
	if options.Mode != "production" {
		return nil, errors.New("TLS configuration requires production mode")
	}
	if options.CAFile == "" || options.CertFile == "" || options.KeyFile == "" {
		return nil, errors.New("production requires CA, certificate, and private key files")
	}
	caPEM, err := os.ReadFile(options.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("TLS CA contains no valid certificates")
	}
	certificate, err := tls.LoadX509KeyPair(options.CertFile, options.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate and key: %w", err)
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
	}
	if server {
		config.ClientCAs = pool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	} else {
		config.RootCAs = pool
		config.ServerName = options.ServerName
	}
	return config, nil
}

func validateOptions(options *Options) error {
	host, port, err := net.SplitHostPort(options.Address)
	if err != nil || host == "" || port == "" {
		return errors.New("controller address must be host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("controller port must be an integer from 1 to 65535")
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return errors.New("controller port must contain only decimal digits")
		}
	}
	switch options.Mode {
	case "development":
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return errors.New("development transport requires a loopback address")
		}
	case "production":
	default:
		return errors.New("security mode must be development or production")
	}
	if options.MaxMessageBytes == 0 {
		options.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if options.MaxMessageBytes < 512 || options.MaxMessageBytes > DefaultMaxMessageBytes {
		return errors.New("maximum message size must be between 512 bytes and 4 MiB")
	}
	return nil
}
