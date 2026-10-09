package sanitize

import (
	"encoding/json"
	"strings"
	"testing"

	"InfraPulseAgent/internal/model"
)

func TestHardwareSecretRedactionAndOwnership(t *testing.T) {
	password := "bmc-private-password"
	d := model.Device{ID: "resource", Name: "BMC password=" + password, SerialNumber: password, Hardware: &model.HardwareInventory{ResourceType: "manager", HardwareUUID: password, BIOSVersion: password, NetworkAddresses: []string{password}}, IdentityEvidence: []model.IdentityEvidence{{Type: model.IdentitySerial, Value: password}}}
	e := model.Envelope{Inventory: &model.InventoryUpdate{Devices: []model.Device{d}}}
	clean := (KnownSecrets{Values: []string{password}}).Sanitize(e)
	data, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), password) || clean.Inventory.Devices[0].SerialNumber != "" || clean.Inventory.Devices[0].Hardware.HardwareUUID != "" || len(clean.Inventory.Devices[0].IdentityEvidence) > 0 {
		t.Fatal("hardware secret survived")
	}
	if e.Inventory.Devices[0].SerialNumber != password || d.Hardware.NetworkAddresses[0] != password {
		t.Fatal("sanitizer mutated adapter values")
	}
	st := model.HardwareStatus{Health: "unknown", HealthRollup: "unknown", State: "Authorization=Basic dXNlcjpwYXNzd29yZA=="}
	telemetry := model.Envelope{Telemetry: &model.TelemetrySample{HardwareHealth: []model.ComponentHealth{{Name: password, Status: st}}, Metrics: []model.Metric{{Status: &st}}}}
	clean = (KnownSecrets{Values: []string{password}}).Sanitize(telemetry)
	if clean.Telemetry.HardwareHealth[0].Status.State != "unknown" || clean.Telemetry.Metrics[0].Status.State != "unknown" || st.State == "unknown" {
		t.Fatal("status redaction or ownership failed")
	}
}
