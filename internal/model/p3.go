package model

import "time"

// RollbackAction references Agent-owned state; it never carries device payloads.
type RollbackAction struct {
	SnapshotID string `json:"snapshot_id"`
}

// ConfigurationSnapshot contains only the supported interface admin state and
// identity evidence. It is not a device backup or a credential container.
type ConfigurationSnapshot struct {
	ID               string    `json:"id"`
	DeviceID         string    `json:"device_id"`
	ResourceType     string    `json:"resource_type"`
	CanonicalID      string    `json:"canonical_id"`
	Name             string    `json:"name"`
	MAC              string    `json:"mac,omitempty"`
	Alias            string    `json:"alias,omitempty"`
	Description      string    `json:"description,omitempty"`
	Aliases          []string  `json:"aliases"`
	PreviousAdminUp  bool      `json:"previous_admin_up"`
	ExpectedAdminUp  bool      `json:"expected_admin_up"`
	ObservedAt       time.Time `json:"observed_at"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	Source           string    `json:"source"`
	RelatedCommandID string    `json:"related_command_id"`
	Driver           string    `json:"driver"`
	SoftwareVersion  string    `json:"software_version,omitempty"`
	SchemaVersion    uint32    `json:"schema_version"`
	Verified         bool      `json:"verified"`
	Consumed         bool      `json:"consumed"`
	Superseded       bool      `json:"superseded"`
	Generation       uint64    `json:"generation"`
}
