//go:build integration && linux

package agent

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/linux"
	"InfraPulseAgent/internal/telephony"
)

// Live Linux filesystem/proc/sys and actual UDP/gRPC; SIP is a local fixture.
// This does not claim access to a production rsyslog/auditd daemon or OpenWrt.
func TestLiveLinuxLogsTelemetrySIPAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.log")
	if err := os.WriteFile(path, []byte("2026-10-05T00:00:00Z live-host daemon: before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	port, _ := sipFixture(t)
	var logs, local, sip atomic.Int32
	controller := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Logs != nil {
			logs.Add(int32(len(e.Logs.Records)))
		}
		if e.Telemetry != nil && e.Telemetry.Source == "local" && e.Telemetry.Memory != nil && e.Telemetry.Memory.TotalBytes > 0 {
			local.Add(1)
		}
		if e.Telephony != nil {
			sip.Add(1)
		}
	})
	server := startTCPController(t, "127.0.0.1:0", controller)
	c := testConfig(t, server.listener.Addr().String())
	c.Telemetry.CollectTimeout = time.Second
	c.Logs.Sources = []config.LogSource{{ID: "live", Path: path, Format: "syslog"}}
	c.Logs.Interval = 30 * time.Millisecond
	c.Telephony.Endpoints = []telephony.Endpoint{{ID: "live-fixture", Address: "127.0.0.1", Port: port, Interval: 30 * time.Millisecond, Timeout: 100 * time.Millisecond}}
	runP2Agent(t, c, linux.New("/proc", "/sys"))
	awaitP2(t, func() bool { return logs.Load() > 0 && local.Load() > 0 && sip.Load() > 0 })
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("2026-10-05T00:00:01Z live-host daemon: after\n"), 0600); err != nil {
		t.Fatal(err)
	}
	awaitP2(t, func() bool { return logs.Load() >= 2 })
}
