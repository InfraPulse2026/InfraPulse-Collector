package routeros

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc, opts Options) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	opts.Address = server.URL
	opts.TLSConfig = &tls.Config{RootCAs: roots}
	c, err := New(opts, func(context.Context) (Authentication, error) { return Authentication{"monitor", "do-not-expose"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func TestFixedRequestsAuthenticationAndRecordID(t *testing.T) {
	var patches atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "monitor" || p != "do-not-expose" {
			t.Error("Basic authentication missing")
		}
		if r.URL.Path != "/rest/interface/*A" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if strings.Contains(r.RequestURI, "%2A") {
			t.Error("record asterisk encoded")
		}
		if r.Method == http.MethodPatch {
			patches.Add(1)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing content type")
			}
		}
		fmt.Fprint(w, `{".id":"*A","name":"ether1","disabled":"true"}`)
	}, Options{})
	if _, err := c.Interface(context.Background(), "*A"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetInterfaceDisabled(context.Background(), "*A", true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ether1", "*A/../user", "*A?x=y", "*A%2f", "../execute"} {
		if _, err := c.Interface(context.Background(), bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("record %q accepted", bad)
		}
	}
	if patches.Load() != 1 {
		t.Fatal("invalid ID caused network operation")
	}
}
func TestBodyRecordLimitsAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		opts       Options
		want       error
	}{{"body", strings.Repeat("x", 257), Options{MaxResponseBytes: 256}, ErrLimit}, {"rows", `[{},{},{}]`, Options{MaxRecords: 2}, ErrLimit}, {"json", `{`, Options{}, ErrMalformed}, {"number", `{"uptime":2}`, Options{}, ErrMalformed}, {"null", `null`, Options{}, ErrMalformed}, {"null row", `[null]`, Options{}, ErrMalformed}, {"nested", `{"password":{"secret":"value"}}`, Options{}, ErrMalformed}} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }, tc.opts)
			_, err := c.Resource(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}
func TestHTTPErrorAndRedirectNeverExposeBodyOrCredentials(t *testing.T) {
	for _, status := range []int{301, 400, 401, 403, 404, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://example.invalid/steal")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"password":"do-not-expose","detail":"untrusted remote text"}`)
			}, Options{})
			_, err := c.Identity(context.Background())
			if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "do-not-expose") || strings.Contains(err.Error(), "untrusted") {
				t.Fatalf("unsafe error %v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("redirect followed")
			}
		})
	}
}
func TestCancellationAndTimeout(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, Options{Timeout: 30 * time.Millisecond})
	_, err := c.Identity(context.Background())
	if !errors.Is(err, ErrConnectivity) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Identity(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = c.Identity(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context timeout %v", err)
	}
}
func TestTLSVerificationAndOptions(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"name":"fixture"}`) }))
	defer server.Close()
	auth := func(context.Context) (Authentication, error) { return Authentication{"a", "b"}, nil }
	c, err := New(Options{Address: server.URL}, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Identity(context.Background()); !errors.Is(err, ErrConnectivity) {
		t.Fatalf("untrusted CA accepted: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	c, err = New(Options{Address: server.URL, TLSConfig: &tls.Config{RootCAs: roots, ServerName: "wrong.invalid"}}, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Identity(context.Background()); !errors.Is(err, ErrConnectivity) {
		t.Fatalf("wrong SAN accepted: %v", err)
	}
	for _, address := range []string{"http://127.0.0.1", "https://u:p@127.0.0.1", "https://127.0.0.1/rest", "https://127.0.0.1?secret=x", "https://127.0.0.1#frag"} {
		if _, err = New(Options{Address: address}, auth); err == nil {
			t.Fatalf("unsafe address accepted %s", address)
		}
	}
	if _, err = New(Options{Address: server.URL, TLSConfig: &tls.Config{InsecureSkipVerify: true}}, auth); err == nil {
		t.Fatal("insecure TLS accepted")
	}
}
func TestCredentialProviderRotationAndSafeError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		fmt.Fprintf(w, `{"name":"%s/%s"}`, u, p)
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	var calls int
	c, err := New(Options{Address: server.URL, TLSConfig: &tls.Config{RootCAs: roots}}, func(context.Context) (Authentication, error) {
		calls++
		if calls > 2 {
			return Authentication{}, errors.New("secret-provider-password")
		}
		return Authentication{fmt.Sprint(calls), "rotated"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a, err := c.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a["name"] == b["name"] {
		t.Fatal("credential not refreshed")
	}
	_, err = c.Identity(context.Background())
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret-provider") {
		t.Fatalf("unsafe provider error %v", err)
	}
}
