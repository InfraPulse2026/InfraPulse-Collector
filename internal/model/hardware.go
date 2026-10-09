package model

import "slices"

// HardwareStatus keeps equipment health, aggregated health and operational state
// independent. Unknown is explicit; power-off is not a hardware failure.
type HardwareStatus struct {
	Health       string `json:"health"`        // ok, warning, critical, unknown
	HealthRollup string `json:"health_rollup"` // ok, warning, critical, unknown
	State        string `json:"state"`         // source-reported state, unknown when absent
}

// ResourceRelationship is reported management/component evidence within the
// same Target. It neither describes a physical cable nor resolves Backend assets.
type ResourceRelationship struct {
	Type             string `json:"type"`
	TargetResourceID string `json:"target_resource_id"`
}

// HardwareInventory is protocol-neutral equipment data. Optional numeric values
// distinguish unknown from an observed zero. Firmware on Device describes that
// resource; BIOSVersion is explicitly the ComputerSystem BIOS, never BMC firmware.
type HardwareInventory struct {
	ResourceType     string                 `json:"resource_type"`
	HardwareUUID     string                 `json:"hardware_uuid,omitempty"`
	SKU              string                 `json:"sku,omitempty"`
	PartNumber       string                 `json:"part_number,omitempty"`
	BIOSVersion      string                 `json:"bios_version,omitempty"`
	PowerState       string                 `json:"power_state,omitempty"`
	ChassisType      string                 `json:"chassis_type,omitempty"`
	ManagerType      string                 `json:"manager_type,omitempty"`
	AssetTag         string                 `json:"asset_tag,omitempty"`
	PhysicalSecurity string                 `json:"physical_security,omitempty"`
	DeviceLocation   string                 `json:"device_location,omitempty"`
	MemoryType       string                 `json:"memory_type,omitempty"`
	ErrorCorrection  string                 `json:"error_correction,omitempty"`
	Protocol         string                 `json:"protocol,omitempty"`
	MediaType        string                 `json:"media_type,omitempty"`
	CapacityBytes    *uint64                `json:"capacity_bytes,omitempty"`
	Cores            *uint32                `json:"cores,omitempty"`
	Threads          *uint32                `json:"threads,omitempty"`
	NetworkAddresses []string               `json:"network_addresses,omitempty"`
	Status           HardwareStatus         `json:"status"`
	Relations        []ResourceRelationship `json:"relations,omitempty"`
}

// ComponentHealth supports health-only resources; lack of a sensor reading never
// invents a metric. ResourceID and observation identify the actual source.
type ComponentHealth struct {
	ID          string               `json:"id"`
	ResourceID  string               `json:"resource_id"`
	Name        string               `json:"name,omitempty"`
	Status      HardwareStatus       `json:"status"`
	Observation *ObservationMetadata `json:"observation,omitempty"`
}

func CloneHardware(h *HardwareInventory) *HardwareInventory {
	if h == nil {
		return nil
	}
	v := *h
	v.CapacityBytes = clonePointer(h.CapacityBytes)
	v.Cores, v.Threads = clonePointer(h.Cores), clonePointer(h.Threads)
	v.NetworkAddresses = slices.Clone(h.NetworkAddresses)
	v.Relations = slices.Clone(h.Relations)
	return &v
}
