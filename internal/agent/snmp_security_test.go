package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	protocolsnmp "InfraPulseAgent/internal/protocol/snmp"
)

func TestSNMPSecurityStatusAndPasswordSanitizer(t *testing.T) {
	t.Setenv("P3_SNMP_AUTH", "secret-auth-value")
	t.Setenv("P3_SNMP_PRIVACY", "secret-privacy-value")
	a := &Agent{cfg: config.Config{Devices: []config.Device{{ID: "v2", Driver: "snmp", CredentialRef: "secret-reference"}, {ID: "v3", Driver: "snmp", CredentialRef: "secret-reference", SNMP: config.SNMP{Security: protocolsnmp.Security{Version: "v3"}}}, {ID: "router", Driver: "routeros"}}, Credentials: map[string]credentials.Reference{"secret-reference": {UsernameEnv: "USER", AuthPasswordEnv: "P3_SNMP_AUTH", PrivacyPasswordEnv: "P3_SNMP_PRIVACY"}}}}
	status := a.snmpSecurityStatus()
	if len(status) != 2 || status[0].Version != "v2c" || status[1].SecurityLevel != "authPriv" || status[1].AuthProtocol != "SHA256" || status[1].PrivacyProtocol != "AES" {
		t.Fatal(status)
	}
	data, _ := json.Marshal(status)
	for _, forbidden := range []string{"secret-reference", "P3_SNMP", "secret-auth-value", "secret-privacy-value"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("secret material in SNMP diagnostics")
		}
	}
	clean := a.sanitizeText("log contains secret-auth-value and secret-privacy-value")
	if strings.Contains(clean, "secret-auth-value") || strings.Contains(clean, "secret-privacy-value") {
		t.Fatal("configured SNMPv3 secret leaked into log sanitizer")
	}
}
