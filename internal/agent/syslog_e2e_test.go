package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/syslogreceiver"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
)

func syslogTestAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func configureSyslogTCPFixture(t *testing.T, c *config.Config) {
	t.Helper()
	configureDurable(c)
	c.SyslogReceiver.Enabled = true
	c.SyslogReceiver.TLS.Enabled = false
	c.SyslogReceiver.TCP.Enabled = true
	c.SyslogReceiver.TCP.Listen = syslogTestAddress(t)
	c.SyslogReceiver.Sources = []syslogreceiver.Source{{ID: "server-a", PeerCIDRs: []string{"127.0.0.1/32"}, AuditApps: []string{"audisp-syslog"}, Provenance: model.ProvenanceSimulated}}
	c.Telemetry.HeartbeatInterval = time.Hour
}

func syslogTestSend(t *testing.T, conn net.Conn, messages ...string) {
	t.Helper()
	var framed strings.Builder
	for _, message := range messages {
		fmt.Fprintf(&framed, "%d %s", len(message), message)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, framed.String()); err != nil {
		t.Fatal(err)
	}
}

// Certificates belong to the isolated syslog security boundary, independently
// of the development gRPC transport. No host PKI or real credentials are used.
func syslogAgentCertificates(t *testing.T) (syslogreceiver.TLS, *tls.Config) {
	t.Helper()
	dir := testTempDir(t)
	caPublic, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Syslog fixture CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caPublic, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, dns string, usage x509.ExtKeyUsage) (string, string, tls.Certificate) {
		public, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			DNSNames: []string{dns}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, public, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		certFile, keyFile := filepath.Join(dir, dns+".crt"), filepath.Join(dir, dns+".key")
		if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return certFile, keyFile, pair
	}
	serverCert, serverKey, _ := issue(2, "syslog.fixture", x509.ExtKeyUsageServerAuth)
	_, _, client := issue(3, "sender.fixture", x509.ExtKeyUsageClientAuth)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("fixture CA")
	}
	return syslogreceiver.TLS{Enabled: true, Listen: syslogTestAddress(t), CertFile: serverCert, KeyFile: serverKey,
			ClientAuth: "require_and_verify", ClientCAFile: caFile},
		&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "syslog.fixture", Certificates: []tls.Certificate{client}}
}

func TestAgentSyslogTLSLocalLogsAndDurableACK(t *testing.T) {
	var mu sync.Mutex
	observed := map[string]model.Envelope{}
	var telemetry int
	mock := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		if e.Telemetry != nil {
			telemetry++
		}
		if e.Logs != nil {
			for _, record := range e.Logs.Records {
				observed[record.SourceID+":"+record.Message] = e
			}
		}
	})
	backend := startTCPController(t, "127.0.0.1:0", mock)
	c := testConfig(t, backend.listener.Addr().String())
	configureSyslogTCPFixture(t, &c)
	c.Queue.MaxMessages = 128
	c.SyslogReceiver.TCP.Enabled = false
	var senderTLS *tls.Config
	c.SyslogReceiver.TLS, senderTLS = syslogAgentCertificates(t)
	c.SyslogReceiver.Sources[0].TLSIdentity = "dns:sender.fixture"
	localPath := filepath.Join(testTempDir(t), "local.log")
	if err := os.WriteFile(localPath, []byte("local-file-message\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Logs.Sources = []config.LogSource{{ID: "local-system", Path: localPath, Format: "raw"}}
	c.Logs.Interval = 20 * time.Millisecond
	a := newTestAgent(t, c, newFixturePlatform())
	stop := runTestAgent(t, a)
	defer stop()
	awaitP2(t, func() bool { return len(a.syslog.Snapshot().Listening) == 1 })
	sender, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", c.SyslogReceiver.TLS.Listen, senderTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	system := `<14>1 2026-10-08T12:00:00+03:00 claimed-host app 12 ID [example@32473 key="two words"] remote-system-message`
	audit := `<86>1 2026-10-08T12:00:01+03:00 claimed-host audisp-syslog 13 ID - type=SYSCALL msg=audit(1791440401.123:42): uid=1000 pid=13 syscall=59 success=yes exit=0`
	legacy := `<13>Oct  8 12:00:00 legacy-host tag: remote-legacy-message`
	nonUTF8 := "<14>1 - claimed-host app 14 ID - remote-byte-message " + string([]byte{0xff})
	syslogTestSend(t, sender, system, audit, legacy, nonUTF8)
	awaitP2(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(observed) >= 5 && telemetry >= 2
	})
	mu.Lock()
	logs := make(map[string]model.Envelope, len(observed))
	for key, value := range observed {
		logs[key] = value
	}
	mu.Unlock()
	if a.syslog.Snapshot().Admitted != 4 {
		t.Fatal("remote admission mismatch", a.syslog.Snapshot())
	}
	var auditSeen, legacySeen, bytesSeen, localSeen bool
	for _, envelope := range logs {
		if envelope.Logs.Observation == nil {
			t.Fatal("batch observation lost on gRPC")
		}
		for _, record := range envelope.Logs.Records {
			if record.SourceID == "local-system" {
				localSeen = true
				if record.Message != "local-file-message" || record.FileIdentity == "" || record.SourceID == c.SyslogReceiver.Sources[0].ID {
					t.Fatal("local reader identity/contents changed", record)
				}
				continue
			}
			if record.Syslog == nil || record.Observation == nil {
				t.Fatal("remote metadata lost on gRPC")
			}
			s, o := record.Syslog, record.Observation
			if !s.Authenticated || !s.Managed || s.Transport != "tls" || s.PeerIP != "127.0.0.1" || s.TLSIdentity != "dns:sender.fixture" ||
				o.Source.TargetID != "syslog:server-a" || o.Source.CollectorID != a.id.AgentID || o.Source.Protocol != "syslog-tls" ||
				o.Source.Provenance != model.ProvenanceSimulated || record.DeviceID != "syslog:server-a" || record.Source != "simulated" {
				t.Fatal("transport source identity/provenance mismatch", record)
			}
			if !o.Time.ObservedAt.Equal(record.ObservedAt) || o.Time.CollectedAt.IsZero() || o.Time.Quality != model.ClockUnknown || o.Time.ClockOffset != nil || o.Time.Uncertainty != nil {
				t.Fatal("network clock evidence fabricated", o)
			}
			switch {
			case record.Audit != nil:
				auditSeen = record.Audit.EventID == "1791440401.123:42" && record.Audit.RecordType == "SYSCALL" && o.Source.Type == model.SourceAuditd
			case record.Message == "remote-system-message":
				want := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
				if record.Raw != system || s.StructuredData != `[example@32473 key="two words"]` || record.Host != "claimed-host" || record.OriginalTimestamp == nil || !record.OriginalTimestamp.Equal(want) || o.Time.SourceTimestamp == nil || !o.Time.SourceTimestamp.Equal(want) {
					t.Fatal("source timestamp/raw/SD lost", record)
				}
			case record.Message == "remote-legacy-message":
				legacySeen = record.OriginalTimestamp == nil && o.Time.SourceTimestamp == nil && s.RawTimestamp == "Oct  8 12:00:00"
			case strings.HasPrefix(record.Message, "remote-byte-message"):
				bytesSeen = string(s.RawPayload) == nonUTF8 && strings.Contains(record.Message, "\ufffd") && o.Time.SourceTimestamp == nil
			}
		}
		awaitP2(t, func() bool { return !a.queue.Contains(envelope.MessageID) })
	}
	if !auditSeen || !legacySeen || !bytesSeen || !localSeen {
		t.Fatal("missing normalized log domains", auditSeen, legacySeen, bytesSeen, localSeen)
	}
	stop()
	if a.syslog.Snapshot().ActiveConnections != 0 || len(a.syslog.Snapshot().Listening) != 0 {
		t.Fatal("receiver sockets survived Agent shutdown")
	}
}

func TestAgentSyslogBackendOfflineRestartStableReplay(t *testing.T) {
	backendAddress := syslogTestAddress(t)
	c := testConfig(t, backendAddress)
	configureSyslogTCPFixture(t, &c)
	c.Local.Enabled = false
	a := newTestAgent(t, c, nil)
	stop := runTestAgent(t, a)
	awaitP2(t, func() bool { return len(a.syslog.Snapshot().Listening) == 1 })
	sender, err := net.DialTimeout("tcp", c.SyslogReceiver.TCP.Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	syslogTestSend(t, sender, `<14>1 - claim app 1 ID - offline-first`, `<14>1 - claim app 2 ID - offline-second`)
	awaitP2(t, func() bool { return a.syslog.Snapshot().Admitted == 2 })
	_ = sender.Close()
	if a.Snapshot().Connected || !a.queue.IsDurable() || a.queue.State().Messages < 2 {
		t.Fatal("offline logs were not retained durably")
	}
	stop()
	retained, err := queue.NewDurable(c.Queue.DurableOptions(c.Agent.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]model.Envelope{}
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	for len(expected) < 2 {
		e, err := retained.Pop(readCtx)
		if err != nil {
			cancel()
			_ = retained.Close()
			t.Fatal("retained logs absent after restart", err)
		}
		if e.Logs != nil {
			expected[e.MessageID] = e
		}
	}
	cancel()
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
	var replayMu sync.Mutex
	replays := map[string]model.Envelope{}
	_ = startTCPController(t, backendAddress, mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Logs != nil {
			replayMu.Lock()
			replays[e.MessageID] = e
			replayMu.Unlock()
		}
	}))
	b := newTestAgent(t, c, nil)
	stopB := runTestAgent(t, b)
	defer stopB()
	awaitP2(t, func() bool {
		replayMu.Lock()
		defer replayMu.Unlock()
		return len(replays) == 2
	})
	for id, old := range expected {
		replayMu.Lock()
		replayed, exists := replays[id]
		replayMu.Unlock()
		if !exists {
			t.Fatal("retained message ID changed on replay", id)
		}
		before, after := old.Logs.Records[0], replayed.Logs.Records[0]
		if before.ID != after.ID || before.Raw != after.Raw || before.SourceID != after.SourceID || after.Syslog.Transport != "tcp" || after.Syslog.Authenticated ||
			a.id.AgentID != b.id.AgentID || a.id.SessionID == b.id.SessionID || replayed.SessionID != b.id.SessionID || after.Observation.Source.CollectorID != a.id.AgentID {
			t.Fatal("durable replay changed source/record identity", before, after)
		}
		awaitP2(t, func() bool { return !b.queue.Contains(id) })
	}
}

func TestAgentSyslogFullDurableQueueBoundsAdmissionAndShutdown(t *testing.T) {
	c := testConfig(t, syslogTestAddress(t))
	configureSyslogTCPFixture(t, &c)
	c.Local.Enabled = false
	c.Queue.MaxMessages = 4
	c.SyslogReceiver.Limits.MaxConnections = 2
	c.SyslogReceiver.Limits.MaxConnectionsPerSource = 2
	c.SyslogReceiver.Limits.AdmissionTimeout = 100 * time.Millisecond
	a := newTestAgent(t, c, nil)
	stop := runTestAgent(t, a)
	awaitP2(t, func() bool { return len(a.syslog.Snapshot().Listening) == 1 })
	for i := 0; i < c.Queue.MaxMessages; i++ {
		e, err := a.envelope(model.Envelope{CommandResult: &model.CommandResult{CommandID: fmt.Sprintf("protected-%d", i), Status: "rejected"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.queue.Push(e); err != nil {
			t.Fatal(err)
		}
	}
	sender, err := net.DialTimeout("tcp", c.SyslogReceiver.TCP.Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	syslogTestSend(t, sender, `<14>1 - claim app 1 ID - cannot-admit`, `<14>1 - claim app 2 ID - buffered-but-unread`)
	awaitP2(t, func() bool {
		return a.syslog.Snapshot().AdmissionFailures == 1 && a.syslog.Snapshot().ActiveConnections == 0
	})
	stats := a.syslog.Snapshot()
	if stats.Frames != 1 || stats.Admitted != 0 || stats.PeakConnections > 2 || a.queue.State().Messages != 4 || a.queue.State().Bytes > c.Queue.MaxBytes {
		t.Fatal("queue full continued unbounded input", stats, a.queue.State())
	}
	stop()
	if a.syslog.Snapshot().ActiveConnections != 0 {
		t.Fatal("blocked admission prevented shutdown")
	}
}

func TestAgentSyslogCorruptOutboxFailsStartupClosed(t *testing.T) {
	c := testConfig(t, "127.0.0.1:9443")
	configureSyslogTCPFixture(t, &c)
	c.Local.Enabled = false
	dir := filepath.Join(c.Agent.StateDir, "outbox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "outbox.db")
	if err := os.WriteFile(path, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	tr, err := transportgrpc.New(transportgrpc.Options{Address: c.Controller.Address, Mode: "development", MaxMessageBytes: c.Queue.MaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), c, nil, tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || a != nil || !strings.Contains(err.Error(), "requires available durable outbox") {
		t.Fatal("network ingestion silently fell back to RAM", a, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "corrupted" {
		t.Fatal("corrupt storage was rewritten", err)
	}
	listener, err := net.Listen("tcp", c.SyslogReceiver.TCP.Listen)
	if err != nil {
		t.Fatal("failed startup left listener socket open", err)
	}
	_ = listener.Close()
}
