package syslogreceiver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"InfraPulseAgent/internal/testutil"
)

func TestPhase8MultipleTLSSendersPressureRecoveryReplay(t *testing.T) {
	pki := createPKI(t)
	settings := tlsSettings(t, pki)
	settings.Limits.MaxConnections, settings.Limits.MaxConnectionsPerSource = 8, 8
	settings.Limits.ReadTimeout, settings.Limits.AdmissionTimeout = 150*time.Millisecond, 150*time.Millisecond
	opts := queue.DurableOptions{Path: filepath.Join(testutil.TempDir(t), "outbox.db"), MaxMessages: 8, MaxBytes: 256 << 10, MaxMessageBytes: 128 << 10, MaxStorageBytes: 4 << 20, Retention: time.Hour, BatchDelay: time.Millisecond, BatchSize: 8, MaxInflight: 8, ACKTimeout: time.Second}
	outbox, err := queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outbox.Close() })
	// Backend offline: protected already accepted results use all capacity.
	for i := range opts.MaxMessages {
		if err := outbox.Push(model.Envelope{MessageID: fmt.Sprintf("protected-%d", i), CommandResult: &model.CommandResult{CommandID: fmt.Sprintf("command-%d", i), Status: "success"}}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	accepted := make(map[string]string)
	r, address, cancel, _ := startReceiver(t, settings, testOptions(func(ctx context.Context, e model.Envelope) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := outbox.Push(e); err != nil {
			return err
		}
		mu.Lock()
		accepted[e.MessageID] = e.Logs.Records[0].ID
		mu.Unlock()
		return nil
	}))
	clientConfig := &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.client}}
	clients := make([]*tls.Conn, 4)
	for i := range clients {
		clients[i] = dialTLS(t, address, clientConfig.Clone())
	}
	// Slow clients occupy only their bounded connection workers: one never
	// starts a TLS handshake, another supplies an incomplete counted frame.
	slowHandshake := dialTCP(t, address)
	partial := dialTLS(t, address, clientConfig.Clone())
	writeWire(t, partial, "100 partial")
	var sends sync.WaitGroup
	for _, client := range clients {
		sends.Go(func() {
			_ = client.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := client.Write([]byte(octetFrame(sampleSyslog))); err != nil {
				t.Error(err)
			}
		})
	}
	sends.Wait()
	waitReceiver(t, "bounded rejection under durable pressure", func() bool { return r.Snapshot().AdmissionFailures == 4 && r.Snapshot().ActiveConnections == 0 })
	_ = slowHandshake.Close()
	_ = partial.Close()
	if outbox.State().Messages != 8 || r.Snapshot().Admitted != 0 || r.Snapshot().PeakConnections > 8 {
		t.Fatal("pressure/slow clients changed protected records or bounds", outbox.State(), r.Snapshot())
	}
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	for range 8 {
		e, err := outbox.Pop(ctx)
		if err != nil || e.CommandResult == nil {
			t.Fatal("offline protected result lost", e, err)
		}
		if found, err := outbox.Acknowledge(e.MessageID); err != nil || !found {
			t.Fatal("recovery ACK failed", found, err)
		}
	}
	for i := range clients {
		_ = clients[i].Close()
		clients[i] = dialTLS(t, address, clientConfig.Clone())
		writeWire(t, clients[i], octetFrame(sampleSyslog))
	}
	waitReceiver(t, "new TLS senders recover after Backend capacity returns", func() bool { return r.Snapshot().Admitted == 4 })
	cancel()
	waitReceiver(t, "TLS listeners/connections stop", func() bool { return len(r.Snapshot().Listening) == 0 && r.Snapshot().ActiveConnections == 0 })
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}
	outbox, err = queue.NewDurable(opts)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e, err := outbox.Pop(ctx)
		if err != nil || e.Logs == nil {
			t.Fatal("TLS log replay unavailable", err)
		}
		mu.Lock()
		recordID, known := accepted[e.MessageID]
		delete(accepted, e.MessageID)
		mu.Unlock()
		record := e.Logs.Records[0]
		if !known || record.ID != recordID || !record.Syslog.Authenticated || record.Syslog.TLSIdentity != "dns:sender.test" || record.Observation.Source.Provenance != model.ProvenanceReal {
			t.Fatal("restart replay changed original log/peer/provenance", e.MessageID)
		}
		if found, err := outbox.Acknowledge(e.MessageID); err != nil || !found {
			t.Fatal("replayed log ACK failed", found, err)
		}
	}
	if outbox.State().Messages != 0 || len(accepted) != 0 {
		t.Fatal("recovery did not drain retained records", outbox.State())
	}
	t.Logf("loopback TLS: four concurrent senders + two slow clients; initial protected records=8, pressure admission failures=%d, later retained/restarted/replayed/ACKed logs=4, peak_connections=%d", r.Snapshot().AdmissionFailures, r.Snapshot().PeakConnections)
}

func TestPhase8TLSReceiverFiftyStartStopCycles(t *testing.T) {
	pki := createPKI(t)
	settings := tlsSettings(t, pki)
	clientConfig := &tls.Config{RootCAs: pki.roots, ServerName: "collector.test", MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pki.client}}
	var admitted atomic.Int32
	options := testOptions(func(context.Context, model.Envelope) error { admitted.Add(1); return nil })
	fdCount := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return -1
		}
		return len(entries)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	baselineGoroutines, baselineFD := runtime.NumGoroutine(), fdCount()
	peakGoroutines := baselineGoroutines
	started := time.Now()
	var maxShutdown time.Duration
	for cycle := range 50 {
		r, err := New(settings, options)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		waitReceiver(t, "repeated TLS listener startup", func() bool { return len(r.Snapshot().Listening) == 1 })
		address := r.Snapshot().Listening[0]
		client, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, clientConfig.Clone())
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if _, err := client.Write([]byte(octetFrame(sampleSyslog))); err != nil {
			cancel()
			_ = client.Close()
			<-done
			t.Fatal(err)
		}
		waitReceiver(t, "repeated TLS log admission", func() bool { return admitted.Load() == int32(cycle+1) })
		slow, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			cancel()
			_ = client.Close()
			<-done
			t.Fatal(err)
		}
		peakGoroutines = max(peakGoroutines, runtime.NumGoroutine())
		shutdown := time.Now()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("receiver shutdown blocked on slow TLS client")
		}
		maxShutdown = max(maxShutdown, time.Since(shutdown))
		_ = client.Close()
		_ = slow.Close()
		if r.Snapshot().ActiveConnections != 0 || len(r.Snapshot().Listening) != 0 || len(r.peers) != 0 || len(r.sources) != 0 {
			t.Fatal("completed lifecycle retained receiver/socket state")
		}
	}
	waitReceiver(t, "goroutine stabilization after 50 TLS lifecycles", func() bool { return runtime.NumGoroutine() <= baselineGoroutines+4 })
	runtime.GC()
	runtime.ReadMemStats(&after)
	afterFD, afterGoroutines := fdCount(), runtime.NumGoroutine()
	heapDelta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if heapDelta > 8<<20 || peakGoroutines > baselineGoroutines+16 || baselineFD >= 0 && afterFD > baselineFD+2 {
		t.Fatalf("lifecycle resources did not stabilize: heap_delta=%d goroutines=%d/%d/%d fd=%d/%d", heapDelta, baselineGoroutines, peakGoroutines, afterGoroutines, baselineFD, afterFD)
	}
	t.Logf("loopback TLS50cycles: elapsed=%s max_shutdown=%s admitted=%d goroutines=%d/%d/%d fd_before=%d fd_after=%d retained_heap_delta=%d total_alloc=%d; fd=-1 means unavailable, client/server share process", time.Since(started), maxShutdown, admitted.Load(), baselineGoroutines, peakGoroutines, afterGoroutines, baselineFD, afterFD, heapDelta, after.TotalAlloc-before.TotalAlloc)
}
