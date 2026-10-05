package telephony

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type replySpec struct {
	code            int
	delay           time.Duration
	malformed, drop bool
}

// This local fixture echoes RFC transaction headers. It is a SIP protocol test
// server, not a production PBX or hardware verification.
func fixtureServer(t *testing.T, transport string, next func(int) replySpec) Endpoint {
	t.Helper()
	var calls atomic.Int32
	var wg sync.WaitGroup
	var shutdown sync.Once
	stop := make(chan struct{})
	reply := func(request []byte) []byte {
		spec := next(int(calls.Add(1)))
		if spec.drop {
			return nil
		}
		if spec.delay > 0 {
			select {
			case <-time.After(spec.delay):
			case <-stop:
				return nil
			}
		}
		if spec.malformed {
			return []byte("not SIP\r\n\r\n")
		}
		headers := make(map[string]string)
		for _, line := range strings.Split(string(request), "\r\n")[1:] {
			key, value, ok := strings.Cut(line, ":")
			if ok {
				headers[strings.ToLower(key)] = strings.TrimSpace(value)
			}
		}
		return []byte(fmt.Sprintf("SIP/2.0 %d Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s;tag=fixture\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", spec.code, headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"]))
	}
	var address net.Addr
	if transport == "udp" {
		conn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address = conn.LocalAddr()
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer := make([]byte, 4096)
			for {
				n, remote, err := conn.ReadFrom(buffer)
				if err != nil {
					return
				}
				data := reply(buffer[:n])
				if data != nil {
					_, _ = conn.WriteTo(data, remote)
				}
			}
		}()
		t.Cleanup(func() { shutdown.Do(func() { close(stop); _ = conn.Close() }); wg.Wait() })
	} else {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address = listener.Addr()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
					reader := bufio.NewReader(conn)
					var request []byte
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							return
						}
						request = append(request, line...)
						if line == "\r\n" {
							break
						}
					}
					data := reply(request)
					if data != nil {
						_, _ = conn.Write(data)
					} else {
						select {
						case <-stop:
						case <-time.After(time.Second):
						}
					}
				}()
			}
		}()
		t.Cleanup(func() { shutdown.Do(func() { close(stop); _ = listener.Close() }); wg.Wait() })
	}
	host, port, _ := net.SplitHostPort(address.String())
	var number uint16
	_, _ = fmt.Sscan(port, &number)
	return Endpoint{ID: "test-sip", Address: host, Port: number, Transport: transport, Timeout: 100 * time.Millisecond, Interval: time.Second}
}

func TestUDPAndTCPResponseClasses(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		for _, tc := range []struct {
			code            int
			state, category string
		}{{200, "up", ""}, {202, "up", ""}, {401, "degraded", "authentication_required"}, {407, "degraded", "authentication_required"}, {404, "degraded", "request_rejected"}, {503, "degraded", "server_error"}} {
			t.Run(fmt.Sprintf("%s/%d", transport, tc.code), func(t *testing.T) {
				e := fixtureServer(t, transport, func(int) replySpec { return replySpec{code: tc.code} })
				h := Probe(context.Background(), e)
				if !h.Reachable || h.ResponseCode == nil || *h.ResponseCode != uint32(tc.code) || h.LatencyMillis == nil || *h.LatencyMillis < 0 || h.State != tc.state || h.ErrorCategory != tc.category {
					t.Fatalf("%+v", h)
				}
			})
		}
	}
}

func TestTimeoutMalformedCancellationAndLatency(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport+"/timeout", func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{drop: true} })
			h := Probe(context.Background(), e)
			if h.Reachable || h.ErrorCategory != "timeout" {
				t.Fatalf("%+v", h)
			}
		})
		t.Run(transport+"/malformed", func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{malformed: true} })
			h := Probe(context.Background(), e)
			if h.Reachable || h.ErrorCategory != "malformed_sip" {
				t.Fatalf("%+v", h)
			}
		})
		t.Run(transport+"/cancellation", func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{drop: true} })
			e.Timeout = time.Second
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(20*time.Millisecond, cancel)
			defer timer.Stop()
			started := time.Now()
			h := Probe(ctx, e)
			if h.ErrorCategory != "canceled" || time.Since(started) > 500*time.Millisecond {
				t.Fatalf("%+v elapsed=%s", h, time.Since(started))
			}
		})
		t.Run(transport+"/latency", func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{code: 200, delay: 25 * time.Millisecond} })
			h := Probe(context.Background(), e)
			if h.LatencyMillis == nil || *h.LatencyMillis < 20 || *h.LatencyMillis > 250 {
				t.Fatalf("%+v", h)
			}
		})
	}
}

func TestFailuresTransitionsRecoveryAndStateIsolation(t *testing.T) {
	e := fixtureServer(t, "udp", func(n int) replySpec {
		switch n {
		case 1, 5:
			return replySpec{code: 200}
		default:
			return replySpec{drop: true}
		}
	})
	e.Timeout = 30 * time.Millisecond
	c, err := New(e)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"up", "degraded", "degraded", "down", "up"}
	for i, state := range want {
		h, events, err := c.Collect(context.Background())
		if err != nil || h.State != state {
			t.Fatalf("poll %d: %+v %v", i, h, err)
		}
		failures := uint32(i)
		if i == 4 {
			failures = 0
		}
		if h.ConsecutiveFailures != failures {
			t.Fatalf("poll %d failures %+v", i, h)
		}
		if i == 0 || i == 2 {
			if len(events) != 0 {
				t.Fatal(events)
			}
		} else {
			if len(events) != 1 || events[0].Type != "telephony."+state || events[0].DeviceID != "" {
				t.Fatal(events)
			}
		}
	}
	h := c.State()
	*h.ResponseCode = 999
	if *c.State().ResponseCode != 200 {
		t.Fatal("state aliased")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, events, err := c.Collect(ctx)
	if !errors.Is(err, context.Canceled) || len(events) != 0 || c.State().State != "up" {
		t.Fatal(err, events, c.State())
	}
}

func TestLatencyDegradation(t *testing.T) {
	e := fixtureServer(t, "tcp", func(n int) replySpec {
		if n == 2 {
			return replySpec{code: 200, delay: 30 * time.Millisecond}
		}
		return replySpec{code: 200}
	})
	e.LatencyThreshold = 20 * time.Millisecond
	c, err := New(e)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = c.Collect(context.Background())
	h, events, err := c.Collect(context.Background())
	if err != nil || h.State != "degraded" || h.ErrorCategory != "high_latency" || h.ConsecutiveFailures != 0 || len(events) != 1 {
		t.Fatal(h, events, err)
	}
}

func TestUDPRetransmission(t *testing.T) {
	e := fixtureServer(t, "udp", func(n int) replySpec {
		if n == 1 {
			return replySpec{drop: true}
		}
		return replySpec{code: 200}
	})
	e.Timeout = 1200 * time.Millisecond
	h := Probe(context.Background(), e)
	if !h.Reachable || h.LatencyMillis == nil || *h.LatencyMillis < 450 {
		t.Fatalf("%+v", h)
	}
}

func TestParserTransactionAndLimits(t *testing.T) {
	valid := "SIP/2.0 200 OK\r\nVia: SIP/2.0/UDP 127.0.0.1;branch=z9hG4bKtest\r\nFrom: <sip:agent@local>;tag=from\r\nTo: <sip:pbx@local>;tag=to\r\nCall-ID: call\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
	if code, err := parseResponse([]byte(valid), true, "call", "z9hG4bKtest"); err != nil || code != 200 {
		t.Fatal(code, err)
	}
	for _, bad := range []string{strings.Replace(valid, "200 OK", "700 invalid", 1), strings.Replace(valid, "Content-Length: 0", "Content-Length: 32769", 1), strings.Replace(valid, "Content-Length: 0", "Content-Length: -1", 1), strings.Replace(valid, "Content-Length: 0\r\n", "", 1), strings.Replace(valid, "CSeq: 1 OPTIONS", "CSeq: 1 OPTIONS\r\nCSeq: 2 OPTIONS", 1), strings.Repeat("a", maxResponseBytes+1)} {
		if _, err := parseResponse([]byte(bad), true, "call", "z9hG4bKtest"); !errors.Is(err, errMalformed) {
			t.Fatal("accepted malformed", err)
		}
	}
	for _, bad := range []string{strings.Replace(valid, "Call-ID: call", "Call-ID: other", 1), strings.Replace(valid, "CSeq: 1 OPTIONS", "CSeq: 2 OPTIONS", 1), strings.Replace(valid, "z9hG4bKtest", "z9hG4bKother", 1)} {
		if _, err := parseResponse([]byte(bad), true, "call", "z9hG4bKtest"); !errors.Is(err, errUnmatched) {
			t.Fatal("accepted unmatched", err)
		}
	}
	compact := strings.NewReplacer("Via:", "v:", "From:", "f:", "To:", "t:", "Call-ID:", "i:", "Content-Length:", "l:").Replace(valid)
	if _, err := parseResponse([]byte(compact), true, "call", "z9hG4bKtest"); err != nil {
		t.Fatal(err)
	}
}

func TestTCPFramingProvisionalAndBody(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		base := "SIP/2.0 %s\r\nVia: SIP/2.0/TCP local;branch=branch\r\nFrom: <sip:from@local>\r\nTo: <sip:to@local>\r\nCall-ID: call\r\nCSeq: 1 OPTIONS\r\nContent-Length: %d\r\n\r\n%s"
		_, _ = io.WriteString(server, fmt.Sprintf(base, "100 Trying", 0, ""))
		_, _ = io.WriteString(server, fmt.Sprintf(base, "200 OK", 4, "body"))
	}()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	code, err := readTCP(client, "call", "branch")
	if err != nil || code != 200 {
		t.Fatal(code, err)
	}
}

func TestEndpointValidationAndNetworkCategories(t *testing.T) {
	e := Endpoint{ID: "stable-id", Address: "localhost"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Endpoint){func(e *Endpoint) { e.Address = "sip://pbx" }, func(e *Endpoint) { e.FromUser = "agent\r\nBad: header" }, func(e *Endpoint) { e.Domain = "host;header" }, func(e *Endpoint) { e.Transport = "ws" }, func(e *Endpoint) { e.Timeout = 2 * time.Minute }, func(e *Endpoint) { e.FailureThreshold = 101 }} {
		bad := e
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("accepted", bad)
		}
	}
	if category(context.Background(), context.Background(), &net.DNSError{Err: "fixture", Name: "fixture.invalid"}) != "dns_failure" {
		t.Fatal("DNS classification")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	h := Probe(context.Background(), Endpoint{ID: "closed", Address: "127.0.0.1", Port: uint16(address.Port), Transport: "tcp", Timeout: 100 * time.Millisecond})
	if h.ErrorCategory != "connection_failure" || h.Reachable {
		t.Fatal(h)
	}
}

func TestCollectorOwnTimeoutAndCanceledConcurrentPoll(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{drop: true} })
			e.Timeout = 30 * time.Millisecond
			c, err := New(e)
			if err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			h, _, err := c.Collect(parent)
			if err != nil || h.ErrorCategory != "timeout" || h.ConsecutiveFailures != 1 {
				t.Fatal(h, err)
			}
		})
	}
	e := fixtureServer(t, "udp", func(int) replySpec { return replySpec{drop: true} })
	e.Timeout = time.Second
	c, err := New(e)
	if err != nil {
		t.Fatal(err)
	}
	first, stop := context.WithCancel(context.Background())
	defer stop()
	finished := make(chan struct{})
	go func() { defer close(finished); _, _, _ = c.Collect(first) }()
	for len(c.pollGate) == 0 {
		select {
		case <-time.After(time.Millisecond):
		case <-finished:
			t.Fatal("first poll finished too soon")
		}
	}
	second, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = c.Collect(second)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatal(err)
	}
	stop()
	<-finished
	if c.State().State != "unknown" || c.State().ConsecutiveFailures != 0 {
		t.Fatal(c.State())
	}
}

func TestTCPTruncatedFrame(t *testing.T) {
	for _, payload := range []string{"SIP/2.0 200", "SIP/2.0 200 OK\r\nContent-Length: 2\r\n\r\nx"} {
		client, server := net.Pipe()
		go func() { defer server.Close(); _, _ = io.WriteString(server, payload) }()
		_, err := readTCP(client, "call", "branch")
		_ = client.Close()
		if !errors.Is(err, errMalformed) {
			t.Fatal(err)
		}
	}
}

func TestRFCHeaderWhitespaceFoldingAndStrictLength(t *testing.T) {
	valid := "SIP/2.0 200 \r\nVia \t: sip/2.0/udp 127.0.0.1;\r\n\tbranch = z9hG4bKtest\r\nFrom: <sip:agent@local>;tag=from\r\nTo: <sip:pbx@local>;tag=to\r\nCall-ID: call\r\nCSeq: 1\r\n OPTIONS\r\nContent-Length : 0\r\nSupported: timer,\r\n path\r\n\r\n"
	if code, err := parseResponse([]byte(valid), true, "call", "z9hG4bKtest"); err != nil || code != 200 {
		t.Fatal(code, err)
	}
	for _, length := range []string{"+0", "-0", "0x0", "0 extra", "0\u00a0", ""} {
		bad := strings.Replace(valid, "Content-Length : 0", "Content-Length : "+length, 1)
		if _, err := parseResponse([]byte(bad), true, "call", "z9hG4bKtest"); !errors.Is(err, errMalformed) {
			t.Fatal("accepted invalid length", length, err)
		}
	}
	for _, bad := range []string{strings.Replace(valid, "Via \t:", "Bad( :", 1), strings.Replace(valid, "Call-ID: call", "Call-ID: call\r\n i: other", 1), strings.Replace(valid, "branch = z9hG4bKtest", "branch = z9hG4bKtest; branch=other", 1), strings.Replace(valid, "Call-ID: call", "Call-ID: call\r\nCall-ID: other", 1), strings.Replace(valid, "path\r\n", "path\x00\r\n", 1)} {
		if _, err := parseResponse([]byte(bad), true, "call", "z9hG4bKtest"); !errors.Is(err, errMalformed) && !errors.Is(err, errUnmatched) {
			t.Fatal("accepted ambiguous/malformed frame", err)
		}
	}
}

func TestTCPLeadingCRLFAndBoundedNoise(t *testing.T) {
	valid := "SIP/2.0 200 OK\r\nVia: SIP/2.0/TCP local;branch=branch\r\nFrom: <sip:from@local>\r\nTo: <sip:to@local>\r\nCall-ID: call\r\nCSeq: 1 OPTIONS\r\nContent-Length: 0\r\n\r\n"
	for _, tc := range []struct {
		payload   string
		malformed bool
	}{{"\r\n\r\n" + valid, false}, {strings.Repeat("\r\n", maxHeaderLines+1) + valid, true}, {"SIP/2.0 200 OK\r\n\torphan continuation\r\n\r\n", true}, {strings.Replace(valid, "Content-Length: 0", "Content-Length: +0", 1), true}} {
		client, server := net.Pipe()
		finished := make(chan struct{})
		go func() { defer close(finished); defer server.Close(); _, _ = io.WriteString(server, tc.payload) }()
		_ = client.SetDeadline(time.Now().Add(time.Second))
		code, err := readTCP(client, "call", "branch")
		_ = client.Close()
		<-finished
		if tc.malformed {
			if !errors.Is(err, errMalformed) {
				t.Fatal(err)
			}
		} else if err != nil || code != 200 {
			t.Fatal(code, err)
		}
	}
}
