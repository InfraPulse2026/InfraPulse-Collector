package model

import (
	"fmt"
	"testing"
)

func limitFact(source SourceRef, subject string, number int) IdentityEvidence {
	return IdentityEvidence{Type: IdentitySerial, Value: fmt.Sprintf("%06d", number), Source: source, Confidence: "low", SubjectID: subject, SubjectKind: "network_interface"}
}

func assertIdentityBudgets(t *testing.T, values []IdentityEvidence) {
	t.Helper()
	if len(values) > MaxIdentityEvidence {
		t.Fatalf("total budget exceeded: %d", len(values))
	}
	counts := make(map[identitySubjectKey]int)
	for _, v := range values {
		key := identitySubjectKey{v.Source, v.SubjectID, v.SubjectKind}
		counts[key]++
		if counts[key] > MaxIdentityPerSubject {
			t.Fatalf("subject budget exceeded: %+v", key)
		}
	}
}

func TestIdentityTotalLimitReusesSubjectDiagnostic(t *testing.T) {
	ref := SourceRef{Type: SourceSNMP, CollectorID: "collector", TargetID: "remote:switch", Protocol: "snmp", Provenance: ProvenanceSimulated}
	var a identityAccumulator
	for i := 0; i < 2015; i++ {
		a.add(limitFact(ref, fmt.Sprint(i), 1))
		a.add(limitFact(ref, fmt.Sprint(i), 2))
	}
	a.add(limitFact(ref, "one-fact", 1))
	for i := 0; i < 100; i++ {
		a.add(limitFact(ref, "last", i))
	}
	assertIdentityBudgets(t, a.values)
	if len(a.values) != 4095 || a.values[len(a.values)-1].Origin != "collector.identity_evidence_limit" {
		t.Fatal("total overflow must reuse the subject diagnostic", len(a.values))
	}
	merged := MergeIdentityEvidence(nil, a.values)
	assertIdentityBudgets(t, merged)
	if merged[len(merged)-1].Origin != "collector.identity_evidence_limit" {
		t.Fatal("merge lost total-limit diagnostic")
	}
}

func TestIdentityLimitsKeepFullSourceScopesIndependent(t *testing.T) {
	first := SourceRef{Type: SourceSNMP, CollectorID: "collector", TargetID: "remote:device", Protocol: "snmp", InstanceID: "one", ResourceID: "/interface/1", Provenance: ProvenanceReal}
	second := first
	second.Type, second.Protocol, second.InstanceID, second.Provenance = SourceRouterOS, "https", "two", ProvenanceSimulated
	var a identityAccumulator
	for _, ref := range []SourceRef{first, second} {
		for i := 0; i < 100; i++ {
			a.add(limitFact(ref, "same-subject", i))
		}
	}
	assertIdentityBudgets(t, a.values)
	if len(a.values) != 2*MaxIdentityPerSubject {
		t.Fatal("distinct source scopes lost facts", len(a.values))
	}
}

func TestIdentityMergeBoundsDerivedFactsWithoutRemovingExplicitFacts(t *testing.T) {
	ref := SourceRef{Type: SourceSNMP, TargetID: "remote:device", Protocol: "snmp", Provenance: ProvenanceReal}
	existing := []IdentityEvidence{limitFact(ref, "interface", 1000)}
	derived := make([]IdentityEvidence, 100)
	for i := range derived {
		derived[i] = limitFact(ref, "interface", i)
	}
	merged := MergeIdentityEvidence(existing, derived)
	assertIdentityBudgets(t, merged)
	if len(merged) != MaxIdentityPerSubject || merged[0].Value != existing[0].Value || merged[len(merged)-1].Origin != "collector.identity_subject_limit" {
		t.Fatal("explicit facts or truncation diagnostic lost")
	}
	full := make([]IdentityEvidence, MaxIdentityPerSubject)
	for i := range full {
		full[i] = limitFact(ref, "interface", i+1000)
	}
	rejected := MergeIdentityEvidence(full, derived)
	if len(rejected) != MaxIdentityPerSubject+1 {
		t.Fatal("full explicit subject must reject extra derived facts instead of deleting source facts", len(rejected))
	}
	for i := range full {
		if rejected[i].Value != full[i].Value {
			t.Fatal("explicit source fact removed")
		}
	}
}
