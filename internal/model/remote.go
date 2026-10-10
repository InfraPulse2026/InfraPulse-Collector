package model

import "time"

// Nil metadata retains P0 semantics: all counters are available and 64-bit.
// Remote drivers always supply explicit metadata, including measured zeroes.
type CounterAvailability struct {
	RXBytes   bool `json:"rx_bytes"`
	TXBytes   bool `json:"tx_bytes"`
	RXPackets bool `json:"rx_packets"`
	TXPackets bool `json:"tx_packets"`
	RXErrors  bool `json:"rx_errors"`
	TXErrors  bool `json:"tx_errors"`
	RXDropped bool `json:"rx_dropped"`
	TXDropped bool `json:"tx_dropped"`
}
type CounterWidths struct {
	RXBytes   uint8 `json:"rx_bytes"`
	TXBytes   uint8 `json:"tx_bytes"`
	RXPackets uint8 `json:"rx_packets"`
	TXPackets uint8 `json:"tx_packets"`
	RXErrors  uint8 `json:"rx_errors"`
	TXErrors  uint8 `json:"tx_errors"`
	RXDropped uint8 `json:"rx_dropped"`
	TXDropped uint8 `json:"tx_dropped"`
}
type CounterMetadata struct {
	Available          CounterAvailability `json:"available"`
	Widths             CounterWidths       `json:"widths"`
	DiscontinuityTicks *uint32             `json:"discontinuity_ticks,omitempty"`
}
type InterfaceMetadata struct {
	Index       bool `json:"index"`
	MTU         bool `json:"mtu"`
	AdminStatus bool `json:"admin_status"`
}
type Evidence struct {
	Source     string `json:"source"`
	Value      string `json:"value"`
	Confidence string `json:"confidence"`
}
type InventoryComponent struct {
	Hardware         *HardwareInventory   `json:"hardware,omitempty"`
	Observation      *ObservationMetadata `json:"observation,omitempty"`
	ID               string               `json:"id"`
	ParentID         string               `json:"parent_id,omitempty"`
	Class            string               `json:"class"`
	Name             string               `json:"name,omitempty"`
	Description      string               `json:"description,omitempty"`
	Manufacturer     string               `json:"manufacturer,omitempty"`
	Model            string               `json:"model,omitempty"`
	SerialNumber     string               `json:"serial_number,omitempty"`
	HardwareRevision string               `json:"hardware_revision,omitempty"`
	FirmwareVersion  string               `json:"firmware_version,omitempty"`
	SoftwareVersion  string               `json:"software_version,omitempty"`
}
type TopologyUpdate struct {
	Observation      *ObservationMetadata `json:"observation,omitempty"`
	DeviceID         string               `json:"device_id"`
	ObservedAt       time.Time            `json:"observed_at"`
	CollectedAt      time.Time            `json:"collected_at"`
	Source           string               `json:"source"`
	Completeness     string               `json:"completeness"` // complete, partial, failed
	TTLSeconds       uint32               `json:"ttl_seconds"`  // collector assumption, not advertised LLDP TTL
	ExpiryAssumption string               `json:"expiry_assumption,omitempty"`
	Observations     []Neighbor           `json:"observations"`
}
type Neighbor struct {
	ObservationID               string               `json:"observation_id,omitempty"`
	Kind                        string               `json:"kind,omitempty"` // observed_l2_neighbor, never a verified physical cable
	Observation                 *ObservationMetadata `json:"observation,omitempty"`
	IdentityEvidence            []IdentityEvidence   `json:"identity_evidence,omitempty"`
	LocalDeviceID               string               `json:"local_device_id"`
	LocalInterfaceID            string               `json:"local_interface_id,omitempty"`
	LocalInterfaceName          string               `json:"local_interface_name,omitempty"`
	LocalInterfaceIndex         *uint32              `json:"local_interface_index,omitempty"`
	LocalInterfaceMAC           string               `json:"local_interface_mac,omitempty"`
	LocalInterfaceDescription   string               `json:"local_interface_description,omitempty"`
	LocalInterfaceAlias         string               `json:"local_interface_alias,omitempty"`
	LocalPortNumber             uint32               `json:"local_port_number,omitempty"`
	LocalPortID                 []byte               `json:"local_port_id,omitempty"`
	LocalPortSubtype            uint32               `json:"local_port_subtype"`
	RemoteChassisID             []byte               `json:"remote_chassis_id,omitempty"`
	RemoteChassisSubtype        uint32               `json:"remote_chassis_subtype"`
	RemotePortID                []byte               `json:"remote_port_id,omitempty"`
	RemotePortSubtype           uint32               `json:"remote_port_subtype"`
	RemoteSystemName            string               `json:"remote_system_name,omitempty"`
	RemotePortDescription       string               `json:"remote_port_description,omitempty"`
	RemoteManagementAddresses   []ManagementAddress  `json:"remote_management_addresses,omitempty"`
	RemoteSystemDescription     string               `json:"remote_system_description,omitempty"`
	RemotePlatform              string               `json:"remote_platform,omitempty"`
	RemoteModel                 string               `json:"remote_model,omitempty"`
	RemoteSoftwareVersion       string               `json:"remote_software_version,omitempty"`
	RemoteCapabilitiesSupported []string             `json:"remote_capabilities_supported,omitempty"`
	RemoteCapabilitiesEnabled   []string             `json:"remote_capabilities_enabled,omitempty"`
	AgeSeconds                  *uint32              `json:"age_seconds,omitempty"` // reported age since last discovery packet, not TTL
	Protocol                    string               `json:"protocol"`              // lldp, cdp, mndp, unknown
	Source                      string               `json:"source"`
	ObservedAt                  time.Time            `json:"observed_at"`
	ExpiresAt                   time.Time            `json:"expires_at"`
	Evidence                    []Evidence           `json:"evidence"`
	Confidence                  string               `json:"confidence"` // high, medium, low, unresolved
}

// ManagementAddress retains the advertised IANA AddressFamilyNumbers encoding.
// Address is canonical IP only for IPv4(1)/IPv6(2); other families retain raw
// bytes. An advertised address is evidence, never an automatically created target.
type ManagementAddress struct {
	AddressFamily    uint32  `json:"address_family"`
	Address          string  `json:"address,omitempty"`
	RawAddress       []byte  `json:"raw_address"`
	InterfaceSubtype *uint32 `json:"interface_subtype,omitempty"`
	InterfaceID      *uint32 `json:"interface_id,omitempty"`
}
type InterfaceVerification struct {
	InterfaceID     string    `json:"interface_id"`
	Name            string    `json:"name"`
	ExpectedAdminUp bool      `json:"expected_admin_up"`
	ObservedAdminUp *bool     `json:"observed_admin_up,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
	Verified        bool      `json:"verified"`
	Changed         bool      `json:"changed"`
	Uncertain       bool      `json:"uncertain"`
	Evidence        string    `json:"evidence"`
}
