//go:build integration && linux

package agent

import (
	"io"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/linux"
)

// Real host measurements over the existing gRPC/ACK stream at the production
// default cadence. Mock acceptance does not claim Backend/PostgreSQL storage.
func TestCollectorRealLinuxDefaultThreeSecondTelemetry(t *testing.T) {
	observed := make(chan model.Envelope, 128)
	server := startTCPController(t, "127.0.0.1:0", mockcontroller.New(io.Discard, nil, observeChannel(observed)))
	c := testConfig(t, server.listener.Addr().String())
	c.Telemetry.FastInterval = config.Defaults().Telemetry.FastInterval
	c.Telemetry.InventoryInterval = 30 * time.Second
	c.Telemetry.HeartbeatInterval = 2 * time.Second
	c.Telemetry.CollectTimeout = time.Second
	c.Queue.Mode = "durable"
	a := runP2Agent(t, c, linux.New("/proc", "/sys"))
	if !a.queue.IsDurable() || c.Telemetry.FastInterval != 3*time.Second {
		t.Fatal("default cadence or durable outbox changed")
	}
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	var times []time.Time
	var hello, inventory, heartbeat bool
	for len(times) < 3 || !hello || !inventory || !heartbeat {
		select {
		case e := <-observed:
			if e.Hello != nil {
				hello = e.Hello.Device.Driver == "local-linux"
			}
			if e.Inventory != nil {
				inventory = len(e.Inventory.Devices) == 1 && e.Inventory.Devices[0].ID == a.deviceSnapshot().ID
			}
			if e.Heartbeat != nil {
				heartbeat = true
			}
			if s := e.Telemetry; s != nil {
				if s.Source != "local" || s.Memory == nil || s.Memory.TotalBytes == 0 || len(s.Interfaces) == 0 || s.DeviceID != a.deviceSnapshot().ID {
					t.Fatalf("missing real Linux telemetry: %+v", s)
				}
				times = append(times, s.ObservedAt)
			}
		case <-deadline.C:
			t.Fatal("Hello/Heartbeat/Inventory/three default telemetry samples did not arrive")
		}
	}
	for i := 1; i < len(times); i++ {
		interval := times[i].Sub(times[i-1])
		if interval < 2500*time.Millisecond || interval > 3500*time.Millisecond {
			t.Fatalf("default telemetry cadence regressed: %s", interval)
		}
		t.Logf("real telemetry interval %s", interval)
	}
	eventually(t, func() bool { return a.queue.State().Unacked == 0 })
}
