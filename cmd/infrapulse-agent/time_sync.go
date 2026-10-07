package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/platform"
	"InfraPulseAgent/internal/platform/timesync"
)

func timeSyncCommand(ctx context.Context, cfg config.Config, apply bool, out io.Writer) error {
	if !cfg.TimeSync.Enabled {
		return json.NewEncoder(out).Encode(timesync.Disabled())
	}
	if !apply {
		return json.NewEncoder(out).Encode(platform.TimeSyncStatus(ctx, cfg.Local.Platform, cfg.TimeSync))
	}
	provider, err := platform.NewTimeSync(cfg.Local.Platform)
	if err != nil {
		return fmt.Errorf("time synchronization configuration could not be applied: %w", err)
	}
	return applyTimeSync(ctx, cfg.TimeSync, provider, out)
}

func applyTimeSync(ctx context.Context, settings timesync.Settings, provider timesync.Provider, out io.Writer) error {
	applyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s, err := provider.Apply(applyCtx, settings)
	if encodeErr := json.NewEncoder(out).Encode(s); encodeErr != nil {
		return encodeErr
	}
	if err != nil {
		return fmt.Errorf("time synchronization configuration could not be applied: %w", err)
	}
	return nil
}
