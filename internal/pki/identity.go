// Package pki owns local enrollment keys and verified certificate rotation.
package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"

	"github.com/google/uuid"
)

func AgentURI(agentID string) (*url.URL, error) {
	id, err := uuid.Parse(agentID)
	if err != nil || id == uuid.Nil || id.String() != agentID {
		return nil, errors.New("invalid certificate agent identity")
	}
	return url.Parse("spiffe://infrapulse/agent/" + agentID)
}

func VerifyIdentity(cert *x509.Certificate, agentID string) error {
	u, err := AgentURI(agentID)
	if err != nil || cert == nil || len(cert.URIs) != 1 || cert.URIs[0].String() != u.String() {
		return errors.New("certificate identity does not match agent identity")
	}
	return nil
}

func VerifyCertificateFile(certFile, keyFile, agentID string) error {
	certPEM, certErr := readBounded(certFile)
	keyPEM, keyErr := readBounded(keyFile)
	if certErr != nil || keyErr != nil {
		return errors.New("client certificate unavailable")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return errors.New("client certificate unavailable")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("invalid client certificate")
	}
	return VerifyIdentity(leaf, agentID)
}
