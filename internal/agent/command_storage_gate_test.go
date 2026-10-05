package agent

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestDegradedDurableFallbackAlwaysRejectsMutation(t *testing.T) {
	c := testConfig(t, "127.0.0.1:9443")
	a := newTestAgent(t, c, newFixturePlatform())
	a.storageError = "durable_outbox_unavailable"
	command := model.Command{ID: "storage-gate", TargetDeviceID: "router", RequestedAt: time.Now().UTC(), Timeout: time.Second, Initiator: "human", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
	for _, enabled := range []bool{false, true} {
		a.cfg.Commands.Enabled = enabled
		if err := a.executePersistentCommand(context.Background(), command); err != nil {
			t.Fatal(err)
		}
		e, err := a.queue.Pop(context.Background())
		if err != nil || e.CommandResult == nil || e.CommandResult.Status != "rejected" || e.CommandResult.ErrorCode != "storage_unavailable" || e.CommandResult.Changed {
			t.Fatal(e, err)
		}
	}
}
