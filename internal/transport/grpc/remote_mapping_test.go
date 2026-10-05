package grpc

import (
	"bytes"
	"math"
	"reflect"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func roundTripRemote(t *testing.T, input model.Envelope) model.Envelope {
	t.Helper()
	encoded, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := proto.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded := new(pb.AgentEnvelope)
	if err = proto.Unmarshal(wire, decoded); err != nil {
		t.Fatal(err)
	}
	output, err := DecodeEnvelope(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, output) {
		t.Fatalf("remote roundtrip changed data\ninput: %+v\noutput: %+v", input, output)
	}
	return output
}

func remoteTopology() model.TopologyUpdate {
	return model.TopologyUpdate{DeviceID: "switch-1", ObservedAt: testTime, CollectedAt: testTime.Add(time.Second),
		Source: "snmp-lldp", Completeness: "complete", TTLSeconds: 900, ExpiryAssumption: "three successful polling intervals",
		Observations: []model.Neighbor{{LocalDeviceID: "switch-1", LocalInterfaceID: "if:42", LocalInterfaceName: "ether1", LocalPortNumber: 7,
			LocalPortID: []byte("ether1"), LocalPortSubtype: 5, RemoteChassisID: []byte{0, 1, 2, 3, 255, 254}, RemoteChassisSubtype: 4,
			RemotePortID: []byte{0, 17, 34, 51, 68, 85}, RemotePortSubtype: 3, RemoteSystemName: "unresolved-remote", RemotePortDescription: "uplink",
			Protocol: "lldp", Source: "snmp-lldp", ObservedAt: testTime, ExpiresAt: testTime.Add(900 * time.Second),
			Evidence: []model.Evidence{{Source: "local_port_id_ifName", Value: "ether1", Confidence: "high"}}, Confidence: "high"}}}
}

func TestTopologyWireRoundTrip(t *testing.T) {
	for _, completeness := range []string{"complete", "partial", "failed"} {
		for _, protocol := range []string{"lldp", "cdp", "mndp", "unknown"} {
			t.Run(completeness+"/"+protocol, func(t *testing.T) {
				e := testEnvelope()
				u := remoteTopology()
				u.Completeness, u.Observations[0].Protocol = completeness, protocol
				if completeness != "complete" {
					u.Observations[0].LocalInterfaceID = ""
					u.Observations[0].LocalInterfaceName = ""
					u.Observations[0].Confidence = "unresolved"
				}
				e.Topology = &u
				out := roundTripRemote(t, e)
				if out.Topology.Observations[0].LocalPortNumber == 42 {
					t.Fatal("LLDP local port number was replaced by ifIndex")
				}
			})
		}
	}
	e := testEnvelope()
	u := remoteTopology()
	u.Observations = nil
	e.Topology = &u
	roundTripRemote(t, e) // complete empty is a real snapshot, not a missing payload.
}

func TestRemoteInventoryEvidenceRoundTrip(t *testing.T) {
	device := testDevice()
	unavailable := false
	device.SystemDescription, device.SysObjectID = "SYSTEM fixture", ".1.3.6.1.4.1.14988.1"
	device.SystemLocation, device.SystemContact = "arbitrary device text", "operator"
	device.VendorHint = "MikroTik"
	device.VendorEvidence = []model.Evidence{{Source: "configured_hint", Value: "MikroTik", Confidence: "low"}, {Source: "sysObjectID", Value: device.SysObjectID, Confidence: "high"}}
	device.Components = []model.InventoryComponent{{ID: "entity:1", Class: "chassis", Name: "fixture", Description: "chassis fixture", Manufacturer: "fixture",
		Model: "confirmed-model", SerialNumber: "confirmed-serial", HardwareRevision: "rev", FirmwareVersion: "fw", SoftwareVersion: "sw"}, {ID: "entity:3", ParentID: "entity:1", Class: "module"}}
	device.UptimeAvailable, device.UptimeSource = &unavailable, "sysUpTime"
	e := testEnvelope()
	e.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{device}}
	output := roundTripRemote(t, e)
	if output.Inventory.Devices[0].Location.Room != device.Location.Room {
		t.Fatal("sysLocation changed configured physical location")
	}
}

func TestRemoteCounterPresenceAndWidthsRoundTrip(t *testing.T) {
	sample := testSample()
	uptime := false
	sample.UptimeAvailable, sample.UptimeSource = &uptime, "sysUpTime"
	iface := &sample.Interfaces[0]
	iface.Counters = model.Counters{TXBytes: math.MaxUint64, TXPackets: math.MaxUint32}
	iface.Description, iface.Alias = "ifDescr", "ifAlias"
	typeValue, discontinuity := uint32(6), uint32(0)
	iface.Type = &typeValue
	iface.Metadata = &model.InterfaceMetadata{Index: true, AdminStatus: true}
	iface.CounterMetadata = &model.CounterMetadata{Available: model.CounterAvailability{RXBytes: true, TXBytes: true, TXPackets: true},
		Widths: model.CounterWidths{RXBytes: 64, TXBytes: 64, TXPackets: 32}, DiscontinuityTicks: &discontinuity}
	iface.Rates = &model.Rates{TXBytesPerSecond: 10, Available: &model.CounterAvailability{RXBytes: true, TXBytes: true}}
	e := testEnvelope()
	e.Telemetry = &sample
	out := roundTripRemote(t, e).Telemetry.Interfaces[0]
	if !out.CounterMetadata.Available.RXBytes || out.CounterMetadata.Available.RXPackets || out.CounterMetadata.DiscontinuityTicks == nil {
		t.Fatal("measured zero became unavailable, or unavailable became zero")
	}
	if out.Rates.Available.RXPackets || !out.Rates.Available.RXBytes {
		t.Fatal("partial rate availability was lost")
	}
	// Absent metadata remains distinguishable and retains the P0 contract.
	sample = testSample()
	e.Telemetry = &sample
	if roundTripRemote(t, e).Telemetry.Interfaces[0].CounterMetadata != nil {
		t.Fatal("P0 counter metadata was fabricated")
	}
	// A fallback packet sum is an available value with unknown modular width.
	sample.Interfaces[0].Rates = nil
	sample.Interfaces[0].CounterMetadata = &model.CounterMetadata{Available: model.CounterAvailability{RXPackets: true}}
	out = roundTripRemote(t, e).Telemetry.Interfaces[0]
	if !out.CounterMetadata.Available.RXPackets || out.CounterMetadata.Widths.RXPackets != 0 {
		t.Fatal("unknown-width aggregate became unavailable")
	}
}

func TestCommandVerificationRoundTrip(t *testing.T) {
	observed := false
	for _, uncertain := range []bool{false, true} {
		e := testEnvelope()
		e.CommandResult = &model.CommandResult{CommandID: "command", Status: "timeout", StartedAt: testTime, FinishedAt: testTime.Add(time.Second),
			Changed: !uncertain, Uncertain: uncertain, Verification: &model.InterfaceVerification{InterfaceID: "*1", Name: "ether1", ExpectedAdminUp: false,
				ObservedAdminUp: &observed, ObservedAt: testTime, Verified: !uncertain, Changed: !uncertain, Uncertain: uncertain, Evidence: "read-after-write"}}
		if !uncertain {
			e.CommandResult.Status = "success"
		}
		roundTripRemote(t, e)
	}
	e := testEnvelope()
	e.CommandResult = &model.CommandResult{CommandID: "command", Status: "timeout", Uncertain: true,
		Verification: &model.InterfaceVerification{InterfaceID: "*1", Name: "ether1", ExpectedAdminUp: false, Uncertain: true, Evidence: "verification unavailable"}}
	roundTripRemote(t, e)
}

func TestTopologyRejectsMalformedWire(t *testing.T) {
	for name, change := range map[string]func(*pb.TopologyUpdate){
		"nil observation":          func(u *pb.TopologyUpdate) { u.Observations[0] = nil },
		"missing observed time":    func(u *pb.TopologyUpdate) { u.ObservedAt = nil },
		"invalid expiry":           func(u *pb.TopologyUpdate) { u.Observations[0].ExpiresAt = &timestamppb.Timestamp{Nanos: 1000000000} },
		"unknown completeness":     func(u *pb.TopologyUpdate) { u.Completeness = pb.SnapshotCompleteness(99) },
		"unspecified completeness": func(u *pb.TopologyUpdate) { u.Completeness = 0 },
		"unknown protocol":         func(u *pb.TopologyUpdate) { u.Observations[0].Protocol = pb.DiscoveryProtocol(99) },
		"unknown confidence":       func(u *pb.TopologyUpdate) { u.Observations[0].Confidence = "certain" },
		"nil evidence":             func(u *pb.TopologyUpdate) { u.Observations[0].Evidence[0] = nil },
		"wrong local device":       func(u *pb.TopologyUpdate) { u.Observations[0].LocalDeviceId = "other" },
		"bad subtype":              func(u *pb.TopologyUpdate) { u.Observations[0].RemoteChassisSubtype = 8 },
		"oversized raw ID":         func(u *pb.TopologyUpdate) { u.Observations[0].RemotePortId = make([]byte, 1025) },
		"excessive TTL":            func(u *pb.TopologyUpdate) { u.TtlSeconds = math.MaxUint32 },
		"expires before observed":  func(u *pb.TopologyUpdate) { u.Observations[0].ExpiresAt = timestamppb.New(testTime.Add(-time.Second)) },
		"observation limit":        func(u *pb.TopologyUpdate) { u.Observations = make([]*pb.Neighbor, maxRemoteEntries+1) },
	} {
		t.Run(name, func(t *testing.T) {
			e := testEnvelope()
			u := remoteTopology()
			e.Topology = &u
			encoded, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			change(encoded.GetTopology())
			if _, err = DecodeEnvelope(encoded); err == nil {
				t.Fatal("malformed topology accepted")
			}
		})
	}
	e := testEnvelope()
	u := remoteTopology()
	u.Observations[0].Protocol = "arp"
	e.Topology = &u
	if _, err := Encode(e); err == nil {
		t.Fatal("ARP was encoded as physical topology")
	}
	u.Observations[0].Protocol = "lldp"
	e.Inventory = &model.InventoryUpdate{ObservedAt: testTime}
	if _, err := Encode(e); err == nil {
		t.Fatal("ambiguous topology and inventory accepted")
	}
}

func TestCounterMetadataRejectsMalformedWire(t *testing.T) {
	for name, change := range map[string]func(*pb.Interface){
		"nil availability":                 func(i *pb.Interface) { i.CounterMetadata.Available = nil },
		"nil widths":                       func(i *pb.Interface) { i.CounterMetadata.Widths = nil },
		"unknown width":                    func(i *pb.Interface) { i.CounterMetadata.Widths.RxBytes = 48 },
		"overflow before uint8 conversion": func(i *pb.Interface) { i.CounterMetadata.Widths.RxBytes = 320 },
		"32-bit value overflow":            func(i *pb.Interface) { i.CounterMetadata.Widths.RxBytes = 32; i.Counters.RxBytes = math.MaxUint64 },
		"rate for missing counter":         func(i *pb.Interface) { i.Rates = &pb.InterfaceRates{} },
	} {
		t.Run(name, func(t *testing.T) {
			s := testSample()
			s.Interfaces[0].Rates = nil
			s.Interfaces[0].CounterMetadata = &model.CounterMetadata{Available: model.CounterAvailability{RXBytes: true}, Widths: model.CounterWidths{RXBytes: 64}}
			e := testEnvelope()
			e.Telemetry = &s
			encoded, err := Encode(e)
			if err != nil {
				t.Fatal(err)
			}
			change(encoded.GetTelemetry().Samples[0].Interfaces[0])
			if _, err := DecodeEnvelope(encoded); err == nil {
				t.Fatal("malformed counter metadata accepted")
			}
		})
	}
}

func TestVerificationRejectsFabricatedSuccess(t *testing.T) {
	for name, verification := range map[string]*model.InterfaceVerification{
		"no observation":        {InterfaceID: "*1", Verified: true},
		"wrong observed state":  {InterfaceID: "*1", ExpectedAdminUp: true, ObservedAdminUp: proto.Bool(false), ObservedAt: testTime, Verified: true},
		"uncertain verified":    {InterfaceID: "*1", ObservedAdminUp: proto.Bool(false), ObservedAt: testTime, Verified: true, Uncertain: true},
		"missing observed time": {InterfaceID: "*1", ObservedAdminUp: proto.Bool(false)},
	} {
		t.Run(name, func(t *testing.T) {
			e := testEnvelope()
			e.CommandResult = &model.CommandResult{CommandID: "command", Status: "success", Verification: verification}
			if _, err := Encode(e); err == nil {
				t.Fatal("unverified success evidence accepted")
			}
		})
	}
}

func TestTopologyMappingOwnsRawBytes(t *testing.T) {
	e := testEnvelope()
	u := remoteTopology()
	e.Topology = &u
	encoded, err := Encode(e)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), u.Observations[0].RemoteChassisID...)
	u.Observations[0].RemoteChassisID[0] = 128
	if !bytes.Equal(encoded.GetTopology().Observations[0].RemoteChassisId, want) {
		t.Fatal("encoder aliases caller bytes")
	}
	output, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	encoded.GetTopology().Observations[0].RemoteChassisId[0] = 129
	if !bytes.Equal(output.Topology.Observations[0].RemoteChassisID, want) {
		t.Fatal("decoder aliases protobuf bytes")
	}
}

func TestP0WireFieldNumbersRemainStable(t *testing.T) {
	for _, tc := range []struct {
		message protoreflect.MessageDescriptor
		fields  map[protoreflect.Name]protoreflect.FieldNumber
	}{
		{(&pb.AgentEnvelope{}).ProtoReflect().Descriptor(), map[protoreflect.Name]protoreflect.FieldNumber{
			"message_id": 1, "agent_id": 2, "session_id": 3, "sequence": 4, "sent_at": 5, "hello": 10, "heartbeat": 11, "telemetry": 12, "inventory": 13, "topology": 14, "events": 16, "command_result": 18, "error": 20}},
		{(&pb.Device{}).ProtoReflect().Descriptor(), map[protoreflect.Name]protoreflect.FieldNumber{
			"id": 1, "name": 2, "hostname": 3, "management_address": 4, "vendor": 5, "model": 6, "type": 7, "serial_number": 8, "hardware_revision": 9,
			"software_version": 10, "firmware_version": 11, "os": 12, "distribution": 13, "kernel": 14, "architecture": 15, "chassis_id": 16, "driver": 17,
			"uptime_seconds": 18, "boot_id": 19, "online": 20, "last_seen": 21, "location": 22, "capabilities": 23}},
		{(&pb.Interface{}).ProtoReflect().Descriptor(), map[protoreflect.Name]protoreflect.FieldNumber{
			"id": 1, "index": 2, "name": 3, "mac": 4, "mtu": 5, "admin_up": 6, "oper_state": 7, "speed_bits_per_second": 8, "addresses": 9, "counters": 10, "rates": 11}},
		{(&pb.CommandResult{}).ProtoReflect().Descriptor(), map[protoreflect.Name]protoreflect.FieldNumber{
			"command_id": 1, "status": 2, "started_at": 3, "finished_at": 4, "changed": 5, "error_code": 6, "error_message": 7}},
	} {
		for name, number := range tc.fields {
			field := tc.message.Fields().ByName(name)
			if field == nil || field.Number() != number {
				t.Fatalf("%s.%s changed field number", tc.message.Name(), name)
			}
		}
	}
}
