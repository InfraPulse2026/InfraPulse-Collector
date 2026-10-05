package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
)

func TestCLIJSONAndSeparateIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  name: test-base\n  state_dir: state\ncontroller:\n  address: localhost:9443\nsecurity:\n  mode: development\nlocal:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "--scenario", "all", "--duration", "80ms", "--interval", "10ms", "--devices", "2", "--output", "json"}, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var first model.Envelope
	if err := decoder.Decode(&first); err != nil || first.Hello == nil || first.Hello.Device.Source != "simulated" {
		t.Fatal(first, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "simulated", "agent-id")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "agent-id")); !os.IsNotExist(err) {
		t.Fatal("production identity path was used", err)
	}
}

func TestCLIRejectsInvalidScenarioAndLimits(t *testing.T) {
	for _, args := range [][]string{{"--scenario", "invalid"}, {"--devices", "65"}, {"--duration", "25h"}, {"--interval", "1ms"}, {"--output", "invalid"}, {"--start", "invalid"}, {"unexpected"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestCLIControllerAcceptsMaximumValidAgentName(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: listener.Addr().String(), Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(opts...)
	pb.RegisterAgentServiceServer(server, mockcontroller.New(io.Discard, nil, nil))
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer listener.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "agent:\n  name: " + strings.Repeat("x", 128) + "\n  state_dir: state\ncontroller:\n  address: " + listener.Addr().String() + "\nsecurity:\n  mode: development\nlocal:\n  enabled: false\n"
	if err = os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err = run(ctx, []string{"--config", path, "--duration", "10ms", "--interval", "10ms", "--devices", "1", "--output", "controller"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
