package pki

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fixture struct {
	options Options
	server  *httptest.Server
	handler *MockEnrollment
	ca      []byte
	key     []byte
	agentID string
}

func makeCA(t *testing.T) ([]byte, []byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), cert, key
}

func newFixture(t *testing.T, tokenExpiry time.Time, wrap func(http.Handler) http.Handler) *fixture {
	t.Helper()
	ca, key, caCert, caKey := makeCA(t)
	agentID := uuid.NewString()
	token := "fixture-enrollment-private-token"
	mock, err := NewMockEnrollment(ca, key, []Token{{Value: token, AgentID: agentID, ExpiresAt: tokenExpiry}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	handler := http.Handler(mock)
	if wrap != nil {
		handler = wrap(handler)
	}
	s := httptest.NewUnstartedServer(handler)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: serverKey}}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots}
	s.StartTLS()
	t.Cleanup(s.Close)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err = os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PKI_TEST_TOKEN", token)
	return &fixture{options: Options{AgentID: agentID, StateDir: dir, EnrollmentURL: s.URL, BootstrapCAFile: caPath, BootstrapServerName: "localhost", TokenEnv: "PKI_TEST_TOKEN", Timeout: time.Second, RenewBefore: 2 * time.Hour}, server: s, handler: mock, ca: ca, key: key, agentID: agentID}
}

func TestEnrollmentCSRKeyPrivacyAndRestart(t *testing.T) {
	var captured []byte
	f := newFixture(t, time.Now().Add(time.Hour), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/enroll" {
				captured, _ = io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(captured))
			}
			next.ServeHTTP(w, r)
		})
	})
	m, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	var req request
	if json.Unmarshal(captured, &req) != nil {
		t.Fatal("bad captured CSR")
	}
	block, _ := pem.Decode(req.CSR)
	if block == nil {
		t.Fatal("CSR missing")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatal("CSR not signed")
	}
	if bytes.Contains(captured, []byte("PRIVATE KEY")) || bytes.Contains(captured, []byte("fixture-enrollment-private-token")) {
		t.Fatal("request body exposed secret")
	}
	if m.Status().EnrollmentState != "enrolled" || m.Status().ExpiresAt.IsZero() {
		t.Fatal("enrollment status missing")
	}
	data, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	var b bundle
	if json.Unmarshal(data, &b) != nil {
		t.Fatal("bundle corrupt")
	}
	pair, err := tls.X509KeyPair(b.Certificate, b.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := pair.PrivateKey.(*ecdsa.PrivateKey).PublicKey
	if !publicKey.Equal(csr.PublicKey) {
		t.Fatal("issued certificate mismatches generated key")
	}
	t.Setenv("PKI_TEST_TOKEN", "")
	reopened, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal("restart attempted token enrollment", err)
	}
	if !reopened.Status().ExpiresAt.Equal(m.Status().ExpiresAt) {
		t.Fatal("restart lost certificate")
	}
	if _, err = reopened.TLSConfig("localhost"); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(reopened.Status())
	if bytes.Contains(status, []byte("PRIVATE")) || bytes.Contains(status, []byte("token")) {
		t.Fatal("status exposed material")
	}
}

func TestEnrollmentTokenFailures(t *testing.T) {
	for _, name := range []string{"invalid", "expired", "reused"} {
		t.Run(name, func(t *testing.T) {
			expiry := time.Now().Add(time.Hour)
			if name == "expired" {
				expiry = time.Now().Add(-time.Second)
			}
			f := newFixture(t, expiry, nil)
			if name == "invalid" {
				t.Setenv("PKI_TEST_TOKEN", "bad-sensitive-token")
			}
			if name == "reused" {
				if _, err := New(context.Background(), f.options); err != nil {
					t.Fatal(err)
				}
				f.options.StateDir = t.TempDir()
			}
			_, err := New(context.Background(), f.options)
			if err == nil {
				t.Fatal("invalid token accepted")
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "fixture-enrollment") {
				t.Fatal("token leaked")
			}
		})
	}
}

func TestEnrollmentResponseValidation(t *testing.T) {
	for _, name := range []string{"wrong CA", "wrong identity", "wrong EKU", "expired", "malformed", "oversized", "redirect"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, time.Now().Add(time.Hour), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if name == "malformed" {
						_, _ = w.Write([]byte("broken"))
						return
					}
					if name == "oversized" {
						_, _ = w.Write(bytes.Repeat([]byte("x"), maxPKIBytes+1))
						return
					}
					if name == "redirect" {
						http.Redirect(w, r, "https://invalid.example/", http.StatusTemporaryRedirect)
						return
					}
					recorder := httptest.NewRecorder()
					next.ServeHTTP(recorder, r)
					var result response
					if json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
						t.Fatal("mock response corrupt")
					}
					caPEM, _, caCert, caKey := makeCA(t)
					if name == "wrong CA" {
						result.CA = caPEM
					} else {
						block, _ := pem.Decode(result.Certificate)
						leaf, err := x509.ParseCertificate(block.Bytes)
						if err != nil {
							t.Fatal(err)
						}
						leaf.SerialNumber = big.NewInt(42)
						leaf.NotBefore = time.Now().Add(-time.Hour)
						leaf.NotAfter = time.Now().Add(time.Hour)
						leaf.Raw = nil
						if name == "wrong identity" {
							u, _ := AgentURI(uuid.NewString())
							leaf.URIs = []*url.URL{u}
						}
						if name == "wrong EKU" {
							leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
						}
						if name == "expired" {
							leaf.NotAfter = time.Now().Add(-time.Minute)
						}
						der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, leaf.PublicKey, caKey)
						if err != nil {
							t.Fatal(err)
						}
						result.Certificate = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
						result.CA = caPEM
					}
					_ = json.NewEncoder(w).Encode(result)
				})
			})
			_, err := New(context.Background(), f.options)
			if err == nil {
				t.Fatal("unsafe response accepted")
			}
		})
	}
}

func TestRenewalRotationAndFailurePreservesWorkingCertificate(t *testing.T) {
	var fail atomic.Bool
	f := newFixture(t, time.Now().Add(time.Hour), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() && r.URL.Path == "/renew" {
				http.Error(w, "sensitive-server-data", 500)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	m, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	oldTLS, _ := m.TLSConfig("localhost")
	t.Setenv("PKI_TEST_TOKEN", "")
	if rotated, err := m.RenewIfDue(context.Background()); err != nil || rotated {
		t.Fatal("short certificate lifetime caused premature renewal", err)
	}
	m.mu.Lock()
	m.status.ExpiresAt = time.Now().Add(time.Minute)
	m.mu.Unlock()
	rotated, err := m.RenewIfDue(context.Background())
	if err != nil || !rotated {
		t.Fatal("renewal failed", err)
	}
	after, _ := os.ReadFile(m.path())
	newTLS, _ := m.TLSConfig("localhost")
	if bytes.Equal(before, after) || bytes.Equal(oldTLS.Certificates[0].Certificate[0], newTLS.Certificates[0].Certificate[0]) || oldTLS.Certificates[0].PrivateKey.(*ecdsa.PrivateKey).Equal(newTLS.Certificates[0].PrivateKey) {
		t.Fatal("certificate/key did not rotate")
	}
	if m.Status().LastSuccessfulRenewal.IsZero() {
		t.Fatal("renewal status missing")
	}
	fail.Store(true)
	if err = m.Renew(context.Background()); err == nil {
		t.Fatal("failed renewal succeeded")
	}
	retained, _ := os.ReadFile(m.path())
	if !bytes.Equal(after, retained) {
		t.Fatal("failure replaced working certificate")
	}
	if _, err = m.TLSConfig("localhost"); err != nil {
		t.Fatal("failed renewal broke current TLS")
	}
	if m.Status().LastError != "certificate_issue_failed" {
		t.Fatal("unsafe renewal error", m.Status())
	}
	fail.Store(false)
	m.writeBundle = func(string, []byte) error { return os.ErrPermission }
	if err = m.Renew(context.Background()); err == nil {
		t.Fatal("storage failure ignored")
	}
	retained, _ = os.ReadFile(m.path())
	if !bytes.Equal(after, retained) {
		t.Fatal("failed atomic publication changed bundle")
	}
}

func TestEnrollmentTrustCancellationTimeoutAndCorruption(t *testing.T) {
	f := newFixture(t, time.Now().Add(time.Hour), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, f.options); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	rogue, _, _, _ := makeCA(t)
	if err := os.WriteFile(f.options.BootstrapCAFile, rogue, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), f.options); err == nil {
		t.Fatal("wrong bootstrap CA trusted")
	}
	if err := os.WriteFile(f.options.BootstrapCAFile, f.ca, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(m.path(), []byte("{truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = New(context.Background(), f.options); err == nil {
		t.Fatal("corrupted key bundle silently replaced")
	}
	slow := newFixture(t, time.Now().Add(time.Hour), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(250 * time.Millisecond):
			}
		})
	})
	slow.options.Timeout = 100 * time.Millisecond
	if _, err = New(context.Background(), slow.options); err == nil {
		t.Fatal("timeout ignored")
	}
}

func TestCertificateIdentityBinding(t *testing.T) {
	f := newFixture(t, time.Now().Add(time.Hour), nil)
	m, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, _ := m.TLSConfig("localhost")
	leaf := tlsConfig.Certificates[0].Leaf
	if err = VerifyIdentity(leaf, f.agentID); err != nil {
		t.Fatal(err)
	}
	if err = VerifyIdentity(leaf, uuid.NewString()); err == nil {
		t.Fatal("certificate authenticated another AgentID")
	}
}
