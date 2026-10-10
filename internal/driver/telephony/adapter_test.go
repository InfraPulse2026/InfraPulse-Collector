package telephony

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	sip "InfraPulseAgent/internal/telephony"
)

func fixtureSource() model.SourceRef {
	return model.SourceRef{Type: model.SourceSIP, CollectorID: "collector", TargetID: "remote:managed", Protocol: "sip", InstanceID: "managed", Provenance: model.ProvenanceReal}
}

func TestAdapterPreparesTLSBeforeApply(t *testing.T) {
	endpoint := sip.Endpoint{ID: "managed", Address: "127.0.0.1", Transport: "tls", TLS: sip.TLS{CAFile: filepath.Join(t.TempDir(), "missing.pem")}}
	if a, err := New(endpoint, fixtureSource()); err == nil {
		a.Close()
		t.Fatal("missing CA accepted during prepare")
	}
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint.TLS.CAFile = path
	if a, err := New(endpoint, fixtureSource()); err == nil {
		a.Close()
		t.Fatal("invalid CA accepted during prepare")
	}
}

func TestAdapterCloseCancelsInflightProbeAndRemainsClosed(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	a, err := New(sip.Endpoint{ID: "managed", Address: "127.0.0.1", Port: port, Timeout: time.Second}, fixtureSource())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	done := make(chan error, 1)
	go func() { _, _, err := a.CollectTelephony(context.Background()); done <- err }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = conn.ReadFrom(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close did not cancel probe: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inflight probe socket/goroutine did not finish")
	}
	if _, _, err = a.CollectTelephony(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("closed adapter started another probe")
	}
	if a.State().State != "unknown" {
		t.Fatal("cancellation produced a false SIP state transition")
	}
}
