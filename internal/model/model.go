// Package model contains transport-independent normalized InfraPulse values.
package model

import "time"

type Capability struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

type PhysicalLocation struct {
	Site       string `json:"site,omitempty" yaml:"site"`
	Datacenter string `json:"datacenter,omitempty" yaml:"datacenter"`
	Building   string `json:"building,omitempty" yaml:"building"`
	Room       string `json:"room,omitempty" yaml:"room"`
	Rack       string `json:"rack,omitempty" yaml:"rack"`
	RackUnit   int    `json:"rack_unit,omitempty" yaml:"rack_unit"`
	Position   string `json:"position,omitempty" yaml:"position"`
	Comment    string `json:"comment,omitempty" yaml:"comment"`
}

type Device struct {
	Hardware          *HardwareInventory   `json:"hardware,omitempty"`
	Observation       *ObservationMetadata `json:"observation,omitempty"`
	IdentityEvidence  []IdentityEvidence   `json:"identity_evidence,omitempty"`
	Source            string               `json:"source,omitempty"`
	BoardName         string               `json:"board_name,omitempty"`
	PlatformEvidence  []Evidence           `json:"platform_evidence,omitempty"`
	Services          []Service            `json:"services,omitempty"`
	Firewall          *FirewallSnapshot    `json:"firewall,omitempty"`
	Networks          []NetworkState       `json:"networks,omitempty"`
	ID                string               `json:"id"`
	Name              string               `json:"name,omitempty"`
	Hostname          string               `json:"hostname,omitempty"`
	ManagementAddress string               `json:"management_address,omitempty"`
	Vendor            string               `json:"vendor,omitempty"`
	Model             string               `json:"model,omitempty"`
	Type              string               `json:"type"`
	SerialNumber      string               `json:"serial_number,omitempty"`
	HardwareRevision  string               `json:"hardware_revision,omitempty"`
	SoftwareVersion   string               `json:"software_version,omitempty"`
	FirmwareVersion   string               `json:"firmware_version,omitempty"`
	OS                string               `json:"os,omitempty"`
	Distribution      string               `json:"distribution,omitempty"`
	Kernel            string               `json:"kernel,omitempty"`
	Architecture      string               `json:"architecture,omitempty"`
	ChassisID         string               `json:"chassis_id,omitempty"`
	Driver            string               `json:"driver"`
	UptimeSeconds     float64              `json:"uptime_seconds"`
	BootID            string               `json:"boot_id,omitempty"`
	Online            bool                 `json:"online"`
	LastSeen          time.Time            `json:"last_seen"`
	Location          PhysicalLocation     `json:"location"`
	Capabilities      []Capability         `json:"capabilities"`
	SystemDescription string               `json:"system_description,omitempty"`
	SysObjectID       string               `json:"sys_object_id,omitempty"`
	SystemLocation    string               `json:"system_location,omitempty"`
	SystemContact     string               `json:"system_contact,omitempty"`
	VendorHint        string               `json:"vendor_hint,omitempty"`
	VendorEvidence    []Evidence           `json:"vendor_evidence,omitempty"`
	Components        []InventoryComponent `json:"components,omitempty"`
	UptimeAvailable   *bool                `json:"uptime_available,omitempty"`
	UptimeSource      string               `json:"uptime_source,omitempty"`
}

type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

type AgentHello struct {
	ManagedConfigV1         bool                `json:"managed_config_v1,omitempty"`
	RemoteManagementEnabled bool                `json:"remote_management_enabled,omitempty"`
	AppliedConfig           *AppliedConfigState `json:"applied_config,omitempty"`
	ApplicationACK          bool                `json:"application_ack,omitempty"`
	AgentID                 string              `json:"agent_id"`
	SessionID               string              `json:"session_id"`
	Name                    string              `json:"name"`
	Build                   BuildInfo           `json:"build"`
	StartedAt               time.Time           `json:"started_at"`
	Device                  Device              `json:"device"`
	ProtocolVersion         uint32              `json:"protocol_version"`
}

type CPUStats struct {
	LogicalCPUs   int      `json:"logical_cpus"`
	PhysicalCores int      `json:"physical_cores,omitempty"`
	Model         string   `json:"model,omitempty"`
	UsagePercent  *float64 `json:"usage_percent,omitempty"`
	Load1         float64  `json:"load1"`
	Load5         float64  `json:"load5"`
	Load15        float64  `json:"load15"`
}

type MemoryStats struct {
	TotalBytes        uint64   `json:"total_bytes"`
	UsedBytes         uint64   `json:"used_bytes"`
	AvailableBytes    uint64   `json:"available_bytes"`
	FreeBytes         uint64   `json:"free_bytes"`
	SwapTotalBytes    uint64   `json:"swap_total_bytes"`
	SwapUsedBytes     uint64   `json:"swap_used_bytes"`
	PressureSomeAvg10 *float64 `json:"pressure_some_avg10,omitempty"`
}

type Counters struct {
	RXBytes   uint64 `json:"rx_bytes"`
	TXBytes   uint64 `json:"tx_bytes"`
	RXPackets uint64 `json:"rx_packets"`
	TXPackets uint64 `json:"tx_packets"`
	RXErrors  uint64 `json:"rx_errors"`
	TXErrors  uint64 `json:"tx_errors"`
	RXDropped uint64 `json:"rx_dropped"`
	TXDropped uint64 `json:"tx_dropped"`
}

type Rates struct {
	RXBytesPerSecond   float64              `json:"rx_bytes_per_second"`
	TXBytesPerSecond   float64              `json:"tx_bytes_per_second"`
	RXPacketsPerSecond float64              `json:"rx_packets_per_second"`
	TXPacketsPerSecond float64              `json:"tx_packets_per_second"`
	RXErrorsPerSecond  float64              `json:"rx_errors_per_second"`
	TXErrorsPerSecond  float64              `json:"tx_errors_per_second"`
	RXDroppedPerSecond float64              `json:"rx_dropped_per_second"`
	TXDroppedPerSecond float64              `json:"tx_dropped_per_second"`
	Available          *CounterAvailability `json:"available,omitempty"`
}

type Interface struct {
	ID                 string             `json:"id"`
	Index              int                `json:"index"`
	Name               string             `json:"name"`
	MAC                string             `json:"mac,omitempty"`
	MTU                int                `json:"mtu"`
	AdminUp            bool               `json:"admin_up"`
	OperState          string             `json:"oper_state"`
	SpeedBitsPerSecond *uint64            `json:"speed_bits_per_second,omitempty"`
	Addresses          []string           `json:"addresses,omitempty"`
	Counters           Counters           `json:"counters"`
	Rates              *Rates             `json:"rates,omitempty"`
	Description        string             `json:"description,omitempty"`
	Alias              string             `json:"alias,omitempty"`
	Type               *uint32            `json:"type,omitempty"`
	Metadata           *InterfaceMetadata `json:"metadata,omitempty"`
	CounterMetadata    *CounterMetadata   `json:"counter_metadata,omitempty"`
}

type Metric struct {
	ResourceID  string               `json:"resource_id,omitempty"`
	ComponentID string               `json:"component_id,omitempty"`
	Status      *HardwareStatus      `json:"status,omitempty"`
	Observation *ObservationMetadata `json:"observation,omitempty"`
	Name        string               `json:"name"`
	Value       float64              `json:"value"`
	Unit        string               `json:"unit"`
}

type TelemetrySample struct {
	HardwareHealth   []ComponentHealth    `json:"hardware_health,omitempty"`
	Observation      *ObservationMetadata `json:"observation,omitempty"`
	IdentityEvidence []IdentityEvidence   `json:"identity_evidence,omitempty"`
	DeviceID         string               `json:"device_id"`
	ObservedAt       time.Time            `json:"observed_at"`
	CollectedAt      time.Time            `json:"collected_at"`
	Source           string               `json:"source"`
	BootID           string               `json:"boot_id,omitempty"`
	UptimeSeconds    float64              `json:"uptime_seconds"`
	CPU              *CPUStats            `json:"cpu,omitempty"`
	Memory           *MemoryStats         `json:"memory,omitempty"`
	Interfaces       []Interface          `json:"interfaces,omitempty"`
	Metrics          []Metric             `json:"metrics,omitempty"`
	UptimeAvailable  *bool                `json:"uptime_available,omitempty"`
	UptimeSource     string               `json:"uptime_source,omitempty"`
}

type QueueState struct {
	Mode             string    `json:"mode,omitempty"`
	StorageError     string    `json:"storage_error,omitempty"`
	StorageBytes     int64     `json:"storage_bytes,omitempty"`
	OldestPending    time.Time `json:"oldest_pending,omitempty"`
	Unacked          int       `json:"unacked,omitempty"`
	PendingResults   int       `json:"pending_results,omitempty"`
	DroppedLogs      uint64    `json:"dropped_logs"`
	Messages         int       `json:"messages"`
	Bytes            int       `json:"bytes"`
	DroppedTelemetry uint64    `json:"dropped_telemetry"`
	DroppedOther     uint64    `json:"dropped_other"`
}

type Heartbeat struct {
	ManagedConfig     *ConfigApplyResult `json:"managed_config,omitempty"`
	Certificate       *CertificateStatus `json:"certificate,omitempty"`
	Timestamp         time.Time          `json:"timestamp"`
	UptimeSeconds     float64            `json:"uptime_seconds"`
	Build             BuildInfo          `json:"build"`
	Health            string             `json:"health"`
	RemoteDeviceCount int                `json:"remote_device_count"`
	Queue             QueueState         `json:"queue"`
}

type CertificateStatus struct {
	ExpiresAt             time.Time `json:"expires_at"`
	RemainingSeconds      int64     `json:"remaining_seconds"`
	EnrollmentState       string    `json:"enrollment_state"`
	RenewalState          string    `json:"renewal_state"`
	LastSuccessfulRenewal time.Time `json:"last_successful_renewal,omitempty"`
	LastError             string    `json:"last_error,omitempty"`
}

type InventoryUpdate struct {
	Completeness string               `json:"completeness,omitempty"` // additive; empty retains legacy semantics
	Observation  *ObservationMetadata `json:"observation,omitempty"`
	ObservedAt   time.Time            `json:"observed_at"`
	Devices      []Device             `json:"devices"`
}

type Event struct {
	Observation *ObservationMetadata `json:"observation,omitempty"`
	ID          string               `json:"id"`
	Type        string               `json:"type"`
	DeviceID    string               `json:"device_id,omitempty"`
	Timestamp   time.Time            `json:"timestamp"`
	Severity    string               `json:"severity"`
	Message     string               `json:"message"`
	Source      string               `json:"source"`
}

type ErrorReport struct {
	Component string    `json:"component"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

type InterfaceAction struct {
	InterfaceID string `json:"interface_id"`
	Operation   string `json:"operation"`
}

type ServiceAction struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
}

type Command struct {
	ID             string           `json:"id"`
	TargetDeviceID string           `json:"target_device_id"`
	RequestedAt    time.Time        `json:"requested_at"`
	Timeout        time.Duration    `json:"timeout"`
	Initiator      string           `json:"initiator"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
	Interface      *InterfaceAction `json:"interface,omitempty"`
	Service        *ServiceAction   `json:"service,omitempty"`
	Rollback       *RollbackAction  `json:"rollback,omitempty"`
}

type CommandResult struct {
	CommandID    string                 `json:"command_id"`
	Status       string                 `json:"status"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at"`
	Changed      bool                   `json:"changed"`
	ErrorCode    string                 `json:"error_code,omitempty"`
	ErrorMessage string                 `json:"error_message,omitempty"`
	Verification *InterfaceVerification `json:"verification,omitempty"`
	Uncertain    bool                   `json:"uncertain,omitempty"`
	SnapshotID   string                 `json:"snapshot_id,omitempty"`
}

// Envelope has one payload. Validate/mapping rejects missing or multiple payloads.
type Envelope struct {
	ConfigApplyResult *ConfigApplyResult `json:"config_apply_result,omitempty"`
	Logs              *LogBatch          `json:"logs,omitempty"`
	Telephony         *TelephonyUpdate   `json:"telephony,omitempty"`
	MessageID         string             `json:"message_id"`
	AgentID           string             `json:"agent_id"`
	SessionID         string             `json:"session_id"`
	Sequence          uint64             `json:"sequence"`
	SentAt            time.Time          `json:"sent_at"`
	Hello             *AgentHello        `json:"hello,omitempty"`
	Heartbeat         *Heartbeat         `json:"heartbeat,omitempty"`
	Telemetry         *TelemetrySample   `json:"telemetry,omitempty"`
	Inventory         *InventoryUpdate   `json:"inventory,omitempty"`
	Events            []Event            `json:"events,omitempty"`
	CommandResult     *CommandResult     `json:"command_result,omitempty"`
	Error             *ErrorReport       `json:"error,omitempty"`
	Topology          *TopologyUpdate    `json:"topology,omitempty"`
}

type ControllerMessage struct {
	ManagedConfigV1 bool
	DesiredConfig   *DesiredCollectorConfig
	// ConfigError is a fixed safe error code; invalid configuration payloads
	// are rejected by the configuration worker without disrupting ACK/replay.
	ConfigError     string
	ApplicationACK  bool
	ACKMessageID    string
	HelloAccepted   bool
	ProtocolVersion uint32
	Command         *Command
}
