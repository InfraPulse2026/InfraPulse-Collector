package agent

import (
	"os"
	"testing"
	"time"

	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/platform/timesync"
)

func TestTimeSyncFailureDoesNotStopMonitoring(t *testing.T) {
	for _, diagnostic := range []string{"time synchronization configuration could not be applied: " + os.ErrPermission.Error(), "service unavailable", "not synchronized"} {
		t.Run(diagnostic, func(t *testing.T) {
			observed := make(chan model.Envelope, 256)
			controller := startTCPController(t, "127.0.0.1:0", mockcontroller.New(nil, nil, observeChannel(observed)))
			c := testConfig(t, controller.listener.Addr().String())
			c.TimeSync = timesync.Settings{Enabled: true, Servers: []string{"10.10.0.5"}}
			a := newTestAgent(t, c, newFixturePlatform())
			s := timesync.Status{Enabled: true, Provider: "fixture", DesiredServers: []string{"10.10.0.5"}, Diagnostic: diagnostic, CheckedAt: time.Now().UTC()}
			a.SetTimeSyncStatus(s)
			s.DesiredServers[0] = "changed.example"
			stop := runTestAgent(t, a)
			var telemetry, heartbeat bool
			waitEnvelope(t, observed, func(e model.Envelope) bool {
				telemetry = telemetry || e.Telemetry != nil
				heartbeat = heartbeat || e.Heartbeat != nil
				return telemetry && heartbeat
			})
			stop()
			first := a.Snapshot()
			if first.TimeSync == nil || first.TimeSync.Diagnostic != diagnostic || first.TimeSync.DesiredServers[0] != "10.10.0.5" {
				t.Fatalf("time sync diagnostic lost: %+v", first.TimeSync)
			}
			first.TimeSync.DesiredServers[0] = "changed.example"
			if a.Snapshot().TimeSync.DesiredServers[0] != "10.10.0.5" {
				t.Fatal("diagnostics leaked ownership of status")
			}
		})
	}
}
