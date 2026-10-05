package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Token struct {
	Value     string `json:"-"`
	AgentID   string
	ExpiresAt time.Time
}

func (Token) String() string   { return "<redacted enrollment token>" }
func (Token) GoString() string { return "<redacted enrollment token>" }

type tokenRecord struct {
	hash      [32]byte
	agentID   string
	expiresAt time.Time
	used      bool
}

// MockEnrollment is development-only: tokens are one-time within this process,
// bounded, preconfigured and never exposed. Restart persistence is not a CA service.
type MockEnrollment struct {
	ca       *x509.Certificate
	signer   crypto.Signer
	caPEM    []byte
	lifetime time.Duration
	mu       sync.Mutex
	tokens   []tokenRecord
}

func NewMockEnrollment(caPEM, keyPEM []byte, tokens []Token, lifetime time.Duration) (*MockEnrollment, error) {
	if len(tokens) == 0 || len(tokens) > 1024 || lifetime < time.Minute || lifetime > 365*24*time.Hour {
		return nil, errors.New("invalid mock enrollment limits")
	}
	pair, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return nil, errors.New("invalid mock signing CA")
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 || !time.Now().Before(ca.NotAfter) {
		return nil, errors.New("invalid mock signing CA")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("invalid mock signing key")
	}
	m := &MockEnrollment{ca: ca, signer: signer, caPEM: append([]byte(nil), caPEM...), lifetime: lifetime}
	seen := map[[32]byte]bool{}
	for _, token := range tokens {
		if token.Value == "" || len(token.Value) > 4096 || strings.ContainsAny(token.Value, "\r\n\x00") || token.ExpiresAt.IsZero() {
			return nil, errors.New("invalid mock enrollment token")
		}
		if _, err := AgentURI(token.AgentID); err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(token.Value))
		if seen[hash] {
			return nil, errors.New("duplicate mock enrollment token")
		}
		seen[hash] = true
		m.tokens = append(m.tokens, tokenRecord{hash: hash, agentID: token.AgentID, expiresAt: token.ExpiresAt})
	}
	return m, nil
}

func (m *MockEnrollment) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil || r.Method != http.MethodPost || (r.URL.Path != "/enroll" && r.URL.Path != "/renew") || r.URL.RawQuery != "" {
		http.Error(w, "unsupported request", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPKIBytes)
	var req request
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil || len(req.CSR) > 32<<10 {
		http.Error(w, "invalid CSR request", http.StatusBadRequest)
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid CSR request", http.StatusBadRequest)
		return
	}
	block, rest := pem.Decode(req.CSR)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(strings.TrimSpace(string(rest))) != 0 {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	publicKey, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() || csr.CheckSignature() != nil || len(csr.URIs) != 1 || len(csr.DNSNames) > 0 || len(csr.IPAddresses) > 0 || len(csr.EmailAddresses) > 0 {
		http.Error(w, "invalid CSR", http.StatusBadRequest)
		return
	}
	u, err := AgentURI(req.AgentID)
	if err != nil || csr.URIs[0].String() != u.String() {
		http.Error(w, "identity mismatch", http.StatusForbidden)
		return
	}
	var tokenIndex = -1
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.URL.Path == "/enroll" {
		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 4103 {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
		hash := sha256.Sum256([]byte(strings.TrimPrefix(authorization, "Bearer ")))
		for i := range m.tokens {
			token := &m.tokens[i]
			if subtle.ConstantTimeCompare(hash[:], token.hash[:]) == 1 && !token.used && token.agentID == req.AgentID && time.Now().Before(token.expiresAt) {
				tokenIndex = i
				break
			}
		}
		if tokenIndex < 0 {
			http.Error(w, "token rejected", http.StatusUnauthorized)
			return
		}
	} else {
		if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 || VerifyIdentity(r.TLS.PeerCertificates[0], req.AgentID) != nil {
			http.Error(w, "renewal identity rejected", http.StatusForbidden)
			return
		}
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		http.Error(w, "issuance failed", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	expires := now.Add(m.lifetime)
	if expires.After(m.ca.NotAfter) {
		expires = m.ca.NotAfter
	}
	if time.Until(expires) < time.Minute {
		http.Error(w, "CA lifetime too short", http.StatusServiceUnavailable)
		return
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: expires, URIs: csr.URIs, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	certDER, err := x509.CreateCertificate(rand.Reader, template, m.ca, csr.PublicKey, m.signer)
	if err != nil {
		http.Error(w, "issuance failed", http.StatusInternalServerError)
		return
	}
	if tokenIndex >= 0 {
		m.tokens[tokenIndex].used = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), CA: m.caPEM})
}
