package grpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/agent"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/pki"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestEnrollmentAgentMockIdentityAndRotatedReconnect(t *testing.T) {
	dir := pkiE2ETempDir(t)
	identityValue, err := identity.LoadOrCreate(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	caPath := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertPath, serverKeyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if err = os.WriteFile(serverCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(serverKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PKI_E2E_TOKEN", "private-fixture-token")
	enrollment, err := pki.NewMockEnrollment(caPEM, caKeyPEM, []pki.Token{{Value: "private-fixture-token", AgentID: identityValue.AgentID, ExpiresAt: now.Add(time.Hour)}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	https := httptest.NewUnstartedServer(enrollment)
	https.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven}
	https.StartTLS()
	defer https.Close()
	manager, err := pki.New(context.Background(), pki.Options{AgentID: identityValue.AgentID, StateDir: dir, EnrollmentURL: https.URL, BootstrapCAFile: caPath, BootstrapServerName: "localhost", TokenEnv: "PKI_E2E_TOKEN", Timeout: time.Second, RenewBefore: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverOptions, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: listener.Addr().String(), Mode: "production", CAFile: caPath, CertFile: serverCertPath, KeyFile: serverKeyPath})
	if err != nil {
		t.Fatal(err)
	}
	certificates := make(chan string, 8)
	serverOptions = append(serverOptions, grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		remote, _ := peer.FromContext(ss.Context())
		tlsInfo := remote.AuthInfo.(credentials.TLSInfo)
		certificates <- tlsInfo.State.PeerCertificates[0].SerialNumber.String()
		return handler(srv, ss)
	}))
	hellos := make(chan model.Envelope, 8)
	server := grpc.NewServer(serverOptions...)
	pb.RegisterAgentServiceServer(server, mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Hello != nil {
			hellos <- e
		}
	}))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	tr, err := transportgrpc.New(transportgrpc.Options{Address: listener.Addr().String(), Mode: "production", TLSConfigProvider: func() (*tls.Config, error) { return manager.TLSConfig("localhost") }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Agent.StateDir = dir
	cfg.Local.Enabled = false
	cfg.Security.Enrollment = config.Enrollment{Enabled: true, URL: https.URL, CAFile: caPath, TokenEnv: "PKI_E2E_TOKEN"}
	cfg.Controller.Address = listener.Addr().String()
	cfg.Controller.ReconnectInitial = 10 * time.Millisecond
	cfg.Controller.ReconnectMax = 50 * time.Millisecond
	runAgent := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a, err := agent.New(ctx, cfg, nil, tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		a.SetCertificateManager(manager)
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		select {
		case hello := <-hellos:
			if hello.AgentID != identityValue.AgentID {
				t.Fatal("identity changed")
			}
		case <-ctx.Done():
			t.Fatal("Agent did not connect", ctx.Err())
		}
		var serial string
		select {
		case serial = <-certificates:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		cancel()
		select {
		case err = <-done:
			if err != nil {
				t.Fatal("Agent shutdown", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Agent did not stop")
		}
		return serial
	}
	firstSerial := runAgent()
	t.Setenv("PKI_E2E_TOKEN", "")
	if err = manager.Renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondSerial := runAgent()
	if firstSerial == secondSerial {
		t.Fatal("reconnect reused previous client certificate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := tr.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	wrongID := uuid.NewString()
	sessionID := uuid.NewString()
	at := time.Now().UTC()
	err = session.Send(model.Envelope{MessageID: uuid.NewString(), AgentID: wrongID, SessionID: sessionID, Sequence: 1, SentAt: at, Hello: &model.AgentHello{AgentID: wrongID, SessionID: sessionID, StartedAt: at, ProtocolVersion: 1, Device: model.Device{ID: "local:" + wrongID, Type: "unknown", Driver: "none"}}})
	if err == nil {
		_, err = session.Receive()
	}
	if err == nil {
		t.Fatal("trusted client certificate claimed another AgentID")
	}
}
