package managedconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/remote"
)

type Diagnostics struct {
	Enabled              bool                     `json:"enabled"`
	Applied              model.AppliedConfigState `json:"applied"`
	DesiredRevision      uint64                   `json:"desired_revision"`
	LastApplyStatus      string                   `json:"last_apply_status,omitempty"`
	LastApplyTime        time.Time                `json:"last_apply_time,omitempty"`
	LastRejectedRevision uint64                   `json:"last_rejected_revision,omitempty"`
	LastError            string                   `json:"last_error,omitempty"`
	ManagedTargets       int                      `json:"managed_targets"`
	StoredRevision       uint64                   `json:"last_known_good_revision"`
	LastSuccessfulSync   time.Time                `json:"last_successful_sync,omitempty"`
	StorageDegraded      bool                     `json:"storage_degraded"`
}

// Manager has one serialized transition at a time. Session workers are bounded
// by the existing lifecycle; reconnect never creates concurrent transactions.
type Manager struct {
	transition  sync.Mutex
	mu          sync.Mutex
	cfg         config.Config
	id, session string
	registry    *remote.Registry
	factory     remote.AdapterFactory
	provider    credentials.Provider
	resolver    Resolver
	store       *store
	state       diskState
	lastResult  model.ConfigApplyResult
	diagnostics Diagnostics
}

func New(cfg config.Config, id, session string, registry *remote.Registry, factory remote.AdapterFactory, provider credentials.Provider) *Manager {
	m := &Manager{cfg: cfg, id: id, session: session, registry: registry, factory: factory, provider: provider, store: newStore(cfg.Agent.StateDir, min(4<<20, 4*cfg.RemoteManagement.WithDefaults().MaxConfigBytes+8192))}
	m.diagnostics.Enabled = cfg.RemoteManagement.Enabled
	state, err := m.store.load(id)
	m.state = state
	m.lastResult = state.LastResult
	m.diagnostics.DesiredRevision = state.HighestRevision
	if state.LastResult.Status == model.ConfigStatusRejected {
		m.diagnostics.LastRejectedRevision = state.LastResult.Revision
	}
	if state.Applied != nil {
		m.diagnostics.StoredRevision = state.Applied.Revision
	}
	if !cfg.RemoteManagement.Enabled && state.Applied != nil {
		m.lastResult = model.ConfigApplyResult{CollectorID: id, Revision: state.Applied.Revision, ConfigurationDigest: state.Applied.ConfigurationDigest, Status: model.ConfigStatusRejected, ErrorCode: "management_disabled", ErrorMessage: "Managed configuration: management_disabled"}
	}
	if err != nil {
		m.diagnostics.StorageDegraded = true
		m.diagnostics.LastError = err.Error()
		m.diagnostics.LastApplyStatus = model.ConfigStatusFailed
	}
	return m
}

func (m *Manager) Snapshot() Diagnostics             { m.mu.Lock(); defer m.mu.Unlock(); return m.diagnostics }
func (m *Manager) Applied() model.AppliedConfigState { return m.Snapshot().Applied }
func (m *Manager) Result() model.ConfigApplyResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.lastResult
	r.CollectorID = m.id
	r.AppliedRevision = m.diagnostics.Applied.Revision
	r.AppliedDigest = m.diagnostics.Applied.ConfigurationDigest
	return r
}

// Restore is independent of Backend availability and always revalidates the
// present local policy and credential availability. Failure leaves local targets.
func (m *Manager) Restore(ctx context.Context) error {
	m.transition.Lock()
	defer m.transition.Unlock()
	if !m.cfg.RemoteManagement.Enabled {
		return nil
	}
	if m.diagnostics.StorageDegraded {
		return errors.New(m.diagnostics.LastError)
	}
	if m.state.Applied == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.cfg.RemoteManagement.WithDefaults().ApplyTimeout)
	defer cancel()
	d, err := Validate(ctx, m.cfg, *m.state.Applied, m.provider, m.resolver)
	if err == nil {
		var p *remote.PreparedManaged
		p, err = m.registry.PrepareManaged(d, m.factory)
		if err == nil {
			defer p.Close()
			err = p.Apply(ctx)
			if err == nil {
				err = p.Commit()
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagnostics.DesiredRevision = m.state.HighestRevision
	if err != nil {
		m.diagnostics.LastError = "restore_rejected"
		m.diagnostics.LastApplyStatus = model.ConfigStatusFailed
		m.lastResult = model.ConfigApplyResult{CollectorID: m.id, Revision: m.state.Applied.Revision, ConfigurationDigest: m.state.Applied.ConfigurationDigest, Status: model.ConfigStatusFailed, ErrorCode: "restore_rejected", ErrorMessage: "Managed configuration: restore_rejected"}
		return errors.New("restore_rejected")
	}
	m.diagnostics.Applied = m.state.AppliedAt
	m.diagnostics.ManagedTargets = len(m.state.Applied.Targets)
	m.diagnostics.LastApplyStatus = model.ConfigStatusApplied
	m.diagnostics.LastApplyTime = m.state.AppliedAt.AppliedAt
	return nil
}

// Apply is called only by the bounded stream worker. Authentication and identity
// failures never advance the persistent revision watermark.
func (m *Manager) Apply(parent context.Context, in model.DesiredCollectorConfig, authenticated bool, decodeError string, report func(model.ConfigApplyResult)) model.ConfigApplyResult {
	m.transition.Lock()
	defer m.transition.Unlock()
	ctx, cancel := context.WithTimeout(parent, m.cfg.RemoteManagement.WithDefaults().ApplyTimeout)
	defer cancel()
	m.mu.Lock()
	applied := m.diagnostics.Applied
	state := m.state
	blocked := m.diagnostics.StorageDegraded
	m.mu.Unlock()
	r := model.ConfigApplyResult{CollectorID: m.id, Revision: in.Revision, ConfigurationDigest: in.ConfigurationDigest, AppliedRevision: applied.Revision, AppliedDigest: applied.ConfigurationDigest}
	if raw, e := hex.DecodeString(r.ConfigurationDigest); e != nil || len(raw) != 32 || hex.EncodeToString(raw) != r.ConfigurationDigest {
		r.ConfigurationDigest = ""
	}
	emit := func(status, code string) {
		r.Status, r.ErrorCode = status, code
		r.ErrorMessage = ""
		if code != "" {
			r.ErrorMessage = "Managed configuration: " + code
		}
		if report != nil {
			report(r)
		}
	}
	rejectUntracked := func(code string) model.ConfigApplyResult { emit(model.ConfigStatusRejected, code); m.note(r); return r }
	if !m.cfg.RemoteManagement.Enabled {
		return rejectUntracked("management_disabled")
	}
	if !authenticated {
		return rejectUntracked("unauthenticated_delivery")
	}
	if in.CollectorID != m.id || in.SessionID != m.session {
		return rejectUntracked("identity_mismatch")
	}
	if parent.Err() != nil {
		emit(model.ConfigStatusFailed, "apply_cancelled")
		m.note(r)
		return r
	}
	emit(model.ConfigStatusReceived, "")
	emit(model.ConfigStatusValidating, "")
	if in.Revision == 0 {
		return rejectUntracked("invalid_revision")
	}
	if blocked {
		emit(model.ConfigStatusFailed, "state_unavailable")
		m.note(r)
		return r
	}
	normalized, normErr := Normalize(in)
	digest, digestErr := Digest(in)
	fingerprintInput := digest + "\x00" + in.ConfigurationDigest
	if digestErr != nil {
		raw, _ := json.Marshal(in.Targets)
		sum := sha256.Sum256(raw)
		fingerprintInput = hex.EncodeToString(sum[:]) + "\x00" + in.ConfigurationDigest
	}
	sum := sha256.Sum256([]byte(fingerprintInput))
	fingerprint := hex.EncodeToString(sum[:])
	if in.Revision == applied.Revision && applied.Revision > 0 {
		if normErr != nil || digest != applied.ConfigurationDigest || in.ConfigurationDigest != digest || decodeError != "" {
			return rejectUntracked("revision_conflict")
		}
		r.AppliedAt = applied.AppliedAt
		emit(model.ConfigStatusApplied, "")
		m.note(r)
		return r
	}
	if in.Revision < state.HighestRevision {
		return rejectUntracked("stale_revision")
	}
	if in.Revision == state.HighestRevision && state.HighestRevision > 0 {
		if fingerprint != state.HighestFingerprint {
			return rejectUntracked("revision_conflict")
		}
		// A stored success is not evidence that its targets are running after
		// restart: restoration can be denied by the current local policy.
		if state.LastResult.Status == model.ConfigStatusApplied {
			emit(model.ConfigStatusFailed, "applied_state_inactive")
			m.note(r)
			return r
		}
		r = state.LastResult
		r.AppliedRevision = applied.Revision
		r.AppliedDigest = applied.ConfigurationDigest
		if report != nil {
			report(r)
		}
		m.note(r)
		return r
	}
	state.HighestRevision, state.HighestFingerprint = in.Revision, fingerprint
	finishFailure := func(status, code string) model.ConfigApplyResult {
		r.Status, r.ErrorCode, r.ErrorMessage = status, code, "Managed configuration: "+code
		state.LastResult = r
		if err := m.store.save(state); err != nil {
			r.Status, r.ErrorCode, r.ErrorMessage = model.ConfigStatusFailed, "state_write_failed", "Managed configuration: state_write_failed"
			m.mu.Lock()
			m.diagnostics.StorageDegraded = true
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			m.state = state
			m.mu.Unlock()
		}
		if report != nil {
			report(r)
		}
		m.note(r)
		return r
	}
	if decodeError != "" {
		return finishFailure(model.ConfigStatusRejected, "invalid_snapshot")
	}
	if normErr != nil {
		return finishFailure(model.ConfigStatusRejected, normErr.Error())
	}
	if digestErr != nil || digest != in.ConfigurationDigest {
		return finishFailure(model.ConfigStatusRejected, "digest_mismatch")
	}
	d, err := Validate(ctx, m.cfg, normalized, m.provider, m.resolver)
	if err != nil {
		return finishFailure(model.ConfigStatusRejected, err.Error())
	}
	p, err := m.registry.PrepareManaged(d, m.factory)
	if err != nil {
		return finishFailure(model.ConfigStatusFailed, "adapter_prepare_failed")
	}
	defer p.Close()
	if err = p.Apply(ctx); err != nil {
		return finishFailure(model.ConfigStatusFailed, "runtime_apply_failed")
	}
	previous := m.state
	normalized.SessionID = ""
	normalized.IssuedAt = time.Time{}
	now := time.Now().UTC()
	state.Applied = &normalized
	state.AppliedAt = model.AppliedConfigState{Revision: in.Revision, ConfigurationDigest: digest, Status: model.ConfigStatusApplied, AppliedAt: now, TargetCount: len(normalized.Targets)}
	r.Status, r.AppliedAt, r.AppliedRevision, r.AppliedDigest = model.ConfigStatusApplied, now, in.Revision, digest
	state.LastResult = r
	if err = m.store.save(state); err != nil {
		// Jobs prepared for this revision have not run. Resume the previous set,
		// and rewrite its state if publication failed after an atomic rename.
		recovery, cancelRecovery := context.WithTimeout(context.Background(), m.cfg.RemoteManagement.WithDefaults().ApplyTimeout)
		rollbackErr := p.Rollback(recovery)
		cancelRecovery()
		r.AppliedAt = time.Time{}
		r.AppliedRevision, r.AppliedDigest = applied.Revision, applied.ConfigurationDigest
		r.Status, r.ErrorCode, r.ErrorMessage = model.ConfigStatusFailed, "state_write_failed", "Managed configuration: state_write_failed"
		previous.HighestRevision, previous.HighestFingerprint = state.HighestRevision, state.HighestFingerprint
		previous.LastResult = r
		restoreErr := m.store.save(previous)
		code := "state_write_failed"
		if rollbackErr != nil || restoreErr != nil {
			code = "persistence_recovery_failed"
		}
		r.AppliedAt = time.Time{}
		r.AppliedRevision, r.AppliedDigest = applied.Revision, applied.ConfigurationDigest
		emit(model.ConfigStatusFailed, code)
		m.mu.Lock()
		m.diagnostics.StorageDegraded = restoreErr != nil
		if restoreErr == nil {
			m.state = previous
		}
		m.mu.Unlock()
		m.note(r)
		return r
	}
	if err = p.Commit(); err != nil {
		recovery, cancelRecovery := context.WithTimeout(context.Background(), m.cfg.RemoteManagement.WithDefaults().ApplyTimeout)
		_ = p.Rollback(recovery)
		cancelRecovery()
		r.AppliedAt = time.Time{}
		r.AppliedRevision, r.AppliedDigest = applied.Revision, applied.ConfigurationDigest
		r.Status, r.ErrorCode, r.ErrorMessage = model.ConfigStatusFailed, "runtime_commit_failed", "Managed configuration: runtime_commit_failed"
		previous.HighestRevision, previous.HighestFingerprint = state.HighestRevision, state.HighestFingerprint
		previous.LastResult = r
		restoreErr := m.store.save(previous)
		m.mu.Lock()
		m.diagnostics.StorageDegraded = restoreErr != nil
		if restoreErr == nil {
			m.state = previous
		}
		m.mu.Unlock()
		r.AppliedAt = time.Time{}
		r.AppliedRevision, r.AppliedDigest = applied.Revision, applied.ConfigurationDigest
		emit(model.ConfigStatusFailed, "runtime_commit_failed")
		m.note(r)
		return r
	}
	m.mu.Lock()
	m.state = state
	m.diagnostics.Applied = state.AppliedAt
	m.diagnostics.ManagedTargets = len(normalized.Targets)
	m.diagnostics.StoredRevision = in.Revision
	m.mu.Unlock()
	emit(model.ConfigStatusApplied, "")
	m.note(r)
	return r
}

func (m *Manager) note(r model.ConfigApplyResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastResult = r
	if r.ErrorCode != "unauthenticated_delivery" && r.ErrorCode != "identity_mismatch" && r.ErrorCode != "management_disabled" {
		m.diagnostics.DesiredRevision = max(m.diagnostics.DesiredRevision, r.Revision)
	}
	m.diagnostics.LastApplyStatus = r.Status
	m.diagnostics.LastError = r.ErrorCode
	m.diagnostics.LastApplyTime = time.Now().UTC()
	if r.Status == model.ConfigStatusRejected {
		m.diagnostics.LastRejectedRevision = r.Revision
	}
	if r.Status == model.ConfigStatusApplied {
		m.diagnostics.LastSuccessfulSync = time.Now().UTC()
	}
}
