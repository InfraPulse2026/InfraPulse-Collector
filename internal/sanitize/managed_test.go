package sanitize

import (
	"encoding/json"
	"strings"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestManagedStatusSecretsAreRedactedAndNotMutated(t *testing.T) {
	r := model.ConfigApplyResult{Status: model.ConfigStatusRejected, ErrorMessage: "password=hidden credential-value"}
	in := model.Envelope{ConfigApplyResult: &r}
	out := (KnownSecrets{Values: []string{"credential-value"}}).Sanitize(in)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "hidden") || strings.Contains(string(raw), "credential-value") {
		t.Fatal("config error leaked secret")
	}
	if in.ConfigApplyResult.ErrorMessage != r.ErrorMessage || out.ConfigApplyResult == in.ConfigApplyResult {
		t.Fatal("sanitizer mutated incoming status")
	}
	in = model.Envelope{Heartbeat: &model.Heartbeat{ManagedConfig: &r}}
	out = (KnownSecrets{Values: []string{"credential-value"}}).Sanitize(in)
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), "credential-value") {
		t.Fatal("heartbeat leaked secret")
	}
}
