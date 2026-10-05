package grpc

import (
	"context"
	"errors"

	"InfraPulseAgent/internal/pki"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// VerifyPeerIdentity binds a verified mTLS principal to the hello claim.
// Plaintext contexts are reserved for the explicit loopback development mode.
func VerifyPeerIdentity(ctx context.Context, agentID string) error {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return nil
	}
	t, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(t.State.VerifiedChains) == 0 || len(t.State.PeerCertificates) == 0 {
		return errors.New("verified client certificate required")
	}
	return pki.VerifyIdentity(t.State.PeerCertificates[0], agentID)
}
