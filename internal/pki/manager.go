package pki

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxPKIBytes = 128 << 10

type Options struct {
	AgentID, StateDir, EnrollmentURL, BootstrapCAFile, BootstrapServerName, TokenEnv, ControllerCAFile string
	Timeout, RenewBefore                                                                               time.Duration
}

type Status struct {
	EnrollmentState       string    `json:"enrollment_state"`
	RenewalState          string    `json:"renewal_state"`
	ExpiresAt             time.Time `json:"expires_at"`
	RemainingSeconds      int64     `json:"remaining_seconds"`
	LastSuccessfulRenewal time.Time `json:"last_successful_renewal,omitempty"`
	LastError             string    `json:"last_error,omitempty"`
}

type bundle struct {
	Version     int    `json:"version"`
	AgentID     string `json:"agent_id"`
	Certificate []byte `json:"certificate"`
	PrivateKey  []byte `json:"private_key"`
	CA          []byte `json:"ca"`
}

// Manager never exposes its private key or enrollment token in status/errors.
// An immutable TLS snapshot is replaced only after durable bundle publication.
type Manager struct {
	options     Options
	mu          sync.RWMutex
	renewMu     sync.Mutex
	pair        tls.Certificate
	roots       *x509.CertPool
	status      Status
	writeBundle func(string, []byte) error
}

func New(ctx context.Context, o Options) (*Manager, error) {
	if _, err := AgentURI(o.AgentID); err != nil {
		return nil, err
	}
	u, err := url.Parse(o.EnrollmentURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || o.StateDir == "" || o.BootstrapCAFile == "" {
		return nil, errors.New("invalid enrollment options")
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.RenewBefore == 0 {
		o.RenewBefore = 7 * 24 * time.Hour
	}
	if o.Timeout < 100*time.Millisecond || o.Timeout > time.Minute || o.RenewBefore < 0 || o.RenewBefore > 90*24*time.Hour {
		return nil, errors.New("invalid enrollment limits")
	}
	m := &Manager{options: o, writeBundle: atomicWrite, status: Status{EnrollmentState: "pending", RenewalState: "idle"}}
	data, err := readBounded(m.path())
	if err == nil {
		if verifyPrivatePermissions(m.path()) != nil {
			return nil, errors.New("certificate private key permissions are not restrictive")
		}
		var b bundle
		if decodeStrict(data, &b) != nil || b.Version != 1 || b.AgentID != o.AgentID {
			return nil, errors.New("invalid certificate bundle; explicit recovery required")
		}
		pair, roots, err := m.validate(b, false)
		if err != nil {
			return nil, err
		}
		m.pair, m.roots = pair, roots
		m.status.EnrollmentState, m.status.ExpiresAt = "enrolled", pair.Leaf.NotAfter
		return m, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("certificate storage unavailable")
	}
	if err = m.issue(ctx, false); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) path() string {
	return filepath.Join(m.options.StateDir, "pki", "client-bundle.json")
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.status
	s.RemainingSeconds = int64(time.Until(s.ExpiresAt) / time.Second)
	return s
}

func (m *Manager) TLSConfig(serverName string) (*tls.Config, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.pair.Leaf == nil || !time.Now().Before(m.pair.Leaf.NotAfter) {
		return nil, errors.New("client certificate expired or unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: m.roots, ServerName: serverName, Certificates: []tls.Certificate{m.pair}}, nil
}

func (m *Manager) RenewIfDue(ctx context.Context) (bool, error) {
	m.mu.RLock()
	expires := m.status.ExpiresAt
	before := m.options.RenewBefore
	if m.pair.Leaf != nil {
		// A short-lived CA policy must not cause renewal on every scheduler tick
		// when the operator configured a longer advance window.
		if maximum := m.pair.Leaf.NotAfter.Sub(m.pair.Leaf.NotBefore) / 3; before > maximum {
			before = maximum
		}
	}
	m.mu.RUnlock()
	if time.Until(expires) > before {
		return false, nil
	}
	err := m.Renew(ctx)
	return err == nil, err
}

func (m *Manager) Renew(ctx context.Context) error {
	m.renewMu.Lock()
	defer m.renewMu.Unlock()
	return m.issue(ctx, true)
}

type request struct {
	AgentID string `json:"agent_id"`
	CSR     []byte `json:"csr"`
}
type response struct {
	Certificate []byte `json:"certificate"`
	CA          []byte `json:"ca"`
}

func (m *Manager) issue(ctx context.Context, renew bool) (resultErr error) {
	m.mu.Lock()
	m.status.LastError = ""
	if renew {
		m.status.RenewalState = "renewing"
	}
	m.mu.Unlock()
	defer func() {
		if resultErr != nil {
			m.mu.Lock()
			m.status.LastError = "certificate_issue_failed"
			if renew {
				m.status.RenewalState = "failed"
			} else {
				m.status.EnrollmentState = "failed"
			}
			m.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return errors.New("local key generation failed")
	}
	u, _ := AgentURI(m.options.AgentID)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}}, key)
	if err != nil {
		return errors.New("CSR creation failed")
	}
	body, err := json.Marshal(request{AgentID: m.options.AgentID, CSR: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})})
	if err != nil {
		return errors.New("CSR encoding failed")
	}
	roots, err := loadRoots(m.options.BootstrapCAFile)
	if err != nil {
		return errors.New("enrollment trust unavailable")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: m.options.BootstrapServerName}
	path := "/enroll"
	if renew {
		path = "/renew"
		m.mu.RLock()
		tlsConfig.Certificates = []tls.Certificate{m.pair}
		m.mu.RUnlock()
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, DisableKeepAlives: true, MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: m.options.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(m.options.EnrollmentURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid enrollment request")
	}
	r.Header.Set("Content-Type", "application/json")
	if !renew {
		token, ok := os.LookupEnv(m.options.TokenEnv)
		if !ok || token == "" || len(token) > 4096 || bytes.ContainsAny([]byte(token), "\r\n\x00") {
			return errors.New("enrollment token unavailable")
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("enrollment transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("enrollment rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPKIBytes+1))
	if err != nil || len(data) > maxPKIBytes {
		return errors.New("invalid enrollment response")
	}
	var result response
	if decodeStrict(data, &result) != nil {
		return errors.New("invalid enrollment response")
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return errors.New("local key encoding failed")
	}
	b := bundle{Version: 1, AgentID: m.options.AgentID, Certificate: result.Certificate, CA: result.CA, PrivateKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}
	pair, pool, err := m.validate(b, true)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(b)
	if err != nil || len(encoded) > maxPKIBytes {
		return errors.New("certificate bundle too large")
	}
	if err = m.writeBundle(m.path(), encoded); err != nil {
		return errors.New("certificate storage unavailable")
	}
	m.mu.Lock()
	m.pair, m.roots = pair, pool
	m.status.EnrollmentState = "enrolled"
	m.status.RenewalState = "idle"
	m.status.ExpiresAt = pair.Leaf.NotAfter
	if renew {
		m.status.LastSuccessfulRenewal = time.Now().UTC()
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) validate(b bundle, fresh bool) (tls.Certificate, *x509.CertPool, error) {
	pair, err := tls.X509KeyPair(b.Certificate, b.PrivateKey)
	if err != nil || len(pair.Certificate) == 0 {
		return tls.Certificate{}, nil, errors.New("invalid client certificate bundle")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("invalid client certificate")
	}
	if err = VerifyIdentity(leaf, m.options.AgentID); err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CA) {
		return tls.Certificate{}, nil, errors.New("invalid issued CA chain")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return tls.Certificate{}, nil, errors.New("invalid client certificate chain")
		}
		intermediates.AddCert(c)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return tls.Certificate{}, nil, errors.New("client certificate verification failed")
	}
	if fresh && time.Until(leaf.NotAfter) <= m.options.Timeout {
		return tls.Certificate{}, nil, errors.New("issued certificate lifetime too short")
	}
	if m.options.ControllerCAFile != "" {
		pool, err = loadRoots(m.options.ControllerCAFile)
		if err != nil {
			return tls.Certificate{}, nil, errors.New("controller trust unavailable")
		}
	}
	pair.Leaf = leaf
	return pair, pool, nil
}

func loadRoots(path string) (*x509.CertPool, error) {
	data, err := readBounded(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("invalid CA")
	}
	return pool, nil
}
func readBounded(path string) ([]byte, error) {
	// Reject special trust/bundle files before Open, which otherwise waits
	// indefinitely for a FIFO writer outside enrollment's HTTP timeout.
	// Local administrator CA symlinks may still resolve to regular files.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxPKIBytes {
		return nil, errors.New("invalid certificate file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid certificate file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPKIBytes+1))
	if err != nil || len(data) > maxPKIBytes {
		return nil, errors.New("certificate file exceeds limit")
	}
	return data, nil
}
func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".client-bundle-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	err = f.Chmod(0600)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}
