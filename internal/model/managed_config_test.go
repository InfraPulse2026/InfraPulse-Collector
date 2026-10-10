package model

import "testing"

func TestManagedConfigCloneOwnsAllOptions(t *testing.T) {
	port, cipher := 623, 17
	c := DesiredCollectorConfig{Targets: []ManagedTargetSpec{{ID: "original", SNMP: &ManagedSNMPOptions{Version: "v3"}}, {SIP: &ManagedSIPOptions{Transport: "tls"}}, {Redfish: &ManagedRedfishOptions{Auth: "session"}}, {IPMI: &ManagedIPMIOptions{Port: &port, CipherSuite: &cipher}}}}
	clone := CloneDesiredCollectorConfig(c)
	clone.Targets[0].ID = "changed"
	clone.Targets[0].SNMP.Version = "v2c"
	clone.Targets[1].SIP.Transport = "udp"
	clone.Targets[2].Redfish.Auth = "basic"
	*clone.Targets[3].IPMI.Port = 624
	*clone.Targets[3].IPMI.CipherSuite = 3
	if c.Targets[0].ID != "original" || c.Targets[0].SNMP.Version != "v3" || c.Targets[1].SIP.Transport != "tls" || c.Targets[2].Redfish.Auth != "session" || port != 623 || cipher != 17 {
		t.Fatal("configuration ownership aliases a pending snapshot")
	}
}
