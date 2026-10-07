package agent

import "InfraPulseAgent/internal/platform/timesync"

// SetTimeSyncStatus stores a startup observation from the OS setup layer.
// No Apply is exposed to commands or remote adapters. Monitoring stays usable
// when status is unavailable, permissions are missing, or NTP is still converging.
func (a *Agent) SetTimeSyncStatus(s timesync.Status) {
	s = timesync.Clone(s)
	a.mu.Lock()
	a.timeSync = &s
	a.mu.Unlock()
	if s.Enabled && (s.Diagnostic != "" || !s.Available || !s.Active || !s.Configured || s.Synchronized == nil || !*s.Synchronized) {
		a.logger.Warn("OS time synchronization needs attention; monitoring continues", "provider", s.Provider, "available", s.Available, "active", s.Active, "configured", s.Configured, "synchronized", s.Synchronized, "diagnostic", s.Diagnostic)
	}
}
