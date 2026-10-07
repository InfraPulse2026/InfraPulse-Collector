package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"InfraPulseAgent/internal/agent"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/pki"
	"InfraPulseAgent/internal/platform"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"InfraPulseAgent/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "infrapulse-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: infrapulse-agent <run|version|validate-config|diagnostics|probe-device|apply-time-sync|time-sync-status> [--config path]")
	}
	if args[0] == "version" {
		return json.NewEncoder(os.Stdout).Encode(version.Info())
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := fs.String("config", "configs/config.example.yaml", "YAML configuration file")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	switch args[0] {
	case "apply-time-sync", "time-sync-status":
		if fs.NArg() != 0 {
			return errors.New("time synchronization commands take no positional arguments")
		}
		return timeSyncCommand(context.Background(), cfg, args[0] == "apply-time-sync", os.Stdout)
	case "validate-config":
		if fs.NArg() != 0 {
			return errors.New("validate-config takes no positional arguments")
		}
		fmt.Fprintln(os.Stdout, "configuration valid")
		return nil
	case "diagnostics":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		id, err := identity.LoadOrCreate(ctx, cfg.Agent.StateDir)
		if err != nil {
			return err
		}
		diagnostic := map[string]any{"agent_id": id.AgentID, "session_id": nil, "platform": runtime.GOOS + "/" + runtime.GOARCH, "build": version.Info(), "config_path": *path, "controller_address": cfg.Controller.Address, "tls_mode": cfg.Security.Mode, "local_enabled": cfg.Local.Enabled, "remote_device_count": len(cfg.Devices), "note": "persisted runtime snapshot; observed_at may be stale after process exit; session_id is unavailable until an agent run writes a snapshot"}
		diagnostic["time_sync"] = platform.TimeSyncStatus(ctx, cfg.Local.Platform, cfg.TimeSync)
		p := filepath.Join(cfg.Agent.StateDir, "diagnostics.json")
		f, err := os.Open(p)
		if err == nil {
			defer f.Close()
			data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
			if e != nil {
				return e
			}
			if len(data) > 1<<20 {
				return errors.New("diagnostics snapshot exceeds limit")
			}
			var snapshot agent.Diagnostics
			if e = json.Unmarshal(data, &snapshot); e != nil {
				return errors.New("invalid diagnostics snapshot")
			}
			diagnostic["runtime"] = snapshot
			diagnostic["session_id"] = snapshot.SessionID
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(diagnostic)
	case "probe-device":
		if fs.NArg() != 1 {
			return errors.New("usage: probe-device --config path device-id")
		}
		device, err := agent.ProbeDevice(context.Background(), cfg, fs.Arg(0))
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(device)
	case "run":
		if fs.NArg() != 0 {
			return errors.New("run takes no positional arguments")
		}
	default:
		return errors.New("unknown subcommand")
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var p platform.Platform
	if cfg.Local.Enabled {
		p, err = platform.NewMode(cfg.Local.Platform)
		if err != nil {
			return fmt.Errorf("%w; set local.enabled=false for collector-only development", err)
		}
	}
	transportOptions := transportgrpc.Options{Address: cfg.Controller.Address, Mode: cfg.Security.Mode, CAFile: cfg.Security.CAFile, CertFile: cfg.Security.CertFile, KeyFile: cfg.Security.KeyFile, ServerName: cfg.Security.ServerName, MaxMessageBytes: cfg.Queue.MaxMessageBytes}
	var certificates *pki.Manager
	if cfg.Security.Mode == "production" {
		id, err := identity.LoadOrCreate(ctx, cfg.Agent.StateDir)
		if err != nil {
			return err
		}
		if cfg.Security.Enrollment.Enabled {
			e := cfg.Security.Enrollment
			certificates, err = pki.New(ctx, pki.Options{AgentID: id.AgentID, StateDir: cfg.Agent.StateDir, EnrollmentURL: e.URL, BootstrapCAFile: e.CAFile, BootstrapServerName: e.ServerName, TokenEnv: e.TokenEnv, ControllerCAFile: cfg.Security.CAFile, Timeout: e.Timeout, RenewBefore: cfg.Security.RenewBefore})
			if err != nil {
				return err
			}
			transportOptions.TLSConfigProvider = func() (*tls.Config, error) { return certificates.TLSConfig(cfg.Security.ServerName) }
		} else if err = pki.VerifyCertificateFile(cfg.Security.CertFile, cfg.Security.KeyFile, id.AgentID); err != nil {
			return err
		}
	}
	t, err := transportgrpc.New(transportOptions)
	if err != nil {
		return err
	}
	a, err := agent.New(ctx, cfg, p, t, logger)
	if err != nil {
		return err
	}
	a.SetCertificateManager(certificates)
	a.SetTimeSyncStatus(platform.TimeSyncStatus(ctx, cfg.Local.Platform, cfg.TimeSync))
	return a.Run(ctx)
}
