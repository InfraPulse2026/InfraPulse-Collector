package routeros

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPinnedRouterOSDestinationPreservesHTTPSIdentity(t *testing.T) {
	const hostname = "router.fixture.invalid"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(7), DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	var calls atomic.Int32
	var redirect atomic.Bool
	seen := make(chan [2]string, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen <- [2]string{r.Host, r.TLS.ServerName}
		if redirect.Load() {
			w.Header().Set("Location", "https://other.fixture.invalid/steal")
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Fprint(w, `{"name":"pinned-router"}`)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	auth := func(context.Context) (Authentication, error) { return Authentication{"reader", "fixture-secret"}, nil }
	origin := "https://" + net.JoinHostPort(hostname, port)
	c, err := New(Options{Address: origin, DialAddress: "127.0.0.1", TLSConfig: &tls.Config{RootCAs: roots}}, auth)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for range 2 {
		c.transport.CloseIdleConnections()
		identity, err := c.Identity(context.Background())
		if err != nil || identity["name"] != "pinned-router" {
			t.Fatalf("pinned connection failed: %v %v", identity, err)
		}
		observed := <-seen
		if observed != [2]string{net.JoinHostPort(hostname, port), hostname} || c.origin.String() != origin {
			t.Fatalf("pin replaced HTTPS origin or identity: %v %s", observed, c.origin)
		}
	}
	if _, err := c.transport.DialContext(context.Background(), "tcp", net.JoinHostPort("other.fixture.invalid", port)); !errors.Is(err, ErrUnavailable) {
		t.Fatal("pinned dial accepted another origin", err)
	}
	redirect.Store(true)
	if _, err := c.Identity(context.Background()); !errors.Is(err, ErrUnavailable) || calls.Load() != 3 {
		t.Fatal("redirect bypassed origin policy", calls.Load(), err)
	}
	wrong, err := New(Options{Address: "https://" + net.JoinHostPort("wrong.fixture.invalid", port), DialAddress: "127.0.0.1", TLSConfig: &tls.Config{RootCAs: roots}}, auth)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrong.Close() })
	if _, err := wrong.Identity(context.Background()); !errors.Is(err, ErrConnectivity) {
		t.Fatal("pin disabled hostname certificate verification", err)
	}
}

func TestRouterOSPinnedDestinationMustBeNumeric(t *testing.T) {
	auth := func(context.Context) (Authentication, error) { return Authentication{"a", "b"}, nil }
	for _, address := range []string{"localhost", "127.0.0.1:443", "https://127.0.0.1", "[::1]", "fe80::1%eth0"} {
		if c, err := New(Options{Address: "https://router.fixture.invalid", DialAddress: address}, auth); err == nil {
			_ = c.Close()
			t.Fatalf("non-numeric pin accepted: %q", address)
		}
	}
}
