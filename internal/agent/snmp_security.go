package agent

// SNMPSecurityStatus exposes configured transport settings, never the USM user,
// credential references, passwords, engine keys or other secret material.
type SNMPSecurityStatus struct {
	DeviceID        string `json:"device_id"`
	Version         string `json:"version"`
	SecurityLevel   string `json:"security_level,omitempty"`
	AuthProtocol    string `json:"auth_protocol,omitempty"`
	PrivacyProtocol string `json:"privacy_protocol,omitempty"`
}

func (a *Agent) snmpSecurityStatus() []SNMPSecurityStatus {
	var status []SNMPSecurityStatus
	for _, device := range a.cfg.Devices {
		if device.Driver != "snmp" {
			continue
		}
		s := device.SNMP.Security.WithDefaults()
		status = append(status, SNMPSecurityStatus{DeviceID: device.ID, Version: s.Version, SecurityLevel: s.SecurityLevel, AuthProtocol: s.AuthProtocol, PrivacyProtocol: s.PrivacyProtocol})
	}
	return status
}
