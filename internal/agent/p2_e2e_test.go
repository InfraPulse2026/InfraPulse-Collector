package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform"
	"InfraPulseAgent/internal/telephony"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
)

func sipFixture(t *testing.T) (uint16, *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	code := new(atomic.Int32)
	code.Store(200)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			c := code.Load()
			if c == 0 {
				continue
			}
			headers := map[string]string{}
			for _, line := range strings.Split(string(buf[:n]), "\r\n") {
				k, v, ok := strings.Cut(line, ":")
				if ok {
					headers[strings.ToLower(k)] = strings.TrimSpace(v)
				}
			}
			response := fmt.Sprintf("SIP/2.0 %d Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", c, headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"])
			_, _ = conn.WriteTo([]byte(response), addr)
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	return uint16(port), code
}
func awaitP2(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("P2 observation timeout")
		case <-tick.C:
		}
	}
}
func runP2Agent(t *testing.T, c config.Config, p platform.Platform) *Agent {
	t.Helper()
	tr, err := transportgrpc.New(transportgrpc.Options{Address: c.Controller.Address, Mode: "development", MaxMessageBytes: c.Queue.MaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), c, p, tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(6 * time.Second):
			t.Error("P2 shutdown timeout")
		}
	})
	return a
}
func TestAgentLogsAuditTelephonyStreamingAndRestart(t *testing.T) {
	port, code := sipFixture(t)
	dir := t.TempDir()
	sys := filepath.Join(dir, "syslog")
	audit := filepath.Join(dir, "audit.log")
	t.Setenv("P2_SECRET", "p2-private-community-value")
	if err := os.WriteFile(sys, []byte("<14>1 2026-10-05T00:00:00Z host daemon 123 - - Authorization: Bearer hidden-token community=p2-private-community-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audit, []byte("type=SYSCALL msg=audit(1791158400.123:42): arch=c000003e syscall=59 success=yes exit=0 auid=1000 uid=0 pid=123 exe=\"/usr/bin/test\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	records := map[string]int{}
	states := map[string]int{}
	events := map[string]int{}
	var samples int
	var exposed bool
	controller := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		mu.Lock()
		defer mu.Unlock()
		data, _ := json.Marshal(e)
		if strings.Contains(string(data), "p2-private-community-value") || strings.Contains(string(data), "hidden-token") {
			exposed = true
		}
		if e.Logs != nil {
			for _, r := range e.Logs.Records {
				if len(records) < 512 {
					records[r.ID]++
				}
				if r.Audit != nil {
					records["audit:"+r.Audit.EventID]++
				}
			}
		}
		if e.Telephony != nil {
			for _, h := range e.Telephony.Targets {
				states[h.State]++
			}
		}
		for _, event := range e.Events {
			events[event.Type]++
		}
		if e.Telemetry != nil {
			samples++
		}
	})
	server := startTCPController(t, "127.0.0.1:0", controller)
	c := testConfig(t, server.listener.Addr().String())
	c.Queue.MaxMessages = 64
	c.Logs.Sources = []config.LogSource{{ID: "sys", Path: sys, Format: "syslog"}, {ID: "audit", Path: audit, Format: "auditd"}}
	c.Logs.Interval = 20 * time.Millisecond
	c.Logs.Timeout = time.Second
	c.Credentials = map[string]credentials.Reference{"log-redaction": {CommunityEnv: "P2_SECRET"}}
	c.Telephony.Endpoints = []telephony.Endpoint{{ID: "pbx", Address: "127.0.0.1", Port: port, Transport: "udp", Timeout: 60 * time.Millisecond, Interval: 30 * time.Millisecond, FailureThreshold: 2}}
	check := func(f func() bool) func() bool { return func() bool { mu.Lock(); defer mu.Unlock(); return f() } }
	a := runP2Agent(t, c, newFixturePlatform())
	awaitP2(t, check(func() bool { return len(records) >= 3 && states["up"] > 0 && samples > 0 }))
	code.Store(0)
	awaitP2(t, check(func() bool { return states["down"] > 0 && events["telephony.down"] > 0 }))
	code.Store(200)
	awaitP2(t, check(func() bool { return events["telephony.up"] > 0 }))
	if err := os.Rename(sys, sys+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sys, []byte("2026-10-05T00:00:01Z host app: rotated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	awaitP2(t, check(func() bool { return events["log.rotation"] > 0 && len(records) >= 4 }))
	data, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "p2-private-community-value") {
		t.Fatal("secret in diagnostics")
	}
	mu.Lock()
	defer mu.Unlock()
	if exposed {
		t.Fatal("log credentials exposed")
	}
	if records["audit:1791158400.123:42"] != 1 {
		t.Fatalf("audit identity not preserved: %#v", records)
	}
}

func TestAgentCursorRestartOverGRPC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "syslog")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	messages := map[string]int{}
	controller := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Logs != nil {
			mu.Lock()
			defer mu.Unlock()
			for _, r := range e.Logs.Records {
				messages[r.Message]++
			}
		}
	})
	server := startTCPController(t, "127.0.0.1:0", controller)
	c := testConfig(t, server.listener.Addr().String())
	c.Logs.Sources = []config.LogSource{{ID: "system", Path: path, Format: "raw"}}
	c.Logs.Interval = 20 * time.Millisecond
	start := func() (*Agent, context.CancelFunc, <-chan error) {
		tr, err := transportgrpc.New(transportgrpc.Options{Address: c.Controller.Address, Mode: "development", MaxMessageBytes: c.Queue.MaxMessageBytes})
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(context.Background(), c, newFixturePlatform(), tr, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		return a, cancel, done
	}
	stop := func(cancel context.CancelFunc, done <-chan error) {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("shutdown")
		}
	}
	a, cancel, done := start()
	awaitP2(t, func() bool { mu.Lock(); defer mu.Unlock(); return messages["first"] == 1 })
	stop(cancel, done)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("second\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, cancel, done := start()
	defer stop(cancel, done)
	awaitP2(t, func() bool { mu.Lock(); defer mu.Unlock(); return messages["second"] == 1 })
	if a.id.AgentID != b.id.AgentID || a.id.SessionID == b.id.SessionID {
		t.Fatal("restart identity")
	}
	mu.Lock()
	defer mu.Unlock()
	if messages["first"] != 1 {
		t.Fatal("cursor replayed admitted line on normal restart")
	}
}

func TestLogStormPreservesTelemetry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "burst")
	var lines strings.Builder
	for i := 0; i < 4000; i++ {
		lines.WriteString("burst " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path, []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var samples, batches atomic.Int32
	controller := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Telemetry != nil {
			samples.Add(1)
		}
		if e.Logs != nil {
			batches.Add(1)
		}
	})
	server := startTCPController(t, "127.0.0.1:0", controller)
	c := testConfig(t, server.listener.Addr().String())
	c.Logs.Sources = []config.LogSource{{ID: "burst", Path: path, Format: "raw"}}
	c.Logs.Interval = 10 * time.Millisecond
	c.Logs.MaxBatchRecords = 4
	a := runP2Agent(t, c, newFixturePlatform())
	awaitP2(t, func() bool { return samples.Load() >= 10 && batches.Load() >= 10 })
	if a.queue.State().Bytes > c.Queue.MaxBytes {
		t.Fatal("unbounded queue")
	}
}
