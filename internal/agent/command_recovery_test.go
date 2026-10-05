package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
)

func persistFixtureCommandResult(t *testing.T, a *Agent, commandID string) model.Envelope {
	t.Helper()
	now := time.Now().UTC()
	c := model.Command{ID: commandID, TargetDeviceID: "fixture-router", RequestedAt: now, Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	c.IdempotencyKey = "key-" + commandID
	if _, duplicate, err := a.commandStore.Begin(c, now); err != nil || duplicate {
		t.Fatal(duplicate, err)
	}
	r := model.CommandResult{CommandID: commandID, Status: "success", StartedAt: now, FinishedAt: now, Changed: true}
	e, err := a.envelope(model.Envelope{CommandResult: &r})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.commandStore.Complete(c, e, now); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestLegacyFallbackRefillsRecoveredResultsBeyondQueueCapacity(t *testing.T) {
	for _, mode := range []string{"memory", "durable"} {
		t.Run(mode, func(t *testing.T) {
			observed := make(chan model.Envelope, 128)
			var hellos atomic.Int32
			m := mockcontroller.New(nil, nil, func(e model.Envelope) {
				if e.Hello != nil {
					hellos.Add(1)
				}
				observeChannel(observed)(e)
			})
			m.DisableACK = true
			server := startTCPController(t, "127.0.0.1:0", m)
			cfg := testConfig(t, server.listener.Addr().String())
			cfg.Local.Enabled, cfg.Commands.Enabled = false, true
			cfg.Queue.Mode, cfg.Queue.MaxMessages, cfg.Queue.ACKFallback = mode, 16, "send"
			a := newTestAgent(t, cfg, nil)
			const count = 20
			ids := make(map[string]bool, count)
			for i := range count {
				e := persistFixtureCommandResult(t, a, fmt.Sprintf("recovered-%d", i))
				ids[e.MessageID] = true
			}
			stop := runTestAgent(t, a)
			defer stop()
			for len(ids) > 0 {
				e := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.CommandResult != nil && ids[e.MessageID] })
				delete(ids, e.MessageID)
			}
			eventually(t, func() bool { return a.commandStore.Health().PendingResults == 0 })
			if hellos.Load() != 1 {
				t.Fatal("results needed reconnect to drain", hellos.Load())
			}
		})
	}
}

func TestCommandsDisabledAfterRestartStillRecoversPersistedResults(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:9443")
	cfg.Commands.Enabled = true
	a := newTestAgent(t, cfg, newFixturePlatform())
	e := persistFixtureCommandResult(t, a, "completed-before-commands-disabled")
	if err := a.commandStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.queue.Close(); err != nil {
		t.Fatal(err)
	}
	_ = a.remote.Close()
	cfg.Commands.Enabled = false
	b := newTestAgent(t, cfg, newFixturePlatform())
	if b.commandStore == nil || !b.commandStore.Health().Available || b.commandStore.Health().PendingResults != 1 {
		t.Fatal("disabled execution lost journal recovery")
	}
	if err := b.recoverCommandResults(); err != nil {
		t.Fatal(err)
	}
	recovered, err := b.queue.Pop(context.Background())
	if err != nil || recovered.MessageID != e.MessageID || recovered.CommandResult == nil || recovered.CommandResult.CommandID != e.CommandResult.CommandID || recovered.CommandResult.Status != "success" {
		t.Fatal(recovered, err)
	}
	if b.cfg.Commands.Enabled {
		t.Fatal("journal recovery enabled mutations")
	}
	duplicate := model.Command{ID: e.CommandResult.CommandID, TargetDeviceID: "fixture-router", RequestedAt: time.Now().UTC().Add(-24 * time.Hour), Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	if err := b.executePersistentCommand(context.Background(), duplicate); err != nil {
		t.Fatal(err)
	}
	cached, err := b.queue.Pop(context.Background())
	if err != nil || cached.MessageID != e.MessageID || cached.CommandResult == nil || cached.CommandResult.Status != "success" {
		t.Fatal("disabled/expired duplicate lost historical outcome", cached, err)
	}
	duplicate.Interface = &model.InterfaceAction{InterfaceID: "other-port", Operation: "disable"}
	if err := b.executePersistentCommand(context.Background(), duplicate); err != nil {
		t.Fatal(err)
	}
	conflict, err := b.queue.Pop(context.Background())
	if err != nil || conflict.CommandResult == nil || conflict.CommandResult.ErrorCode != "duplicate_conflict" || conflict.CommandResult.Changed {
		t.Fatal("conflicting historical duplicate accepted", conflict, err)
	}
}

func TestCorruptedCommandJournalDoesNotDisconnectMonitoringOnTelemetryACK(t *testing.T) {
	observed := make(chan model.Envelope, 128)
	var hellos atomic.Int32
	m := mockcontroller.New(nil, nil, func(e model.Envelope) {
		if e.Hello != nil {
			hellos.Add(1)
		}
		observeChannel(observed)(e)
	})
	server := startTCPController(t, "127.0.0.1:0", m)
	cfg := testConfig(t, server.listener.Addr().String())
	cfg.Commands.Enabled = true
	dir := filepath.Join(cfg.Agent.StateDir, "commands")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "safety-state.json"), []byte("corrupted-journal"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, cfg, newFixturePlatform())
	if a.commandStore == nil || a.commandStore.Health().Available {
		t.Fatal("corrupt journal unexpectedly available")
	}
	stop := runTestAgent(t, a)
	defer stop()
	for range 4 {
		e := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Heartbeat != nil })
		if e.Heartbeat.Health != "degraded" {
			t.Fatal("corrupt journal health hidden", e.Heartbeat)
		}
	}
	if hellos.Load() != 1 || !a.Snapshot().Connected {
		t.Fatal("journal corruption blocked monitoring ACK", hellos.Load(), a.Snapshot().Connected)
	}
}

func TestHistoricalIdempotencyAliasPersistsBeforeReply(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:9443")
	cfg.Commands.Enabled = true
	a := newTestAgent(t, cfg, newFixturePlatform())
	_ = persistFixtureCommandResult(t, a, "original-A")
	a.cfg.Commands.Enabled = false
	alias := model.Command{ID: "alias-B", IdempotencyKey: "key-original-A", TargetDeviceID: "fixture-router", RequestedAt: time.Now().UTC().Add(-24 * time.Hour), Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	if err := a.executePersistentCommand(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	first, err := a.queue.Pop(context.Background())
	if err != nil || first.CommandResult == nil || first.CommandResult.Status != "success" || first.CommandResult.CommandID != alias.ID {
		t.Fatal(first, err)
	}
	if err := a.commandStore.Close(); err != nil {
		t.Fatal(err)
	}
	_ = a.queue.Close()
	_ = a.remote.Close()
	cfg.Commands.Enabled = true
	b := newTestAgent(t, cfg, newFixturePlatform())
	alias.IdempotencyKey = ""
	alias.RequestedAt = time.Now().UTC()
	if err := b.executePersistentCommand(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	second, err := b.queue.Pop(context.Background())
	if err != nil || second.CommandResult == nil || second.CommandResult.Status != "success" || second.CommandResult.CommandID != alias.ID {
		t.Fatal("alias without key was admitted as a new mutation", second, err)
	}
	if b.commandStore.Health().JournalEntries != 1 {
		t.Fatal("new command execution record created for historical alias", b.commandStore.Health())
	}
}
