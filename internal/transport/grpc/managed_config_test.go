package grpc

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/transport"
	grpcgo "google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

func managedWireFixture() model.DesiredCollectorConfig {
	port, cipher := 623, 17
	return model.DesiredCollectorConfig{CollectorID: "collector", SessionID: "session", Revision: 10, ConfigurationDigest: strings.Repeat("a", 64), IssuedAt: testTime,
		Targets: []model.ManagedTargetSpec{
			{ID: "switch", Name: "Switch", Protocol: "snmp", Enabled: true, Endpoint: "192.0.2.1", CredentialRef: "snmp", Polling: model.TargetPolling{FastInterval: 3 * time.Second, Timeout: time.Second}, SNMP: &model.ManagedSNMPOptions{Version: "v3", SecurityLevel: "authPriv", AuthProtocol: "SHA256", PrivacyProtocol: "AES", ContextName: "network", Port: 161, Retries: 1}},
			{ID: "router", Protocol: "routeros", Endpoint: "https://router.example", CARef: "corporate", Enabled: true, CredentialRef: "router"},
			{ID: "pbx", Protocol: "sip", Enabled: true, Endpoint: "pbx.example", Polling: model.TargetPolling{FastInterval: 30 * time.Second, Timeout: time.Second}, SIP: &model.ManagedSIPOptions{Port: 5061, Transport: "tls", FromUser: "monitor", ToUser: "health", Domain: "pbx.example", ServerName: "pbx.example", FailureThreshold: 3, LatencyThreshold: time.Second}},
			{ID: "redfish", Protocol: "redfish", Enabled: false, Endpoint: "https://bmc.example", CredentialRef: "bmc", CARef: "bmc", Redfish: &model.ManagedRedfishOptions{Auth: "session", MaxResources: 128, MaxPages: 8, MaxRequests: 512, RequestTimeout: 5 * time.Second}},
			{ID: "ipmi", Protocol: "ipmi", Enabled: true, Endpoint: "192.0.2.2", CredentialRef: "bmc", IPMI: &model.ManagedIPMIOptions{Port: &port, CipherSuite: &cipher, Privilege: "user", Retries: 1, RequestTimeout: 2 * time.Second, MaxSessionDuration: 30 * time.Second, MaxOperations: 1024, MaxSDRRecords: 128, MaxSDRBytes: 128 << 10, MaxFRURecords: 16, MaxFRUBytes: 64 << 10, MaxPollBytes: 1 << 20, SDRCacheTTL: 5 * time.Minute}},
		}}
}

func TestManagedSnapshotTypedWireRoundtrip(t *testing.T) {
	c := managedWireFixture()
	w, err := EncodeDesiredConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	parsed := new(pb.DesiredConfigSnapshot)
	if err = proto.Unmarshal(data, parsed); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDesiredConfig(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, got) {
		t.Fatalf("typed snapshot changed: %+v", got)
	}
	zero := 0
	c.Targets[4].IPMI.CipherSuite = &zero
	w, err = EncodeDesiredConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DecodeDesiredConfig(w)
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[4].IPMI.CipherSuite == nil || *got.Targets[4].IPMI.CipherSuite != 0 {
		t.Fatal("explicit unsafe zero lost presence before policy validation")
	}
	c.Targets[4].IPMI.CipherSuite = nil
	w, err = EncodeDesiredConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DecodeDesiredConfig(w)
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[4].IPMI.CipherSuite != nil {
		t.Fatal("missing cipher was fabricated")
	}
}

func TestManagedConfigClosedSchemaAndWireBounds(t *testing.T) {
	for _, name := range []string{"top-level administrative field", "nested insecure field", "oversized", "oversized correlation", "malformed digest", "duration overflow", "bad duration", "port overflow", "nil target"} {
		t.Run(name, func(t *testing.T) {
			w, err := EncodeDesiredConfig(managedWireFixture())
			if err != nil {
				t.Fatal(err)
			}
			unknown := protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1)
			switch name {
			case "top-level administrative field":
				w.ProtoReflect().SetUnknown(unknown)
			case "nested insecure field":
				w.Targets[3].GetRedfish().ProtoReflect().SetUnknown(unknown)
			case "oversized":
				w.Targets[0].Endpoint = strings.Repeat("x", MaxManagedConfigBytes)
			case "oversized correlation":
				w.CollectorId = strings.Repeat("x", 129)
			case "malformed digest":
				w.ConfigurationDigest = strings.Repeat("A", 64)
			case "duration overflow":
				w.Targets[0].Polling.Timeout = &durationpb.Duration{Seconds: 10_000_000_000}
			case "bad duration":
				w.Targets[0].Polling.Timeout = &durationpb.Duration{Seconds: 1, Nanos: -1}
			case "port overflow":
				w.Targets[0].GetSnmp().Port = 65536
			case "nil target":
				w.Targets = append(w.Targets, nil)
			}
			if _, err := DecodeDesiredConfig(w); err == nil {
				t.Fatal("invalid configuration wire admitted")
			}
		})
	}
	c := managedWireFixture()
	c.Targets[0].SIP = &model.ManagedSIPOptions{}
	if _, err := EncodeDesiredConfig(c); err == nil {
		t.Fatal("ambiguous protocol options encoded")
	}
}

func TestManagedConfigStatusAndHelloRoundtrip(t *testing.T) {
	r := model.ConfigApplyResult{CollectorID: "agent-1", Revision: 2, ConfigurationDigest: strings.Repeat("a", 64), Status: model.ConfigStatusApplied, AppliedAt: testTime, AppliedRevision: 2, AppliedDigest: strings.Repeat("a", 64)}
	e := testEnvelope()
	e.ConfigApplyResult = &r
	w, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeEnvelope(w)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e, got) {
		t.Fatal("status roundtrip changed")
	}
	e = helloEnvelope()
	e.Hello.ManagedConfigV1 = true
	e.Hello.RemoteManagementEnabled = true
	e.Hello.AppliedConfig = &model.AppliedConfigState{Revision: 2, ConfigurationDigest: r.ConfigurationDigest, Status: model.ConfigStatusApplied, AppliedAt: testTime, TargetCount: 5}
	w, err = Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DecodeEnvelope(w)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e, got) {
		t.Fatal("capability/applied state changed")
	}
	e = testEnvelope()
	e.Heartbeat = &model.Heartbeat{Timestamp: testTime, ManagedConfig: &r}
	w, err = Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err = DecodeEnvelope(w)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(e, got) {
		t.Fatal("heartbeat config status changed")
	}
	r.ErrorMessage = strings.Repeat("x", MaxConfigStatusErrorBytes+1)
	e = testEnvelope()
	e.ConfigApplyResult = &r
	if _, err := Encode(e); err == nil {
		t.Fatal("oversized status admitted")
	}
}

func TestManagedConfigReceiveRejectsMalformedWithoutLosingACK(t *testing.T) {
	w, err := EncodeDesiredConfig(managedWireFixture())
	if err != nil {
		t.Fatal(err)
	}
	w.CollectorId = strings.Repeat("x", 4096)
	w.ConfigurationDigest = "untrusted-digest"
	w.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 999, protowire.VarintType), 1))
	address := startController(t, Options{Mode: "development"}, func(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		if err := s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_DesiredConfig{DesiredConfig: w}}); err != nil {
			return err
		}
		if err := s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_MessageAck{MessageAck: &pb.MessageACK{MessageId: "retained-record"}}}); err != nil {
			return err
		}
		<-s.Context().Done()
		return s.Context().Err()
	})
	c, err := New(Options{Address: address, Mode: "development"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Send(helloEnvelope()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if got.ConfigError != "invalid_config_wire" || got.DesiredConfig == nil || got.DesiredConfig.CollectorID != "" || got.DesiredConfig.ConfigurationDigest != "" {
		t.Fatal("malformed correlation is unbounded or not rejected")
	}
	ack, err := s.Receive()
	if err != nil || ack.ACKMessageID != "retained-record" {
		t.Fatal("malformed config interrupted application ACK", err)
	}
	if s.(transport.AuthenticatedSession).Authenticated() {
		t.Fatal("plaintext session authenticated")
	}
}

func TestManagedConfigVerifiedSessionAuthentication(t *testing.T) {
	dir := t.TempDir()
	ca := makeCA(t, dir, "managed")
	serverCert, serverKey := makeCertificate(t, dir, "controller", ca, true)
	clientCert, clientKey := makeCertificate(t, dir, "collector", ca, false)
	address := startController(t, Options{Mode: "production", CAFile: ca.path, CertFile: serverCert, KeyFile: serverKey}, func(s grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
		if _, err := s.Recv(); err != nil {
			return err
		}
		if err := s.Send(&pb.ControllerEnvelope{Payload: &pb.ControllerEnvelope_HelloAck{HelloAck: &pb.HelloAck{Accepted: true, ProtocolVersion: 1, ManagedConfigV1: true}}}); err != nil {
			return err
		}
		<-s.Context().Done()
		return s.Context().Err()
	})
	c, err := New(Options{Address: address, Mode: "production", CAFile: ca.path, CertFile: clientCert, KeyFile: clientKey, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := c.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.(transport.AuthenticatedSession).Authenticated() {
		t.Fatal("authentication claimed before verified response")
	}
	if err = s.Send(helloEnvelope()); err != nil {
		t.Fatal(err)
	}
	ack, err := s.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if !ack.ManagedConfigV1 || !s.(transport.AuthenticatedSession).Authenticated() {
		t.Fatal("verified mTLS session not attested")
	}
}
