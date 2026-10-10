package redfish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
)

func TestPinnedRedfishDestinationPreservesOriginAndTLS(t *testing.T) {
	const hostname = "bmc.fixture.invalid"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(8), DNSNames: []string{hostname}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
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
	f := newFixture(t)
	var wrongOrigin atomic.Bool
	var redirect atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, splitErr := net.SplitHostPort(r.Host)
		if splitErr != nil || host != hostname || r.TLS.ServerName != hostname {
			wrongOrigin.Store(true)
		}
		if redirect.Load() {
			w.Header().Set("Location", "https://other.fixture.invalid/redfish/v1/")
			w.WriteHeader(http.StatusFound)
			return
		}
		f.serve(w, r)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://" + net.JoinHostPort(hostname, port)
	a, err := New(driver.Options{ID: "bmc", Address: origin}, testProvider{}, "fixture", Options{DialAddress: "127.0.0.1", TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	for range 2 {
		a.http.CloseIdleConnections()
		inventory, err := a.InventoryBatch(context.Background())
		if err != nil || len(inventory.Devices) == 0 || wrongOrigin.Load() || a.origin.String() != origin || a.opts.Address != origin {
			t.Fatalf("pinned Redfish connection changed origin: %v %v %v", inventory.Devices, wrongOrigin.Load(), err)
		}
	}
	if _, err := a.transport.base.DialContext(context.Background(), "tcp", net.JoinHostPort("other.fixture.invalid", port)); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("pinned dial accepted a cross-origin destination", err)
	}
	if _, err := a.canonical("https://other.fixture.invalid/redfish/v1/Systems", false); !errors.Is(err, driver.ErrUnavailable) {
		t.Fatal("pin allowed a cross-origin resource", err)
	}
	redirect.Store(true)
	if _, err := a.InventoryBatch(context.Background()); err == nil {
		t.Fatal("pin allowed an HTTP redirect")
	}
	wrong, err := New(driver.Options{ID: "bmc", Address: "https://" + net.JoinHostPort("wrong.fixture.invalid", port)}, testProvider{}, "fixture", Options{DialAddress: "127.0.0.1", TLSConfig: &tls.Config{RootCAs: roots}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wrong.Close() })
	if _, err := wrong.InventoryBatch(context.Background()); err == nil {
		t.Fatal("pin disabled hostname certificate verification")
	}
}

func TestRedfishPinnedDestinationMustBeNumeric(t *testing.T) {
	for _, address := range []string{"localhost", "127.0.0.1:443", "https://127.0.0.1", "[::1]", "fe80::1%eth0"} {
		if a, err := New(driver.Options{ID: "bmc", Address: "https://bmc.fixture.invalid"}, testProvider{}, "fixture", Options{DialAddress: address}); err == nil {
			_ = a.Close()
			t.Fatalf("non-numeric pin accepted: %q", address)
		}
	}
}
