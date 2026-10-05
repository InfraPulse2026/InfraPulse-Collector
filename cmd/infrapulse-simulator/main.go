package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/simulator"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "infrapulse-simulator:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, writer io.Writer) error {
	fs := flag.NewFlagSet("infrapulse-simulator", flag.ContinueOnError)
	configPath := fs.String("config", "configs/config.simulator.example.yaml", "base controller/security and separate simulator identity configuration")
	scenario := fs.String("scenario", "healthy", "scenario (healthy, cpu-increase, memory-pressure, traffic-increase, interface-errors, interface-flap, device-recovery, topology-change, log-burst, audit-activity, sip-latency, sip-recovery, all)")
	duration := fs.Duration("duration", 30*time.Second, "scenario duration, maximum 24h")
	interval := fs.Duration("interval", time.Second, "logical and runtime sample interval, minimum 10ms")
	seed := fs.Int64("seed", 42, "deterministic seed")
	devices := fs.Int("devices", 4, "number of simulated devices, 1..64")
	output := fs.String("output", "json", "output: json or controller")
	start := fs.String("start", "2026-01-01T00:00:00Z", "deterministic logical timestamp anchor, RFC3339")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("simulator takes no positional arguments")
	}
	if *output != "json" && *output != "controller" {
		return errors.New("simulator output must be json or controller")
	}
	logicalStart, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		return errors.New("simulator start must be RFC3339")
	}
	generator, err := simulator.NewGenerator(simulator.Options{Scenario: *scenario, Duration: *duration, Interval: *interval, Seed: *seed, Devices: *devices, Start: logicalStart})
	if err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// The simulator never shares an identity file with a real Agent, even when
	// a caller reuses an existing base Agent configuration file.
	id, err := identity.LoadOrCreate(ctx, filepath.Join(cfg.Agent.StateDir, "simulated"))
	if err != nil {
		return err
	}
	if *output == "json" {
		err = simulator.WriteJSON(ctx, generator, id, writer)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	t, err := transportgrpc.New(transportgrpc.Options{Address: cfg.Controller.Address, Mode: cfg.Security.Mode, CAFile: cfg.Security.CAFile, CertFile: cfg.Security.CertFile, KeyFile: cfg.Security.KeyFile, ServerName: cfg.Security.ServerName, MaxMessageBytes: cfg.Queue.MaxMessageBytes})
	if err != nil {
		return err
	}
	runner, err := simulator.NewRunner(generator, id, t, simulator.RuntimeOptions{Name: cfg.Agent.Name, ConnectTimeout: cfg.Controller.ConnectTimeout, SendTimeout: cfg.Controller.SendTimeout, ReconnectInitial: cfg.Controller.ReconnectInitial, ReconnectMax: cfg.Controller.ReconnectMax, MaxMessages: cfg.Queue.MaxMessages, MaxBytes: cfg.Queue.MaxBytes, MaxMessageBytes: cfg.Queue.MaxMessageBytes})
	if err != nil {
		return err
	}
	return runner.Run(ctx)
}
