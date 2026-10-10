package model

import (
	"slices"
	"time"
)

const (
	ManagedConfigCapability = "managed_config_v1"
	ConfigStatusReceived    = "received"
	ConfigStatusValidating  = "validating"
	ConfigStatusApplied     = "applied"
	ConfigStatusRejected    = "rejected"
	ConfigStatusFailed      = "failed"
)

// DesiredCollectorConfig is a full snapshot of the Backend-owned namespace.
// SessionID and IssuedAt correlate delivery, and are excluded from its digest.
// No credential values, local paths or administrative settings are representable.
type DesiredCollectorConfig struct {
	CollectorID         string              `json:"collector_id"`
	SessionID           string              `json:"session_id,omitempty"`
	Revision            uint64              `json:"revision"`
	ConfigurationDigest string              `json:"configuration_digest"`
	IssuedAt            time.Time           `json:"issued_at,omitempty"`
	Targets             []ManagedTargetSpec `json:"targets"`
}

type ManagedTargetSpec struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name,omitempty"`
	Protocol      string                 `json:"protocol"`
	Enabled       bool                   `json:"enabled"`
	Endpoint      string                 `json:"endpoint"`
	CredentialRef string                 `json:"credential_ref,omitempty"`
	VendorHint    string                 `json:"vendor_hint,omitempty"`
	Location      PhysicalLocation       `json:"location,omitempty"`
	Polling       TargetPolling          `json:"polling"`
	CARef         string                 `json:"ca_ref,omitempty"`
	SNMP          *ManagedSNMPOptions    `json:"snmp,omitempty"`
	SIP           *ManagedSIPOptions     `json:"sip,omitempty"`
	Redfish       *ManagedRedfishOptions `json:"redfish,omitempty"`
	IPMI          *ManagedIPMIOptions    `json:"ipmi,omitempty"`
}

type ManagedSNMPOptions struct {
	Version         string `json:"version,omitempty"`
	SecurityLevel   string `json:"security_level,omitempty"`
	AuthProtocol    string `json:"auth_protocol,omitempty"`
	PrivacyProtocol string `json:"privacy_protocol,omitempty"`
	ContextName     string `json:"context_name,omitempty"`
	Port            uint16 `json:"port,omitempty"`
	Retries         int    `json:"retries,omitempty"`
}

type ManagedSIPOptions struct {
	Port             uint16        `json:"port,omitempty"`
	Transport        string        `json:"transport,omitempty"`
	FromUser         string        `json:"from_user,omitempty"`
	ToUser           string        `json:"to_user,omitempty"`
	Domain           string        `json:"domain,omitempty"`
	ServerName       string        `json:"server_name,omitempty"`
	FailureThreshold uint32        `json:"failure_threshold,omitempty"`
	LatencyThreshold time.Duration `json:"latency_threshold,omitempty"`
}

type ManagedRedfishOptions struct {
	Auth           string        `json:"auth,omitempty"`
	RequestTimeout time.Duration `json:"request_timeout,omitempty"`
	MaxResources   int           `json:"max_resources,omitempty"`
	MaxPages       int           `json:"max_pages,omitempty"`
	MaxRequests    int           `json:"max_requests,omitempty"`
}

type ManagedIPMIOptions struct {
	Port               *int          `json:"port,omitempty"`
	CipherSuite        *int          `json:"cipher_suite,omitempty"`
	Privilege          string        `json:"privilege,omitempty"`
	Retries            int           `json:"retries,omitempty"`
	RequestTimeout     time.Duration `json:"request_timeout,omitempty"`
	MaxSessionDuration time.Duration `json:"max_session_duration,omitempty"`
	MaxOperations      int           `json:"max_operations,omitempty"`
	MaxSDRRecords      int           `json:"max_sdr_records,omitempty"`
	MaxSDRBytes        int           `json:"max_sdr_bytes,omitempty"`
	MaxFRURecords      int           `json:"max_fru_records,omitempty"`
	MaxFRUBytes        int           `json:"max_fru_bytes,omitempty"`
	MaxPollBytes       int           `json:"max_poll_bytes,omitempty"`
	SDRCacheTTL        time.Duration `json:"sdr_cache_ttl,omitempty"`
}

type AppliedConfigState struct {
	Revision            uint64    `json:"revision"`
	ConfigurationDigest string    `json:"configuration_digest,omitempty"`
	Status              string    `json:"status,omitempty"`
	AppliedAt           time.Time `json:"applied_at,omitempty"`
	TargetCount         int       `json:"target_count"`
}

// ConfigApplyResult reports a configuration transaction, not telemetry receipt.
type ConfigApplyResult struct {
	CollectorID         string    `json:"collector_id"`
	Revision            uint64    `json:"revision"`
	ConfigurationDigest string    `json:"configuration_digest,omitempty"`
	Status              string    `json:"status"`
	AppliedAt           time.Time `json:"applied_at,omitempty"`
	ErrorCode           string    `json:"error_code,omitempty"`
	ErrorMessage        string    `json:"error_message,omitempty"`
	AppliedRevision     uint64    `json:"applied_revision"`
	AppliedDigest       string    `json:"applied_digest,omitempty"`
}

func CloneDesiredCollectorConfig(c DesiredCollectorConfig) DesiredCollectorConfig {
	c.Targets = slices.Clone(c.Targets)
	for i := range c.Targets {
		t := &c.Targets[i]
		t.SNMP = clonePointer(t.SNMP)
		t.SIP = clonePointer(t.SIP)
		t.Redfish = clonePointer(t.Redfish)
		t.IPMI = clonePointer(t.IPMI)
		if t.IPMI != nil {
			t.IPMI.Port, t.IPMI.CipherSuite = clonePointer(t.IPMI.Port), clonePointer(t.IPMI.CipherSuite)
		}
	}
	return c
}
