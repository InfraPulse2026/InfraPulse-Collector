package grpc

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type controller struct {
	pb.UnimplementedAgentServiceServer
	handle func(grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error
}

func (c *controller) Connect(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	return c.handle(stream)
}

func startController(t *testing.T, options Options, handle func(grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	options.Address = listener.Addr().String()
	serverOptions, err := ServerOptions(options)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := grpcgo.NewServer(serverOptions...)
	pb.RegisterAgentServiceServer(server, &controller{handle: handle})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return options.Address
}

func helloEnvelope() model.Envelope {
	envelope := testEnvelope()
	envelope.Hello = &model.AgentHello{AgentID: envelope.AgentID, SessionID: envelope.SessionID,
		Name: "collector", StartedAt: testTime, Device: testDevice(), ProtocolVersion: 1}
	return envelope
}

func TestDevelopmentBidirectionalStream(t *testing.T) {
	received := make(chan model.Envelope, 2)
	address := startController(t, Options{Mode: "development"}, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		envelope, err := DecodeEnvelope(message)
		if err != nil {
			return err
		}
		received <- envelope
		if err := stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1}}}); err != nil {
			return err
		}
		command, err := EncodeCommand(testCommand())
		if err != nil {
			return err
		}
		if err := stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_Command{Command: command}}); err != nil {
			return err
		}
		message, err = stream.Recv()
		if err != nil {
			return err
		}
		envelope, err = DecodeEnvelope(message)
		if err != nil {
			return err
		}
		received <- envelope
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	client, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Send(helloEnvelope()); err != nil {
		t.Fatal(err)
	}
	ack, err := session.Receive()
	if err != nil || !ack.HelloAccepted || ack.ProtocolVersion != 1 {
		t.Fatalf("hello ack: %#v, %v", ack, err)
	}
	command, err := session.Receive()
	if err != nil || command.Command == nil || command.Command.Interface.Operation != "disable" {
		t.Fatalf("command: %#v, %v", command, err)
	}
	result := testEnvelope()
	result.CommandResult = &model.CommandResult{CommandID: command.Command.ID, Status: "unsupported", ErrorCode: "capability_unsupported", StartedAt: testTime, FinishedAt: testTime}
	if err := session.Send(result); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		select {
		case envelope := <-received:
			if index == 0 && envelope.Hello == nil || index == 1 && envelope.CommandResult == nil {
				t.Fatal("controller received wrong payload")
			}
		case <-ctx.Done():
			t.Fatal("controller did not receive messages")
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal("close must be idempotent", err)
	}
}

func TestSessionLifetimeCancellation(t *testing.T) {
	address := startController(t, Options{Mode: "development"}, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	client, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := client.Connect(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer session.Close()
	finished := make(chan error, 1)
	go func() { _, err := session.Receive(); finished <- err }()
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("receive succeeded after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session outlived its context")
	}
}

func TestCloseUnblocksReceive(t *testing.T) {
	address := startController(t, Options{Mode: "development"}, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	client, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { _, err := session.Receive(); finished <- err }()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("receive succeeded after closing")
		}
	case <-ctx.Done():
		t.Fatal("close did not unblock receive")
	}
}

func TestTransportOptionValidation(t *testing.T) {
	for name, options := range map[string]Options{
		"implicit security":       {Address: "127.0.0.1:9443"},
		"unknown mode":            {Address: "127.0.0.1:9443", Mode: "insecure"},
		"remote plaintext":        {Address: "192.0.2.1:9443", Mode: "development"},
		"hostname plaintext":      {Address: "controller.example:9443", Mode: "development"},
		"all addresses plaintext": {Address: "0.0.0.0:9443", Mode: "development"},
		"missing host":            {Address: ":9443", Mode: "development"},
		"missing port":            {Address: "localhost", Mode: "development"},
		"zero port":               {Address: "localhost:0", Mode: "development"},
		"negative port":           {Address: "localhost:-1", Mode: "development"},
		"oversized port":          {Address: "localhost:65536", Mode: "development"},
		"service name port":       {Address: "localhost:https", Mode: "development"},
		"plus port":               {Address: "localhost:+9443", Mode: "development"},
		"space port":              {Address: "localhost: 9443", Mode: "development"},
		"negative limit":          {Address: "127.0.0.1:9443", Mode: "development", MaxMessageBytes: -1},
		"too small limit":         {Address: "127.0.0.1:9443", Mode: "development", MaxMessageBytes: 511},
		"unbounded limit":         {Address: "127.0.0.1:9443", Mode: "development", MaxMessageBytes: DefaultMaxMessageBytes + 1},
		"missing mTLS files":      {Address: "127.0.0.1:9443", Mode: "production"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(options); err == nil {
				t.Fatal("unsafe or invalid options accepted")
			}
		})
	}
	for _, address := range []string{"127.0.0.1:9443", "[::1]:9443", "localhost:9443", "LOCALHOST:9443"} {
		if _, err := New(Options{Address: address, Mode: "development"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMessageLimit(t *testing.T) {
	address := startController(t, Options{Mode: "development", MaxMessageBytes: 1024}, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		_, err := stream.Recv()
		return err
	})
	client, err := New(Options{Address: address, Mode: "development", MaxMessageBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	envelope := helloEnvelope()
	envelope.Hello.Name = string(make([]byte, 2048))
	if err := session.Send(envelope); err == nil {
		t.Fatal("oversized message was accepted")
	}
}

type testCA struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
	path        string
}

func makeCA(t *testing.T, directory, name string) testCA {
	t.Helper()
	publicKey, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name+"-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return testCA{certificate: certificate, key: key, path: path}
}

func makeCertificate(t *testing.T, directory, name string, ca testCA, server bool) (string, string) {
	t.Helper()
	publicKey, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, publicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(directory, name+".pem"), filepath.Join(directory, name+"-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestMutualTLS(t *testing.T) {
	directory := t.TempDir()
	ca := makeCA(t, directory, "trusted")
	rogue := makeCA(t, directory, "untrusted")
	serverCert, serverKey := makeCertificate(t, directory, "controller", ca, true)
	clientCert, clientKey := makeCertificate(t, directory, "agent", ca, false)
	rogueCert, rogueKey := makeCertificate(t, directory, "rogue", rogue, false)
	serverOptions := Options{Mode: "production", CAFile: ca.path, CertFile: serverCert, KeyFile: serverKey}
	address := startController(t, serverOptions, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := stream.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1}}}); err != nil {
			return err
		}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	valid := Options{Address: address, Mode: "production", CAFile: ca.path, CertFile: clientCert, KeyFile: clientKey, ServerName: "localhost"}
	t.Run("trusted certificate", func(t *testing.T) {
		client, err := New(valid)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		session, err := client.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := session.Send(helloEnvelope()); err != nil {
			t.Fatal(err)
		}
		ack, err := session.Receive()
		if err != nil || !ack.HelloAccepted {
			t.Fatalf("mTLS acknowledgement: %#v %v", ack, err)
		}
	})
	for _, name := range []string{"untrusted client certificate", "wrong server name", "untrusted server"} {
		t.Run(name, func(t *testing.T) {
			options := valid
			switch name {
			case "untrusted client certificate":
				options.CertFile, options.KeyFile = rogueCert, rogueKey
			case "wrong server name":
				options.ServerName = "different.example"
			case "untrusted server":
				options.CAFile = rogue.path
			}
			client, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			session, err := client.Connect(ctx)
			if err == nil {
				defer session.Close()
				err = session.Send(helloEnvelope())
				if err == nil {
					_, err = session.Receive()
				}
			}
			if err == nil {
				t.Fatal("unverified mTLS peer was accepted")
			}
		})
	}
	t.Run("missing client certificate", func(t *testing.T) {
		roots := x509.NewCertPool()
		roots.AddCert(ca.certificate)
		connection, err := grpcgo.NewClient(address, grpcgo.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, ServerName: "localhost"})))
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		stream, err := pb.NewAgentServiceClient(connection).Connect(ctx)
		if err == nil {
			message, encodeErr := Encode(helloEnvelope())
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			err = stream.Send(message)
			if err == nil {
				_, err = stream.Recv()
			}
		}
		if err == nil {
			t.Fatal("server accepted a missing client certificate")
		}
	})
	config, err := TLSConfig(valid, false)
	if err != nil {
		t.Fatal(err)
	}
	if config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS12 {
		t.Fatal("TLS verification or minimum version is unsafe")
	}
	serverConfig, err := TLSConfig(serverOptions, true)
	if err != nil {
		t.Fatal(err)
	}
	if serverConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("server does not require verified client certificates")
	}
}

func TestMalformedControllerPayload(t *testing.T) {
	for _, payload := range []*pb.ControllerEnvelope{{}, {Payload: &pb.ControllerEnvelope_Command{}}, {Payload: &pb.ControllerEnvelope_HelloAck{}}} {
		address := startController(t, Options{Mode: "development"}, func(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
			return stream.Send(payload)
		})
		client, err := New(Options{Address: address, Mode: "development"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		session, err := client.Connect(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, err = session.Receive()
		_ = session.Close()
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("malformed payload was not rejected immediately")
		}
	}
}
