package model

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const MaxIdentityPerSubject = 64
const MaxIdentityEvidence = 4096

// Normalizers never generate missing identifiers or perform DNS/network I/O.
// Text identifiers retain case, punctuation and leading zeroes.
func NormalizeMAC(value string) (string, error) {
	if !safeIdentifierText(value, 128) {
		return "", errors.New("invalid MAC identifier")
	}
	v, err := net.ParseMAC(strings.TrimSpace(value))
	if err != nil {
		return "", errors.New("invalid MAC identifier")
	}
	return v.String(), nil
}

func NormalizeUUID(value string) (string, error) {
	if !safeIdentifierText(value, 128) {
		return "", errors.New("invalid UUID identifier")
	}
	v, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || v == uuid.Nil || v == uuid.Max {
		return "", errors.New("invalid or unspecified UUID identifier")
	}
	return v.String(), nil
}

func NormalizeIP(value string) (canonical, family string, err error) {
	if !safeIdentifierText(value, 128) {
		return "", "", errors.New("invalid IP identifier")
	}
	v, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || v.Zone() != "" {
		return "", "", errors.New("invalid or unscoped IP identifier")
	}
	// Preserve IPv4-mapped IPv6 as IPv6 rather than assuming false equality.
	family = "ipv6"
	if v.Is4() {
		family = "ipv4"
	}
	return v.String(), family, nil
}

func NormalizeSerial(value string) (string, error) {
	return normalizeText(value, 1024)
}

func NormalizeHostname(value string) (string, error) {
	// Case folding, trailing-dot removal and Unicode normalization can create
	// false equality for opaque protocol names. Keep those distinctions.
	return normalizeText(value, 255)
}

func normalizeText(value string, max int) (string, error) {
	if !safeIdentifierText(value, max) {
		return "", errors.New("invalid identifier text")
	}
	value = strings.Trim(value, " ")
	if value == "" {
		return "", errors.New("missing identifier")
	}
	return value, nil
}

func safeIdentifierText(value string, max int) bool {
	return value != "" && utf8.ValidString(value) && boundedText(value, max)
}

// NewIdentityFact keeps the original when normalization changes a value. Invalid
// identifiers are diagnostics, never matching keys. Rejected inputs are replaced
// by a fixed bounded diagnostic, without echoing hostile data in errors.
func NewIdentityFact(kind IdentityEvidenceType, value string, source SourceRef, confidence, subjectID, subjectKind, origin string, at *ObservationTime) (IdentityEvidence, error) {
	e := IdentityEvidence{Type: kind, Value: value, Source: source.ForLegacy(""), Confidence: confidence,
		SubjectID: subjectID, SubjectKind: subjectKind, Origin: origin, Time: CloneObservationTime(at)}
	if !safeIdentifierText(value, 4096) {
		e.Value, e.Normalization = "[rejected identifier]", "rejected"
		return e, errors.New("identity input rejected by text limit")
	}
	var canonical string
	var err error
	switch kind {
	case IdentityMAC:
		canonical, err = NormalizeMAC(value)
	case IdentityHardwareUUID:
		canonical, err = NormalizeUUID(value)
	case IdentityIP, IdentityManagementIP:
		canonical, _, err = NormalizeIP(value)
	case IdentityHostname, IdentitySysName:
		canonical, err = NormalizeHostname(value)
	case IdentitySerial:
		canonical, err = NormalizeSerial(value)
	default:
		canonical, err = normalizeText(value, 4096)
	}
	if err != nil {
		e.Normalization = "invalid"
		return e, err
	}
	e.Value, e.Normalization = canonical, "canonical"
	if canonical != value {
		e.OriginalValue = value
	}
	return e, nil
}

func CloneObservationTime(at *ObservationTime) *ObservationTime {
	if at == nil {
		return nil
	}
	v := *at
	v.SourceTimestamp = clonePointer(at.SourceTimestamp)
	v.ClockOffset = clonePointer(at.ClockOffset)
	v.Uncertainty = clonePointer(at.Uncertainty)
	return &v
}

// MergeIdentityEvidence only deduplicates an exact fact within this observation.
// Conflicting values, different subjects, origins or protocols remain separate.
// Admission validates total and per-subject budgets instead of silently dropping
// explicit source facts. A malformed batch is rejected with a safe diagnostic.
func MergeIdentityEvidence(existing, derived []IdentityEvidence) []IdentityEvidence {
	if len(existing) > MaxIdentityEvidence {
		// Already-owned adapter data remains explicit for admission to reject;
		// never amplify an invalid source list by deriving and cloning more facts.
		return existing
	}
	result := make([]IdentityEvidence, 0, min(len(existing)+len(derived), MaxIdentityEvidence))
	seen := make(map[string]bool)
	counts := make(map[identitySubjectKey]int)
	diagnostics := make(map[identitySubjectKey]int)
	add := func(values []IdentityEvidence) {
		for _, v := range values {
			// Existing source facts are not deleted on a partial collection cycle.
			key := identityFactKey(v)
			if seen[key] {
				continue
			}
			seen[key] = true
			result = append(result, v)
			subject := identitySubjectKey{v.Source, v.SubjectID, v.SubjectKind}
			counts[subject]++
			if isIdentityLimitDiagnostic(v) {
				diagnostics[subject] = len(result) - 1
			}
		}
	}
	add(existing)
	observed := make(map[identityValueKey]bool, len(existing))
	for _, v := range existing {
		observed[identityValue(v)] = true
	}
	blocked := make(map[identitySubjectKey]bool)
	for _, candidate := range derived {
		if !observed[identityValue(candidate)] && !seen[identityFactKey(candidate)] {
			subject := identitySubjectKey{candidate.Source, candidate.SubjectID, candidate.SubjectKind}
			if len(result) >= MaxIdentityEvidence-1 {
				for _, v := range result {
					if v.Normalization == "rejected" && v.Origin == "collector.identity_evidence_limit" {
						return cloneIdentityEvidence(result)
					}
				}
				if index, exists := diagnostics[subject]; exists {
					// Reuse this subject's reserved diagnostic instead of adding
					// a 65th fact when the total and subject limits coincide.
					result[index].Origin = "collector.identity_evidence_limit"
				} else {
					add([]IdentityEvidence{identityLimitDiagnostic(candidate, "collector.identity_evidence_limit")})
				}
				break
			}
			if blocked[subject] {
				continue
			}
			if counts[subject] >= MaxIdentityPerSubject-1 {
				if _, exists := diagnostics[subject]; !exists {
					// Explicit facts stay intact. If they already use all 64
					// slots, this diagnostic makes admission reject the batch
					// instead of silently removing an explicit source fact.
					diagnostic := identityLimitDiagnostic(candidate, "collector.identity_subject_limit")
					if isIdentityLimitDiagnostic(candidate) {
						diagnostic = candidate
					}
					add([]IdentityEvidence{diagnostic})
				}
				blocked[subject] = true
				continue
			}
			add([]IdentityEvidence{candidate})
			if candidate.Normalization == "rejected" && candidate.Origin == "collector.identity_evidence_limit" {
				break
			}
		}
	}
	return cloneIdentityEvidence(result)
}

func identityLimitDiagnostic(source IdentityEvidence, origin string) IdentityEvidence {
	source.Value, source.OriginalValue, source.RawValue, source.Subtype, source.Normalization = "[rejected identifier]", "", nil, nil, "rejected"
	source.Origin = origin
	return source
}

func isIdentityLimitDiagnostic(v IdentityEvidence) bool {
	return v.Normalization == "rejected" && (v.Origin == "collector.identity_subject_limit" || v.Origin == "collector.identity_evidence_limit")
}

// identityAccumulator bounds derived facts while they are being constructed,
// before cloning/encoding. Limits produce an explicit nonmatching diagnostic.
type identityAccumulator struct {
	values      []IdentityEvidence
	counts      map[identitySubjectKey]int
	diagnostics map[identitySubjectKey]int
	full        bool
}
type identitySubjectKey struct {
	source   SourceRef
	id, kind string
}

func (a *identityAccumulator) add(v IdentityEvidence) {
	if a.full {
		return
	}
	if a.counts == nil {
		a.counts = make(map[identitySubjectKey]int)
		a.diagnostics = make(map[identitySubjectKey]int)
	}
	key := identitySubjectKey{v.Source, v.SubjectID, v.SubjectKind}
	if len(a.values) >= MaxIdentityEvidence-1 {
		if index, exists := a.diagnostics[key]; exists {
			a.values[index].Origin = "collector.identity_evidence_limit"
		} else {
			a.diagnostics[key] = len(a.values)
			a.values = append(a.values, identityLimitDiagnostic(v, "collector.identity_evidence_limit"))
			a.counts[key]++
		}
		a.full = true
		return
	}
	if a.counts[key] >= MaxIdentityPerSubject-1 {
		if _, exists := a.diagnostics[key]; !exists {
			a.diagnostics[key] = len(a.values)
			a.values = append(a.values, identityLimitDiagnostic(v, "collector.identity_subject_limit"))
			a.counts[key]++
		}
		return
	}
	a.counts[key]++
	a.values = append(a.values, v)
}

type identityValueKey struct {
	Type                IdentityEvidenceType
	Value, Raw, Subject string
	Source              SourceRef
	Subtype             uint32
	HasSubtype          bool
}

func identityValue(e IdentityEvidence) identityValueKey {
	v := identityValueKey{Type: e.Type, Value: e.Value, Raw: string(e.RawValue), Subject: e.SubjectID, Source: e.Source}
	if e.Subtype != nil {
		v.Subtype = *e.Subtype
		v.HasSubtype = true
	}
	return v
}
