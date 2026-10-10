package ipmi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/redfish"
	"InfraPulseAgent/internal/model"
	grpcwire "InfraPulseAgent/internal/transport/grpc"
)

func TestIPMIManagementIdentityPreservesMappedIPv6(t *testing.T) {
	a := fixtureAdapter(t, protocolFixture(), Options{Provenance: model.ProvenanceSimulated})
	a.opts.Address = "::ffff:192.0.2.31"
	p := poll{a: a}
	d := p.bmc(DeviceInfo{})
	for _, fact := range d.IdentityEvidence {
		if fact.Type == model.IdentityManagementIP {
			if fact.Value != a.opts.Address || fact.SubjectKind != "manager" {
				t.Fatal("mapped IPv6 collapsed into IPv4 or another subject", fact)
			}
			return
		}
	}
	t.Fatal("configured literal management address lost")
}

func TestIPMIIdentitySubjectsOriginsAndTime(t *testing.T) {
	a := fixtureAdapter(t, protocolFixture(), Options{Provenance: model.ProvenanceSimulated})
	inventory, err := a.InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seenSerial := map[string]string{}
	var bmc model.Device
	for _, device := range inventory.Devices {
		if device.Hardware.HardwareUUID != "" || device.SerialNumber != "" {
			t.Fatal("BMC command identity became a server serial/UUID", device)
		}
		if device.Hardware.ResourceType == "Manager" {
			bmc = device
		}
		for _, fact := range device.IdentityEvidence {
			if fact.Validate() != nil || fact.Time == nil || fact.Time.SourceTimestamp != nil || fact.Source.Provenance != model.ProvenanceSimulated || fact.Source.Type != model.SourceIPMI || fact.Source.TargetID != "remote:legacy-bmc" {
				t.Fatal("IPMI identity lost real protocol/time/provenance", fact)
			}
			if fact.Type == model.IdentitySerial {
				if fact.SubjectID == "" || fact.SubjectID == device.ID || !strings.HasPrefix(fact.Source.ResourceID, "/ipmi/fru/") || !strings.HasPrefix(fact.Origin, "ipmi.fru.") {
					t.Fatal("FRU serial assigned to BMC resource", fact)
				}
				seenSerial[fact.Value] = fact.SubjectKind
			} else if fact.Source.ResourceID == "/ipmi/bmc" && (fact.SubjectKind != "manager" || fact.SubjectID != device.ID) {
				t.Fatal("BMC protocol identity lost manager subject", fact)
			}
		}
	}
	if seenSerial["CHASSIS-SERIAL"] != "chassis" || seenSerial["BOARD-SERIAL"] != "component" || seenSerial["SYSTEM-SERIAL"] != "component" || seenSerial["PSU-SERIAL"] != "component" {
		t.Fatal("FRU area subjects were merged/inferred as ComputerSystem", seenSerial)
	}
	if len(bmc.VendorEvidence) != 6 || bmc.VendorEvidence[0].Source != "ipmi.get-device-id.device_id" || bmc.VendorEvidence[0].Value != "32" || bmc.VendorEvidence[2].Source != "ipmi.get-device-id.product_id" {
		t.Fatal("BMC numeric protocol facts lost", bmc.VendorEvidence)
	}
	firstComponentIDs := map[string]string{}
	for _, component := range bmc.Components {
		firstComponentIDs[component.Observation.Source.ResourceID] = component.ID
	}
	second, err := fixtureAdapter(t, protocolFixture(), Options{Provenance: model.ProvenanceSimulated}).InventoryBatch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range second.Devices {
		for _, component := range device.Components {
			if firstComponentIDs[component.Observation.Source.ResourceID] != component.ID {
				t.Fatal("FRU/SDR resource identity changed after restart")
			}
		}
	}
}

func replaceProductSerial(t *testing.T, fixture *fakeClient, serial string) {
	t.Helper()
	if len(serial) != len("SYSTEM-SERIAL") {
		t.Fatal("fixture replacement must preserve FRU offsets")
	}
	data := bytes.ReplaceAll(fixture.frus[0], []byte("SYSTEM-SERIAL"), []byte(serial))
	start := int(data[4]) * 8
	end := start + int(data[start+1])*8
	var sum byte
	for _, value := range data[start : end-1] {
		sum += value
	}
	data[end-1] = -sum
	fixture.frus[0] = data
}

func TestIPMIMissingAndLeadingZeroSerial(t *testing.T) {
	for _, serial := range []string{"0000012345678", "             "} {
		t.Run(strings.TrimSpace(serial), func(t *testing.T) {
			fixture := protocolFixture()
			replaceProductSerial(t, fixture, serial)
			inventory, err := fixtureAdapter(t, fixture, Options{}).InventoryBatch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, device := range inventory.Devices {
				for _, fact := range device.IdentityEvidence {
					if fact.Source.ResourceID == "/ipmi/fru/0/product" && fact.Type == model.IdentitySerial {
						seen = true
						if fact.Value != serial {
							t.Fatal("significant serial zeroes changed", fact)
						}
					}
				}
			}
			if seen != (strings.TrimSpace(serial) != "") {
				t.Fatal("missing FRU serial fabricated")
			}
		})
	}
}

func crossProtocolRedfish(t *testing.T, systemSerial string) (*redfish.Adapter, string) {
	t.Helper()
	ref := func(id string) any { return map[string]any{"@odata.id": id} }
	collection := func(ids ...string) any {
		members := make([]any, 0, len(ids))
		for _, id := range ids {
			members = append(members, ref(id))
		}
		return map[string]any{"Members": members}
	}
	documents := map[string]any{
		"/redfish/v1/":           map[string]any{"Systems": ref("/redfish/v1/Systems"), "Chassis": ref("/redfish/v1/Chassis"), "Managers": ref("/redfish/v1/Managers")},
		"/redfish/v1/Systems":    collection("/redfish/v1/Systems/A", "/redfish/v1/Systems/B"),
		"/redfish/v1/Chassis":    collection("/redfish/v1/Chassis/A", "/redfish/v1/Chassis/B"),
		"/redfish/v1/Managers":   collection("/redfish/v1/Managers/A", "/redfish/v1/Managers/B"),
		"/redfish/v1/Systems/A":  map[string]any{"SerialNumber": systemSerial, "UUID": "71a31700-031a-4e72-87ba-889ae57e3900"},
		"/redfish/v1/Systems/B":  map[string]any{"SerialNumber": "another-system"},
		"/redfish/v1/Chassis/A":  map[string]any{"SerialNumber": "CHASSIS-SERIAL"},
		"/redfish/v1/Chassis/B":  map[string]any{"SerialNumber": "another-chassis"},
		"/redfish/v1/Managers/A": map[string]any{"ManagerType": "BMC", "UUID": "e88f4ad5-3db8-4d63-a145-d29b6ef1f490"},
		"/redfish/v1/Managers/B": map[string]any{"ManagerType": "BMC"},
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := documents[r.URL.Path]
		if value == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	a, err := redfish.New(driver.Options{ID: "redfish-bmc", Address: server.URL}, fixedCredentials{}, "fixture", redfish.Options{Auth: "basic", TLSConfig: &tls.Config{RootCAs: pool}, Provenance: model.ProvenanceSimulated})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	u, err := url.Parse(server.URL)
	if err != nil || net.ParseIP(u.Hostname()) == nil {
		t.Fatal("expected loopback IP fixture")
	}
	return a, u.Hostname()
}

func TestCrossProtocolOverlappingAndConflictingSerialsKeepSourceObservations(t *testing.T) {
	for _, serial := range []string{"SYSTEM-SERIAL", "CONFLICTING-SYSTEM-SERIAL"} {
		t.Run(serial, func(t *testing.T) {
			rf, address := crossProtocolRedfish(t, serial)
			ipmi := fixtureAdapter(t, protocolFixture(), Options{Provenance: model.ProvenanceSimulated})
			if address != ipmi.opts.Address {
				t.Fatal("cross-protocol fixture must share management IP")
			}
			rfInventory, err := rf.InventoryBatch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ipmiInventory, err := ipmi.InventoryBatch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(rfInventory.Devices) != 6 || len(ipmiInventory.Devices) != 2 {
				t.Fatal("same IP/serial collapsed multiple protocol resources")
			}
			seen := map[string]bool{}
			serialSources := map[model.SourceType][]string{}
			for i, inventory := range []model.InventoryUpdate{rfInventory, ipmiInventory} {
				ref := model.SourceRef{CollectorID: "fixture-collector"}
				envelope := model.ObserveEnvelope(model.Envelope{Inventory: &inventory}, ref, time.Now().UTC())
				envelope.AgentID, envelope.SessionID, envelope.MessageID, envelope.SentAt = "fixture-collector", "fixture-session", "fixture-message", time.Now().UTC()
				wire, err := grpcwire.Encode(envelope)
				if err != nil {
					t.Fatal(err)
				}
				restored, err := grpcwire.DecodeEnvelope(wire)
				if err != nil {
					t.Fatal(err)
				}
				for _, device := range restored.Inventory.Devices {
					if seen[device.ID] || device.Observation.Source.CollectorID != "fixture-collector" || device.Observation.Source.Provenance != model.ProvenanceSimulated {
						t.Fatal("protocol resource identity/provenance merged", device)
					}
					seen[device.ID] = true
					expected := model.SourceRedfish
					if i == 1 {
						expected = model.SourceIPMI
					}
					if device.Observation.Source.Type != expected {
						t.Fatal("source protocol rewritten")
					}
					if expected == model.SourceRedfish && device.ManagementAddress != "" {
						t.Fatal("connection endpoint was assigned to an unverified Redfish resource")
					}
					for _, fact := range device.IdentityEvidence {
						if fact.Type == model.IdentitySerial && (fact.Value == serial || fact.Value == "SYSTEM-SERIAL") {
							if fact.Source.Type != expected || fact.Time == nil || fact.SubjectKind == "manager" {
								t.Fatal("server serial transferred to BMC subject", fact)
							}
							serialSources[fact.Source.Type] = append(serialSources[fact.Source.Type], fact.Value)
						}
					}
				}
			}
			if len(serialSources[model.SourceRedfish]) != 1 || len(serialSources[model.SourceIPMI]) != 1 || len(seen) != 8 {
				t.Fatal("cross-source serial observations lost", serialSources, seen)
			}
		})
	}
}
