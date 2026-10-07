package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/platform/timesync"
)

type fixtureTimeSync struct {
	applyCalled bool
	err         error
}

func (*fixtureTimeSync) Status(context.Context, timesync.Settings) (timesync.Status, error) {
	return timesync.Status{}, errors.New("unexpected status call")
}

func (p *fixtureTimeSync) Apply(ctx context.Context, s timesync.Settings) (timesync.Status, error) {
	p.applyCalled = true
	if _, ok := ctx.Deadline(); !ok {
		return timesync.Status{}, errors.New("apply is not bounded")
	}
	return timesync.Status{Enabled: s.Enabled, DesiredServers: s.Servers, Diagnostic: "fixture"}, p.err
}

func TestApplyTimeSyncReportsPermissionError(t *testing.T) {
	p := &fixtureTimeSync{err: os.ErrPermission}
	var out bytes.Buffer
	err := applyTimeSync(context.Background(), timesync.Settings{Enabled: true, Servers: []string{"10.10.0.5"}}, p, &out)
	if !p.applyCalled || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("apply result: %v", err)
	}
	var status timesync.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Diagnostic != "fixture" {
		t.Fatalf("diagnostic: %v / %s", err, out.String())
	}
}

func TestDisabledTimeSyncCommandDoesNotRequirePlatform(t *testing.T) {
	for _, apply := range []bool{false, true} {
		var out bytes.Buffer
		cfg := config.Defaults()
		cfg.Local.Platform = "invalid-should-not-be-used"
		if err := timeSyncCommand(context.Background(), cfg, apply, &out); err != nil {
			t.Fatal(err)
		}
		var status timesync.Status
		if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.Enabled || status.Changed {
			t.Fatalf("disabled result: %v / %s", err, out.String())
		}
	}
}
