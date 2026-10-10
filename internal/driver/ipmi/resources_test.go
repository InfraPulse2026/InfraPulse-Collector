package ipmi

import (
	"context"
	"encoding/json"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/model"
)

func nativeResourceAdapter(t testing.TB, fixture *ipmitest.BMC, factory Factory) *Adapter {
	t.Helper()
	a, err := NewWithFactory(driver.Options{ID: "native-resource-bmc", Address: fixture.Address, MaxRows: 16}, fixedCredentials{}, "fixture", Options{
		Port: fixture.Port, CipherSuite: 17, Privilege: "user", Provenance: model.ProvenanceSimulated,
		Timeout: 5 * time.Second, RequestTimeout: time.Second, MaxSessionDuration: 5 * time.Second,
		MaxOperations: 128, MaxResponseBytes: 4096, MaxPollBytes: 64 << 10,
		MaxSDRRecords: 16, MaxSDRBytes: 4096, MaxFRURecords: 4, MaxFRUBytes: 4096,
	}, factory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func nativePayloadSizes(t testing.TB, inventory model.InventoryUpdate, samples []model.TelemetrySample) (int, int) {
	t.Helper()
	data, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	inventoryBytes, sampleBytes := len(data), 0
	for _, sample := range samples {
		data, err = json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		sampleBytes = max(sampleBytes, len(data))
	}
	return inventoryBytes, sampleBytes
}

// The measured path includes a fresh authenticated UDP LANplus session per
// health poll, using the native client and the reference BMC + protocol fixtures.
// Discovery is warmed separately; this is not a production BMC throughput test.
func BenchmarkHardwareNativeReferenceBMC(b *testing.B) {
	fixture := ipmitest.Start(b)
	a := nativeResourceAdapter(b, fixture, newClient)
	ctx := context.Background()
	inventory, err := a.InventoryBatch(ctx)
	if err != nil {
		b.Fatal(err)
	}
	samples, err := a.Hardware(ctx)
	if err != nil {
		b.Fatal(err)
	}
	inventoryBytes, sampleBytes := nativePayloadSizes(b, inventory, samples)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.Hardware(ctx); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if fixture.ActiveSessions() != 0 {
		b.Fatal("BMC sessions remain after completed health polls")
	}
	b.ReportMetric(float64(inventoryBytes), "inventory-json-B")
	b.ReportMetric(float64(sampleBytes), "sample-json-B")
	b.ReportMetric(float64(len(a.cache.sensors)), "sensors/poll")
}

type nativeWireStats struct {
	mu                                                  sync.Mutex
	active, peak, opened, closed                        int
	writes, reads, sentBytes, receivedBytes             int
	maxSessionWrites, maxSessionBytes, maxDatagramBytes int
}

type nativeMeasuredConn struct {
	net.Conn
	stats     *nativeWireStats
	closeOnce sync.Once
	writes    int
	bytes     int
}

func (s *nativeWireStats) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.active++
	s.opened++
	s.peak = max(s.peak, s.active)
	s.mu.Unlock()
	return &nativeMeasuredConn{Conn: conn, stats: s}, nil
}

func (c *nativeMeasuredConn) Write(packet []byte) (int, error) {
	n, err := c.Conn.Write(packet)
	c.stats.mu.Lock()
	if n > 0 {
		c.writes++
		c.stats.writes++
		c.bytes += n
		c.stats.sentBytes += n
		c.stats.maxDatagramBytes = max(c.stats.maxDatagramBytes, n)
	}
	c.stats.mu.Unlock()
	return n, err
}

func (c *nativeMeasuredConn) Read(packet []byte) (int, error) {
	n, err := c.Conn.Read(packet)
	c.stats.mu.Lock()
	if n > 0 {
		c.stats.reads++
		c.bytes += n
		c.stats.receivedBytes += n
		c.stats.maxDatagramBytes = max(c.stats.maxDatagramBytes, n)
	}
	c.stats.mu.Unlock()
	return n, err
}

func (c *nativeMeasuredConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		c.stats.mu.Lock()
		defer c.stats.mu.Unlock()
		c.stats.active--
		c.stats.closed++
		c.stats.maxSessionWrites = max(c.stats.maxSessionWrites, c.writes)
		c.stats.maxSessionBytes = max(c.stats.maxSessionBytes, c.bytes)
	})
	return err
}

func TestResourceUsageBoundedNativePolling(t *testing.T) {
	fixture := ipmitest.Start(t)
	stats := &nativeWireStats{}
	a := nativeResourceAdapter(t, fixture, func(ctx context.Context, address string, port int, credential credentials.Credential, opts SessionOptions) (Client, error) {
		return newClientWithDialer(ctx, address, port, credential, opts, stats.dial)
	})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil || inventory.Completeness != "complete" || len(inventory.Devices) != 2 {
		t.Fatalf("native inventory discovery failed: devices=%d completeness=%q err=%v", len(inventory.Devices), inventory.Completeness, err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	initialGoroutines := runtime.NumGoroutine()
	started := time.Now()
	var maxSampleBytes, maxMetrics, maxHealth int
	const polls = 50
	for i := 0; i < polls; i++ {
		samples, err := a.Hardware(context.Background())
		if err != nil || len(samples) != 1 {
			t.Fatalf("native health poll %d: samples=%d err=%v", i, len(samples), err)
		}
		_, sampleBytes := nativePayloadSizes(t, inventory, samples)
		maxSampleBytes = max(maxSampleBytes, sampleBytes)
		maxMetrics = max(maxMetrics, len(samples[0].Metrics))
		maxHealth = max(maxHealth, len(samples[0].HardwareHealth))
		if len(samples[0].Metrics) != 4 || len(samples[0].HardwareHealth) != 6 || fixture.ActiveSessions() != 0 {
			t.Fatalf("poll %d lost fixture observations or retained sessions: metrics=%d health=%d sessions=%d", i, len(samples[0].Metrics), len(samples[0].HardwareHealth), fixture.ActiveSessions())
		}
	}
	elapsed := time.Since(started)
	runtime.GC()
	runtime.ReadMemStats(&after)
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	goroutineDelta := runtime.NumGoroutine() - initialGoroutines
	if heapDelta > 8<<20 || goroutineDelta > 8 || len(a.cache.sensors) > a.limits.MaxSDRRecords || len(a.cache.frus) > a.limits.MaxFRURecords {
		t.Fatalf("retained resource bounds exceeded: heap_delta=%d goroutine_delta=%d sensors=%d frus=%d", heapDelta, goroutineDelta, len(a.cache.sensors), len(a.cache.frus))
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// A bounded cleanup command and reply are allowed after the wire budget.
	if stats.active != 0 || stats.peak != 1 || stats.opened != polls+1 || stats.closed != stats.opened ||
		stats.maxSessionWrites > a.limits.MaxOperations+1 || stats.maxSessionBytes > a.limits.MaxPollBytes+2*a.limits.MaxResponseBytes || stats.maxDatagramBytes > a.limits.MaxResponseBytes {
		t.Fatalf("native session/wire bounds exceeded: active=%d peak=%d opened=%d closed=%d session_writes=%d session_bytes=%d datagram_bytes=%d", stats.active, stats.peak, stats.opened, stats.closed, stats.maxSessionWrites, stats.maxSessionBytes, stats.maxDatagramBytes)
	}
	inventoryBytes, _ := nativePayloadSizes(t, inventory, nil)
	t.Logf("reference-BMC/protocol-fixture; health_polls=%d duration=%s retained_heap_delta_bytes=%d total_alloc_bytes=%d goroutine_delta=%d socket_peak=%d opened_sessions=%d closed_sockets=%d active_BMC_sessions=%d writes=%d reads=%d sent_bytes=%d received_bytes=%d max_session_writes=%d max_session_bytes=%d max_datagram_bytes=%d inventory_json_bytes=%d max_sample_json_bytes=%d max_sample_metrics=%d max_sample_health=%d SDR_sensors=%d FRU_ids=%d max_poll_bytes=%d", polls, elapsed, heapDelta, after.TotalAlloc-before.TotalAlloc, goroutineDelta, stats.peak, stats.opened, stats.closed, fixture.ActiveSessions(), stats.writes, stats.reads, stats.sentBytes, stats.receivedBytes, stats.maxSessionWrites, stats.maxSessionBytes, stats.maxDatagramBytes, inventoryBytes, maxSampleBytes, maxMetrics, maxHealth, len(a.cache.sensors), len(a.cache.frus), a.limits.MaxPollBytes)
}
