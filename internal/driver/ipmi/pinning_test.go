package ipmi

import (
	"context"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/model"
)

func TestPinnedIPMIDestinationPreservesTargetIdentity(t *testing.T) {
	fixture := ipmitest.Start(t)
	const hostname = "bmc.fixture.invalid"
	a, err := New(driver.Options{ID: "pinned-bmc", Address: hostname}, fixedCredentials{}, "fixture", Options{DialAddress: fixture.Address, Port: fixture.Port, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	for range 2 {
		inventory, err := a.InventoryBatch(context.Background())
		if err != nil || len(inventory.Devices) == 0 {
			t.Fatalf("pinned LANplus session failed: %v %v", inventory.Devices, err)
		}
		if a.opts.Address != hostname || fixture.ActiveSessions() != 0 {
			t.Fatal("pin replaced configured address or leaked a session", a.opts.Address, fixture.ActiveSessions())
		}
		for _, device := range inventory.Devices {
			if device.Observation == nil || device.Observation.Source.TargetID != "remote:pinned-bmc" {
				t.Fatal("pin changed original SourceRef", device.Observation)
			}
			for _, fact := range device.IdentityEvidence {
				if fact.Type == model.IdentityManagementIP && fact.Value == fixture.Address {
					t.Fatal("internal pin became observed management identity", fact)
				}
			}
		}
	}
}

func TestIPMIPinnedDestinationMustBeNumeric(t *testing.T) {
	for _, address := range []string{"localhost", "127.0.0.1:623", "udp://127.0.0.1", "[::1]", "fe80::1%eth0"} {
		if a, err := New(driver.Options{ID: "bmc", Address: "bmc.fixture.invalid"}, fixedCredentials{}, "fixture", Options{DialAddress: address}); err == nil {
			_ = a.Close()
			t.Fatalf("non-numeric pin accepted: %q", address)
		}
	}
}
