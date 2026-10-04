package grpc

import (
	"math"
	"reflect"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var testTime = time.Date(2026, 10, 4, 10, 20, 30, 123000000, time.UTC)

func testEnvelope() model.Envelope {
	return model.Envelope{MessageID: "message-1", AgentID: "agent-1", SessionID: "session-1", Sequence: 42, SentAt: testTime}
}

func testDevice() model.Device {
	return model.Device{
		ID: "local-agent-1", Name: "collector", Hostname: "linux-1", ManagementAddress: "192.0.2.1",
		Vendor: "test-vendor", Model: "test-model", Type: "server", SerialNumber: "serial-1",
		HardwareRevision: "A", SoftwareVersion: "1", FirmwareVersion: "2", OS: "linux", Distribution: "Test Linux",
		Kernel: "6", Architecture: "arm64", ChassisID: "chassis-1", Driver: "linux", UptimeSeconds: 100,
		BootID: "boot-1", Online: true, LastSeen: testTime,
		Location:     model.PhysicalLocation{Site: "demo", Datacenter: "dc", Building: "b", Room: "r", Rack: "rack", RackUnit: 2, Position: "front", Comment: "test"},
		Capabilities: []model.Capability{{Name: "system.read", Supported: true}, {Name: "interfaces.disable", Reason: "P0 read-only"}},
	}
}

func testSample() model.TelemetrySample {
	zero := 0.0
	speed := uint64(1000000000)
	return model.TelemetrySample{
		DeviceID: "local-agent-1", ObservedAt: testTime, CollectedAt: testTime.Add(time.Millisecond),
		Source: "linux", BootID: "boot-1", UptimeSeconds: 100,
		CPU:    &model.CPUStats{LogicalCPUs: 4, PhysicalCores: 2, Model: "cpu", UsagePercent: &zero, Load1: 1, Load5: 2, Load15: 3},
		Memory: &model.MemoryStats{TotalBytes: 4096, UsedBytes: 2048, AvailableBytes: 2048, FreeBytes: 1024, SwapTotalBytes: 512, SwapUsedBytes: 256, PressureSomeAvg10: &zero},
		Interfaces: []model.Interface{{ID: "1", Index: 1, Name: "eth0", MAC: "00:11:22:33:44:55", MTU: 1500,
			AdminUp: true, OperState: "up", SpeedBitsPerSecond: &speed, Addresses: []string{"192.0.2.1/24", "2001:db8::1/64"},
			Counters: model.Counters{RXBytes: math.MaxUint64, TXBytes: 2, RXPackets: 3, TXPackets: 4, RXErrors: 5, TXErrors: 6, RXDropped: 7, TXDropped: 8},
			Rates:    &model.Rates{RXBytesPerSecond: 1, TXBytesPerSecond: 2, RXPacketsPerSecond: 3, TXPacketsPerSecond: 4, RXErrorsPerSecond: 5, TXErrorsPerSecond: 6, RXDroppedPerSecond: 7, TXDroppedPerSecond: 8},
		}},
		Metrics: []model.Metric{{Name: "pressure.cpu.some.avg10", Value: 0.5, Unit: "percent"}},
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	hello := testEnvelope()
	hello.Hello = &model.AgentHello{AgentID: hello.AgentID, SessionID: hello.SessionID, Name: "test", Build: model.BuildInfo{Version: "dev", Commit: "123", BuildTime: "now"}, StartedAt: testTime, Device: testDevice(), ProtocolVersion: 1}
	heartbeat := testEnvelope()
	heartbeat.Heartbeat = &model.Heartbeat{Timestamp: testTime, UptimeSeconds: 123, Build: hello.Hello.Build, Health: "healthy", RemoteDeviceCount: 2, Queue: model.QueueState{Messages: 10, Bytes: 1000, DroppedTelemetry: 42, DroppedOther: 1}}
	telemetry := testEnvelope()
	sample := testSample()
	telemetry.Telemetry = &sample
	inventory := testEnvelope()
	inventory.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{testDevice()}}
	events := testEnvelope()
	events.Events = []model.Event{{ID: "event-1", Type: "agent.started", DeviceID: "local-agent-1", Timestamp: testTime, Severity: "info", Message: "started", Source: "agent"}}
	result := testEnvelope()
	result.CommandResult = &model.CommandResult{CommandID: "command-1", Status: "unsupported", StartedAt: testTime, FinishedAt: testTime.Add(time.Second), ErrorCode: "capability_unsupported", ErrorMessage: "P0 is read-only"}
	report := testEnvelope()
	report.Error = &model.ErrorReport{Component: "collector", Code: "timeout", Message: "unavailable", Timestamp: testTime}
	for name, input := range map[string]model.Envelope{"hello": hello, "heartbeat": heartbeat, "telemetry": telemetry, "inventory": inventory, "events": events, "command_result": result, "error": report} {
		t.Run(name, func(t *testing.T) {
			encoded, err := Encode(input)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := proto.Marshal(encoded)
			if err != nil {
				t.Fatal(err)
			}
			decodedWire := new(pb.AgentEnvelope)
			if err := proto.Unmarshal(wire, decodedWire); err != nil {
				t.Fatal(err)
			}
			output, err := DecodeEnvelope(decodedWire)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(input, output) {
				t.Fatalf("round trip mismatch\ninput: %#v\noutput: %#v", input, output)
			}
		})
	}
}

func TestOptionalMetricsAndUTC(t *testing.T) {
	sample := testSample()
	sample.ObservedAt = sample.ObservedAt.In(time.FixedZone("Moscow", 3*3600))
	sample.CPU.UsagePercent = nil
	sample.Memory.PressureSomeAvg10 = nil
	sample.Interfaces[0].SpeedBitsPerSecond = nil
	sample.Interfaces[0].Rates = nil
	envelope := testEnvelope()
	envelope.Telemetry = &sample
	encoded, err := Encode(envelope)
	if err != nil {
		t.Fatal(err)
	}
	output, err := DecodeEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if output.Telemetry.CPU.UsagePercent != nil || output.Telemetry.Memory.PressureSomeAvg10 != nil || output.Telemetry.Interfaces[0].SpeedBitsPerSecond != nil || output.Telemetry.Interfaces[0].Rates != nil {
		t.Fatal("missing metric became a measured zero")
	}
	if output.Telemetry.ObservedAt.Location() != time.UTC || !output.Telemetry.ObservedAt.Equal(sample.ObservedAt) {
		t.Fatal("observed time is not the same UTC instant")
	}
}

func TestEncodeRejectsInvalidEnvelope(t *testing.T) {
	valid := testEnvelope()
	sample := testSample()
	valid.Telemetry = &sample
	for name, change := range map[string]func(*model.Envelope){
		"missing payload":        func(e *model.Envelope) { e.Telemetry = nil },
		"multiple payloads":      func(e *model.Envelope) { e.Error = &model.ErrorReport{Timestamp: testTime} },
		"missing sent time":      func(e *model.Envelope) { e.SentAt = time.Time{} },
		"out of range sent time": func(e *model.Envelope) { e.SentAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"missing agent identity": func(e *model.Envelope) { e.AgentID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			change(&input)
			if _, err := Encode(input); err == nil {
				t.Fatal("invalid envelope was accepted")
			}
		})
	}
	badMetric := testSample()
	badMetric.Metrics[0].Value = math.NaN()
	valid.Telemetry = &badMetric
	if _, err := Encode(valid); err == nil {
		t.Fatal("NaN was accepted")
	}
}

func TestDecodeRejectsMalformedMessages(t *testing.T) {
	for name, input := range map[string]*pb.AgentEnvelope{
		"nil envelope":       nil,
		"missing payload":    {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: timestamppb.New(testTime)},
		"nil oneof wrapper":  {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: timestamppb.New(testTime), Payload: (*pb.AgentEnvelope_Hello)(nil)},
		"nil oneof message":  {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: timestamppb.New(testTime), Payload: &pb.AgentEnvelope_Hello{}},
		"invalid timestamp":  {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: &timestamppb.Timestamp{Seconds: 0, Nanos: 1000000000}},
		"nil sample":         {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: timestamppb.New(testTime), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{nil}}}},
		"multi-sample batch": {MessageId: "m", AgentId: "a", SessionId: "s", SentAt: timestamppb.New(testTime), Payload: &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{Samples: []*pb.TelemetrySample{{}, {}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEnvelope(input); err == nil {
				t.Fatal("malformed message was accepted")
			}
		})
	}
	input := testEnvelope()
	input.Inventory = &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{testDevice()}}
	encoded, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded.GetInventory().Devices[0].Type = pb.DeviceType(999)
	if _, err := DecodeEnvelope(encoded); err == nil {
		t.Fatal("unknown device enum was accepted")
	}
	input.Inventory = nil
	input.CommandResult = &model.CommandResult{CommandID: "c", Status: "unsupported"}
	encoded, err = Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	encoded.GetCommandResult().Status = pb.CommandStatus(999)
	if _, err := DecodeEnvelope(encoded); err == nil {
		t.Fatal("unknown result status was accepted")
	}
}

func TestEnvelopeRequiresStablePayloadIdentity(t *testing.T) {
	for name, input := range map[string]model.Envelope{
		"hello version": {
			MessageID: "m", AgentID: "a", SessionID: "s", SentAt: testTime,
			Hello: &model.AgentHello{AgentID: "a", SessionID: "s", StartedAt: testTime, Device: testDevice()},
		},
		"inventory device ID": {
			MessageID: "m", AgentID: "a", SessionID: "s", SentAt: testTime,
			Inventory: &model.InventoryUpdate{ObservedAt: testTime, Devices: []model.Device{{Type: "unknown"}}},
		},
		"result command ID": {
			MessageID: "m", AgentID: "a", SessionID: "s", SentAt: testTime,
			CommandResult: &model.CommandResult{Status: "unsupported"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Encode(input); err == nil {
				t.Fatal("missing payload identity or version was accepted")
			}
		})
	}
}

func testCommand() model.Command {
	return model.Command{ID: "command-1", TargetDeviceID: "router-1", RequestedAt: testTime, Timeout: 5 * time.Second,
		Initiator: "human", IdempotencyKey: "request-1", Interface: &model.InterfaceAction{InterfaceID: "ether2", Operation: "disable"}}
}

func TestCommandRoundTrip(t *testing.T) {
	for _, initiator := range []string{"human", "ai_suggested", "automation"} {
		for _, kind := range []string{"interface", "service"} {
			t.Run(initiator+"/"+kind, func(t *testing.T) {
				input := testCommand()
				input.Initiator = initiator
				if kind == "service" {
					input.Interface = nil
					input.Service = &model.ServiceAction{Service: "demo.service", Operation: "restart"}
				}
				encoded, err := EncodeCommand(input)
				if err != nil {
					t.Fatal(err)
				}
				output, err := DecodeCommand(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(input, output) {
					t.Fatalf("command changed: %#v", output)
				}
			})
		}
	}
}

func TestCommandRejectsUnknownAndMissingValues(t *testing.T) {
	for name, change := range map[string]func(*pb.Command){
		"initiator":             func(c *pb.Command) { c.Initiator = pb.CommandInitiator(99) },
		"unspecified initiator": func(c *pb.Command) { c.Initiator = pb.CommandInitiator_COMMAND_INITIATOR_UNSPECIFIED },
		"operation":             func(c *pb.Command) { c.GetInterface().Operation = pb.InterfaceOperation(99) },
		"nil interface":         func(c *pb.Command) { c.Action = &pb.Command_Interface{} },
		"nil service":           func(c *pb.Command) { c.Action = &pb.Command_Service{} },
		"nil wrapper":           func(c *pb.Command) { c.Action = (*pb.Command_Interface)(nil) },
		"missing action":        func(c *pb.Command) { c.Action = nil },
		"missing time":          func(c *pb.Command) { c.RequestedAt = nil },
		"missing timeout":       func(c *pb.Command) { c.Timeout = nil },
		"zero timeout":          func(c *pb.Command) { c.Timeout = durationpb.New(0) },
		"negative timeout":      func(c *pb.Command) { c.Timeout = durationpb.New(-time.Second) },
		"overflow timeout":      func(c *pb.Command) { c.Timeout = &durationpb.Duration{Seconds: 315576000000} },
		"invalid timeout nanos": func(c *pb.Command) { c.Timeout = &durationpb.Duration{Nanos: 1000000000} },
		"missing target":        func(c *pb.Command) { c.TargetDeviceId = "" },
		"missing interface ID":  func(c *pb.Command) { c.GetInterface().InterfaceId = "" },
	} {
		t.Run(name, func(t *testing.T) {
			command, err := EncodeCommand(testCommand())
			if err != nil {
				t.Fatal(err)
			}
			change(command)
			if _, err := DecodeCommand(command); err == nil {
				t.Fatal("invalid command was accepted")
			}
		})
	}
	command := testCommand()
	command.Service = &model.ServiceAction{Service: "s", Operation: "stop"}
	if _, err := EncodeCommand(command); err == nil {
		t.Fatal("multiple typed actions were accepted")
	}
	if _, err := DecodeCommand(nil); err == nil {
		t.Fatal("nil command was accepted")
	}
}
