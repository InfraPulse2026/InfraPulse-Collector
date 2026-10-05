package agent

import (
	"InfraPulseAgent/internal/pki"
	"context"
	"time"
)

// SetCertificateManager is called before Run. The manager publishes immutable
// credentials; reconnect loads the new TLS snapshot after a successful renewal.
func (a *Agent) SetCertificateManager(m *pki.Manager) { a.certificates = m }
func (a *Agent) certificateLoop(ctx context.Context) error {
	if a.certificates == nil {
		<-ctx.Done()
		return nil
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		rotated, err := a.certificates.RenewIfDue(ctx)
		if err != nil && ctx.Err() == nil {
			a.recordError("certificate: renewal failed")
			a.event("certificate.renewal_failed", "warn", "certificate renewal failed; current certificate retained")
		}
		if rotated {
			a.event("certificate.rotated", "info", "client certificate rotated")
			a.mu.Lock()
			cancel := a.activeCancel
			a.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		timer.Reset(time.Minute)
	}
}
