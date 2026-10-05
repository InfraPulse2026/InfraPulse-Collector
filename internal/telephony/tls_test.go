package telephony

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tlsCertificates(t *testing.T, expired bool) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "SIP fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pbx.fixture.local"}, DNSNames: []string{"pbx.fixture.local"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	if expired {
		leaf.NotAfter = now.Add(-time.Minute)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func tlsFixture(t *testing.T, expired bool, next func(int) replySpec) (Endpoint, <-chan string) {
	t.Helper()
	cert, ca := tlsCertificates(t, expired)
	sni := make(chan string, 16)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case sni <- hello.ServerName:
		default:
		}
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, ca, 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var calls atomic.Int32
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
				r := bufio.NewReader(conn)
				headers := make(map[string]string)
				first, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if !strings.HasPrefix(first, "OPTIONS sips:") {
					return
				}
				for lines := 0; lines < 64; lines++ {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" {
						break
					}
					name, value, _ := strings.Cut(line, ":")
					headers[strings.ToLower(name)] = strings.TrimSpace(value)
				}
				if !strings.HasPrefix(headers["via"], "SIP/2.0/TLS ") {
					return
				}
				spec := next(int(calls.Add(1)))
				if spec.delay > 0 || spec.drop {
					delay := spec.delay
					if spec.drop {
						delay = time.Second
					}
					select {
					case <-time.After(delay):
					case <-stop:
						return
					}
				}
				if spec.drop {
					return
				}
				if spec.malformed {
					_, _ = io.WriteString(conn, "not SIP\r\n\r\n")
					return
				}
				_, _ = io.WriteString(conn, fmt.Sprintf("SIP/2.0 %d Fixture\r\nVia: %s\r\nFrom: %s\r\nTo: %s;tag=fixture\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n", spec.code, headers["via"], headers["from"], headers["to"], headers["call-id"], headers["cseq"]))
			}()
		}
	}()
	t.Cleanup(func() { close(stop); _ = listener.Close(); wg.Wait() })
	address := listener.Addr().(*net.TCPAddr)
	return Endpoint{ID: "tls-pbx", Address: "127.0.0.1", Port: uint16(address.Port), Transport: "tls", TLS: TLS{CAFile: path, ServerName: "pbx.fixture.local"}, Timeout: time.Second}, sni
}

func TestSIPTLSVerifyCANameExpirationAndSNI(t *testing.T) {
	e, sni := tlsFixture(t, false, func(int) replySpec { return replySpec{code: 200} })
	h := Probe(context.Background(), e)
	if h.State != "up" || !h.Reachable || h.Transport != "tls" {
		t.Fatal(h)
	}
	select {
	case name := <-sni:
		if name != "pbx.fixture.local" {
			t.Fatal(name)
		}
	default:
		t.Fatal("SNI was not sent")
	}
	wrongName := e
	wrongName.TLS.ServerName = "other.fixture.local"
	if h = Probe(context.Background(), wrongName); h.ErrorCategory != "tls_certificate" || h.Reachable {
		t.Fatal(h)
	}
	wrongCA := e
	_, ca := tlsCertificates(t, false)
	wrongCA.TLS.CAFile = filepath.Join(t.TempDir(), "wrong-ca.pem")
	if err := os.WriteFile(wrongCA.TLS.CAFile, ca, 0600); err != nil {
		t.Fatal(err)
	}
	if h = Probe(context.Background(), wrongCA); h.ErrorCategory != "tls_certificate" || h.Reachable {
		t.Fatal(h)
	}
	defaultName := e
	defaultName.TLS.ServerName = ""
	if h = Probe(context.Background(), defaultName); h.ErrorCategory != "tls_certificate" || h.Reachable {
		t.Fatal(h)
	}
	expired, _ := tlsFixture(t, true, func(int) replySpec { return replySpec{code: 200} })
	if h = Probe(context.Background(), expired); h.ErrorCategory != "tls_certificate" || h.Reachable {
		t.Fatal(h)
	}
}

func TestSIPTLSHealthTimeoutMalformedAndRecovery(t *testing.T) {
	for _, spec := range []replySpec{{code: 401}, {code: 503}, {malformed: true}, {drop: true}} {
		e, _ := tlsFixture(t, false, func(int) replySpec { return spec })
		e.Timeout = 100 * time.Millisecond
		h := Probe(context.Background(), e)
		expected := "authentication_required"
		if spec.code == 503 {
			expected = "server_error"
		}
		if spec.malformed {
			expected = "malformed_sip"
		}
		if spec.drop {
			expected = "timeout"
		}
		if h.ErrorCategory != expected {
			t.Fatalf("%+v: %+v", spec, h)
		}
	}
	e, _ := tlsFixture(t, false, func(n int) replySpec {
		if n <= 2 {
			return replySpec{drop: true}
		}
		return replySpec{code: 200}
	})
	e.Timeout, e.FailureThreshold = 100*time.Millisecond, 2
	c, err := New(e)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = c.Collect(context.Background())
	h, events, err := c.Collect(context.Background())
	if err != nil || h.State != "down" || len(events) != 1 {
		t.Fatal(h, events, err)
	}
	h, events, err = c.Collect(context.Background())
	if err != nil || h.State != "up" || h.ConsecutiveFailures != 0 || len(events) != 1 {
		t.Fatal(h, events, err)
	}
}

func TestSIPTLSHandshakeCancellationAndConfigurationLimits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(closed)
			return
		}
		defer conn.Close()
		close(accepted)
		var b [4096]byte
		for {
			if _, err := conn.Read(b[:]); err != nil {
				close(closed)
				return
			}
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	e := Endpoint{ID: "canceled-tls", Address: "127.0.0.1", Port: uint16(addr.Port), Transport: "tls", Timeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- Probe(ctx, e).ErrorCategory }()
	<-accepted
	cancel()
	select {
	case category := <-done:
		if category != "canceled" {
			t.Fatal(category)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake cancellation stuck")
	}
	<-closed
	if d := (Endpoint{Transport: "tls"}).WithDefaults(); d.Port != 5061 {
		t.Fatal(d.Port)
	}
	e.TLS.CAFile = filepath.Join(t.TempDir(), "missing-secret-name.pem")
	if h := Probe(context.Background(), e); h.ErrorCategory != "tls_configuration" || strings.Contains(h.ErrorCategory, "secret") {
		t.Fatal(h)
	}
	for _, data := range [][]byte{[]byte("invalid PEM"), []byte(strings.Repeat("x", (1<<20)+1))} {
		if err := os.WriteFile(e.TLS.CAFile, data, 0600); err != nil {
			t.Fatal(err)
		}
		if h := Probe(context.Background(), e); h.ErrorCategory != "tls_configuration" {
			t.Fatal(h)
		}
	}
	for _, bad := range []Endpoint{{ID: "sip", Address: "localhost", Transport: "udp", TLS: TLS{ServerName: "localhost"}}, {ID: "sip", Address: "localhost", Transport: "tls", TLS: TLS{ServerName: "bad;host"}}} {
		if bad.Validate() == nil {
			t.Fatal("invalid TLS setting accepted")
		}
	}
}
