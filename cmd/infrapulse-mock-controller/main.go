package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "infrapulse-mock-controller:", err)
		os.Exit(1)
	}
}
func run() error {
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
