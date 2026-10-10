package redfish

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"testing"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
)

func resourceByURI(t *testing.T, inventory model.InventoryUpdate, uri string) model.Device {
	t.Helper()
	for _, device := range inventory.Devices {
		if device.Observation.Source.ResourceID == uri {
			return device
		}
	}
	t.Fatalf("resource %q missing", uri)
	return model.Device{}
}

func identityByOrigin(t *testing.T, device model.Device, origin, resource string) model.IdentityEvidence {
	t.Helper()
	for _, fact := range device.IdentityEvidence {
		if fact.Origin == origin && fact.Source.ResourceID == resource {
			return fact
		}
	}
	t.Fatalf("identity origin=%q resource=%q missing: %+v", origin, resource, device.IdentityEvidence)
	return model.IdentityEvidence{}
}

func TestIdentityResourceSubjectsUUIDAndEthernetInterface(t *testing.T) {
	f := newFixture(t)
	f.doc["/redfish/v1/Systems/A"].(map[string]any)["UUID"] = "71A31700-031A-4E72-87BA-889AE57E3900"
	f.doc["/redfish/v1/Chassis/C"].(map[string]any)["UUID"] = "b7a05d61-b83e-4092-8099-dc7881a16d6e"
	const nicURI = "/redfish/v1/Systems/A/EthernetInterfaces/NIC"
	nic := f.doc[nicURI].(map[string]any)
	nic["Name"], nic["MACAddress"], nic["PermanentMACAddress"] = "NIC 1", "AA-BB-CC-DD-EE-01", "aa:bb:cc:dd:ee:02"
	nic["IPv6Addresses"] = []any{map[string]any{"Address": "2001:0db8:0:0::42"}}
	a := f.adapter(Options{Provenance: model.ProvenanceSimulated})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil || inventory.Completeness != "complete" || len(inventory.Devices) != 6 {
		t.Fatalf("inventory=%+v err=%v", inventory, err)
	}
	for _, entry := range []struct{ uri, kind, uuid string }{
		{"/redfish/v1/Systems/A", "computer_system", "71a31700-031a-4e72-87ba-889ae57e3900"},
		{"/redfish/v1/Chassis/C", "chassis", "b7a05d61-b83e-4092-8099-dc7881a16d6e"},
		{"/redfish/v1/Managers/M", "manager", "e88f4ad5-3db8-4d63-a145-d29b6ef1f490"},
	} {
		device := resourceByURI(t, inventory, entry.uri)
		fact := identityByOrigin(t, device, "redfish.UUID", entry.uri)
		if fact.Type != model.IdentityHardwareUUID || fact.Value != entry.uuid || device.Hardware.HardwareUUID != entry.uuid || fact.SubjectKind != entry.kind || fact.SubjectID != "" {
			t.Fatalf("different hardware subject identity collapsed: %+v", fact)
		}
		if fact.Time == nil || fact.Time.SourceTimestamp != nil || fact.Time.ClockOffset != nil || fact.Time.Quality != model.ClockUnknown || !fact.Time.ObservedAt.Equal(device.Observation.Time.ObservedAt) || fact.Source.Provenance != model.ProvenanceSimulated {
			t.Fatalf("identity timing/provenance missing or invented: %+v", fact)
		}
		if fact.Validate() != nil {
			t.Fatal("invalid hardware identity", fact)
		}
		if entry.kind == "computer_system" && fact.OriginalValue != "71A31700-031A-4E72-87BA-889AE57E3900" {
			t.Fatal("original UUID form lost", fact)
		}
	}
	system := resourceByURI(t, inventory, "/redfish/v1/Systems/A")
	var iface model.InventoryComponent
	for _, component := range system.Components {
		if component.Class == "EthernetInterface" {
			iface = component
		}
	}
	if iface.ID != stableID("bmc", "EthernetInterface", nicURI) || iface.ParentID != system.ID || iface.Observation.Source.ResourceID != nicURI || len(iface.Hardware.NetworkAddresses) != 2 {
		t.Fatalf("interface resource/source/address missing: %+v", iface)
	}
	for _, entry := range []struct{ origin, value string }{{"redfish.MACAddress", "aa:bb:cc:dd:ee:01"}, {"redfish.PermanentMACAddress", "aa:bb:cc:dd:ee:02"}, {"redfish.IPv6Addresses.Address", "2001:db8::42"}} {
		fact := identityByOrigin(t, system, entry.origin, nicURI)
		if fact.Value != entry.value || fact.SubjectID != iface.ID || fact.SubjectKind != "network_interface" || fact.Source.Provenance != model.ProvenanceSimulated || fact.Time == nil {
			t.Fatal("interface fact attributed to system/target", fact)
		}
	}
	second, err := f.adapter(Options{Provenance: model.ProvenanceSimulated}).InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, device := range inventory.Devices {
		if second.Devices[i].ID != device.ID {
			t.Fatal("resource identity changed after adapter restart")
		}
		for j, component := range device.Components {
			if second.Devices[i].Components[j].ID != component.ID {
				t.Fatal("component identity changed after adapter restart")
			}
		}
	}
	reads := f.count("GET", nicURI)
	samples, err := a.Hardware(context.Background())
	if err != nil || f.count("GET", nicURI) != reads {
		t.Fatalf("identity enrichment added a hardware interface poll: reads=%d=>%d err=%v", reads, f.count("GET", nicURI), err)
	}
	encodeAdapterObservations(t, inventory, samples)
}

func TestMalformedHardwareIdentityPreservesResourcesAndDiagnostics(t *testing.T) {
	f := newFixture(t)
	f.doc["/redfish/v1/Managers/M"].(map[string]any)["UUID"] = "not-a-uuid"
	f.doc["/redfish/v1/Systems/A"].(map[string]any)["SerialNumber"] = "secret\ncorrupted"
	f.doc["/redfish/v1/Chassis/C"].(map[string]any)["Model"] = strings.Repeat("x", 5000)
	f.doc["/redfish/v1/Systems/A/EthernetInterfaces/NIC"].(map[string]any)["MACAddress"] = "invalid-mac"
	inventory, err := f.adapter(Options{}).InventoryBatch(context.Background())
	if err != nil || inventory.Completeness != "partial" || len(inventory.Devices) != 6 {
		t.Fatalf("bad optional facts erased good resource observations: %s devices=%d err=%v", inventory.Completeness, len(inventory.Devices), err)
	}
	manager := resourceByURI(t, inventory, "/redfish/v1/Managers/M")
	fact := identityByOrigin(t, manager, "redfish.UUID", "/redfish/v1/Managers/M")
	if manager.Hardware.HardwareUUID != "" || fact.Normalization != "invalid" || fact.Value != "not-a-uuid" || manager.FirmwareVersion != "BMC 3" {
		t.Fatal("malformed UUID became real identity or destroyed firmware", fact, manager.Hardware)
	}
	system := resourceByURI(t, inventory, "/redfish/v1/Systems/A")
	fact = identityByOrigin(t, system, "redfish.SerialNumber", "/redfish/v1/Systems/A")
	if system.SerialNumber != "" || fact.Normalization != "rejected" || strings.Contains(fact.Value+fact.OriginalValue, "\n") {
		t.Fatal("unsafe serial preserved as an actionable identifier", fact)
	}
	fact = identityByOrigin(t, resourceByURI(t, inventory, "/redfish/v1/Chassis/C"), "redfish.Model", "/redfish/v1/Chassis/C")
	if fact.Normalization != "rejected" || len(fact.Value)+len(fact.OriginalValue) > 4096 {
		t.Fatal("oversized source identity is not a bounded diagnostic", fact)
	}
	encodeAdapterObservations(t, inventory, nil)
}

func TestInterfaceIdentityBoundsAndEqualMACOrigins(t *testing.T) {
	f := newFixture(t)
	const nicURI = "/redfish/v1/Systems/A/EthernetInterfaces/NIC"
	nic := f.doc[nicURI].(map[string]any)
	nic["MACAddress"], nic["PermanentMACAddress"] = "aa:bb:cc:dd:ee:ff", "aa:bb:cc:dd:ee:ff"
	var addresses []any
	for i := 1; i <= 70; i++ {
		addresses = append(addresses, map[string]any{"Address": fmt.Sprintf("2001:db8::%x", i)})
	}
	delete(nic, "IPv4Addresses")
	nic["IPv6Addresses"] = addresses
	inventory, err := f.adapter(Options{}).InventoryBatch(context.Background())
	if err != driver.ErrLimit || inventory.Completeness != "partial" {
		t.Fatalf("oversized interface addresses not bounded: completeness=%s err=%v", inventory.Completeness, err)
	}
	system := resourceByURI(t, inventory, "/redfish/v1/Systems/A")
	current := identityByOrigin(t, system, "redfish.MACAddress", nicURI)
	permanent := identityByOrigin(t, system, "redfish.PermanentMACAddress", nicURI)
	if current.Value != permanent.Value || current.SubjectID != permanent.SubjectID {
		t.Fatal("equal MAC facts changed subject")
	}
	if len(system.Hardware.NetworkAddresses) != model.MaxIdentityPerSubject-2 || len(system.IdentityEvidence) > model.MaxIdentityEvidence {
		t.Fatal("interface evidence bounds exceeded")
	}
	encodeAdapterObservations(t, inventory, nil)
}

func TestIdenticalMACAcrossInterfacesAndTargetsDoesNotMergeResources(t *testing.T) {
	f := newFixture(t)
	const nicURI = "/redfish/v1/Systems/A/EthernetInterfaces/NIC"
	const secondURI = "/redfish/v1/Systems/A/EthernetInterfaces/Second"
	f.doc[nicURI].(map[string]any)["MACAddress"] = "aa:bb:cc:dd:ee:ff"
	f.doc[secondURI] = map[string]any{"MACAddress": "aa:bb:cc:dd:ee:ff"}
	f.doc["/redfish/v1/Systems/A/EthernetInterfaces"].(map[string]any)["Members"] = []any{map[string]any{"@odata.id": nicURI}, map[string]any{"@odata.id": secondURI}}
	pool := x509.NewCertPool()
	pool.AddCert(f.server.Certificate())
	first := f.adapter(Options{})
	second, err := New(driver.Options{ID: "another-target", Address: f.server.URL}, testProvider{}, "fixture", Options{TLSConfig: &tls.Config{RootCAs: pool}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	seen := map[string]bool{}
	for _, adapter := range []*Adapter{first, second} {
		inventory, err := adapter.InventoryBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		system := resourceByURI(t, inventory, "/redfish/v1/Systems/A")
		for _, uri := range []string{nicURI, secondURI} {
			fact := identityByOrigin(t, system, "redfish.MACAddress", uri)
			if fact.Value != "aa:bb:cc:dd:ee:ff" || seen[fact.SubjectID] || fact.Source.TargetID != "remote:"+adapter.opts.ID {
				t.Fatal("equal MAC merged distinct target/interface subjects", fact)
			}
			seen[fact.SubjectID] = true
		}
	}
	if len(seen) != 4 {
		t.Fatal("source observations disappeared")
	}
}
