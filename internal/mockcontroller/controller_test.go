package mockcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"reflect"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestControllerReceivesAndDisplaysTopology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var output bytes.Buffer
	observed := make(chan model.Envelope, 4)
	controller := New(&output, nil, func(e model.Envelope) { observed <- e })
	server := grpc.NewServer(grpc.MaxRecvMsgSize(1 << 20))
	pb.RegisterAgentServiceServer(server, controller)
	defer server.Stop()
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	base := model.Envelope{MessageID: "hello-message", AgentID: "agent", SessionID: "session", Sequence: 1, SentAt: at}
	hello := base
	hello.Hello = &model.AgentHello{AgentID: base.AgentID, SessionID: base.SessionID, StartedAt: at, ProtocolVersion: 1,
		Device: model.Device{ID: "local:agent", Type: "unknown", Driver: "none"}}
	encoded, err := transportgrpc.Encode(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(encoded); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetHelloAck() == nil || !ack.GetHelloAck().Accepted {
		t.Fatalf("handshake: %v, %v", ack, err)
	}
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	update := model.TopologyUpdate{DeviceID: "switch-1", ObservedAt: at, CollectedAt: at, Source: "snmp-lldp", Completeness: "partial", TTLSeconds: 900,
		ExpiryAssumption: "collector polling assumption", Observations: []model.Neighbor{{LocalDeviceID: "switch-1", LocalPortNumber: 7,
			LocalPortID: []byte("unmapped-port"), LocalPortSubtype: 5, RemoteChassisID: []byte{0, 1, 2, 3, 4, 5}, RemoteChassisSubtype: 4,
			RemotePortID: []byte("remote-port"), RemotePortSubtype: 5, RemoteSystemName: "unknown-device", Protocol: "lldp", Source: "snmp-lldp",
			ObservedAt: at, ExpiresAt: at.Add(900 * time.Second), Confidence: "unresolved",
			Evidence: []model.Evidence{{Source: "local_port", Value: "no unique interface mapping", Confidence: "unresolved"}}}}}
	for i := range 2 {
		e := base
		e.MessageID, e.Sequence = "topology-message", uint64(i+2)
		if i == 1 {
			update.Completeness, update.Observations = "complete", nil
		}
		e.Topology = &update
		encoded, err := transportgrpc.Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		if err = stream.Send(encoded); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-observed:
			if got.Topology == nil || !reflect.DeepEqual(*got.Topology, update) {
				t.Fatalf("topology changed: %+v", got.Topology)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Observe runs after JSON output, so the complete receipt synchronizes reads.
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var displayed []model.Envelope
	for decoder.More() {
		var e model.Envelope
		if err := decoder.Decode(&e); err != nil {
			t.Fatal(err)
		}
		displayed = append(displayed, e)
	}
	if len(displayed) != 3 || displayed[1].Topology == nil || displayed[1].Topology.Completeness != "partial" || displayed[2].Topology.Completeness != "complete" {
		t.Fatalf("mock output omitted topology semantics: %s", output.String())
	}
	if displayed[1].Topology.Observations[0].LocalInterfaceID != "" || displayed[1].Topology.Observations[0].Confidence != "unresolved" {
		t.Fatal("Mock fabricated a local interface for unresolved observation")
	}
}
