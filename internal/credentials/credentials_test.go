package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEnvironmentReferencesAndRotation(t *testing.T) {
	t.Setenv("TEST_SNMP_SECRET", "private-community")
	refs := map[string]Reference{"snmp": {CommunityEnv: "TEST_SNMP_SECRET"}}
	p := NewEnvironment(refs)
	delete(refs, "snmp")
	c, err := p.Get(context.Background(), "snmp")
	if err != nil || c.Community != "private-community" {
		t.Fatal("reference resolution failed", err)
	}
	t.Setenv("TEST_SNMP_SECRET", "rotated")
	c, err = p.Get(context.Background(), "snmp")
	if err != nil || c.Community != "rotated" {
		t.Fatal("rotation failed")
	}
	data, _ := json.Marshal(c)
	for _, s := range []string{string(data), fmt.Sprint(c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, "rotated") {
			t.Fatal("credential leaked")
		}
	}
}
func TestMissingAndCanceledAreSafe(t *testing.T) {
	p := NewEnvironment(map[string]Reference{"secret-name": {PasswordEnv: "ABSENT_TEST_ENV"}})
	for _, ref := range []string{"secret-name", "unknown-secret"} {
		_, err := p.Get(context.Background(), ref)
		if err == nil || strings.Contains(err.Error(), ref) {
			t.Fatal("unsafe or missing error")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Get(ctx, "secret-name"); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestSNMPv3EnvironmentSecretsAndRedaction(t *testing.T) {
	t.Setenv("TEST_V3_USER", "private-username")
	t.Setenv("TEST_V3_AUTH", "private-auth-secret")
	t.Setenv("TEST_V3_PRIVACY", "private-privacy-secret")
	p := NewEnvironment(map[string]Reference{"v3": {UsernameEnv: "TEST_V3_USER", AuthPasswordEnv: "TEST_V3_AUTH", PrivacyPasswordEnv: "TEST_V3_PRIVACY"}})
	c, err := p.Get(context.Background(), "v3")
	if err != nil || c.Username != "private-username" || c.AuthPassword != "private-auth-secret" || c.PrivacyPassword != "private-privacy-secret" {
		t.Fatal("v3 resolution failed", err)
	}
	data, _ := json.Marshal(c)
	for _, output := range []string{string(data), fmt.Sprint(c), fmt.Sprintf("%#v", c)} {
		for _, secret := range []string{c.Username, c.AuthPassword, c.PrivacyPassword} {
			if strings.Contains(output, secret) {
				t.Fatal("v3 credential leak")
			}
		}
	}
	t.Setenv("TEST_V3_AUTH", "rotated-private-auth")
	c, err = p.Get(context.Background(), "v3")
	if err != nil || c.AuthPassword != "rotated-private-auth" {
		t.Fatal("v3 rotation failed")
	}
	t.Setenv("TEST_V3_PRIVACY", "")
	if _, err = p.Get(context.Background(), "v3"); err == nil || strings.Contains(err.Error(), "TEST_V3") {
		t.Fatal("missing privacy credential accepted or leaked")
	}
}
