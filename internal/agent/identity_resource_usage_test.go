package agent

import (
	"encoding/json"
	"runtime"
	"strconv"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

func TestResourceUsageBoundedIdentityTopologyAdmission(t *testing.T) {
	c := testConfig(t, "127.0.0.1:19443")
	c.Local.Enabled = false
	c.Queue.MaxMessages = 32
	c.Queue.MaxBytes = 1 << 20
	c.Queue.MaxMessageBytes = 256 << 10
	a := newTestAgent(t, c, nil)
	at := time.Now().UTC()
	neighbors := make([]model.Neighbor, 64)
	for i := range neighbors {
		neighbors[i] = model.Neighbor{LocalDeviceID: "switch", LocalInterfaceID: "ifindex:" + strconv.Itoa(i+1), LocalInterfaceName: "ether" + strconv.Itoa(i+1), RemoteChassisID: []byte{2, 0, 0, 0, 0, byte(i)}, RemoteChassisSubtype: 4, RemotePortID: []byte("remote-port"), RemotePortSubtype: 7, RemoteSystemName: "unresolved-neighbor", Protocol: "lldp", Source: "simulated", Confidence: "unresolved", ObservedAt: at, ExpiresAt: at.Add(time.Minute), RemoteManagementAddresses: []model.ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}}}
	}
	ref := model.SourceRef{Type: model.SourceLLDP, CollectorID: a.id.AgentID, TargetID: "remote:fixture-switch", Protocol: "snmp", Provenance: model.ProvenanceSimulated}
	envelope := model.Envelope{Topology: &model.TopologyUpdate{DeviceID: "switch", Source: "simulated", ObservedAt: at, CollectedAt: at, Completeness: "complete", TTLSeconds: 60, Observations: neighbors}}
	runtime.GC()
	var before, after, closed runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutines := runtime.NumGoroutine()
	started := time.Now()
	peakBytes, peakMessages := 0, 0
	for i := 0; i < 100; i++ {
		if err := a.push(model.ObserveEnvelope(envelope, ref, at)); err != nil {
			t.Fatal(err)
		}
		state := a.queue.State()
		peakBytes = max(peakBytes, state.Bytes)
		peakMessages = max(peakMessages, state.Messages)
		if state.Bytes > c.Queue.MaxBytes || state.Messages > c.Queue.MaxMessages {
			t.Fatal("unbounded outbox", state)
		}
	}
	elapsed := time.Since(started)
	runtime.GC()
	runtime.ReadMemStats(&after)
	state := a.queue.State()
	if state.DroppedOther == 0 {
		t.Fatal("finite pressure did not exercise priority eviction")
	}
	data, _ := json.Marshal(model.ObserveEnvelope(envelope, ref, at))
	if err := a.queue.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&closed)
	if runtime.NumGoroutine() > goroutines+1 {
		t.Fatal("admission leaked goroutines")
	}
	t.Logf("100 rounds x64 simulated neighbors: duration=%s retained_heap_with_outbox=%d retained_heap_after_close=%d total_alloc=%d goroutine_delta=%d peak_messages=%d peak_bytes=%d json_bytes=%d dropped_other=%d", elapsed, int64(after.HeapAlloc)-int64(before.HeapAlloc), int64(closed.HeapAlloc)-int64(before.HeapAlloc), after.TotalAlloc-before.TotalAlloc, runtime.NumGoroutine()-goroutines, peakMessages, peakBytes, len(data), state.DroppedOther)
}
