package managedconfig

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

// The fixed v1 digest must survive normalization, target ordering and delivery
// metadata changes. JSON input also exercises malformed and mismatched options.
func FuzzManagedCanonicalDigest(f *testing.F) {
	for _, seed := range []model.DesiredCollectorConfig{
		{CollectorID: collectorID},
		{CollectorID: collectorID, Targets: []model.ManagedTargetSpec{target("snmp", "switch"), target("ipmi", "bmc"), target("sip", "pbx")}},
		{CollectorID: collectorID, Targets: []model.ManagedTargetSpec{{ID: "bad", Protocol: "snmp", IPMI: &model.ManagedIPMIOptions{}}}},
	} {
		raw, err := json.Marshal(seed)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	f.Add([]byte(`{"targets":[null]}`))
	f.Add([]byte(`{"targets":[{"protocol":"unknown"}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		var in model.DesiredCollectorConfig
		if json.Unmarshal(raw, &in) != nil {
			return
		}
		normalized, err := Normalize(in)
		if err != nil {
			return
		}
		first, err := Digest(in)
		if err != nil {
			t.Fatal("normalized snapshot cannot be digested", err)
		}
		second, err := Digest(normalized)
		if err != nil || second != first {
			t.Fatal("normalization changed canonical digest", err)
		}
		// Duplicate target IDs are rejected by Validate and have no valid ordering
		// contract. Restrict the ordering property to unique IDs.
		seen := make(map[string]bool, len(normalized.Targets))
		for _, target := range normalized.Targets {
			if seen[target.ID] {
				return
			}
			seen[target.ID] = true
		}
		slices.Reverse(normalized.Targets)
		normalized.Revision = ^uint64(0)
		normalized.SessionID = "different-delivery-session"
		normalized.ConfigurationDigest = "untrusted-supplied-digest"
		normalized.IssuedAt = time.Unix(1, 0).UTC()
		third, err := Digest(normalized)
		if err != nil || third != first {
			t.Fatal("target order/delivery metadata changed canonical digest", err)
		}
	})
}
