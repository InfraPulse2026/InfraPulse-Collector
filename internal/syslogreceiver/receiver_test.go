package syslogreceiver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
)

const sampleSyslog = "<165>1 2026-10-08T10:00:00.123456+03:00 spoofed-host app 42 ID47 [example@32473 value=\"a b\"] hello"

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func tcpSettings(t *testing.T) Settings {
	t.Helper()
	s := Defaults()
	s.Enabled, s.TLS.Enabled, s.TCP.Enabled = true, false, true
	s.TCP.Listen = freeAddress(t)
	s.Sources = []Source{{ID: "approved", PeerCIDRs: []string{"127.0.0.1/32"}, AuditApps: []string{"audispd"}}}
	s.Limits.ReadTimeout, s.Limits.IdleTimeout, s.Limits.AdmissionTimeout = 500*time.Millisecond, 2*time.Second, 300*time.Millisecond
	return s
}

func testOptions(emit func(context.Context, model.Envelope) error) Options {
	return Options{CollectorID: "collector-test", MaxBatchBytes: 1 << 20, Sanitize: func(v string) string { return v }, Emit: emit}
}

func captureEmitter(capacity int) (chan model.Envelope, func(context.Context, model.Envelope) error) {
	records := make(chan model.Envelope, capacity)
	return records, func(ctx context.Context, e model.Envelope) error {
		select {
		case records <- e:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitReceiver(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func startReceiver(t *testing.T, s Settings, options Options) (*Receiver, string, context.CancelFunc, <-chan error) {
	t.Helper()
	r, err := New(s, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("receiver shutdown:", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("receiver failed to shut down")
		}
	})
	waitReceiver(t, "listener", func() bool { return len(r.Snapshot().Listening) > 0 })
	return r, r.Snapshot().Listening[0], cancel, done
}

func dialTCP(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeWire(t *testing.T, conn net.Conn, wire string) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, wire); err != nil {
		t.Fatal(err)
	}
}

func receiveEnvelope(t *testing.T, records <-chan model.Envelope) model.Envelope {
	t.Helper()
	select {
	case e := <-records:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("no admitted log")
		return model.Envelope{}
	}
}

func assertClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var b [1]byte
	_, err := conn.Read(b[:])
	if err == nil {
		t.Fatal("connection remained readable")
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Timeout() {
		t.Fatal("connection remained open until test deadline")
	}
}

func TestPlainTCPProcessingAndSourceTime(t *testing.T) {
	for _, framing := range []string{"octet_counting", "lf"} {
		t.Run(framing, func(t *testing.T) {
			s := tcpSettings(t)
			s.TCP.Framing = framing
			records, emit := captureEmitter(8)
			r, address, _, _ := startReceiver(t, s, testOptions(emit))
			conn := dialTCP(t, address)
			wire := ""
			for range 3 {
				if framing == "lf" {
					wire += sampleSyslog + "\n"
				} else {
					wire += octetFrame(sampleSyslog)
				}
			}
			// Deliberately split the framing header and then send multiple frames.
			writeWire(t, conn, wire[:1])
			writeWire(t, conn, wire[1:])
			ids := map[string]bool{}
			for range 3 {
				e := receiveEnvelope(t, records)
				if e.MessageID == "" || ids[e.MessageID] || e.Logs == nil || len(e.Logs.Records) != 1 {
					t.Fatal("unstable/duplicate batch identity", e)
				}
				ids[e.MessageID] = true
				log := e.Logs.Records[0]
				ref := log.Observation.Source
				if log.Raw != sampleSyslog || log.Message != "hello" || log.SourceID != "approved" || log.DeviceID != "syslog:approved" || log.Host != "spoofed-host" || log.Syslog.PeerIP != "127.0.0.1" || log.Syslog.Transport != "tcp" || log.Syslog.Authenticated || !log.Syslog.Managed || log.Syslog.TLSIdentity != "" || ref.TargetID != "syslog:approved" || ref.CollectorID != "collector-test" || ref.Type != model.SourceSyslog || ref.Provenance != model.ProvenanceReal {
					t.Fatalf("source evidence or raw lost: %+v %+v", log, ref)
				}
				when := log.Observation.Time
				if when.SourceTimestamp == nil || !when.SourceTimestamp.Equal(time.Date(2026, 10, 8, 7, 0, 0, 123456000, time.UTC)) || !when.ObservedAt.Equal(log.ObservedAt) || when.CollectedAt.Before(when.ObservedAt) || when.Quality != model.ClockUnknown || when.ClockOffset != nil || when.Uncertainty != nil {
					t.Fatal("source/collector/backend clock semantics altered", when)
				}
			}
			waitReceiver(t, "three admissions", func() bool { return r.Snapshot().Admitted == 3 })
		})
	}
}

func TestReceiverAuditContextLegacyAndSimulatedProvenance(t *testing.T) {
	s := tcpSettings(t)
	s.Sources[0].Provenance = model.ProvenanceSimulated
	records, emit := captureEmitter(4)
	_, address, _, _ := startReceiver(t, s, testOptions(emit))
	conn := dialTCP(t, address)
	audit := "<14>Oct 11 22:14:15 spoofed-host audispd[42]: type=EXECVE msg=audit(1700000000.123:42): argc=1 a0=\"example\""
	ordinary := "<14>1 - spoofed-host app 42 - - type=SYSCALL msg=audit(1700000000.123:42): uid=0"
	malformed := "<14>1 - spoofed-host audispd 42 - - type=PATH msg=audit(no:42): name=\"example\""
	writeWire(t, conn, octetFrame(audit)+octetFrame(ordinary)+octetFrame(malformed))
	for i, raw := range []string{audit, ordinary, malformed} {
		log := receiveEnvelope(t, records).Logs.Records[0]
		if log.Raw != raw || log.Source != "simulated" || log.Observation.Source.Provenance != model.ProvenanceSimulated || log.OriginalTimestamp != nil || log.Observation.Time.SourceTimestamp != nil {
			t.Fatal("legacy timestamp invented or simulated provenance lost", log)
		}
		if i == 0 {
			if log.Audit == nil || log.Audit.RecordType != "EXECVE" || log.Audit.EventID != "1700000000.123:42" || log.Observation.Source.Type != model.SourceAuditd || log.Syslog.RawTimestamp != "Oct 11 22:14:15" {
				t.Fatal("configured audit context not parsed", log)
			}
		} else if log.Audit != nil || log.Observation.Source.Type != model.SourceSyslog {
			t.Fatal("ordinary/malformed text treated as audit", log)
		}
	}
}

func TestUnknownSourcesSpoofingAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, policy, error string
		sources             []Source
	}{
		{"reject", "reject", "syslog_unknown_source", nil},
		{"unmanaged", "unmanaged", "", nil},
		{"spoof hostname", "reject", "syslog_unknown_source", []Source{{ID: "spoofed-host", PeerCIDRs: []string{"192.0.2.1/32"}}}},
		{"ambiguous", "reject", "syslog_ambiguous_source", []Source{{ID: "one", PeerCIDRs: []string{"127.0.0.0/8"}}, {ID: "two", PeerCIDRs: []string{"127.0.0.1/32"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tcpSettings(t)
			s.UnknownSourcePolicy, s.Sources = tc.policy, tc.sources
			records, emit := captureEmitter(1)
			r, address, _, _ := startReceiver(t, s, testOptions(emit))
			conn := dialTCP(t, address)
			if tc.error != "" {
				assertClosed(t, conn)
				if r.Snapshot().LastError != tc.error {
					t.Fatal("wrong attribution failure", r.Snapshot())
				}
				select {
				case e := <-records:
					t.Fatal("rejected sender admitted", e)
				default:
				}
			} else {
				msg := "<14>1 - audispd audispd - - - type=SYSCALL msg=audit(1700000000:42): uid=0"
				writeWire(t, conn, octetFrame(msg))
				log := receiveEnvelope(t, records).Logs.Records[0]
				if log.Audit != nil || log.Syslog.Managed || log.Syslog.Authenticated || log.SourceID != "unmanaged" || log.Observation.Source.TargetID != "" {
					t.Fatal("unmanaged sender gained identity or audit trust", log)
				}
			}
		})
	}
}

func TestMalformedClientIsolationAndDeadlines(t *testing.T) {
	for _, tc := range []struct{ name, wire, category string }{
		{"invalid", "-1 secret\nspoof", "syslog_incomplete_or_invalid_frame"},
		{"oversized", "999999999999999 secret", "syslog_oversized_frame"},
		{"partial header", "12", "syslog_incomplete_or_invalid_frame"},
		{"partial body", "100 ab", "syslog_incomplete_or_invalid_frame"},
		{"idle", "", "syslog_idle_or_read_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tcpSettings(t)
			s.Limits.ReadTimeout, s.Limits.IdleTimeout = 80*time.Millisecond, 80*time.Millisecond
			records, emit := captureEmitter(1)
			r, address, _, _ := startReceiver(t, s, testOptions(emit))
			bad := dialTCP(t, address)
			if tc.wire != "" {
				writeWire(t, bad, tc.wire)
			}
			assertClosed(t, bad)
			if got := r.Snapshot().LastError; got != tc.category || strings.Contains(got, "secret") || strings.ContainsAny(got, "\r\n") {
				t.Fatal("unsafe/incorrect diagnostic", got)
			}
			good := dialTCP(t, address)
			writeWire(t, good, octetFrame(sampleSyslog))
			if receiveEnvelope(t, records).Logs.Records[0].Message != "hello" {
				t.Fatal("bad client stopped healthy ingestion")
			}
		})
	}
}

func TestReceiverBackpressureStableIDAndFailure(t *testing.T) {
	for _, recoverAdmission := range []bool{true, false} {
		t.Run(fmt.Sprintf("recovery=%v", recoverAdmission), func(t *testing.T) {
			s := tcpSettings(t)
			s.Limits.AdmissionTimeout = 180 * time.Millisecond
			var attempts atomic.Int32
			var admitted atomic.Bool
			var mu sync.Mutex
			var firstMessageID, firstRecordID string
			var firstEnvelope model.Envelope
			records, capture := captureEmitter(2)
			emit := func(ctx context.Context, e model.Envelope) error {
				mu.Lock()
				if firstMessageID == "" {
					firstMessageID, firstRecordID, firstEnvelope = e.MessageID, e.Logs.Records[0].ID, e
				}
				if !admitted.Load() && (firstMessageID != e.MessageID || firstRecordID != e.Logs.Records[0].ID) {
					t.Error("admission retry generated a new identity")
				}
				mu.Unlock()
				n := attempts.Add(1)
				if !recoverAdmission || n < 3 {
					return queue.ErrFull
				}
				admitted.Store(true)
				return capture(ctx, e)
			}
			r, address, _, _ := startReceiver(t, s, testOptions(emit))
			conn := dialTCP(t, address)
			writeWire(t, conn, octetFrame(sampleSyslog)+octetFrame(sampleSyslog))
			waitReceiver(t, "first rejected admission", func() bool { return attempts.Load() > 0 })
			if r.Snapshot().Frames != 1 {
				t.Fatal("read ahead during rejected admission", r.Snapshot())
			}
			if recoverAdmission {
				for range 2 {
					receiveEnvelope(t, records)
				}
				waitReceiver(t, "recovered admission", func() bool { return r.Snapshot().Admitted == 2 })
				if r.Snapshot().AdmissionFailures != 0 || attempts.Load() != 4 {
					t.Fatal("unexpected retry accounting", r.Snapshot(), attempts.Load())
				}
			} else {
				assertClosed(t, conn)
				if r.Snapshot().Frames != 1 || r.Snapshot().Admitted != 0 || r.Snapshot().AdmissionFailures != 1 || attempts.Load() > 4 {
					t.Fatal("unbounded reads/retry on full outbox", r.Snapshot(), attempts.Load())
				}
			}
			mu.Lock()
			if firstEnvelope.Logs.Records[0].Raw != sampleSyslog {
				t.Fatal("retry payload changed")
			}
			mu.Unlock()
		})
	}
}

func TestReceiverStorageFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(context.Context, model.Envelope) error
	}{
		{"storage unavailable", func(context.Context, model.Envelope) error {
			return errors.New("storage unavailable with sensitive-path")
		}},
		{"cancel blocked admission", func(ctx context.Context, _ model.Envelope) error { <-ctx.Done(); return ctx.Err() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, address, cancel, _ := startReceiver(t, tcpSettings(t), testOptions(tc.emit))
			conn := dialTCP(t, address)
			writeWire(t, conn, octetFrame(sampleSyslog))
			waitReceiver(t, "frame", func() bool { return r.Snapshot().Frames == 1 })
			if tc.name == "cancel blocked admission" {
				cancel()
			}
			assertClosed(t, conn)
			waitReceiver(t, "admission failure", func() bool { return r.Snapshot().AdmissionFailures == 1 })
			if r.Snapshot().LastError != "syslog_durable_admission_failed" {
				t.Fatal("storage error contents exposed", r.Snapshot())
			}
		})
	}
}

func TestDisabledReceiverAndBindFailure(t *testing.T) {
	s := Defaults()
	r, err := New(s, testOptions(func(context.Context, model.Envelope) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	if err = r.Run(context.Background()); err != nil || len(r.Snapshot().Listening) != 0 || r.Snapshot().ActiveConnections != 0 || runtime.NumGoroutine() > before {
		t.Fatal("disabled receiver started resources", err, r.Snapshot())
	}
	s = tcpSettings(t)
	l, err := net.Listen("tcp", s.TCP.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r, err = New(s, testOptions(func(context.Context, model.Envelope) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Run(context.Background()); err == nil || err.Error() != "syslog_listener_bind_failed" || len(r.Snapshot().Listening) > 0 {
		t.Fatal("bind failure not bounded/clean", err, r.Snapshot())
	}
}

func TestReceiverConnectionLimitAndShutdown(t *testing.T) {
	for _, perPeer := range []int{1, 2} {
		t.Run(fmt.Sprintf("per-peer=%d", perPeer), func(t *testing.T) {
			s := tcpSettings(t)
			s.Limits.MaxConnections, s.Limits.MaxConnectionsPerSource = 2, perPeer
			r, address, cancel, _ := startReceiver(t, s, testOptions(func(context.Context, model.Envelope) error { return nil }))
			for i := 0; i < perPeer; i++ {
				dialTCP(t, address)
				waitReceiver(t, "connection reservation", func() bool { return r.Snapshot().ActiveConnections == i+1 })
			}
			rejected := dialTCP(t, address)
			assertClosed(t, rejected)
			if r.Snapshot().Rejected != 1 || r.Snapshot().PeakConnections != perPeer {
				t.Fatal("connection bound exceeded", r.Snapshot())
			}
			cancel()
			waitReceiver(t, "socket cleanup", func() bool { return r.Snapshot().ActiveConnections == 0 && len(r.Snapshot().Listening) == 0 })
			l, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal("shutdown retained listening socket", err)
			}
			_ = l.Close()
		})
	}
}

func TestReceiverLogicalSourceLimitAcrossPeers(t *testing.T) {
	s := tcpSettings(t)
	s.Limits.MaxConnections, s.Limits.MaxConnectionsPerSource = 4, 1
	s.Sources[0].PeerCIDRs = []string{"192.0.2.0/24"}
	r, err := New(s, testOptions(func(context.Context, model.Envelope) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var clients []net.Conn
	for i := 1; i <= 2; i++ {
		server, client := net.Pipe()
		clients = append(clients, client)
		peer := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i))
		if !r.reserve(server, peer) {
			t.Fatal("global/per-peer reservation incorrectly rejected")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer server.Close()
			defer r.release(server, peer)
			r.serve(ctx, server, peer, "tcp", "octet_counting")
		}()
		if i == 1 {
			waitReceiver(t, "logical source reservation", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.sources["approved"] == 1 })
		} else {
			assertClosed(t, client)
		}
	}
	if r.Snapshot().Rejected != 1 || r.Snapshot().LastError != "syslog_source_connection_limit" {
		t.Fatal("logical source limit not enforced", r.Snapshot())
	}
	cancel()
	for _, client := range clients {
		_ = client.Close()
	}
	wg.Wait()
	if r.Snapshot().ActiveConnections != 0 || len(r.sources) != 0 || len(r.peers) != 0 {
		t.Fatal("logical source state leaked")
	}
}

type pkiFixtures struct {
	serverFile, keyFile, caFile string
	server                      tls.Certificate
	client, other, expired      tls.Certificate
	roots, otherRoots           *x509.CertPool
}

func createPKI(t *testing.T) pkiFixtures {
	t.Helper()
	dir := t.TempDir()
	serial := int64(0)
	create := func(name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, usages []x509.ExtKeyUsage, expired bool) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial++
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages, DNSNames: []string{name}, BasicConstraintsValid: true}
		if name == "collector.test" {
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		if name == "sender.test" {
			uri, _ := url.Parse("spiffe://example.test/host/sender")
			template.URIs = []*url.URL{uri}
		}
		if expired {
			template.NotBefore, template.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
		}
		if ca == nil {
			template.IsCA, template.KeyUsage = true, x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature
			ca, caKey = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	ca, caKey, caPEM, _ := create("ca.test", nil, nil, nil, false)
	otherCA, otherCAKey, otherPEM, _ := create("other-ca.test", nil, nil, nil, false)
	_, _, serverPEM, serverKey := create("collector.test", ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, false)
	_, _, clientPEM, clientKey := create("sender.test", ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false)
	_, _, unknownPEM, unknownKey := create("sender.test", otherCA, otherCAKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false)
	_, _, expiredPEM, expiredKey := create("sender.test", ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, true)
	load := func(certPEM, keyPEM []byte) tls.Certificate {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	f := pkiFixtures{serverFile: filepath.Join(dir, "server.crt"), keyFile: filepath.Join(dir, "server.key"), caFile: filepath.Join(dir, "clients-ca.crt"), server: load(serverPEM, serverKey), client: load(clientPEM, clientKey), other: load(unknownPEM, unknownKey), expired: load(expiredPEM, expiredKey), roots: x509.NewCertPool(), otherRoots: x509.NewCertPool()}
	f.roots.AppendCertsFromPEM(caPEM)
	f.otherRoots.AppendCertsFromPEM(otherPEM)
	for path, data := range map[string][]byte{f.serverFile: serverPEM, f.keyFile: serverKey, f.caFile: caPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func tlsSettings(t *testing.T, pki pkiFixtures) Settings {
	s := tcpSettings(t)
	s.TCP.Enabled, s.TLS.Enabled = false, true
	s.TLS.Listen, s.TLS.CertFile, s.TLS.KeyFile, s.TLS.ClientCAFile = freeAddress(t), pki.serverFile, pki.keyFile, pki.caFile
	s.Sources[0].TLSIdentity = "dns:sender.test"
	return s
}

func dialTLS(t *testing.T, address string, config *tls.Config) *tls.Conn {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTLSVerifiedIdentityAndRFC5425Framing(t *testing.T) {
	pki := createPKI(t)
	for _, identity := range []string{"dns:sender.test", "uri:spiffe://example.test/host/sender"} {
		t.Run(identity, func(t *testing.T) {
			s := tlsSettings(t, pki)
			s.Sources[0].TLSIdentity = identity
			records, emit := captureEmitter(4)
			r, address, cancel, _ := startReceiver(t, s, testOptions(emit))
			conn := dialTLS(t, address, &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.client}})
			message := sampleSyslog + "\nembedded"
			writeWire(t, conn, octetFrame(message)+octetFrame(sampleSyslog))
			for i := range 2 {
				log := receiveEnvelope(t, records).Logs.Records[0]
				if !log.Syslog.Authenticated || !log.Syslog.Managed || log.Syslog.TLSIdentity != identity || log.Syslog.Transport != "tls" || log.SourceID != "approved" || log.Host != "spoofed-host" || i == 0 && log.Message != "hello\nembedded" {
					t.Fatal("verified peer identity/framing lost", log)
				}
			}
			cancel()
			assertClosed(t, conn)
			waitReceiver(t, "TLS worker shutdown", func() bool { return r.Snapshot().ActiveConnections == 0 })
		})
	}
}

func TestTLSHandshakeFailuresAreIsolated(t *testing.T) {
	pki := createPKI(t)
	for _, tc := range []struct {
		name  string
		roots *x509.CertPool
		certs []tls.Certificate
	}{
		{"untrusted server", pki.otherRoots, []tls.Certificate{pki.client}},
		{"wrong server hostname", pki.roots, []tls.Certificate{pki.client}},
		{"missing client", pki.roots, nil},
		{"unknown client CA", pki.roots, []tls.Certificate{pki.other}},
		{"expired client", pki.roots, []tls.Certificate{pki.expired}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, emit := captureEmitter(2)
			r, address, _, _ := startReceiver(t, tlsSettings(t, pki), testOptions(emit))
			serverName := "collector.test"
			if tc.name == "wrong server hostname" {
				serverName = "wrong.test"
			}
			clientConfig := &tls.Config{RootCAs: tc.roots, ServerName: serverName, MinVersion: tls.VersionTLS12, Certificates: tc.certs}
			if len(tc.certs) == 1 {
				// Present unknown/expired certificates deliberately, even when the
				// server's acceptable-CA hint would suppress their automatic choice.
				clientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tc.certs[0], nil }
			}
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clientConfig)
			if err == nil {
				// TLS1.3 may return to the sender before the peer's client-auth alert.
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, _ = io.WriteString(conn, octetFrame(sampleSyslog))
				var b [1]byte
				_, err = conn.Read(b[:])
				_ = conn.Close()
			}
			if err == nil {
				t.Fatal("invalid TLS session succeeded")
			}
			waitReceiver(t, "TLS failure", func() bool {
				return r.Snapshot().LastError == "syslog_tls_handshake_failed" && r.Snapshot().ActiveConnections == 0
			})
			select {
			case e := <-records:
				t.Fatal("unauthenticated TLS sender admitted", e)
			default:
			}
			good := dialTLS(t, address, &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.client}})
			writeWire(t, good, octetFrame(sampleSyslog))
			receiveEnvelope(t, records)
		})
	}
}

func TestTLSServerAuthenticationRestrictedByIP(t *testing.T) {
	pki := createPKI(t)
	s := tlsSettings(t, pki)
	s.TLS.ClientAuth, s.TLS.ClientCAFile = "none", ""
	s.AllowedPeers = []string{"127.0.0.1/32"}
	s.Sources[0].TLSIdentity = ""
	records, emit := captureEmitter(1)
	_, address, _, _ := startReceiver(t, s, testOptions(emit))
	conn := dialTLS(t, address, &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12})
	writeWire(t, conn, octetFrame(sampleSyslog))
	log := receiveEnvelope(t, records).Logs.Records[0]
	if log.Syslog.Authenticated || log.Syslog.TLSIdentity != "" || !log.Syslog.Managed || log.Syslog.Transport != "tls" {
		t.Fatal("server-auth TLS incorrectly authenticated sender", log)
	}
}

func TestTLSRejectsLFFramingAndUnknownVerifiedIdentity(t *testing.T) {
	pki := createPKI(t)
	for _, identity := range []string{"dns:sender.test", "dns:other.test"} {
		t.Run(identity, func(t *testing.T) {
			s := tlsSettings(t, pki)
			s.Sources[0].TLSIdentity = identity
			r, address, _, _ := startReceiver(t, s, testOptions(func(context.Context, model.Envelope) error { t.Error("rejected TLS payload admitted"); return nil }))
			conn := dialTLS(t, address, &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.client}})
			if identity == "dns:sender.test" {
				writeWire(t, conn, sampleSyslog+"\n")
			}
			assertClosed(t, conn)
			want := "syslog_unknown_source"
			if identity == "dns:sender.test" {
				want = "syslog_incomplete_or_invalid_frame"
			}
			if r.Snapshot().LastError != want {
				t.Fatal("unexpected TLS policy/framing failure", r.Snapshot())
			}
		})
	}
}

func TestReceiverDurableRestartReplayAndApplicationACK(t *testing.T) {
	opts := queue.DurableOptions{Path: filepath.Join(t.TempDir(), "outbox.db"), MaxMessages: 8, MaxBytes: 256 << 10, MaxMessageBytes: 128 << 10, MaxStorageBytes: 4 << 20, Retention: time.Hour, BatchDelay: time.Millisecond, BatchSize: 8, MaxInflight: 4, ACKTimeout: time.Second}
	outbox, err := queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outbox.Close() })
	r, address, cancel, _ := startReceiver(t, tcpSettings(t), testOptions(func(ctx context.Context, e model.Envelope) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return outbox.Push(e)
	}))
	conn := dialTCP(t, address)
	writeWire(t, conn, octetFrame(sampleSyslog))
	waitReceiver(t, "durable admission while Backend offline", func() bool { return r.Snapshot().Admitted == 1 })
	if outbox.State().Messages != 1 {
		t.Fatal("network read did not reach durable outbox", outbox.State())
	}
	cancel()
	waitReceiver(t, "receiver shutdown", func() bool { return len(r.Snapshot().Listening) == 0 })
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	first, err := outbox.Pop(ctx)
	if err != nil || first.Logs == nil || first.Logs.Records[0].Raw != sampleSyslog {
		t.Fatal("normalized log not retained", first, err)
	}
	if err = outbox.Close(); err != nil {
		t.Fatal(err)
	}
	outbox, err = queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := outbox.Pop(ctx)
	if err != nil || replayed.MessageID != first.MessageID || replayed.Logs.Records[0].ID != first.Logs.Records[0].ID || replayed.Logs.Records[0].Raw != sampleSyslog {
		t.Fatal("retained log identity changed on restart", replayed, err)
	}
	if found, err := outbox.Acknowledge(first.MessageID); err != nil || !found || outbox.State().Messages != 0 {
		t.Fatal("application ACK did not release durable message", found, err, outbox.State())
	}
}

func TestSustainedInputFixtureResourceSample(t *testing.T) {
	const count = 1000
	var admitted atomic.Int32
	r, address, _, _ := startReceiver(t, tcpSettings(t), testOptions(func(context.Context, model.Envelope) error { admitted.Add(1); return nil }))
	conn := dialTCP(t, address)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	baselineGoroutines := runtime.NumGoroutine()
	peakGoroutines := baselineGoroutines
	started := time.Now()
	for range count / 20 {
		writeWire(t, conn, strings.Repeat(octetFrame(sampleSyslog), 20))
		peakGoroutines = max(peakGoroutines, runtime.NumGoroutine())
	}
	waitReceiver(t, "sustained fixture admission", func() bool { return admitted.Load() == count && r.Snapshot().Admitted == count })
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	if r.Snapshot().PeakConnections > 1 || r.Snapshot().Admitted != count || peakGoroutines > baselineGoroutines+8 {
		t.Fatal("per-message goroutines or connection growth", r.Snapshot(), baselineGoroutines, peakGoroutines)
	}
	t.Logf("loopback fixture only: messages=%d elapsed=%s messages_per_second=%.1f total_alloc_bytes=%d heap_alloc_before=%d heap_alloc_after=%d goroutines_baseline=%d goroutines_peak_sample=%d connections_peak=%d", count, elapsed, float64(count)/elapsed.Seconds(), after.TotalAlloc-before.TotalAlloc, before.HeapAlloc, after.HeapAlloc, baselineGoroutines, peakGoroutines, r.Snapshot().PeakConnections)
}
