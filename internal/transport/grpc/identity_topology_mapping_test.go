package grpc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

func evidenceObservation(kind model.SourceType, protocol string) *model.ObservationMetadata {
	ref := model.SourceRef{Type: kind, CollectorID: "collector-A", TargetID: "configured:target-A", Protocol: protocol, Provenance: model.ProvenanceSimulated}
	sourceAt := testTime.Add(-time.Second)
	o := model.NewObservation(ref, "simulated", testTime, testTime.Add(time.Second), &sourceAt)
	zero, uncertainty := time.Duration(0), 10*time.Millisecond
	o.Time.ClockOffset, o.Time.Uncertainty, o.Time.Quality = &zero, &uncertainty, model.ClockSynced
	return o
}

func TestGenericInventoryAndTelemetryIdentityMetadataWire(t *testing.T) {
	for _, kind := range []model.SourceType{model.SourceSNMP, model.SourceRouterOS, model.SourceLocal} {
		t.Run(string(kind), func(t *testing.T) {
			o := evidenceObservation(kind, "configured-protocol")
			d := model.Device{ID: "device-A", Type: "switch", Driver: string(kind), Source: "simulated", Hostname: "same-hostname", SerialNumber: "00042", Observation: o,
				Components: []model.InventoryComponent{{ID: "entity:1", Class: "chassis", Observation: o}}}
			d.IdentityEvidence = []model.IdentityEvidence{{Type: model.IdentitySerial, Value: "00042", Source: o.Source, Confidence: "high", SubjectID: d.ID, SubjectKind: "network_device", OriginalValue: " 00042 ", Origin: "protocol.serial", Time: &o.Time, Normalization: "canonical"}}
			e := testEnvelope()
			e.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Observation: o, Devices: []model.Device{d}}
			back := roundTripRemote(t, e)
			if back.Inventory.Completeness != "" || back.Inventory.Observation.Time.ClockOffset == nil || *back.Inventory.Observation.Time.ClockOffset != 0 {
				t.Fatal("unknown completeness or measured-zero clock presence lost")
			}
			e.Inventory = nil
			e.Telemetry = &model.TelemetrySample{DeviceID: d.ID, Source: "simulated", ObservedAt: testTime, CollectedAt: testTime.Add(time.Second), Observation: o, IdentityEvidence: d.IdentityEvidence}
			roundTripRemote(t, e)
		})
	}
}

func TestTopologyEnrichmentWireRoundTrip(t *testing.T) {
	for _, protocol := range []string{"lldp", "cdp", "mndp", "unknown"} {
		t.Run(protocol, func(t *testing.T) {
			o := evidenceObservation(model.SourceLLDP, "snmp")
			chassis := []byte{0, 1, 2, 3, 255, 254}
			subtype, ifIndex, age, interfaceSubtype, interfaceID := uint32(4), uint32(42), uint32(0), uint32(2), uint32(7)
			u := remoteTopology()
			u.Observation = o
			n := &u.Observations[0]
			n.Protocol, n.Source, n.Observation = protocol, "simulated", o
			n.ObservationID, n.Kind = strings.Repeat("a", 64), "observed_l2_neighbor"
			n.LocalInterfaceIndex, n.LocalInterfaceMAC = &ifIndex, "00:01:02:03:04:05"
			n.LocalInterfaceDescription, n.LocalInterfaceAlias = "ifDescr", "ifAlias"
			n.RemoteManagementAddresses = []model.ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}, InterfaceSubtype: &interfaceSubtype, InterfaceID: &interfaceID}, {AddressFamily: 999, RawAddress: []byte{1, 2, 3}}}
			n.RemoteSystemDescription, n.RemotePlatform, n.RemoteModel, n.RemoteSoftwareVersion = "sysDescr", "platform", "model", "1.0"
			n.RemoteCapabilitiesSupported, n.RemoteCapabilitiesEnabled, n.AgeSeconds = []string{"bridge", "router"}, []string{"bridge"}, &age
			n.IdentityEvidence = []model.IdentityEvidence{{Type: model.IdentityChassisID, Value: base64.StdEncoding.EncodeToString(chassis), RawValue: chassis, Subtype: &subtype, Source: o.Source, Confidence: "low", SubjectID: "neighbor:stable-1", SubjectKind: "remote_neighbor", Origin: protocol + ".advertised_chassis_id", Time: &o.Time}}
			e := testEnvelope()
			e.Topology = &u
			roundTripRemote(t, e)
			// An unresolved remote port remains absent rather than fabricated.
			n.RemotePortID, n.RemotePortSubtype = nil, 0
			roundTripRemote(t, e)
		})
	}
}

func TestLifecycleEventAndOpenWrtSIPObservationWire(t *testing.T) {
	o := evidenceObservation(model.SourceLocal, "native")
	d := testDevice()
	d.Observation = o
	d.Services = []model.Service{{Name: "service", State: "unknown", Source: "simulated", ObservedAt: testTime, Observation: o}}
	d.Networks = []model.NetworkState{{Name: "lan", Source: "simulated", ObservedAt: testTime, Observation: o}}
	d.Firewall = &model.FirewallSnapshot{Source: "simulated", Completeness: "partial", ObservedAt: testTime, Observation: o}
	e := testEnvelope()
	e.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Observation: o, Devices: []model.Device{d}}
	roundTripRemote(t, e)
	e.Inventory = nil
	e.Events = []model.Event{{ID: "event", Type: "device.online", Timestamp: testTime, Source: "simulated", Severity: "info", Observation: o}}
	roundTripRemote(t, e)
	e.Events = nil
	e.Telephony = &model.TelephonyUpdate{Source: "simulated", ObservedAt: testTime, Observation: o, Targets: []model.TelephonyHealth{{TargetID: "sip-A", Address: "192.0.2.1:5060", Transport: "udp", State: "unknown", Source: "simulated", ObservedAt: testTime, Observation: o}}}
	roundTripRemote(t, e)
}

func TestIdentityDiagnosticsAndOptionalFieldsWire(t *testing.T) {
	o := evidenceObservation(model.SourceRedfish, "redfish")
	e := testEnvelope()
	e.Telemetry = &model.TelemetrySample{DeviceID: "system", Source: "simulated", ObservedAt: testTime, CollectedAt: testTime, Observation: o, IdentityEvidence: []model.IdentityEvidence{
		{Type: model.IdentityHardwareUUID, Value: "malformed-uuid", Source: o.Source, Confidence: "unresolved", SubjectID: "system", SubjectKind: "computer_system", Normalization: "invalid"},
		{Type: model.IdentitySerial, Value: "[rejected identifier]", Source: o.Source, Confidence: "unresolved", SubjectID: "system", SubjectKind: "computer_system", Normalization: "rejected"},
	}}
	roundTripRemote(t, e)
	encoded, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	encoded.GetTelemetry().Samples[0].Observation = nil
	encoded.GetTelemetry().Samples[0].IdentityEvidence = nil
	encoded.GetTelemetry().Samples[0].ExtendedIdentityEvidence = nil
	back, err := DecodeEnvelope(encoded)
	if err != nil || back.Telemetry.Observation != nil || back.Telemetry.IdentityEvidence != nil {
		t.Fatal("legacy message fabricated metadata", err)
	}
}

func TestIdentityEnrichmentRejectsMalformedWire(t *testing.T) {
	o := evidenceObservation(model.SourceSNMP, "snmp")
	e := testEnvelope()
	e.Telemetry = &model.TelemetrySample{DeviceID: "switch", Source: "simulated", ObservedAt: testTime, CollectedAt: testTime, Observation: o, IdentityEvidence: []model.IdentityEvidence{{Type: model.IdentitySerial, Value: "00042", Source: o.Source, Confidence: "high", SubjectID: "switch", SubjectKind: "network_device", Time: &o.Time}}}
	for name, mutate := range map[string]func(*pb.TelemetrySample){
		"nil fact":              func(s *pb.TelemetrySample) { s.IdentityEvidence[0] = nil },
		"nil source":            func(s *pb.TelemetrySample) { s.IdentityEvidence[0].Source = nil },
		"source type":           func(s *pb.TelemetrySample) { s.IdentityEvidence[0].Source.Type = "invented" },
		"subject type":          func(s *pb.TelemetrySample) { s.IdentityEvidence[0].SubjectKind = "global_asset" },
		"unknown normalization": func(s *pb.TelemetrySample) { s.IdentityEvidence[0].Normalization = "globally_unique" },
		"oversized original":    func(s *pb.TelemetrySample) { s.IdentityEvidence[0].OriginalValue = strings.Repeat("x", 4097) },
		"missing time":          func(s *pb.TelemetrySample) { s.IdentityEvidence[0].Time.ObservedAt = nil },
		"negative uncertainty":  func(s *pb.TelemetrySample) { s.IdentityEvidence[0].Time.Uncertainty = durationpb.New(-time.Second) },
		"per subject limit": func(s *pb.TelemetrySample) {
			s.IdentityEvidence = make([]*pb.IdentityEvidence, 65)
			for i := range s.IdentityEvidence {
				s.IdentityEvidence[i] = identityToProto(e.Telemetry.IdentityEvidence)[0]
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			mutate(p.GetTelemetry().Samples[0])
			if _, err = DecodeEnvelope(p); err == nil {
				t.Fatal("accepted malformed evidence")
			}
		})
	}
}

func TestTopologyEnrichmentRejectsMalformedWire(t *testing.T) {
	for name, mutate := range map[string]func(*pb.Neighbor){
		"nil address":        func(n *pb.Neighbor) { n.RemoteManagementAddresses = []*pb.ManagementAddress{nil} },
		"address list limit": func(n *pb.Neighbor) { n.RemoteManagementAddresses = make([]*pb.ManagementAddress, 17) },
		"invalid IP": func(n *pb.Neighbor) {
			n.RemoteManagementAddresses = []*pb.ManagementAddress{{AddressFamily: 1, Address: "192.0.2.999"}}
		},
		"capability list limit": func(n *pb.Neighbor) { n.RemoteCapabilitiesSupported = make([]string, 17) },
		"interface control":     func(n *pb.Neighbor) { n.LocalInterfaceAlias = "evil\nname" },
		"fake cable":            func(n *pb.Neighbor) { n.Kind = "verified_physical_cable" },
		"evidence control":      func(n *pb.Neighbor) { n.Evidence[0].Value = "evil\nname" },
		"evidence non UTF8":     func(n *pb.Neighbor) { n.Evidence[0].Value = string([]byte{0xff}) },
	} {
		t.Run(name, func(t *testing.T) {
			e := testEnvelope()
			u := remoteTopology()
			e.Topology = &u
			p, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			mutate(p.GetTopology().Observations[0])
			if _, err = DecodeEnvelope(p); err == nil {
				t.Fatal("accepted malformed topology enrichment")
			}
		})
	}
}

func TestClientValidationUsesMappingAndEncodedSize(t *testing.T) {
	c := &client{options: Options{MaxMessageBytes: 512}}
	e := testEnvelope()
	e.Events = []model.Event{{ID: "event", Type: "state", Source: "local", Timestamp: testTime, Message: strings.Repeat("x", 1024)}}
	if err := c.ValidateEnvelope(e); err == nil {
		t.Fatal("oversized encoded message accepted")
	}
	e.Events[0].Message = "ok"
	if err := c.ValidateEnvelope(e); err != nil {
		t.Fatal(err)
	}
	e.Events[0].Message = string([]byte{0xff})
	if err := c.ValidateEnvelope(e); err == nil {
		t.Fatal("invalid UTF-8 would poison retained delivery")
	}
	e.Events[0].Message = "ok"
	e.Events[0].Observation = evidenceObservation(model.SourceLocal, "native")
	e.Events[0].Observation.Source.Type = "invented"
	if err := c.ValidateEnvelope(e); err == nil {
		t.Fatal("invalid source accepted before durable admission")
	}
}

func TestPhase6FieldNumbersAndLegacyFields(t *testing.T) {
	for message, fields := range map[protoreflect.MessageDescriptor]map[protoreflect.Name]protoreflect.FieldNumber{
		(&pb.Device{}).ProtoReflect().Descriptor():           {"identity_evidence": 41, "extended_identity_evidence": 42},
		(&pb.TelemetrySample{}).ProtoReflect().Descriptor():  {"identity_evidence": 15, "extended_identity_evidence": 16},
		(&pb.IdentityEvidence{}).ProtoReflect().Descriptor(): {"type": 1, "source": 5, "subject_id": 7, "subject_kind": 8, "time": 11, "normalization": 12},
		(&pb.TopologyUpdate{}).ProtoReflect().Descriptor():   {"device_id": 1, "observations": 8, "observation": 9},
		(&pb.Neighbor{}).ProtoReflect().Descriptor():         {"remote_chassis_id": 7, "remote_chassis_subtype": 8, "remote_port_id": 9, "remote_port_subtype": 10, "confidence": 18, "observation": 19, "identity_evidence": 20, "observation_id": 21, "age_seconds": 34},
		(&pb.Event{}).ProtoReflect().Descriptor():            {"source": 7, "observation": 8},
	} {
		for name, number := range fields {
			field := message.Fields().ByName(name)
			if field == nil || field.Number() != number {
				t.Fatalf("%s.%s field changed", message.Name(), name)
			}
		}
	}
	method := pb.File_infrapulse_v1_agent_proto.Services().ByName("AgentService").Methods().ByName("Connect")
	if !method.IsStreamingClient() || !method.IsStreamingServer() || method.Input().Name() != "AgentEnvelope" || method.Output().Name() != "ControllerEnvelope" {
		t.Fatal("RPC contract changed")
	}
}

func TestIdentityMappingOwnsRawSubtypeAndTime(t *testing.T) {
	o := evidenceObservation(model.SourceLLDP, "snmp")
	subtype := uint32(7)
	input := []model.IdentityEvidence{{Type: model.IdentityLLDPChassisID, Value: "AQI=", RawValue: []byte{1, 2}, Subtype: &subtype, Source: o.Source, Confidence: "low", Time: &o.Time}}
	p := identityToProto(input)
	input[0].RawValue[0] = 9
	subtype = 4
	*input[0].Time.ClockOffset = time.Second
	if p[0].RawValue[0] != 1 || *p[0].Subtype != 7 || p[0].Time.ClockOffset.AsDuration() != 0 {
		t.Fatal("encoder aliases identity evidence")
	}
	back, err := identityFromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	want := model.IdentityEvidence{Type: model.IdentityLLDPChassisID, Value: "AQI=", RawValue: []byte{1, 2}, Subtype: proto.Uint32(7), Source: o.Source, Confidence: "low", Time: back[0].Time}
	if !reflect.DeepEqual(back[0], want) {
		t.Fatal("identity mapping lost data")
	}
	p[0].RawValue[0] = 9
	*p[0].Subtype = 4
	if back[0].RawValue[0] != 1 || *back[0].Subtype != 7 {
		t.Fatal("decoder aliases identity evidence")
	}
}

func BenchmarkPhase6TopologyWire(b *testing.B) {
	for _, count := range []int{1, 64, 1024} {
		for _, enriched := range []bool{false, true} {
			b.Run(fmt.Sprintf("neighbors=%d/enriched=%t", count, enriched), func(b *testing.B) {
				u := remoteTopology()
				first := u.Observations[0]
				u.Observations = nil
				for i := 0; i < count; i++ {
					n := model.CloneNeighbor(first)
					n.LocalInterfaceID, n.LocalInterfaceName = fmt.Sprintf("if:%d", i+1), fmt.Sprintf("ether%d", i+1)
					if enriched {
						n.Observation = evidenceObservation(model.SourceLLDP, "snmp")
						n.LocalInterfaceIndex = proto.Uint32(uint32(i + 1))
						n.LocalInterfaceMAC = "00:01:02:03:04:05"
						n.RemoteManagementAddresses = []model.ManagementAddress{{AddressFamily: 1, Address: "192.0.2.1", RawAddress: []byte{192, 0, 2, 1}}}
						n.RemoteCapabilitiesSupported = []string{"bridge", "router"}
						n = model.EnrichNeighbor(n)
						annotated := model.ObserveEnvelope(model.Envelope{Topology: &model.TopologyUpdate{DeviceID: u.DeviceID, Source: u.Source, ObservedAt: u.ObservedAt, CollectedAt: u.CollectedAt, Completeness: u.Completeness, Observations: []model.Neighbor{n}}}, n.Observation.Source, u.CollectedAt)
						n = annotated.Topology.Observations[0]
					}
					u.Observations = append(u.Observations, n)
				}
				if enriched {
					u.Observation = evidenceObservation(model.SourceLLDP, "snmp")
				}
				e := testEnvelope()
				e.Topology = &u
				p, err := Encode(e)
				if err != nil {
					b.Fatal(err)
				}
				jsonBytes, err := json.Marshal(e)
				if err != nil {
					b.Fatal(err)
				}
				c := &client{options: Options{MaxMessageBytes: 4 << 20}}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if err := c.ValidateEnvelope(e); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(proto.Size(p)), "protobuf_bytes")
				b.ReportMetric(float64(len(jsonBytes)), "outbox_json_bytes")
			})
		}
	}
}
