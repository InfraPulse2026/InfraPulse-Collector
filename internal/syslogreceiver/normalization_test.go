package syslogreceiver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"InfraPulseAgent/internal/model"
)

func TestNormalizationSanitizerPrivateKeysAndOpaqueOctets(t *testing.T) {
	s := tcpSettings(t)
	options := testOptions(func(context.Context, model.Envelope) error { return nil })
	options.Sanitize = func(v string) string { return strings.ReplaceAll(v, "token=secret", "token=[REDACTED]") }
	r, err := New(s, options)
	if err != nil {
		t.Fatal(err)
	}
	var privateKey bool
	for _, tc := range []struct{ raw, expected string }{
		{sampleSyslog + " token=secret", sampleSyslog + " token=[REDACTED]"},
		{"-----BEGIN PRIVATE KEY-----", "[REDACTED PRIVATE KEY]"},
		{"secret-key-material", "[REDACTED PRIVATE KEY]"},
		{"-----END PRIVATE KEY-----", "[REDACTED PRIVATE KEY]"},
		{sampleSyslog, sampleSyslog},
	} {
		e, err := r.normalize([]byte(tc.raw), &s.Sources[0], netip.MustParseAddr("127.0.0.1"), "tcp", "", false, time.Now().UTC(), &privateKey)
		if err != nil || e.Logs.Records[0].Raw != tc.expected || bytes.Contains(e.Logs.Records[0].Syslog.RawPayload, []byte("secret-key-material")) {
			t.Fatal("private material exported", e, err)
		}
	}
	frame := []byte(sampleSyslog + " \xff\x00")
	e, err := r.normalize(frame, &s.Sources[0], netip.MustParseAddr("127.0.0.1"), "tcp", "", false, time.Now().UTC(), &privateKey)
	if err != nil {
		t.Fatal(err)
	}
	log := e.Logs.Records[0]
	if !bytes.Equal(log.Syslog.RawPayload, frame) || !utf8.ValidString(log.Raw) || !utf8.ValidString(log.Message) || !strings.ContainsRune(log.Message, '\ufffd') {
		t.Fatal("non-UTF8 source octets not retained independently", log)
	}
	options.Sanitize = func(string) string { return strings.Repeat("x", s.Limits.MaxMessageBytes+1) }
	r, err = New(s, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.normalize([]byte("one"), nil, netip.MustParseAddr("127.0.0.1"), "tcp", "", false, time.Now().UTC(), &privateKey); err != ErrOversized {
		t.Fatal("unbounded sanitizer expansion accepted", err)
	}
}

func TestPeerAllowlistAndTLSHandshakeTimeout(t *testing.T) {
	s := tcpSettings(t)
	s.AllowedPeers = []string{"192.0.2.0/24"}
	r, address, _, _ := startReceiver(t, s, testOptions(func(context.Context, model.Envelope) error { t.Error("denied peer admitted"); return nil }))
	assertClosed(t, dialTCP(t, address))
	if r.Snapshot().Rejected != 1 || r.Snapshot().Accepted != 0 || r.Snapshot().LastError != "syslog_connection_rejected" {
		t.Fatal("access policy not applied before processing", r.Snapshot())
	}
	pki := createPKI(t)
	s = tlsSettings(t, pki)
	s.Limits.ReadTimeout = 80 * time.Millisecond
	r, address, _, _ = startReceiver(t, s, testOptions(func(context.Context, model.Envelope) error { t.Error("no-handshake client admitted"); return nil }))
	conn := dialTCP(t, address)
	assertClosed(t, conn)
	waitReceiver(t, "handshake timeout diagnostic", func() bool { return r.Snapshot().LastError == "syslog_tls_handshake_failed" })
	if r.Snapshot().LastError != "syslog_tls_handshake_failed" {
		t.Fatal("handshake timeout unbounded", r.Snapshot())
	}
}

func TestLoadTLSRejectsInvalidMaterialAndKeepsMinimumVersion(t *testing.T) {
	pki := createPKI(t)
	s := tlsSettings(t, pki).TLS
	config, err := LoadTLS(s)
	if err != nil || config.MinVersion != tls.VersionTLS12 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.InsecureSkipVerify {
		t.Fatal("production TLS policy incorrect", config, err)
	}
	for _, tc := range []struct{ name, cert, key, ca string }{
		{"missing certificate", "missing-cert", s.KeyFile, s.ClientCAFile},
		{"missing key", s.CertFile, "missing-key", s.ClientCAFile},
		{"missing client CA", s.CertFile, s.KeyFile, "missing-ca"},
		{"bad client CA", s.CertFile, s.KeyFile, filepath.Join(t.TempDir(), "invalid-ca")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "bad client CA" {
				if err := os.WriteFile(tc.ca, []byte("not a CA"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			bad := s
			bad.CertFile, bad.KeyFile, bad.ClientCAFile = tc.cert, tc.key, tc.ca
			if _, err := LoadTLS(bad); err == nil || strings.Contains(err.Error(), "missing-") {
				t.Fatal("invalid TLS material accepted/path leaked", err)
			}
		})
	}
	dir := t.TempDir()
	expiredCert, expiredKey := filepath.Join(dir, "expired.crt"), filepath.Join(dir, "expired.key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(pki.expired.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{expiredCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pki.expired.Certificate[0]}), expiredKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bad := s
	bad.CertFile, bad.KeyFile = expiredCert, expiredKey
	if _, err := LoadTLS(bad); err == nil || err.Error() != "syslog_tls_invalid_server_certificate" {
		t.Fatal("expired listener certificate accepted", err)
	}
	bad.CertFile = s.CertFile // valid server certificate with another private key
	if _, err := LoadTLS(bad); err == nil || err.Error() != "syslog_tls_invalid_server_key_pair" {
		t.Fatal("mismatched certificate/key pair accepted", err)
	}
}

func TestListenerStartupRollbackAndDuplicateRun(t *testing.T) {
	pki := createPKI(t)
	s := tlsSettings(t, pki)
	s.TCP.Enabled, s.TCP.Listen = true, freeAddress(t)
	busy, err := net.Listen("tcp", s.TCP.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	r, err := New(s, testOptions(func(context.Context, model.Envelope) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Run(context.Background()); err == nil || err.Error() != "syslog_listener_bind_failed" {
		t.Fatal("second listener bind failure ignored", err)
	}
	freed, err := net.Listen("tcp", s.TLS.Listen)
	if err != nil {
		t.Fatal("partially bound TLS listener not rolled back", err)
	}
	_ = freed.Close()
	r, _, _, _ = startReceiver(t, tcpSettings(t), testOptions(func(context.Context, model.Envelope) error { return nil }))
	if err = r.Run(context.Background()); err == nil || err.Error() != "syslog_receiver_already_running" {
		t.Fatal("duplicate Run not rejected", err)
	}
}
