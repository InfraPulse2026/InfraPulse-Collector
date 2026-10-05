package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/telephony"
)

func TestP2HeartbeatHealthRecovery(t *testing.T) {
	port, code := sipFixture(t)
	c := testConfig(t, "127.0.0.1:9443")
	c.Telephony.Endpoints = []telephony.Endpoint{{ID: "pbx", Address: "127.0.0.1", Port: port, Timeout: 40 * time.Millisecond, FailureThreshold: 1}}
	a := newTestAgent(t, c, newFixturePlatform())
	defer a.remote.Close()
	collector := a.sip[0]
	if a.health() != "ok" {
		t.Fatal("initial collector health")
	}
	code.Store(0)
	h, _, err := collector.Collect(context.Background())
	if err != nil || h.State != "down" {
		t.Fatal(h, err)
	}
	if a.health() != "degraded" {
		t.Fatal("SIP outage missing from health")
	}
	code.Store(200)
	if _, _, err = collector.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.health() != "ok" {
		t.Fatal("SIP recovery remains degraded")
	}
}
func TestCorruptedLogCursorRejectsAgentStartup(t *testing.T) {
	dir := testTempDir(t)
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, "127.0.0.1:9443")
	c.Logs.Sources = []config.LogSource{{ID: "sys", Path: path, Format: "raw"}}
	a := newTestAgent(t, c, newFixturePlatform())
	if err := a.logs.Poll(context.Background(), "sys"); err != nil {
		t.Fatal(err)
	}
	_ = a.logs.Close()
	_ = a.remote.Close()
	files, err := filepath.Glob(filepath.Join(c.Agent.StateDir, "log-cursors", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if err = os.WriteFile(files[0], []byte("{corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = New(context.Background(), c, newFixturePlatform(), a.transport, a.logger)
	if err == nil {
		t.Fatal("corrupted cursor silently reset")
	}
}
func TestServiceEventsRequireConfirmedTransition(t *testing.T) {
	c := testConfig(t, "127.0.0.1:9443")
	a := newTestAgent(t, c, newFixturePlatform())
	defer a.remote.Close()
	at := time.Now().UTC()
	d := a.deviceSnapshot()
	d.Services = []model.Service{{Name: "dnsmasq", State: "running", Source: "ubus.service.list", ObservedAt: at}}
	a.setDevice(d)
	if a.queue.State().Messages != 0 {
		t.Fatal("initial discovery fabricated service transition")
	}
	d.Services[0].State = "stopped"
	a.setDevice(d)
	e, err := a.queue.Pop(context.Background())
	if err != nil || len(e.Events) != 1 || e.Events[0].Type != "service.stopped" {
		t.Fatal(e, err)
	}
	d.Services[0].State = "unknown"
	a.setDevice(d)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = a.queue.Pop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unknown fabricated event", err)
	}
}
