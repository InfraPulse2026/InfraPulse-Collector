package managedconfig

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"InfraPulseAgent/internal/model"
)

// This fixture was encoded independently with Python struct.pack('>Q'/'>q')
// and hashlib.sha256. Backend implementations can consume the same vector.
func TestDigestV1IndependentContractVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/digest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Input        model.DesiredCollectorConfig `json:"input"`
		CanonicalHex string                       `json:"canonical_hex"`
		SHA256       string                       `json:"sha256"`
	}
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("invalid vector")
	}
	b, e := CanonicalBytes(v.Input)
	if e != nil {
		t.Fatal(e)
	}
	if hex.EncodeToString(b) != v.CanonicalHex {
		t.Fatal("canonical v1 contract changed")
	}
	digest, e := Digest(v.Input)
	if e != nil || digest != v.SHA256 {
		t.Fatal("digest v1 contract changed", digest, e)
	}
}
