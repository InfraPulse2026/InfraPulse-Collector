package snmp

import (
	"errors"
	"strings"

	"github.com/gosnmp/gosnmp"
)

func init() {
	// The upstream cache is a process-global, unbounded map keyed by
	// passphrases. Disable it before any exchanges start so credential
	// rotations do not retain every previous secret in that map. Localized
	// keys remain scoped to bounded individual sessions.
	gosnmp.PasswordCaching(false)
}

// Security describes the protocol and algorithms, never credential material.
// Names match the pinned GoSNMP constants, including the distinct AES variants.
type Security struct {
	Version         string `yaml:"version"`
	SecurityLevel   string `yaml:"security_level"`
	AuthProtocol    string `yaml:"auth_protocol"`
	PrivacyProtocol string `yaml:"privacy_protocol"`
	ContextName     string `yaml:"context_name"`
}

func (s Security) WithDefaults() Security {
	if s.Version == "" {
		s.Version = "v2c"
	}
	if s.Version == "v3" {
		if s.SecurityLevel == "" {
			s.SecurityLevel = "authPriv"
		}
		if s.AuthProtocol == "" {
			s.AuthProtocol = "SHA256"
		}
		if s.SecurityLevel == "authPriv" && s.PrivacyProtocol == "" {
			s.PrivacyProtocol = "AES"
		}
	}
	return s
}

func (s Security) Validate() error {
	s = s.WithDefaults()
	if s.Version == "v2c" {
		if s.SecurityLevel != "" || s.AuthProtocol != "" || s.PrivacyProtocol != "" || s.ContextName != "" {
			return errors.New("SNMPv2c cannot contain SNMPv3 security settings")
		}
		return nil
	}
	if s.Version != "v3" || s.SecurityLevel != "authPriv" && s.SecurityLevel != "authNoPriv" || len(s.ContextName) > 32 || strings.ContainsAny(s.ContextName, "\x00\r\n") {
		return errors.New("invalid SNMP version, security level or context")
	}
	if _, ok := authProtocols[s.AuthProtocol]; !ok {
		return errors.New("unsupported SNMP authentication algorithm")
	}
	if s.SecurityLevel == "authPriv" {
		if _, ok := privacyProtocols[s.PrivacyProtocol]; !ok {
			return errors.New("unsupported SNMP privacy algorithm")
		}
	} else if s.PrivacyProtocol != "" {
		return errors.New("authNoPriv cannot contain a privacy algorithm")
	}
	return nil
}

var authProtocols = map[string]gosnmp.SnmpV3AuthProtocol{
	"MD5": gosnmp.MD5, "SHA": gosnmp.SHA, "SHA224": gosnmp.SHA224,
	"SHA256": gosnmp.SHA256, "SHA384": gosnmp.SHA384, "SHA512": gosnmp.SHA512,
}

var privacyProtocols = map[string]gosnmp.SnmpV3PrivProtocol{
	"DES": gosnmp.DES, "AES": gosnmp.AES, "AES192": gosnmp.AES192,
	"AES256": gosnmp.AES256, "AES192C": gosnmp.AES192C, "AES256C": gosnmp.AES256C,
}
