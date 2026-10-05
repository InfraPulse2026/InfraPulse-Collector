package sanitize

import (
	"strings"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestTextConfiguredSecretsAndPrivateKeys(t *testing.T) {
	text := "unlabelled snmp-value/router-password Authorization: Bearer abc.def token=token-value password=router-password community=snmp-value\n-----BEGIN RSA PRIVATE KEY-----\nTOPSECRETBASE64\n-----END RSA PRIVATE KEY-----"
	values := []string{"snmp-value", "router-password", "token-value"}
	result := TextWithSecrets(text, values)
	for _, secret := range []string{"snmp-value", "router-password", "abc.def", "token-value", "TOPSECRETBASE64"} {
		if strings.Contains(result, secret) {
			t.Fatalf("secret %q remains in sanitized text", secret)
		}
	}
	if values[0] != "snmp-value" {
		t.Fatal("sanitizer mutated configured values")
	}
}

func TestKnownSecretsClonesLogsAndAudit(t *testing.T) {
	original := model.Envelope{Logs: &model.LogBatch{Source: "auditd", Records: []model.LogRecord{{Message: "password=unmasked", Raw: "configured-value", Host: "configured-value", Process: "configured-value", Audit: &model.AuditRecord{Executable: "/tmp/configured-value", Host: "configured-value", Result: "configured-value"}}}}}
	result := (KnownSecrets{Values: []string{"configured-value"}}).Sanitize(original)
	r := result.Logs.Records[0]
	if strings.Contains(r.Message, "unmasked") || strings.Contains(r.Raw+r.Host+r.Process+r.Audit.Executable+r.Audit.Host+r.Audit.Result, "configured-value") {
		t.Fatal("log secret retained")
	}
	if original.Logs.Records[0].Raw != "configured-value" || original.Logs.Records[0].Audit.Host != "configured-value" {
		t.Fatal("sanitizer mutated source model")
	}
}

func TestTextSecretsOverlapAndEmptyValues(t *testing.T) {
	if got := TextWithSecrets("secret-long", []string{"", "secret", "secret-long", "secret-long"}); got != "[REDACTED]" {
		t.Fatal(got)
	}
}

func TestShortSecretsOnePassAndIdempotentMarkers(t *testing.T) {
	for _, values := range [][]string{
		{"D", "E", "R"},
		{"[REDACTED]", "[REDACTED]suffix"},
		{"[", "]", "REDACTED", "BEGIN"},
		{"abc", "bcde"},
	} {
		input := "password=value Authorization: Bearer value [REDACTED]suffix abcde " + strings.Join(values, " ")
		first := TextWithSecrets(input, values)
		second := TextWithSecrets(first, values)
		if first != second {
			t.Fatalf("non-idempotent short-secret sanitation: %q -> %q", first, second)
		}
		if len(first) > 10*len(input) {
			t.Fatal("replacement text was expanded recursively")
		}
		for _, value := range values {
			if strings.Contains(first, value) {
				t.Fatalf("configured marker/substring %q remains", value)
			}
		}
	}
}

func TestPEMPatternAppliedBeforeCredentialSubstrings(t *testing.T) {
	input := "-----BEGIN RSA PRIVATE KEY-----\nKEYBODY_NOT_A_CONFIGURED_SECRET\n-----END RSA PRIVATE KEY-----"
	for _, values := range [][]string{{"BEGIN"}, {"PRIVATE"}, {"BEGIN", "PRIVATE", "D", "E"}} {
		first := TextWithSecrets(input, values)
		if strings.Contains(first, "KEYBODY_NOT_A_CONFIGURED_SECRET") || first != TextWithSecrets(first, values) {
			t.Fatal("literal replacement broke PEM detection or idempotence")
		}
	}
}

func TestAllKnownMarkersSensitiveFailsClosed(t *testing.T) {
	values := []string{"[", "█", "■", "●", "�", "~", "#", "!"}
	if got := TextWithSecrets("all marker characters "+strings.Join(values, " "), values); got != "" {
		t.Fatal("no safe marker exists but sensitive text remains")
	}
}
