package redfish

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	grpcwire "InfraPulseAgent/internal/transport/grpc"
)

func encodeAdapterObservations(t *testing.T, inventory model.InventoryUpdate, samples []model.TelemetrySample) {
	t.Helper()
	envelopes := []model.Envelope{{Inventory: &inventory}}
	for i := range samples {
		envelopes = append(envelopes, model.Envelope{Telemetry: &samples[i]})
	}
	for _, envelope := range envelopes {
		envelope.MessageID, envelope.AgentID, envelope.SessionID = "fixture-message", "fixture-collector", "fixture-session"
		envelope.SentAt = time.Now().UTC()
		if _, err := grpcwire.Encode(envelope); err != nil {
			t.Fatalf("adapter emitted an unencodable durable observation: %v", err)
		}
	}
}

func TestOversizedOptionalFactsRemainEncodableAndKeepOtherResources(t *testing.T) {
	f := newFixture(t)
	system := f.doc["/redfish/v1/Systems/A"].(map[string]any)
	system["SKU"] = strings.Repeat("x", 1025)
	system["Status"] = map[string]any{"State": strings.Repeat("x", 129), "Health": "Warning"}
	f.doc["/redfish/v1/Chassis/C"].(map[string]any)["AssetTag"] = strings.Repeat("x", 1025)
	a := f.adapter(Options{})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil || inventory.Completeness != "partial" || len(inventory.Devices) != 6 {
		t.Fatalf("optional oversized facts discarded valid inventory: %s resources=%d err=%v", inventory.Completeness, len(inventory.Devices), err)
	}
	for _, device := range inventory.Devices {
		if device.Name == "system A" && (device.Hardware.SKU != "" || device.Hardware.Status.State != "unknown" || device.Hardware.Status.Health != "warning") {
			t.Fatal("oversized SKU/State was not suppressed independently of valid health", device)
		}
		if device.Name == "chassis C" && device.Hardware.AssetTag != "" {
			t.Fatal("oversized optional AssetTag retained")
		}
	}
	samples, err := a.Hardware(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encodeAdapterObservations(t, inventory, samples)
}

func TestRelationshipBoundsPreserveAnEncodablePrefix(t *testing.T) {
	f := newFixture(t)
	refs := make([]any, 1025)
	for i := range refs {
		refs[i] = map[string]any{"@odata.id": "/redfish/v1/Chassis/C"}
	}
	f.doc["/redfish/v1/Systems/A"].(map[string]any)["Links"] = map[string]any{"Chassis": refs}
	a := f.adapter(Options{})
	inventory, err := a.InventoryBatch(context.Background())
	if !errors.Is(err, driver.ErrLimit) || inventory.Completeness != "partial" || len(inventory.Devices) != 6 {
		t.Fatalf("relationship bound lost valid inventory: resources=%d err=%v", len(inventory.Devices), err)
	}
	for _, device := range inventory.Devices {
		if len(device.Hardware.Relations) > 1024 {
			t.Fatal("relationship limit exceeded")
		}
	}
	encodeAdapterObservations(t, inventory, nil)
}

func TestUnavailableSourceTimeRemainsEncodable(t *testing.T) {
	for _, readingTime := range []string{"0001-01-01T00:00:00Z", "0001-01-01T00:00:00+01:00", "9999-12-31T23:59:59-01:00"} {
		t.Run(readingTime, func(t *testing.T) {
			f := newFixture(t)
			f.doc["/redfish/v1/Chassis/C/Sensors/Temp"].(map[string]any)["ReadingTime"] = readingTime
			a := f.adapter(Options{})
			inventory, err := a.InventoryBatch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			samples, err := a.Hardware(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, sample := range samples {
				for _, metric := range sample.Metrics {
					if metric.ResourceID == "/redfish/v1/Chassis/C/Sensors/Temp" && metric.Observation.Time.SourceTimestamp != nil {
						t.Fatal("zero/out-of-range source timestamp retained")
					}
				}
			}
			encodeAdapterObservations(t, inventory, samples)
		})
	}
}

func TestGeneratedFragmentBoundsKeepValidHealthAndInventory(t *testing.T) {
	for _, kind := range []string{"modern", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			prefix := "/redfish/v1/Chassis/C/"
			uri := prefix + strings.Repeat("x", 2040-len(prefix))
			chassis := f.doc["/redfish/v1/Chassis/C"].(map[string]any)
			if kind == "modern" {
				chassis["EnvironmentMetrics"] = map[string]any{"@odata.id": uri}
				f.doc[uri] = map[string]any{"AmbientTemperatureCelsius": map[string]any{"Reading": 22}}
			} else {
				chassis["Thermal"] = map[string]any{"@odata.id": uri}
				f.doc[uri] = map[string]any{"Temperatures": []any{map[string]any{"MemberId": "long-source", "ReadingCelsius": 22}}}
			}
			a := f.adapter(Options{})
			inventory, err := a.InventoryBatch(context.Background())
			if err != nil || inventory.Completeness != "partial" || len(inventory.Devices) != 6 {
				t.Fatalf("long generated fragment lost valid resources: %s count=%d err=%v", inventory.Completeness, len(inventory.Devices), err)
			}
			samples, err := a.Hardware(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, sample := range samples {
				for _, metric := range sample.Metrics {
					if metric.Value == 22 || len(metric.ResourceID) > 2048 {
						t.Fatal("oversized generated metric identity was retained", metric)
					}
				}
			}
			encodeAdapterObservations(t, inventory, samples)
		})
	}
}

func TestMalformedAdvertisedLinksDoNotEraseDiscoveryPlan(t *testing.T) {
	for _, kind := range []string{"root-empty-link", "root-wrong-link-type", "optional-link", "missing-known-collection", "no-collections"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			a := f.adapter(Options{})
			if _, err := a.InventoryBatch(context.Background()); err != nil {
				t.Fatal(err)
			}
			previous := len(a.plan)
			root := f.doc["/redfish/v1/"].(map[string]any)
			switch kind {
			case "root-empty-link":
				root["Systems"] = map[string]any{}
			case "root-wrong-link-type":
				root["Systems"] = map[string]any{"@odata.id": 42}
			case "optional-link":
				f.doc["/redfish/v1/Systems/A"].(map[string]any)["Storage"] = map[string]any{}
			case "missing-known-collection":
				delete(root, "Systems")
			case "no-collections":
				delete(root, "Systems")
				delete(root, "Chassis")
				delete(root, "Managers")
			}
			inventory, err := a.InventoryBatch(context.Background())
			if inventory.Completeness != "partial" || len(a.plan) != previous {
				t.Fatalf("incomplete links invalidated existing discovery: completeness=%s plan=%d old=%d err=%v", inventory.Completeness, len(a.plan), previous, err)
			}
			if strings.HasPrefix(kind, "root-") && !errors.Is(err, driver.ErrMalformed) {
				t.Fatal("malformed critical root link claimed success", err)
			}
			if kind == "no-collections" && !errors.Is(err, driver.ErrUnavailable) {
				t.Fatal("unsupported empty service claimed complete inventory", err)
			}
			if (kind == "optional-link" || kind == "missing-known-collection") && err != nil {
				t.Fatal("optional malformed/missing link failed target", err)
			}
			encodeAdapterObservations(t, inventory, nil)
		})
	}
}
