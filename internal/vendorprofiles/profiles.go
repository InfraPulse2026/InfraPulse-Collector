// Package vendorprofiles holds evidence-backed manufacturer detection rules.
// A profile changes neither driver operations nor device capabilities.
package vendorprofiles

import (
	"strconv"
	"strings"

	"InfraPulseAgent/internal/model"
)

type Profile struct {
	ID, Vendor, EnterprisePrefix, EvidenceURL string
}

// The PEN registry defines these roots, not the availability of vendor MIBs on
// an arbitrary device. No product or health OIDs are inferred from a PEN.
var profiles = [...]Profile{
	{"cisco", "Cisco", ".1.3.6.1.4.1.9", "https://www.iana.org/assignments/enterprise-numbers/?page=1"},
	{"huawei", "Huawei", ".1.3.6.1.4.1.2011", "https://www.iana.org/assignments/enterprise-numbers/?page=21"},
	{"mikrotik", "MikroTik", ".1.3.6.1.4.1.14988", "https://www.iana.org/assignments/enterprise-numbers/?page=150"},
	{"juniper", "Juniper", ".1.3.6.1.4.1.2636", "https://www.iana.org/assignments/enterprise-numbers/?page=27"},
	{"eltex", "Eltex", ".1.3.6.1.4.1.35265", "https://www.iana.org/assignments/enterprise-numbers/?page=353"},
	{"qtech", "QTECH", ".1.3.6.1.4.1.27514", "https://www.iana.org/assignments/enterprise-numbers/?page=276"},
}

func All() []Profile { return append([]Profile(nil), profiles[:]...) }

// Detect preserves raw observations and hints as separate evidence. Exact arc
// boundaries prevent e.g. Cisco's 9 from matching another enterprise's 90.
// Unknown products still use standard SYSTEM/ENTITY/IF/LLDP mappings.
func Detect(objectID, description, hint string) (string, []model.Evidence) {
	var evidence []model.Evidence
	if hint != "" {
		evidence = append(evidence, model.Evidence{Source: "config.vendor_hint", Value: hint, Confidence: "low"})
	}
	if objectID != "" {
		evidence = append(evidence, model.Evidence{Source: "snmp.sysObjectID", Value: objectID, Confidence: "high"})
	}
	if oid, ok := canonicalOID(objectID); ok {
		for _, p := range profiles {
			if oid == p.EnterprisePrefix || strings.HasPrefix(oid, p.EnterprisePrefix+".") {
				evidence = append(evidence, model.Evidence{Source: "verified.sysObjectID.prefix", Value: p.Vendor, Confidence: "high"}, model.Evidence{Source: "vendor.profile.registry", Value: p.EvidenceURL, Confidence: "high"})
				return p.Vendor, evidence
			}
		}
	}
	// Preserve P1's explicit weak MikroTik description observation. It cannot
	// establish a product model or make an unsupported operation available.
	if strings.Contains(strings.ToLower(description), "mikrotik") {
		evidence = append(evidence, model.Evidence{Source: "snmp.sysDescr", Value: "MikroTik", Confidence: "low"})
		return "MikroTik", evidence
	}
	return "", evidence
}

func canonicalOID(value string) (string, bool) {
	if len(value) == 0 || len(value) > 2048 || strings.HasSuffix(value, ".") {
		return "", false
	}
	value = strings.TrimPrefix(value, ".")
	arcs := strings.Split(value, ".")
	if len(arcs) < 2 || len(arcs) > 128 {
		return "", false
	}
	for _, arc := range arcs {
		if arc == "" || len(arc) > 1 && arc[0] == '0' {
			return "", false
		}
		if _, err := strconv.ParseUint(arc, 10, 32); err != nil {
			return "", false
		}
	}
	first, _ := strconv.ParseUint(arcs[0], 10, 32)
	second, _ := strconv.ParseUint(arcs[1], 10, 32)
	if first > 2 || first < 2 && second > 39 {
		return "", false
	}
	return "." + value, true
}
