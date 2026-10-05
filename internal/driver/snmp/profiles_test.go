package snmp

import (
	"context"
	"errors"
	"testing"

	"InfraPulseAgent/internal/driver"
	protocol "InfraPulseAgent/internal/protocol/snmp"
	"InfraPulseAgent/internal/vendorprofiles"
)

func TestProfilesKeepGenericFallbackAndObservedCapabilities(t *testing.T) {
	for _, profile := range vendorprofiles.All() {
		t.Run(profile.ID, func(t *testing.T) {
			f := fixture()
			f.get[systemRoot+".1.0"] = val(systemRoot+".1.0", protocol.OctetString, "unclassified product")
			f.get[systemRoot+".2.0"] = val(systemRoot+".2.0", protocol.ObjectIdentifier, profile.EnterprisePrefix)
			f.errors[entityRoot] = protocol.ErrUnavailable
			d := newFixtureDriver(f)
			dev, err := d.Inventory(context.Background())
			if err != nil || dev.Vendor != profile.Vendor || dev.Driver != "generic-snmp" || dev.Model != "" || dev.SerialNumber != "" || dev.FirmwareVersion != "" {
				t.Fatalf("%+v %v", dev, err)
			}
			for _, cap := range dev.Capabilities {
				if cap.Supported && cap.Name != "system.read" && cap.Name != "inventory.read" {
					t.Fatalf("profile promoted unobserved capability %+v", cap)
				}
			}
			interfaceFixture(f)
			d = newFixtureDriver(f)
			sample, err := d.Collect(context.Background())
			if err != nil || len(sample.Interfaces) != 1 {
				t.Fatalf("Generic IF-MIB fallback %+v %v", sample, err)
			}
			delete(f.get, systemRoot+".2.0")
			dev, err = d.Probe(context.Background())
			if err != nil || dev.Vendor != "" {
				t.Fatalf("missing OID %+v %v", dev, err)
			}
			f.get[systemRoot+".2.0"] = val(systemRoot+".2.0", protocol.OctetString, profile.EnterprisePrefix)
			dev, err = d.Probe(context.Background())
			if !errors.Is(err, driver.ErrMalformed) || dev.Vendor != "" {
				t.Fatalf("malformed OID %+v %v", dev, err)
			}
		})
	}
}
