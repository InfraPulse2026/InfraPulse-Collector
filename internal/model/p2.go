package model

import "time"

// P2 read-only inventory never conveys a command, argv, or raw configuration.
type Service struct {
	Observation *ObservationMetadata `json:"observation,omitempty"`
	Name        string               `json:"name"`
	State       string               `json:"state"`
	Enabled     *bool                `json:"enabled,omitempty"`
	ObservedAt  time.Time            `json:"observed_at"`
	Source      string               `json:"source"`
	Instances   []ServiceInstance    `json:"instances,omitempty"`
}
type ServiceInstance struct {
	Name    string  `json:"name"`
	Running *bool   `json:"running,omitempty"`
	PID     *uint32 `json:"pid,omitempty"`
}
type NetworkState struct {
	Observation *ObservationMetadata `json:"observation,omitempty"`
	Name        string               `json:"name"`
	Device      string               `json:"device,omitempty"`
	Protocol    string               `json:"protocol,omitempty"`
	Up          *bool                `json:"up,omitempty"`
	Pending     *bool                `json:"pending,omitempty"`
	Addresses   []string             `json:"addresses,omitempty"`
	Source      string               `json:"source"`
	ObservedAt  time.Time            `json:"observed_at"`
}
type FirewallSnapshot struct {
	Observation  *ObservationMetadata `json:"observation,omitempty"`
	ObservedAt   time.Time            `json:"observed_at"`
	Source       string               `json:"source"`
	Completeness string               `json:"completeness"`
	Enabled      *bool                `json:"enabled,omitempty"`
	Zones        []FirewallZone       `json:"zones,omitempty"`
	Forwardings  []FirewallForwarding `json:"forwardings,omitempty"`
	Rules        []FirewallRule       `json:"rules,omitempty"`
	Redirects    []FirewallRedirect   `json:"redirects,omitempty"`
}
type FirewallZone struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Networks []string `json:"networks,omitempty"`
	Input    string   `json:"input,omitempty"`
	Output   string   `json:"output,omitempty"`
	Forward  string   `json:"forward,omitempty"`
	Enabled  *bool    `json:"enabled,omitempty"`
}
type FirewallForwarding struct {
	ID              string `json:"id"`
	SourceZone      string `json:"source_zone,omitempty"`
	DestinationZone string `json:"destination_zone,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
}
type FirewallRule struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	SourceZone      string `json:"source_zone,omitempty"`
	DestinationZone string `json:"destination_zone,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	SourceIP        string `json:"source_ip,omitempty"`
	DestinationIP   string `json:"destination_ip,omitempty"`
	SourcePort      string `json:"source_port,omitempty"`
	DestinationPort string `json:"destination_port,omitempty"`
	Target          string `json:"target,omitempty"`
	Family          string `json:"family,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"`
}
type FirewallRedirect struct {
	FirewallRule
	ExternalPort string `json:"external_port,omitempty"`
	InternalIP   string `json:"internal_ip,omitempty"`
	InternalPort string `json:"internal_port,omitempty"`
}
type AuditRecord struct {
	EventID    string  `json:"event_id,omitempty"`
	RecordType string  `json:"record_type,omitempty"`
	UID        *uint32 `json:"uid,omitempty"`
	AUID       *uint32 `json:"auid,omitempty"`
	PID        *uint32 `json:"pid,omitempty"`
	Executable string  `json:"executable,omitempty"`
	Syscall    string  `json:"syscall,omitempty"`
	Success    *bool   `json:"success,omitempty"`
	Result     string  `json:"result,omitempty"`
	Host       string  `json:"host,omitempty"`
}
type LogRecord struct {
	Observation       *ObservationMetadata `json:"observation,omitempty"`
	ID                string               `json:"id"`
	DeviceID          string               `json:"device_id"`
	Source            string               `json:"source"`
	SourceID          string               `json:"source_id"`
	ObservedAt        time.Time            `json:"observed_at"`
	OriginalTimestamp *time.Time           `json:"original_timestamp,omitempty"`
	Severity          *uint32              `json:"severity,omitempty"`
	Facility          *uint32              `json:"facility,omitempty"`
	Host              string               `json:"host,omitempty"`
	Process           string               `json:"process,omitempty"`
	ProcessID         string               `json:"process_id,omitempty"`
	Message           string               `json:"message"`
	Raw               string               `json:"raw,omitempty"`
	Truncated         bool                 `json:"truncated"`
	FileIdentity      string               `json:"file_identity,omitempty"`
	Offset            int64                `json:"offset"`
	Audit             *AuditRecord         `json:"audit,omitempty"`
}
type LogBatch struct {
	Observation *ObservationMetadata `json:"observation,omitempty"`
	ObservedAt  time.Time            `json:"observed_at"`
	Source      string               `json:"source"`
	Records     []LogRecord          `json:"records"`
}
type TelephonyHealth struct {
	Observation         *ObservationMetadata `json:"observation,omitempty"`
	TargetID            string               `json:"target_id"`
	Address             string               `json:"address"`
	Transport           string               `json:"transport"`
	Reachable           bool                 `json:"reachable"`
	ResponseCode        *uint32              `json:"response_code,omitempty"`
	LatencyMillis       *float64             `json:"latency_ms,omitempty"`
	ObservedAt          time.Time            `json:"observed_at"`
	ConsecutiveFailures uint32               `json:"consecutive_failures"`
	State               string               `json:"state"`
	ErrorCategory       string               `json:"error_category,omitempty"`
	Source              string               `json:"source"`
}
type TelephonyUpdate struct {
	Observation *ObservationMetadata `json:"observation,omitempty"`
	ObservedAt  time.Time            `json:"observed_at"`
	Source      string               `json:"source"`
	Targets     []TelephonyHealth    `json:"targets"`
}
