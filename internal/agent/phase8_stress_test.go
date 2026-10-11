package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/managedconfig"
	"InfraPulseAgent/internal/mockcontroller"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/remote"
)

// Opt-in: real adapters talking only to isolated protocol fixtures. This is a
// Collector reliability measurement, never a production capacity/hardware claim.
// Hardware runs at 30s, inventory/topology at 5m; no accelerated BMC polling.
func TestPhase8MixedProtocolStress(t *testing.T) {
	seconds, _ := strconv.Atoi(os.Getenv("INFRAPULSE_STRESS_SECONDS"))
	if seconds == 0 {
		t.Skip("set INFRAPULSE_STRESS_SECONDS=65 for opt-in 10/50/100-target fixture stress")
	}
	if seconds < 60 || seconds > 600 {
		t.Fatal("stress duration must be 60..600 seconds per workload")
	}
	for _, count := range []int{10, 50, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) { phase8Stress(t, count, time.Duration(seconds)*time.Second) })
	}
}

func phase8Stress(t *testing.T, count int, duration time.Duration) {
	authority := newManagedTLSAuthority(t)
	address := syslogTestAddress(t) // Backend initially offline; only loopback.
	c := managedTLSConfig(t, authority, address)
	configureSyslogTCPFixture(t, &c)
	c.Telemetry.HeartbeatInterval = time.Second
	c.Controller.ReconnectInitial, c.Controller.ReconnectMax = 250*time.Millisecond, 2*time.Second
	c.Remote.Concurrency = 4
	c.Queue.MaxMessages, c.Queue.MaxInflight = 128, 16
	c.Queue.MaxBytes, c.Queue.MaxMessageBytes = 2<<20, 256<<10
	c.Queue.MaxStorageBytes = 16 << 20
	c.Queue.ACKTimeout = 2 * time.Second
	c.RemoteManagement.AllowedProtocols = []string{"snmp", "routeros", "sip", "redfish", "ipmi"}
	c.RemoteManagement.MinPollingInterval = time.Second
	c.RemoteManagement.MinHardwareInterval = 30 * time.Second
	c.RemoteManagement.ApplyTimeout = 10 * time.Second
	c.Credentials = map[string]credentials.Reference{}
	c.RemoteManagement.TLSCARefs = map[string]string{}
	merge := func(other config.Config) {
		for key, value := range other.Credentials {
			c.Credentials[key] = value
			c.RemoteManagement.AllowedCredentialRefs = append(c.RemoteManagement.AllowedCredentialRefs, key)
		}
	}

	snmpHost, snmpPort := phase6SNMPFixture(t)
	t.Setenv("PHASE8_SNMP_COMMUNITY", "phase6-private-community")
	c.Credentials["snmp-stress"] = credentials.Reference{CommunityEnv: "PHASE8_SNMP_COMMUNITY"}
	c.RemoteManagement.AllowedCredentialRefs = append(c.RemoteManagement.AllowedCredentialRefs, "snmp-stress")
	routerFixture := &remoteRouterFixture{}
	routerConfig := config.Defaults()
	routerServer := configureRouterFixture(t, &routerConfig, routerFixture)
	merge(routerConfig)
	c.RemoteManagement.TLSCARefs["router-stress"] = routerConfig.Devices[0].TLS.CAFile
	redfishFixture := newRedfishAgentFixture()
	redfishServer := httptest.NewTLSServer(redfishFixture)
	t.Cleanup(redfishServer.Close)
	ca := filepath.Join(testTempDir(t), "stress-redfish-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redfishServer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	redfishConfig := config.Defaults()
	redfishFixtureConfig(t, &redfishConfig, redfishServer.URL, ca, "session")
	merge(redfishConfig)
	c.RemoteManagement.TLSCARefs["redfish-stress"] = ca
	bmc := ipmitest.Start(t)
	ipmiConfig := config.Defaults()
	ipmiFixtureConfig(t, &ipmiConfig, bmc)
	merge(ipmiConfig)
	sipPort, sipCode := sipFixture(t)
	fast := model.TargetPolling{FastInterval: time.Second, MediumInterval: 30 * time.Second, InventoryInterval: 5 * time.Minute, TopologyInterval: 5 * time.Minute, Timeout: 3 * time.Second}
	hardware := model.TargetPolling{MediumInterval: 30 * time.Second, InventoryInterval: 5 * time.Minute, Timeout: 10 * time.Second}
	port, suite := bmc.Port, 17
	templates := []model.ManagedTargetSpec{
		{Protocol: "snmp", Endpoint: snmpHost, CredentialRef: "snmp-stress", Polling: fast, SNMP: &model.ManagedSNMPOptions{Port: snmpPort}},
		{Protocol: "routeros", Endpoint: routerServer.URL, CredentialRef: "router", CARef: "router-stress", Polling: fast},
		{Protocol: "sip", Endpoint: "127.0.0.1", Polling: model.TargetPolling{FastInterval: time.Second, Timeout: time.Second}, SIP: &model.ManagedSIPOptions{Port: sipPort, Transport: "udp"}},
		{Protocol: "redfish", Endpoint: redfishServer.URL, CredentialRef: "redfish-fixture", CARef: "redfish-stress", Polling: hardware, Redfish: &model.ManagedRedfishOptions{Auth: "session", RequestTimeout: time.Second}},
		{Protocol: "ipmi", Endpoint: bmc.Address, CredentialRef: "ipmi-fixture", Polling: hardware, IPMI: &model.ManagedIPMIOptions{Port: &port, CipherSuite: &suite, RequestTimeout: time.Second, MaxSessionDuration: 10 * time.Second}},
	}
	targets := make([]model.ManagedTargetSpec, count)
	for i := range targets {
		targets[i] = templates[i%len(templates)]
		targets[i].ID, targets[i].Enabled = fmt.Sprintf("stress-%s-%03d", targets[i].Protocol, i), true
	}
	a := newManagedTLSAgent(t, c)
	// Explicit test-only provenance marking before sanitizer/outbox; production
	// factories retain real provenance. This also covers synthetic SIP/events.
	_ = a.remote.Close()
	provider := credentials.NewEnvironment(c.Credentials)
	factory := RemoteAdapterFactory(c, provider, a.id.AgentID)
	readerTimes := new(phase8ReaderTimes)
	baseFactory := factory
	factory = func(dc config.Device) (driver.Adapter, error) {
		adapter, err := baseFactory(dc)
		if err != nil {
			return adapter, err
		}
		if d, ok := adapter.(driver.Driver); ok {
			return &phase8NetworkReader{Driver: d, times: readerTimes}, nil
		}
		if d, ok := adapter.(driver.HardwareReader); ok {
			return &phase8HardwareReader{Adapter: adapter, inventory: adapter.(driver.InventoryBatchReader), hardware: d, times: readerTimes}, nil
		}
		if d, ok := adapter.(driver.TelephonyReader); ok {
			return &phase8SIPReader{Adapter: adapter, reader: d, times: readerTimes}, nil
		}
		return adapter, nil
	}
	var admitted, rejected atomic.Uint64
	registry, err := remote.NewAdapters(c, func(dc config.Device) (driver.Adapter, error) { return factory(dc) }, func(e model.Envelope) error {
		e = phase8Simulated(e)
		err := a.push(e)
		if err == nil {
			admitted.Add(1)
		} else {
			rejected.Add(1)
		}
		return err
	}, remote.WithCollectorID(a.id.AgentID))
	if err != nil {
		t.Fatal(err)
	}
	a.remote = registry
	a.managed = managedconfig.New(c, a.id.AgentID, a.id.SessionID, registry, factory, provider)
	apply := func(revision uint64, specs []model.ManagedTargetSpec) time.Duration {
		snapshot := managedSnapshot(t, a.id.AgentID, revision, specs...)
		snapshot.SessionID = a.id.SessionID
		start := time.Now()
		result := a.managed.Apply(context.Background(), snapshot, true, "", nil)
		if result.Status != model.ConfigStatusApplied {
			t.Fatalf("apply %d: %+v", revision, result)
		}
		return time.Since(start)
	}
	initialApply := apply(1, targets)
	runtime.GC()
	var before, peak, after runtime.MemStats
	runtime.ReadMemStats(&before)
	initialGoroutines, initialFD := runtime.NumGoroutine(), phase8FDCount()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Error("cleanup shutdown deadline")
			}
		}
	}()
	runStarted := time.Now()
	go func() { done <- a.Run(ctx) }()
	awaitP2(t, func() bool { return len(a.syslog.Snapshot().Listening) > 0 })
	startupElapsed := time.Since(runStarted)
	var conn net.Conn
	conn, err = net.DialTimeout("tcp", c.SyslogReceiver.TCP.Listen, time.Second)
	if err != nil {
		cancel()
		<-done
		finished = true
		t.Fatal(err)
	}
	sendCtx, stopSender := context.WithCancel(ctx)
	senderDone := make(chan error, 1)
	var syslogWrites, syslogReconnects atomic.Uint64
	go func() {
		current := conn
		defer func() { _ = current.Close() }()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		message := "<134>1 2026-10-11T12:00:00Z fixture-host app - STRESS - simulated load"
		frame := fmt.Sprintf("%d %s", len(message), message)
		for {
			select {
			case <-sendCtx.Done():
				senderDone <- nil
				return
			case <-ticker.C:
				_ = current.SetWriteDeadline(time.Now().Add(time.Second))
				if _, e := io.WriteString(current, frame); e != nil {
					_ = current.Close()
					for sendCtx.Err() == nil {
						pause := time.NewTimer(250 * time.Millisecond)
						select {
						case <-sendCtx.Done():
							pause.Stop()
							senderDone <- nil
							return
						case <-pause.C:
						}
						fresh, dialErr := (&net.Dialer{Timeout: time.Second}).DialContext(sendCtx, "tcp", c.SyslogReceiver.TCP.Listen)
						if dialErr == nil {
							current = fresh
							syslogReconnects.Add(1)
							break
						}
					}
					continue
				}
				syslogWrites.Add(1)
			}
		}
	}()
	defer func() { stopSender(); _ = conn.Close(); cancel() }()
	var received atomic.Uint64
	var invalidProvenance atomic.Uint64
	var seenProtocols sync.Map
	var controller *tcpController
	peakGoroutines, peakFD, peakQueue, peakQueueBytes := initialGoroutines, initialFD, 0, 0
	peakRSS, peakSockets := int64(-1), -1
	var rapidApply time.Duration
	started := time.Now()
	for time.Since(started) < duration {
		elapsed := time.Since(started)
		if elapsed > duration/3 && controller == nil {
			controller = startManagedTLSController(t, address, authority, &managedACKGate{mock: mockcontroller.New(io.Discard, nil, func(e model.Envelope) {
				received.Add(1)
				metadata := phase8Metadata(e)
				if metadata != nil && strings.HasPrefix(metadata.Source.TargetID, "remote:stress-") {
					seenProtocols.Store(metadata.Source.Protocol, true)
					if metadata.Source.Provenance != model.ProvenanceSimulated {
						invalidProvenance.Add(1)
					}
				}
			})})
			sipCode.Store(503) // Safe fixture-only degraded/recovered SIP.
		}
		if elapsed > duration/2 && rapidApply == 0 {
			changed := append([]model.ManagedTargetSpec(nil), targets...)
			changed[0].Enabled = false
			rapidApply += apply(2, changed)
			rapidApply += apply(3, targets)
			rapidApply += apply(4, targets[1:])
			rapidApply += apply(5, targets)
			sipCode.Store(200)
		}
		var sample runtime.MemStats
		runtime.ReadMemStats(&sample)
		peak.HeapAlloc = max(peak.HeapAlloc, sample.HeapAlloc)
		peak.HeapInuse = max(peak.HeapInuse, sample.HeapInuse)
		peakGoroutines = max(peakGoroutines, runtime.NumGoroutine())
		peakFD = max(peakFD, phase8FDCount())
		peakRSS = max(peakRSS, phase8RSS())
		peakSockets = max(peakSockets, phase8SocketCount())
		state := a.queue.State()
		peakQueue, peakQueueBytes = max(peakQueue, state.Messages), max(peakQueueBytes, state.Bytes)
		if state.Messages > c.Queue.MaxMessages || state.Bytes > c.Queue.MaxBytes {
			t.Fatal("outbox exceeded limits", state)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopSender()
	_ = conn.Close()
	<-senderDone // Peer may have closed a pressured source; reported by receiver.
	state, receiver := a.queue.State(), a.syslog.Snapshot()
	stats := a.remote.Stats()
	var runs, errors uint64
	lastErrors := map[string]int{}
	for _, s := range stats {
		runs += s.Runs
		if s.LastError != "" {
			errors++
			lastErrors[a.sanitizeText(s.LastError)]++
		}
	}
	shutdown := time.Now()
	cancel()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown deadline")
	}
	shutdownElapsed := time.Since(shutdown)
	if controller != nil {
		controller.stop()
	}
	if a.managed.Applied().Revision != 5 || routerFixture.writes.Load() != 0 || redfishFixture.writes.Load() != 0 {
		t.Fatal("final revision or read-only invariant")
	}
	if invalidProvenance.Load() != 0 {
		t.Fatal("synthetic observation appeared real on wire")
	}
	for _, protocol := range []string{"snmp", "routeros", "sip", "redfish", "ipmi"} {
		if _, ok := seenProtocols.Load(protocol); !ok {
			t.Fatal("protocol produced no observations", protocol)
		}
	}
	if redfishFixture.logins.Load() != redfishFixture.logouts.Load() {
		t.Fatal("unbalanced Redfish sessions")
	}
	if bmc.ActiveSessions() != 0 {
		t.Fatal("active IPMI session after shutdown")
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	result := map[string]any{"targets": count, "seconds": time.Since(started).Seconds(), "initial_apply_ns": initialApply.Nanoseconds(), "rapid_apply_ns": rapidApply.Nanoseconds(), "listener_start_elapsed_ns": startupElapsed.Nanoseconds(), "idle_heap_alloc": before.HeapAlloc, "peak_heap_alloc": peak.HeapAlloc, "peak_heap_inuse": peak.HeapInuse, "final_heap_alloc": after.HeapAlloc, "total_alloc_bytes": after.TotalAlloc - before.TotalAlloc, "allocations": after.Mallocs - before.Mallocs, "goroutines_before": initialGoroutines, "goroutines_peak": peakGoroutines, "goroutines_after": runtime.NumGoroutine(), "fd_before": initialFD, "fd_peak": peakFD, "fd_after": phase8FDCount(), "queue_peak_messages": peakQueue, "queue_peak_bytes": peakQueueBytes, "admitted": admitted.Load(), "rejected": rejected.Load(), "received": received.Load(), "poll_runs": runs, "jobs_with_last_error": errors, "queue": state, "syslog": receiver, "shutdown_ns": shutdownElapsed.Nanoseconds(), "redfish_logins": redfishFixture.logins.Load(), "redfish_logouts": redfishFixture.logouts.Load()}
	data, _ := json.Marshal(result)
	result["reader_calls"], result["reader_errors"] = readerTimes.calls.Load(), readerTimes.errors.Load()
	result["reader_total_ns"], result["reader_max_ns"] = readerTimes.total.Load(), readerTimes.max.Load()
	result["rss_peak_bytes"], result["sockets_peak"], result["job_last_errors"] = peakRSS, peakSockets, lastErrors
	result["syslog_writes"], result["syslog_reconnects"] = syslogWrites.Load(), syslogReconnects.Load()
	data, _ = json.Marshal(result)
	t.Logf("PHASE8_RESOURCE %s", data)
}

type phase8ReaderTimes struct{ calls, errors, total, max atomic.Uint64 }

func (p *phase8ReaderTimes) record(start time.Time, err error) {
	n := uint64(time.Since(start))
	p.calls.Add(1)
	p.total.Add(n)
	if err != nil {
		p.errors.Add(1)
	}
	for old := p.max.Load(); n > old; old = p.max.Load() {
		if p.max.CompareAndSwap(old, n) {
			break
		}
	}
}

type phase8NetworkReader struct {
	driver.Driver
	times *phase8ReaderTimes
}

func (d *phase8NetworkReader) Probe(ctx context.Context) (value model.Device, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.Driver.Probe(ctx)
}
func (d *phase8NetworkReader) Inventory(ctx context.Context) (value model.Device, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.Driver.Inventory(ctx)
}
func (d *phase8NetworkReader) Collect(ctx context.Context) (value model.TelemetrySample, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.Driver.Collect(ctx)
}
func (d *phase8NetworkReader) Topology(ctx context.Context) (value model.TopologyUpdate, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.Driver.Topology(ctx)
}

type phase8HardwareReader struct {
	driver.Adapter
	inventory driver.InventoryBatchReader
	hardware  driver.HardwareReader
	times     *phase8ReaderTimes
}

func (d *phase8HardwareReader) InventoryBatch(ctx context.Context) (value model.InventoryUpdate, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.inventory.InventoryBatch(ctx)
}
func (d *phase8HardwareReader) Hardware(ctx context.Context) (value []model.TelemetrySample, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.hardware.Hardware(ctx)
}

type phase8SIPReader struct {
	driver.Adapter
	reader driver.TelephonyReader
	times  *phase8ReaderTimes
}

func (d *phase8SIPReader) CollectTelephony(ctx context.Context) (value model.TelephonyUpdate, events []model.Event, err error) {
	start := time.Now()
	defer func() { d.times.record(start, err) }()
	return d.reader.CollectTelephony(ctx)
}

func phase8RSS() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				value, _ := strconv.ParseInt(fields[1], 10, 64)
				return value * 1024
			}
		}
	}
	return -1
}
func phase8SocketCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		link, _ := os.Readlink("/proc/self/fd/" + entry.Name())
		if strings.HasPrefix(link, "socket:[") {
			count++
		}
	}
	return count
}

func phase8FDCount() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

func phase8Metadata(e model.Envelope) *model.ObservationMetadata {
	if e.Telemetry != nil {
		return e.Telemetry.Observation
	}
	if e.Inventory != nil {
		return e.Inventory.Observation
	}
	if e.Topology != nil {
		return e.Topology.Observation
	}
	if e.Telephony != nil {
		return e.Telephony.Observation
	}
	if len(e.Events) > 0 {
		return e.Events[0].Observation
	}
	return nil
}

func phase8Simulated(e model.Envelope) model.Envelope {
	var ref model.SourceRef
	if e.Telemetry != nil && e.Telemetry.Observation != nil {
		ref = e.Telemetry.Observation.Source
		e.Telemetry.Source = "simulated"
	}
	if e.Inventory != nil && e.Inventory.Observation != nil {
		ref = e.Inventory.Observation.Source
		e.Inventory.Observation = model.CloneObservation(e.Inventory.Observation)
		e.Inventory.Observation.Source.Provenance = model.ProvenanceSimulated
		for i := range e.Inventory.Devices {
			e.Inventory.Devices[i].Source = "simulated"
		}
	}
	if e.Topology != nil && e.Topology.Observation != nil {
		ref = e.Topology.Observation.Source
		e.Topology.Source = "simulated"
		for i := range e.Topology.Observations {
			e.Topology.Observations[i].Source = "simulated"
		}
	}
	if e.Telephony != nil && e.Telephony.Observation != nil {
		ref = e.Telephony.Observation.Source
		e.Telephony.Source = "simulated"
		for i := range e.Telephony.Targets {
			e.Telephony.Targets[i].Source = "simulated"
		}
	}
	for i := range e.Events {
		if e.Events[i].Observation != nil {
			ref = e.Events[i].Observation.Source
		}
		e.Events[i].Source = "simulated"
	}
	ref.Provenance = model.ProvenanceSimulated
	return model.ObserveEnvelope(e, ref, time.Now().UTC())
}
