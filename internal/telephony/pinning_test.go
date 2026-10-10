package telephony

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestSIPPinnedDestinationPreservesAddressAndTLSName(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			e := fixtureServer(t, transport, func(int) replySpec { return replySpec{code: 200} })
			e.DialAddress, e.Address = e.Address, "pbx.fixture.invalid"
			for range 2 {
				health := Probe(context.Background(), e)
				if !health.Reachable || health.State != "up" || health.Address != net.JoinHostPort(e.Address, strconv.Itoa(int(e.Port))) {
					t.Fatal("pin changed observed endpoint or did not prevent a DNS lookup", health)
				}
			}
		})
	}
	e, sni := tlsFixture(t, false, func(int) replySpec { return replySpec{code: 200} })
	e.DialAddress, e.Address, e.TLS.ServerName = e.Address, "pbx.fixture.local", ""
	if health := Probe(context.Background(), e); !health.Reachable || health.State != "up" {
		t.Fatal("pinned TLS probe failed", health)
	}
	if name := <-sni; name != e.Address {
		t.Fatal("pin replaced original TLS identity", name)
	}
	e.Address = "wrong.fixture.invalid"
	if health := Probe(context.Background(), e); health.Reachable || health.ErrorCategory != "tls_certificate" {
		t.Fatal("pin disabled TLS hostname verification", health)
	}
}

func TestSIPPinnedDestinationPreservesRequestHeaders(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	requests := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		n, remote, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		request := string(buffer[:n])
		requests <- request
		headers := make(map[string]string)
		for _, line := range strings.Split(request, "\r\n")[1:] {
			if key, value, ok := strings.Cut(line, ":"); ok {
				headers[strings.ToLower(key)] = strings.TrimSpace(value)
			}
		}
		response := fmt.Sprintf("SIP/2.0 200 Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s;tag=fixture\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"])
		_, _ = conn.WriteTo([]byte(response), remote)
	}()
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	e := Endpoint{ID: "pinned-sip", Address: "pbx.fixture.invalid", DialAddress: "127.0.0.1", Port: port, Transport: "udp", Timeout: time.Second}
	if health := Probe(context.Background(), e); !health.Reachable {
		t.Fatal(health)
	}
	<-done
	request := <-requests
	if !strings.HasPrefix(request, fmt.Sprintf("OPTIONS sip:pbx.fixture.invalid:%d SIP/2.0\r\n", port)) || !strings.Contains(request, "From: <sip:infrapulse@pbx.fixture.invalid>") || !strings.Contains(request, fmt.Sprintf("To: <sip:pbx.fixture.invalid:%d>", port)) {
		t.Fatal("pin replaced original SIP header domain", request)
	}
}

func TestSIPPinnedDestinationIsNumericAndInternal(t *testing.T) {
	for _, address := range []string{"localhost", "127.0.0.1:5060", "udp://127.0.0.1", "[::1]", "fe80::1%eth0"} {
		if err := (Endpoint{ID: "sip", Address: "pbx.fixture.invalid", DialAddress: address}).Validate(); err == nil {
			t.Fatalf("non-numeric pin accepted: %q", address)
		}
	}
	var endpoint Endpoint
	decoder := yaml.NewDecoder(strings.NewReader("id: sip\naddress: pbx.fixture.invalid\ndialaddress: 127.0.0.1\n"))
	decoder.KnownFields(true)
	if err := decoder.Decode(&endpoint); err == nil || endpoint.DialAddress != "" {
		t.Fatal("internal pin was accepted from YAML", err)
	}
}
