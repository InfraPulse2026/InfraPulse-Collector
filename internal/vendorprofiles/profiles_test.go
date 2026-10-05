package vendorprofiles

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRegistryFixtures(t *testing.T) {
	data, err := os.ReadFile("testdata/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var records []struct {
		ID, Vendor    string
		EnterpriseOID string `json:"enterprise_oid"`
		Source        string
	}
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatal(len(records))
	}
	for _, record := range records {
		t.Run(record.ID, func(t *testing.T) {
			for _, oid := range []string{record.EnterpriseOID, strings.TrimPrefix(record.EnterpriseOID, "."), record.EnterpriseOID + ".4294967294"} {
				vendor, evidence := Detect(oid, "unknown model", "wrong configured hint")
				if vendor != record.Vendor {
					t.Fatalf("%s -> %q", oid, vendor)
				}
				if evidence[len(evidence)-1].Value != record.Source {
					t.Fatal(evidence)
				}
			}
			for _, oid := range []string{"", record.EnterpriseOID + "0", record.EnterpriseOID + ".bad", record.EnterpriseOID + ".", record.EnterpriseOID + "..1", record.EnterpriseOID + ".4294967296", "garbage"} {
				if vendor, _ := Detect(oid, "unclassified", record.Vendor); vendor != "" {
					t.Fatalf("false detection %q -> %s", oid, vendor)
				}
			}
		})
	}
}

func TestEvidenceAndImmutableRegistry(t *testing.T) {
	vendor, evidence := Detect(".1.3.6.1.4.1.999.1", "MikroTik RouterOS", "Cisco")
	if vendor != "MikroTik" || evidence[0].Source != "config.vendor_hint" || evidence[len(evidence)-1].Confidence != "low" {
		t.Fatal(vendor, evidence)
	}
	list := All()
	list[0].Vendor = "modified"
	if All()[0].Vendor != "Cisco" {
		t.Fatal("registry aliased")
	}
}
