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
