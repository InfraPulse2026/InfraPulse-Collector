package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/identity"
	"InfraPulseAgent/internal/managedconfig"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	transportgrpc "InfraPulseAgent/internal/transport/grpc"
	grpcgo "google.golang.org/grpc"
)

// This gate suppresses only development receipt ACKs. The real Mock still
// authenticates the stream, negotiates ACK, receives statuses and delivers the
// typed desired snapshots through its single stream sender.
type managedACKGate struct {
	pb.UnimplementedAgentServiceServer
	mock     *mockcontroller.Controller
	hold     atomic.Bool
	retained atomic.Pointer[model.Envelope]
}

type managedACKStream struct {
	grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]
	gate *managedACKGate
}

func (s *managedACKStream) Send(message *pb.ControllerEnvelope) error {
	if ack := message.GetMessageAck(); ack != nil && s.gate.hold.Load() {
		if retained := s.gate.retained.Load(); retained != nil && ack.MessageId == retained.MessageID {
			return nil
		}
	}
	return s.BidiStreamingServer.Send(message)
}

func (g *managedACKGate) Connect(stream grpcgo.BidiStreamingServer[pb.AgentEnvelope, pb.ControllerEnvelope]) error {
	return g.mock.Connect(&managedACKStream{BidiStreamingServer: stream, gate: g})
}

type managedTLSAuthority struct {
	certificate          *x509.Certificate
	key                  *ecdsa.PrivateKey
	ca, cert, privateKey string
	serial               atomic.Int64
}

func newManagedTLSAuthority(t *testing.T) *managedTLSAuthority {
	t.Helper()
	dir := testTempDir(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Phase 7 fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	a := &managedTLSAuthority{certificate: ca, key: key, ca: filepath.Join(dir, "ca.pem")}
	a.serial.Store(1)
	if err := os.WriteFile(a.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	a.cert, a.privateKey = a.issue(t, dir, "server", "")
	return a
}

func (a *managedTLSAuthority) issue(t *testing.T, dir, name, collector string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(a.serial.Add(1)), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if collector == "" {
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		leaf.DNSNames, leaf.IPAddresses = []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		binding, err := url.Parse("spiffe://infrapulse/agent/" + collector)
		if err != nil {
			t.Fatal(err)
		}
		leaf.ExtKeyUsage, leaf.URIs = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, []*url.URL{binding}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, a.certificate, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func managedTLSConfig(t *testing.T, authority *managedTLSAuthority, address string) config.Config {
	t.Helper()
	c := testConfig(t, address)
	c.Local.Enabled, c.Commands.Enabled = false, false
	id, err := identity.LoadOrCreate(context.Background(), c.Agent.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	cert, key := authority.issue(t, testTempDir(t), "collector", id.AgentID)
	c.Security = config.Security{Mode: "production", CAFile: authority.ca, CertFile: cert, KeyFile: key, ServerName: "localhost"}
	c.Controller.ConnectTimeout, c.Controller.SendTimeout = time.Second, time.Second
	c.RemoteManagement = config.RemoteManagement{Enabled: true, AllowedProtocols: []string{"snmp", "redfish"}, AllowedDestinations: []string{"127.0.0.0/8"}, MinPollingInterval: 10 * time.Millisecond, MinHardwareInterval: 10 * time.Millisecond, ApplyTimeout: 2 * time.Second}
	c.Telemetry.HeartbeatInterval, c.Telemetry.CollectTimeout = 100*time.Millisecond, 2*time.Second
	configureDurable(&c)
	c.Queue.MaxMessages, c.Queue.MaxInflight, c.Queue.ACKTimeout = 256, 256, time.Second
	return c
}

func startManagedTLSController(t *testing.T, address string, authority *managedTLSAuthority, gate *managedACKGate) *tcpController {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	options, err := transportgrpc.ServerOptions(transportgrpc.Options{Address: listener.Addr().String(), Mode: "production", CAFile: authority.ca, CertFile: authority.cert, KeyFile: authority.privateKey})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	server := grpcgo.NewServer(options...)
	pb.RegisterAgentServiceServer(server, gate)
	running := &tcpController{server: server, listener: listener, finished: make(chan error, 1)}
	go func() { running.finished <- server.Serve(listener) }()
	t.Cleanup(running.stop)
	return running
}

func newManagedTLSAgent(t *testing.T, c config.Config) *Agent {
	t.Helper()
	transport, err := transportgrpc.New(transportgrpc.Options{Address: c.Controller.Address, Mode: "production", CAFile: c.Security.CAFile, CertFile: c.Security.CertFile, KeyFile: c.Security.KeyFile, ServerName: c.Security.ServerName, MaxMessageBytes: c.Queue.MaxMessageBytes})
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), c, nil, transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = a.remote.Close()
		if a.commandStore != nil {
			_ = a.commandStore.Close()
		}
		_ = a.queue.Close()
	})
	return a
}

func managedSnapshot(t *testing.T, collector string, revision uint64, targets ...model.ManagedTargetSpec) model.DesiredCollectorConfig {
	t.Helper()
	snapshot := model.DesiredCollectorConfig{CollectorID: collector, Revision: revision, Targets: targets}
	digest, err := managedconfig.Digest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ConfigurationDigest = digest
	return snapshot
}

func sendManagedSnapshot(t *testing.T, mock *mockcontroller.Controller, collector string, snapshot model.DesiredCollectorConfig) {
	t.Helper()
	if err := mock.SendConfig(collector, snapshot); err != nil {
		t.Fatal(err)
	}
}

func waitManagedStatus(t *testing.T, observed <-chan model.Envelope, collector string, revision uint64, status string, lifecycle bool) model.ConfigApplyResult {
	t.Helper()
	received, validating := false, false
	envelope := waitEnvelope(t, observed, func(e model.Envelope) bool {
		r := e.ConfigApplyResult
		if r == nil || r.CollectorID != collector || r.Revision != revision {
			return false
		}
		received = received || r.Status == model.ConfigStatusReceived
		validating = validating || r.Status == model.ConfigStatusValidating
		if r.Status == model.ConfigStatusApplied || r.Status == model.ConfigStatusRejected || r.Status == model.ConfigStatusFailed {
			if r.Status != status {
				t.Fatalf("revision %d terminal result: %+v", revision, r)
			}
			return true
		}
		return false
	})
	if lifecycle && (!received || !validating) {
		t.Fatalf("revision %d did not report received/validating before %s", revision, status)
	}
	return *envelope.ConfigApplyResult
}

func managedSNMPInstance(t *testing.T, a *Agent, id string) driver.Adapter {
	t.Helper()
	var adapter driver.Adapter
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.remote.WithDevice(ctx, id, func(instance driver.Adapter) model.CommandResult { adapter = instance; return model.CommandResult{} }); err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestManagedConfigurationAuthenticatedMockFullLifecycle(t *testing.T) {
	authority := newManagedTLSAuthority(t)
	observed := make(chan model.Envelope, 4096)
	var initialStatusIDs sync.Map
	mock := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.ConfigApplyResult != nil {
			initialStatusIDs.Store(e.MessageID, true)
		}
		observeChannel(observed)(e)
	})
	gate := &managedACKGate{mock: mock}
	controller := startManagedTLSController(t, "127.0.0.1:0", authority, gate)
	c := managedTLSConfig(t, authority, controller.listener.Addr().String())
	snmpHost, snmpPort := phase6SNMPFixture(t)
	t.Setenv("PHASE7_SNMP_COMMUNITY", "phase6-private-community")
	t.Setenv("PHASE7_REDFISH_USER", "fixture-monitor")
	t.Setenv("PHASE7_REDFISH_PASSWORD", "private-redfish-fixture-password")
	c.Credentials = map[string]credentials.Reference{"managed-snmp": {CommunityEnv: "PHASE7_SNMP_COMMUNITY"}, "managed-redfish": {UsernameEnv: "PHASE7_REDFISH_USER", PasswordEnv: "PHASE7_REDFISH_PASSWORD"}}
	c.RemoteManagement.AllowedCredentialRefs = []string{"managed-snmp", "managed-redfish"}
	rf := newRedfishAgentFixture()
	rfServer := httptest.NewTLSServer(rf)
	t.Cleanup(rfServer.Close)
	ca := filepath.Join(testTempDir(t), "redfish.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rfServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c.RemoteManagement.TLSCARefs = map[string]string{"fixture-bmc": ca}
	a := newManagedTLSAgent(t, c)
	stopA := runTestAgent(t, a)
	hello := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Hello != nil })
	if !hello.Hello.ManagedConfigV1 || !hello.Hello.RemoteManagementEnabled || hello.Hello.AppliedConfig == nil || hello.Hello.AppliedConfig.Revision != 0 || !hello.Hello.ApplicationACK {
		t.Fatal("managed capability/initial state/ACK negotiation missing", hello.Hello)
	}
	snmp := model.ManagedTargetSpec{ID: "managed-switch", Protocol: "snmp", Enabled: true, Endpoint: snmpHost, CredentialRef: "managed-snmp", SNMP: &model.ManagedSNMPOptions{Version: "v2c", Port: snmpPort}, Polling: model.TargetPolling{FastInterval: 200 * time.Millisecond, MediumInterval: time.Hour, InventoryInterval: time.Hour, TopologyInterval: time.Hour, Timeout: time.Second}}
	revision1 := managedSnapshot(t, a.id.AgentID, 1, snmp)
	sendManagedSnapshot(t, mock, a.id.AgentID, revision1)
	first := waitManagedStatus(t, observed, a.id.AgentID, 1, model.ConfigStatusApplied, true)
	if first.AppliedRevision != 1 || first.AppliedDigest != revision1.ConfigurationDigest || first.AppliedAt.IsZero() {
		t.Fatal(first)
	}
	telemetry := waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Telemetry != nil && e.Telemetry.DeviceID == snmp.ID })
	if telemetry.Telemetry.Observation == nil || telemetry.Telemetry.Observation.Source.TargetID != "remote:"+snmp.ID || telemetry.Telemetry.Observation.Source.CollectorID != a.id.AgentID {
		t.Fatal("managed SNMP source binding lost", telemetry.Telemetry)
	}
	eventually(t, func() bool { return !a.queue.Contains(telemetry.MessageID) })
	firstAdapter := managedSNMPInstance(t, a, snmp.ID)
	snmp.Polling.FastInterval = 400 * time.Millisecond
	redfish := model.ManagedTargetSpec{ID: "managed-bmc", Protocol: "redfish", Enabled: true, Endpoint: rfServer.URL, CredentialRef: "managed-redfish", CARef: "fixture-bmc", Polling: model.TargetPolling{MediumInterval: 300 * time.Millisecond, InventoryInterval: time.Hour, Timeout: 2 * time.Second}, Redfish: &model.ManagedRedfishOptions{RequestTimeout: time.Second}}
	revision2 := managedSnapshot(t, a.id.AgentID, 2, snmp, redfish)
	sendManagedSnapshot(t, mock, a.id.AgentID, revision2)
	second := waitManagedStatus(t, observed, a.id.AgentID, 2, model.ConfigStatusApplied, true)
	if second.AppliedRevision != 2 || len(a.remote.Targets()) != 2 || a.id.SessionID != hello.SessionID || managedSNMPInstance(t, a, snmp.ID) == firstAdapter {
		t.Fatal("hot update/add did not replace only the affected instance", second, a.remote.Targets())
	}
	for _, target := range a.remote.Targets() {
		if target.ID == "remote:"+snmp.ID && target.Polling.FastInterval != 400*time.Millisecond {
			t.Fatal("runtime interval was not updated", target)
		}
	}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Inventory != nil && e.Inventory.Observation != nil && e.Inventory.Observation.Source.TargetID == "remote:"+redfish.ID && len(e.Inventory.Devices) >= 4
	})
	invalidSNMP := snmp
	invalidSNMP.Polling.FastInterval = 5 * time.Millisecond
	revision3 := managedSnapshot(t, a.id.AgentID, 3, invalidSNMP, redfish)
	sendManagedSnapshot(t, mock, a.id.AgentID, revision3)
	rejected := waitManagedStatus(t, observed, a.id.AgentID, 3, model.ConfigStatusRejected, true)
	if rejected.ErrorCode != "polling_interval_forbidden" || rejected.AppliedRevision != 2 || a.managed.Applied().Revision != 2 || len(a.remote.Targets()) != 2 {
		t.Fatal("rejected desired state changed last-known-good runtime", rejected)
	}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		return e.Telemetry != nil && e.Telemetry.DeviceID == snmp.ID && e.Telemetry.CollectedAt.After(second.AppliedAt)
	})
	controller.stop()
	stopA()
	startedRestore := time.Now()
	b := newManagedTLSAgent(t, c)
	if b.managed.Applied().Revision != 2 || b.managed.Applied().ConfigurationDigest != revision2.ConfigurationDigest || len(b.remote.Targets()) != 2 || b.id.AgentID != a.id.AgentID || b.id.SessionID == a.id.SessionID {
		t.Fatal("offline restore lost state or identity", b.Snapshot())
	}
	restoreDuration := time.Since(startedRestore)
	stopB := runTestAgent(t, b)
	eventually(t, func() bool {
		snapshot := b.Snapshot()
		if snapshot.Connected || len(snapshot.RemoteDevices) != 2 || b.queue.State().Messages < 4 {
			return false
		}
		for _, state := range snapshot.RemoteDevices {
			if state.ConnectivityState != "online" || state.Target.ID == "remote:"+redfish.ID && len(state.Devices) < 4 {
				return false
			}
		}
		return true
	})
	reconnected := make(chan model.Envelope, 4096)
	reconnectGate := &managedACKGate{}
	reconnectGate.hold.Store(true)
	reconnectMock := mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
		if e.Telemetry != nil && e.Telemetry.DeviceID == snmp.ID {
			owned := e
			reconnectGate.retained.CompareAndSwap(nil, &owned)
		}
		// Replay of old durable status is valid, but cannot prove that a new
		// duplicate delivery was processed by the current stream worker.
		if e.ConfigApplyResult != nil {
			if _, old := initialStatusIDs.Load(e.MessageID); old {
				return
			}
		}
		observeChannel(reconnected)(e)
	})
	reconnectGate.mock = reconnectMock
	reconnectController := startManagedTLSController(t, c.Controller.Address, authority, reconnectGate)
	reconnectedHello := waitEnvelope(t, reconnected, func(e model.Envelope) bool { return e.Hello != nil })
	if reconnectedHello.Hello.AppliedConfig == nil || reconnectedHello.Hello.AppliedConfig.Revision != 2 || reconnectedHello.Hello.AppliedConfig.ConfigurationDigest != revision2.ConfigurationDigest || !reconnectedHello.Hello.ManagedConfigV1 {
		t.Fatal("reconnect did not report persisted revision/digest", reconnectedHello.Hello)
	}
	beforeDuplicate := managedSNMPInstance(t, b, snmp.ID)
	beforeJobs := b.remote.Stats()
	sendManagedSnapshot(t, reconnectMock, b.id.AgentID, revision2)
	duplicate := waitManagedStatus(t, reconnected, b.id.AgentID, 2, model.ConfigStatusApplied, true)
	if duplicate.AppliedAt != second.AppliedAt || managedSNMPInstance(t, b, snmp.ID) != beforeDuplicate || len(b.remote.Targets()) != 2 || len(b.remote.Stats()) != len(beforeJobs) {
		t.Fatal("duplicate recreated instances or schedules", duplicate, b.remote.Stats())
	}
	eventually(t, func() bool { return reconnectGate.retained.Load() != nil })
	retained := *reconnectGate.retained.Load()
	if !b.queue.Contains(retained.MessageID) {
		t.Fatal("controlled receipt ACK removed retained SNMP telemetry")
	}
	revision4 := managedSnapshot(t, b.id.AgentID, 4, redfish)
	sendManagedSnapshot(t, reconnectMock, b.id.AgentID, revision4)
	removed := waitManagedStatus(t, reconnected, b.id.AgentID, 4, model.ConfigStatusApplied, true)
	if len(b.remote.Targets()) != 1 || b.remote.Targets()[0].ID != "remote:"+redfish.ID || len(b.remote.Stats()) != 1 || !b.queue.Contains(retained.MessageID) {
		t.Fatal("removal left a stale target/schedule or deleted retained telemetry", b.remote.Targets(), b.remote.Stats(), b.queue.State())
	}
	waitEnvelope(t, reconnected, func(e model.Envelope) bool {
		if e.Telemetry != nil && e.Telemetry.DeviceID == snmp.ID && e.Telemetry.CollectedAt.After(removed.AppliedAt) {
			t.Fatal("removed target continued polling", e.Telemetry)
		}
		return e.Telemetry != nil && e.Telemetry.Observation != nil && e.Telemetry.Observation.Source.TargetID == "remote:"+redfish.ID && e.Telemetry.CollectedAt.After(removed.AppliedAt)
	})
	reconnectGate.hold.Store(false)
	reconnectController.stop()
	_ = startManagedTLSController(t, c.Controller.Address, authority, reconnectGate)
	replay := waitEnvelope(t, reconnected, func(e model.Envelope) bool { return e.MessageID == retained.MessageID })
	if !reflect.DeepEqual(replay.Telemetry.Observation.Source, retained.Telemetry.Observation.Source) || replay.Telemetry.DeviceID != snmp.ID {
		t.Fatal("removed target replay changed original SourceRef", retained, replay)
	}
	eventually(t, func() bool { return !b.queue.Contains(retained.MessageID) })
	diagnostics, err := json.Marshal(b.Snapshot())
	if err != nil || strings.Contains(string(diagnostics), "phase6-private-community") || strings.Contains(string(diagnostics), "private-redfish-fixture-password") || strings.Contains(string(diagnostics), "private-redfish-fixture-token") {
		t.Fatal("managed diagnostics disclosed credential values", err)
	}
	stopB()
	if rf.writes.Load() != 0 || rf.logins.Load() != rf.logouts.Load() {
		t.Fatal("managed fixture mutated equipment or leaked Redfish sessions", rf.writes.Load(), rf.logins.Load(), rf.logouts.Load())
	}
	t.Logf("20-step fixture E2E: authenticated mTLS managed_config_v1; revision1 SNMP/durableACK; revision2 interval+Redfish hot reload; invalid3 rejected/current2 preserved; offline restart restore2=%s; reconnect hello2/digest; duplicate2 preserved adapter/schedules/appliedAt; remove4 retained/replayed/ACKed original message %s and SourceRef; no equipment writes", restoreDuration, retained.MessageID)
}

func TestManagedConfigurationTwoCollectorsIdentityAndSessionBinding(t *testing.T) {
	authority := newManagedTLSAuthority(t)
	observed := make(chan model.Envelope, 1024)
	mock := mockcontroller.New(io.Discard, nil, observeChannel(observed))
	controller := startManagedTLSController(t, "127.0.0.1:0", authority, &managedACKGate{mock: mock})
	a := newManagedTLSAgent(t, managedTLSConfig(t, authority, controller.listener.Addr().String()))
	b := newManagedTLSAgent(t, managedTLSConfig(t, authority, controller.listener.Addr().String()))
	stopA, stopB := runTestAgent(t, a), runTestAgent(t, b)
	defer stopA()
	defer stopB()
	seen := map[string]bool{}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.Hello != nil {
			seen[e.AgentID] = true
		}
		return seen[a.id.AgentID] && seen[b.id.AgentID]
	})
	forA := managedSnapshot(t, a.id.AgentID, 1)
	sendManagedSnapshot(t, mock, b.id.AgentID, forA)
	rejected := waitManagedStatus(t, observed, b.id.AgentID, 1, model.ConfigStatusRejected, false)
	if rejected.ErrorCode != "identity_mismatch" || a.managed.Applied().Revision != 0 || b.managed.Applied().Revision != 0 {
		t.Fatal("Collector A snapshot changed Collector B", rejected)
	}
	sendManagedSnapshot(t, mock, a.id.AgentID, forA)
	if applied := waitManagedStatus(t, observed, a.id.AgentID, 1, model.ConfigStatusApplied, true); applied.AppliedRevision != 1 || b.managed.Applied().Revision != 0 {
		t.Fatal("valid Collector A snapshot leaked across identities", applied)
	}
	wrongSession := managedSnapshot(t, a.id.AgentID, 2)
	wrongSession.SessionID = b.id.SessionID
	sendManagedSnapshot(t, mock, a.id.AgentID, wrongSession)
	rejected = waitManagedStatus(t, observed, a.id.AgentID, 2, model.ConfigStatusRejected, false)
	if rejected.ErrorCode != "identity_mismatch" || a.managed.Applied().Revision != 1 || b.managed.Applied().Revision != 0 {
		t.Fatal("another process session bypassed identity binding", rejected)
	}
	t.Log("two independently certified Collectors: wrong Collector and wrong session snapshots rejected; only intended Collector applied revision1")
}

// Uses the existing HTTPS RouterOS, SIP UDP and reference LANplus BMC
// fixtures through the production adapter factories and authenticated stream.
func TestManagedConfigurationRouterOSSIPIPMIFixtureLifecycle(t *testing.T) {
	authority := newManagedTLSAuthority(t)
	observed := make(chan model.Envelope, 4096)
	mock := mockcontroller.New(io.Discard, nil, observeChannel(observed))
	controller := startManagedTLSController(t, "127.0.0.1:0", authority, &managedACKGate{mock: mock})
	c := managedTLSConfig(t, authority, controller.listener.Addr().String())
	routerFixture := &remoteRouterFixture{}
	routerConfig := c
	routerServer := configureRouterFixture(t, &routerConfig, routerFixture)
	bmc := ipmitest.Start(t)
	ipmiConfig := c
	ipmiFixtureConfig(t, &ipmiConfig, bmc)
	c.Credentials = map[string]credentials.Reference{"router": routerConfig.Credentials["router"], "ipmi-fixture": ipmiConfig.Credentials["ipmi-fixture"]}
	c.RemoteManagement.AllowedProtocols = []string{"routeros", "sip", "ipmi"}
	c.RemoteManagement.AllowedCredentialRefs = []string{"router", "ipmi-fixture"}
	c.RemoteManagement.TLSCARefs = map[string]string{"fixture-router": routerConfig.Devices[0].TLS.CAFile}
	sipPort, _ := sipFixture(t)
	updatedSIPPort, updatedSIPResponse := sipFixture(t)
	updatedSIPResponse.Store(202)
	a := newManagedTLSAgent(t, c)
	stop := runTestAgent(t, a)
	defer stop()
	waitEnvelope(t, observed, func(e model.Envelope) bool { return e.Hello != nil })
	router := model.ManagedTargetSpec{ID: "managed-router", Protocol: "routeros", Enabled: true, Endpoint: routerServer.URL, CredentialRef: "router", CARef: "fixture-router", Polling: model.TargetPolling{FastInterval: 200 * time.Millisecond, MediumInterval: time.Hour, InventoryInterval: time.Hour, TopologyInterval: time.Hour, Timeout: 2 * time.Second}}
	sip := model.ManagedTargetSpec{ID: "managed-pbx", Protocol: "sip", Enabled: true, Endpoint: "127.0.0.1", Polling: model.TargetPolling{FastInterval: 200 * time.Millisecond, Timeout: 300 * time.Millisecond}, SIP: &model.ManagedSIPOptions{Port: sipPort, Transport: "udp", FromUser: "phase7-monitor", FailureThreshold: 2}}
	port, suite := bmc.Port, 17
	ipmi := model.ManagedTargetSpec{ID: "managed-ipmi", Protocol: "ipmi", Enabled: true, Endpoint: bmc.Address, CredentialRef: "ipmi-fixture", Polling: model.TargetPolling{MediumInterval: 300 * time.Millisecond, InventoryInterval: time.Hour, Timeout: 2 * time.Second}, IPMI: &model.ManagedIPMIOptions{Port: &port, CipherSuite: &suite, RequestTimeout: 200 * time.Millisecond, MaxSessionDuration: 2 * time.Second}}
	sendManagedSnapshot(t, mock, a.id.AgentID, managedSnapshot(t, a.id.AgentID, 1, router, sip, ipmi))
	waitManagedStatus(t, observed, a.id.AgentID, 1, model.ConfigStatusApplied, true)
	sources := map[string]model.SourceRef{}
	checkSource := func(metadata *model.ObservationMetadata, protocol, id string) {
		t.Helper()
		if metadata == nil || metadata.Source.CollectorID != a.id.AgentID || metadata.Source.TargetID != "remote:"+id || metadata.Source.Protocol != protocol {
			t.Fatalf("managed %s source binding lost: %+v", protocol, metadata)
		}
		if previous, ok := sources[protocol]; ok && previous.TargetID != metadata.Source.TargetID {
			t.Fatal("hot update changed target SourceRef", previous, metadata.Source)
		}
		sources[protocol] = metadata.Source
	}
	var ipmiDevice string
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.Telemetry != nil && e.Telemetry.Observation != nil {
			switch e.Telemetry.Observation.Source.Protocol {
			case "routeros":
				checkSource(e.Telemetry.Observation, "routeros", router.ID)
				if e.Telemetry.DeviceID != router.ID || len(e.Telemetry.Interfaces) != 2 {
					t.Fatal("RouterOS fixture collection missing", e.Telemetry)
				}
			case "ipmi":
				checkSource(e.Telemetry.Observation, "ipmi", ipmi.ID)
				if len(e.Telemetry.Metrics) == 0 {
					t.Fatal("LANplus fixture sensors missing")
				}
				ipmiDevice = e.Telemetry.DeviceID
			}
		}
		if e.Telephony != nil && len(e.Telephony.Targets) == 1 {
			checkSource(e.Telephony.Observation, "sip", sip.ID)
			health := e.Telephony.Targets[0]
			checkSource(health.Observation, "sip", sip.ID)
			if health.State != "up" || health.ResponseCode == nil || *health.ResponseCode != 200 {
				t.Fatal("SIP OPTIONS fixture not up", health)
			}
		}
		return len(sources) == 3
	})
	if len(a.remote.Targets()) != 3 || len(a.remote.Stats()) != 3 {
		t.Fatal("hot add did not schedule exactly three targets", a.remote.Stats())
	}
	oldRouterAdapter := managedSNMPInstance(t, a, router.ID)
	sip.SIP.Port, sip.SIP.FromUser, sip.Polling.FastInterval = updatedSIPPort, "phase7-updated-monitor", 350*time.Millisecond
	ipmi.Polling.MediumInterval, ipmi.IPMI.RequestTimeout = 500*time.Millisecond, 300*time.Millisecond
	sendManagedSnapshot(t, mock, a.id.AgentID, managedSnapshot(t, a.id.AgentID, 2, router, sip, ipmi))
	updated := waitManagedStatus(t, observed, a.id.AgentID, 2, model.ConfigStatusApplied, true)
	if managedSNMPInstance(t, a, router.ID) != oldRouterAdapter {
		t.Fatal("unmodified RouterOS adapter was recreated")
	}
	updatedProtocols := map[string]bool{}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.Telephony != nil && e.Telephony.ObservedAt.After(updated.AppliedAt) && len(e.Telephony.Targets) == 1 {
			checkSource(e.Telephony.Observation, "sip", sip.ID)
			h := e.Telephony.Targets[0]
			if h.ResponseCode == nil || *h.ResponseCode != 202 {
				t.Fatal("SIP hot update still probes previous endpoint", h)
			}
			updatedProtocols["sip"] = true
		}
		if e.Telemetry != nil && e.Telemetry.Observation != nil && e.Telemetry.Observation.Source.Protocol == "ipmi" && e.Telemetry.CollectedAt.After(updated.AppliedAt) {
			checkSource(e.Telemetry.Observation, "ipmi", ipmi.ID)
			if e.Telemetry.DeviceID != ipmiDevice {
				t.Fatal("IPMI equipment identity changed across hot update")
			}
			updatedProtocols["ipmi"] = true
		}
		return len(updatedProtocols) == 2
	})
	for _, target := range a.remote.Targets() {
		if target.ID == "remote:"+sip.ID && target.Polling.FastInterval != 350*time.Millisecond || target.ID == "remote:"+ipmi.ID && target.Polling.MediumInterval != 500*time.Millisecond {
			t.Fatal("hot update did not replace protocol polling interval", target)
		}
	}
	router.Enabled = false
	sendManagedSnapshot(t, mock, a.id.AgentID, managedSnapshot(t, a.id.AgentID, 3, router, sip, ipmi))
	disabled := waitManagedStatus(t, observed, a.id.AgentID, 3, model.ConfigStatusApplied, true)
	if len(a.remote.Targets()) != 3 || len(a.remote.Stats()) != 2 {
		t.Fatal("disabled RouterOS left an active schedule", a.remote.Stats())
	}
	for _, target := range a.remote.Targets() {
		if target.ID == "remote:"+router.ID && target.Enabled {
			t.Fatal("RouterOS target remained enabled")
		}
	}
	routerCounter := routerFixture.counter.Load()
	remaining := map[string]bool{}
	waitEnvelope(t, observed, func(e model.Envelope) bool {
		if e.Telemetry != nil && e.Telemetry.Observation != nil && e.Telemetry.CollectedAt.After(disabled.AppliedAt) {
			if e.Telemetry.Observation.Source.Protocol == "routeros" {
				t.Fatal("disabled RouterOS continued polling")
			}
			if e.Telemetry.Observation.Source.Protocol == "ipmi" {
				remaining["ipmi"] = true
			}
		}
		if e.Telephony != nil && e.Telephony.ObservedAt.After(disabled.AppliedAt) {
			remaining["sip"] = true
		}
		return len(remaining) == 2
	})
	if routerFixture.counter.Load() != routerCounter {
		t.Fatal("disabled RouterOS continued equipment requests")
	}
	// Removing a SIP target during a deliberately unanswered OPTIONS probe
	// exercises the same bounded cancellation used for other in-flight jobs.
	updatedSIPResponse.Store(0)
	eventually(t, func() bool {
		for _, state := range a.remote.Stats() {
			if state.Name == "remote:"+sip.ID && state.Running {
				return true
			}
		}
		return false
	})
	removeStarted := time.Now()
	sendManagedSnapshot(t, mock, a.id.AgentID, managedSnapshot(t, a.id.AgentID, 4))
	waitManagedStatus(t, observed, a.id.AgentID, 4, model.ConfigStatusApplied, true)
	if len(a.remote.Targets()) != 0 || len(a.remote.Stats()) != 0 || len(a.remote.Telephony()) != 0 {
		t.Fatal("removal left stale target, worker or SIP state", a.remote.Targets(), a.remote.Stats())
	}
	eventually(t, func() bool { return bmc.ActiveSessions() == 0 })
	if routerFixture.writes.Load() != 0 {
		t.Fatal("managed RouterOS monitoring invoked equipment mutations")
	}
	stop()
	t.Logf("authenticated fixture lifecycle: RouterOS/SIP/LANplus added; SIP endpoint and IPMI settings/interval hot updated with stable SourceRef/identity; RouterOS disabled without new requests; all removed during unanswered SIP poll in %s; no active jobs or BMC sessions; RouterOS equipment writes=0", time.Since(removeStarted))
}
