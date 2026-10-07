package model

import (
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// SourceType describes the observation domain, independently of the vendor or
// the carrier protocol. An LLDP observation can, for example, arrive over SNMP.
type SourceType string

const (
	SourceLocal    SourceType = "local"
	SourceSNMP     SourceType = "snmp"
	SourceRouterOS SourceType = "routeros"
	SourceLLDP     SourceType = "lldp"
	SourceCDP      SourceType = "cdp"
	SourceMNDP     SourceType = "mndp"
	SourceSIP      SourceType = "sip"
	SourceSyslog   SourceType = "syslog"
	SourceAuditd   SourceType = "auditd"
	// These values reserve domain vocabulary, not implemented capabilities.
	SourceRedfish SourceType = "redfish"
	SourceIPMI    SourceType = "ipmi"
)

func (s SourceType) Valid() bool {
	switch s {
	case SourceLocal, SourceSNMP, SourceRouterOS, SourceLLDP, SourceCDP, SourceMNDP,
		SourceSIP, SourceSyslog, SourceAuditd, SourceRedfish, SourceIPMI:
		return true
	default:
		return false
	}
}

type Provenance string

const (
	ProvenanceReal      Provenance = "real"
	ProvenanceSimulated Provenance = "simulated"
	ProvenanceManual    Provenance = "manual"
	ProvenanceExternal  Provenance = "external"
)

// SourceRef is safe provenance metadata. Credentials, endpoints and vendor
// claims belong to their own domains and must not be copied here. TargetID is
// supplied by orchestration; it is never inferred from an observed DeviceID.
type SourceRef struct {
	Type        SourceType `json:"type"`
	CollectorID string     `json:"collector_id,omitempty"`
	TargetID    string     `json:"target_id,omitempty"`
	Protocol    string     `json:"protocol,omitempty"`
	InstanceID  string     `json:"instance_id,omitempty"`
	Provenance  Provenance `json:"provenance"`
}

func (s SourceRef) Validate() error {
	if !s.Type.Valid() {
		return errors.New("unknown observation source type")
	}
	for _, value := range []string{s.CollectorID, s.TargetID, s.InstanceID} {
		if !opaqueIdentifier(value, 256, true) {
			return errors.New("invalid observation source identifier")
		}
	}
	if !identifier(s.Protocol, 128, true) {
		return errors.New("invalid observation carrier protocol")
	}
	switch s.Provenance {
	case "", ProvenanceReal, ProvenanceSimulated, ProvenanceManual, ProvenanceExternal:
		return nil
	default:
		return errors.New("unknown observation provenance")
	}
}

// ForLegacy preserves the domain/carrier while retaining the historical
// simulated marker. Legacy source strings remain unchanged on the payload.
func (s SourceRef) ForLegacy(source string) SourceRef {
	if s.Provenance == "" {
		s.Provenance = ProvenanceReal
	}
	if source == "simulated" {
		s.Provenance = ProvenanceSimulated
	}
	return s
}

// Target is a configured connection/polling target, not a Backend asset or a
// promise that one target yields exactly one device. Existing configuration
// remains responsible for protocol-specific options and credential validation.
type Target struct {
	ID            string            `json:"id"`
	Name          string            `json:"name,omitempty"`
	Type          SourceType        `json:"type"`
	Protocol      string            `json:"protocol,omitempty"`
	Endpoint      string            `json:"endpoint,omitempty"`
	CredentialRef string            `json:"-"`
	Enabled       bool              `json:"enabled"`
	Polling       TargetPolling     `json:"polling"`
	Location      PhysicalLocation  `json:"location,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
}

// Zero intervals mean that no corresponding collection job is configured.
// Adapters keep their existing bounded scheduling and protocol-specific limits.
type TargetPolling struct {
	FastInterval      time.Duration `json:"fast_interval,omitempty"`
	MediumInterval    time.Duration `json:"medium_interval,omitempty"`
	InventoryInterval time.Duration `json:"inventory_interval,omitempty"`
	TopologyInterval  time.Duration `json:"topology_interval,omitempty"`
	Timeout           time.Duration `json:"timeout,omitempty"`
}

func (t Target) Validate() error {
	if !opaqueIdentifier(t.ID, 256, false) || !t.Type.Valid() {
		return errors.New("target requires a bounded identifier and known source type")
	}
	if !boundedText(t.Name, 256) || !identifier(t.Protocol, 128, true) || !opaqueIdentifier(t.CredentialRef, 256, true) {
		return errors.New("invalid target metadata")
	}
	if !boundedText(t.Endpoint, 4096) || t.Enabled && t.Type != SourceLocal && t.Endpoint == "" {
		return errors.New("target endpoint is missing or invalid")
	}
	if strings.Contains(t.Endpoint, "://") {
		u, err := url.Parse(t.Endpoint)
		if err != nil || u.User != nil {
			return errors.New("target endpoint must not contain credentials")
		}
	}
	for _, interval := range []time.Duration{t.Polling.FastInterval, t.Polling.MediumInterval,
		t.Polling.InventoryInterval, t.Polling.TopologyInterval, t.Polling.Timeout} {
		if interval < 0 {
			return errors.New("target polling durations must not be negative")
		}
	}
	if len(t.Labels) > 64 {
		return errors.New("target label count exceeds limit")
	}
	for key, value := range t.Labels {
		if !identifier(key, 128, false) || !boundedText(value, 1024) {
			return errors.New("invalid target label")
		}
	}
	return nil
}

type ClockQuality string

const (
	ClockUnknown    ClockQuality = "unknown"
	ClockSynced     ClockQuality = "synced"
	ClockAcceptable ClockQuality = "acceptable"
	ClockDegraded   ClockQuality = "degraded"
	ClockUnsynced   ClockQuality = "unsynced"
)

// ObservationTime separates independent clocks. SourceTimestamp is reported by
// the source; ObservedAt is the Collector's assessment of the state time;
// CollectedAt is completion/recording at the Collector. ReceivedAt belongs to
// the Backend and is intentionally absent. ClockOffset, when actually measured,
// is source clock minus Collector clock. Uncertainty is a nonnegative +/- bound.
// Nil offset/uncertainty are unknown, rather than measured zero. No ordering is
// enforced between wall times: clock correction can move them backwards.
type ObservationTime struct {
	SourceTimestamp *time.Time     `json:"source_timestamp,omitempty"`
	ObservedAt      time.Time      `json:"observed_at"`
	CollectedAt     time.Time      `json:"collected_at"`
	ClockOffset     *time.Duration `json:"clock_offset,omitempty"`
	Uncertainty     *time.Duration `json:"uncertainty,omitempty"`
	Quality         ClockQuality   `json:"clock_quality"`
}

func (t ObservationTime) Validate() error {
	if !observationInstant(t.ObservedAt) || !observationInstant(t.CollectedAt) ||
		t.SourceTimestamp != nil && !observationInstant(*t.SourceTimestamp) {
		return errors.New("observation time is missing or outside supported range")
	}
	if t.Uncertainty != nil && *t.Uncertainty < 0 {
		return errors.New("observation uncertainty must not be negative")
	}
	switch t.Quality {
	case "", ClockUnknown, ClockSynced, ClockAcceptable, ClockDegraded, ClockUnsynced:
		return nil
	default:
		return errors.New("unknown observation clock quality")
	}
}

type ObservationMetadata struct {
	Source SourceRef       `json:"source"`
	Time   ObservationTime `json:"time"`
}

func (o ObservationMetadata) Validate() error {
	return errors.Join(o.Source.Validate(), o.Time.Validate())
}

// NewObservation adapts known legacy times without pretending to measure clock
// synchronization. A missing observed time uses the collection recording time.
// Source time is optional and never implicitly becomes ObservedAt or an offset.
func NewObservation(ref SourceRef, legacySource string, observed, collected time.Time, sourceTimestamp *time.Time) *ObservationMetadata {
	if observed.IsZero() {
		observed = collected
	}
	o := &ObservationMetadata{Source: ref.ForLegacy(legacySource), Time: ObservationTime{
		ObservedAt: observed.UTC(), CollectedAt: collected.UTC(), Quality: ClockUnknown,
	}}
	if sourceTimestamp != nil {
		at := sourceTimestamp.UTC()
		o.Time.SourceTimestamp = &at
	}
	return o
}

func CloneObservation(o *ObservationMetadata) *ObservationMetadata {
	if o == nil {
		return nil
	}
	clone := *o
	clone.Time.SourceTimestamp = clonePointer(o.Time.SourceTimestamp)
	clone.Time.ClockOffset = clonePointer(o.Time.ClockOffset)
	clone.Time.Uncertainty = clonePointer(o.Time.Uncertainty)
	return &clone
}

func observationFor(existing *ObservationMetadata, ref SourceRef, legacy string, observed, collected time.Time, sourceTimestamp *time.Time) *ObservationMetadata {
	if existing == nil {
		return NewObservation(ref, legacy, observed, collected, sourceTimestamp)
	}
	o := CloneObservation(existing)
	// A source may produce metadata before the runtime identity is available.
	// Binding never rewrites target identity, source clocks or previous timings.
	if o.Source.CollectorID == "" {
		o.Source.CollectorID = ref.CollectorID
	}
	if legacy == "simulated" {
		o.Source.Provenance = ProvenanceSimulated
	}
	return o
}

func observationInstant(t time.Time) bool {
	return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999
}

func boundedText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, ch := range s {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func identifier(s string, max int, optional bool) bool {
	if s == "" {
		return optional
	}
	if !boundedText(s, max) {
		return false
	}
	for _, ch := range s {
		if unicode.IsSpace(ch) {
			return false
		}
	}
	return true
}

// Existing configured IDs are opaque strings and can contain spaces. Domain
// adaptation preserves them exactly, while rejecting control-character input.
func opaqueIdentifier(s string, max int, optional bool) bool {
	return (s != "" || optional) && boundedText(s, max)
}
