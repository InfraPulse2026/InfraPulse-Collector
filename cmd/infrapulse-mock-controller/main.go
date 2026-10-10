package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/commands"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "infrapulse-mock-controller:", err)
		os.Exit(1)
	}
}
func run() error {
	var configFiles []string
	flag.Func("desired-config", "typed DesiredConfigSnapshot protojson file; repeat flag for a scenario; collector_id and digest must be precomputed", func(path string) error {
		if path == "" || len(configFiles) >= 128 {
			return errors.New("desired-config requires a path, maximum 128 snapshots")
		}
		configFiles = append(configFiles, path)
		return nil
	})
	repeatConfig := flag.Bool("repeat-config", false, "repeat each desired configuration once to exercise idempotency")
	disconnectConfig := flag.Int("disconnect-after-config-status", 0, "disconnect once after N terminal configuration statuses to exercise reconnect")
	address := flag.String("address", "127.0.0.1:9443", "loopback development listener")
	mode := flag.String("security-mode", "development", "development or production (mTLS)")
	ca := flag.String("ca-file", "", "trusted client CA PEM")
	cert := flag.String("cert-file", "", "server certificate PEM")
	key := flag.String("key-file", "", "server private key PEM")
	action := flag.String("command", "", "typed interface.enable/disable, resource.rollback or unsupported service action")
	snapshot := flag.String("snapshot", "", "Agent-owned snapshot ID for resource.rollback")
	disableACK := flag.Bool("disable-ack", false, "development legacy Controller fixture: no application ACK")
	target := flag.String("target", "auto-local", "target device id or auto-local")
	iface := flag.String("interface", "", "interface id/name for typed command")
	service := flag.String("service", "", "service for typed command")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *disconnectConfig < 0 || *disconnectConfig > 256 {
		return errors.New("disconnect-after-config-status must be 0..256")
	}
	var desiredConfigs []model.DesiredCollectorConfig
	for _, path := range configFiles {
		f, err := os.Open(path)
		if err != nil {
			return errors.New("mock desired configuration file unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(f, transportgrpc.MaxManagedConfigBytes+1))
		_ = f.Close()
		if err != nil || len(data) > transportgrpc.MaxManagedConfigBytes {
			return errors.New("mock desired configuration file exceeds limit or cannot be read")
		}
		wire := new(pb.DesiredConfigSnapshot)
		if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(data, wire); err != nil {
			return errors.New("mock desired configuration must use the typed DesiredConfigSnapshot protojson schema")
		}
		config, err := transportgrpc.DecodeDesiredConfig(wire)
		if err != nil {
			return errors.New("mock desired configuration is not representable by managed_config_v1")
		}
		if config.CollectorID == "" || config.ConfigurationDigest == "" {
			return errors.New("mock desired configuration requires collector_id and its precomputed digest")
		}
		desiredConfigs = append(desiredConfigs, config)
	}
	var command *model.Command
	if *action != "" {
		c := model.Command{ID: "validate-template", TargetDeviceID: *target, RequestedAt: time.Now().UTC(), Timeout: 5 * time.Second, Initiator: "human"}
		parts := strings.Split(*action, ".")
		if len(parts) != 2 {
			return errors.New("invalid typed command")
		}
		switch parts[0] {
		case "resource":
			if parts[1] != "rollback" {
				return errors.New("unsupported resource operation")
			}
			c.Rollback = &model.RollbackAction{SnapshotID: *snapshot}
		case "interface":
			c.Interface = &model.InterfaceAction{InterfaceID: *iface, Operation: parts[1]}
		case "service":
			c.Service = &model.ServiceAction{Service: *service, Operation: parts[1]}
		default:
			return errors.New("unsupported typed command")
		}
		if err := commands.Validate(c, time.Now()); err != nil {
			return err
		}
		command = &c
	}
	options, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: *address, Mode: *mode, CAFile: *ca, CertFile: *cert, KeyFile: *key, MaxMessageBytes: 1 << 20})
	if err != nil {
		return err
	}
	options = append(options, grpc.MaxConcurrentStreams(16))
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpc.NewServer(options...)
	controller := mockcontroller.New(os.Stdout, command, nil)
	controller.DisableACK = *disableACK
	controller.RepeatConfig, controller.DisconnectAfterConfigStatus = *repeatConfig, *disconnectConfig
	if err = controller.SetDesiredConfigs(desiredConfigs); err != nil {
		return err
	}
	pb.RegisterAgentServiceServer(server, controller)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("development Mock Controller listening", "address", listener.Addr(), "security_mode", *mode)
	select {
	case err = <-done:
	case <-ctx.Done():
		graceful := make(chan struct{})
		go func() { server.GracefulStop(); close(graceful) }()
		select {
		case <-graceful:
		case <-time.After(3 * time.Second):
			server.Stop()
			<-graceful
		}
		err = <-done
	}
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}
