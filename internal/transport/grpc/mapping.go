package grpc

import (
	"errors"
	"fmt"
	"math"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var deviceTypes = map[string]pb.DeviceType{
	"":              pb.DeviceType_DEVICE_TYPE_UNSPECIFIED,
	"server":        pb.DeviceType_DEVICE_TYPE_SERVER,
	"workstation":   pb.DeviceType_DEVICE_TYPE_WORKSTATION,
	"router":        pb.DeviceType_DEVICE_TYPE_ROUTER,
	"switch":        pb.DeviceType_DEVICE_TYPE_SWITCH,
	"firewall":      pb.DeviceType_DEVICE_TYPE_FIREWALL,
	"gateway":       pb.DeviceType_DEVICE_TYPE_GATEWAY,
	"access-point":  pb.DeviceType_DEVICE_TYPE_ACCESS_POINT,
	"voice-gateway": pb.DeviceType_DEVICE_TYPE_VOICE_GATEWAY,
	"pbx":           pb.DeviceType_DEVICE_TYPE_PBX,
	"hypervisor":    pb.DeviceType_DEVICE_TYPE_HYPERVISOR,
	"unknown":       pb.DeviceType_DEVICE_TYPE_UNKNOWN,
}

var commandStatuses = map[string]pb.CommandStatus{
	"accepted":    pb.CommandStatus_COMMAND_STATUS_ACCEPTED,
	"running":     pb.CommandStatus_COMMAND_STATUS_RUNNING,
	"success":     pb.CommandStatus_COMMAND_STATUS_SUCCESS,
	"failed":      pb.CommandStatus_COMMAND_STATUS_FAILED,
	"rejected":    pb.CommandStatus_COMMAND_STATUS_REJECTED,
	"timeout":     pb.CommandStatus_COMMAND_STATUS_TIMEOUT,
	"unsupported": pb.CommandStatus_COMMAND_STATUS_UNSUPPORTED,
}

// Encode rejects an absent or ambiguous normalized payload. Proto oneof then
// guarantees exactly one supported wire payload. All times are encoded in UTC.
func Encode(envelope model.Envelope) (*pb.AgentEnvelope, error) {
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	message := &pb.AgentEnvelope{
		MessageId: envelope.MessageID, AgentId: envelope.AgentID,
		SessionId: envelope.SessionID, Sequence: envelope.Sequence,
		SentAt: timestamp(envelope.SentAt),
	}
	switch {
	case envelope.Hello != nil:
		h := envelope.Hello
		message.Payload = &pb.AgentEnvelope_Hello{Hello: &pb.AgentHello{
			AgentId: h.AgentID, SessionId: h.SessionID, Name: h.Name,
			Build: buildToProto(h.Build), StartedAt: timestamp(h.StartedAt),
			Device: deviceToProto(h.Device), ProtocolVersion: h.ProtocolVersion,
		}}
	case envelope.Heartbeat != nil:
		h := envelope.Heartbeat
		message.Payload = &pb.AgentEnvelope_Heartbeat{Heartbeat: &pb.Heartbeat{
			Timestamp: timestamp(h.Timestamp), UptimeSeconds: h.UptimeSeconds,
			Build: buildToProto(h.Build), Health: h.Health,
			RemoteDeviceCount: uint32(h.RemoteDeviceCount), Queue: &pb.QueueState{
				Messages: uint64(h.Queue.Messages), Bytes: uint64(h.Queue.Bytes),
				DroppedTelemetry: h.Queue.DroppedTelemetry, DroppedOther: h.Queue.DroppedOther,
			},
		}}
	case envelope.Telemetry != nil:
		message.Payload = &pb.AgentEnvelope_Telemetry{Telemetry: &pb.TelemetryBatch{
			Samples: []*pb.TelemetrySample{sampleToProto(*envelope.Telemetry)},
		}}
	case envelope.Inventory != nil:
		i := envelope.Inventory
		update := &pb.InventoryUpdate{ObservedAt: timestamp(i.ObservedAt)}
		for _, device := range i.Devices {
			update.Devices = append(update.Devices, deviceToProto(device))
		}
		message.Payload = &pb.AgentEnvelope_Inventory{Inventory: update}
	case len(envelope.Events) > 0:
		batch := &pb.EventBatch{}
		for _, event := range envelope.Events {
			batch.Events = append(batch.Events, &pb.Event{
				Id: event.ID, Type: event.Type, DeviceId: event.DeviceID,
				Timestamp: timestamp(event.Timestamp), Severity: event.Severity,
				Message: event.Message, Source: event.Source,
			})
		}
		message.Payload = &pb.AgentEnvelope_Events{Events: batch}
	case envelope.CommandResult != nil:
		r := envelope.CommandResult
		message.Payload = &pb.AgentEnvelope_CommandResult{CommandResult: &pb.CommandResult{
			CommandId: r.CommandID, Status: commandStatuses[r.Status],
			StartedAt: optionalTimestamp(r.StartedAt), FinishedAt: optionalTimestamp(r.FinishedAt),
			Changed: r.Changed, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage,
		}}
	case envelope.Error != nil:
		e := envelope.Error
		message.Payload = &pb.AgentEnvelope_Error{Error: &pb.ErrorReport{
			Component: e.Component, Code: e.Code, Message: e.Message,
			Timestamp: timestamp(e.Timestamp),
		}}
	}
	return message, nil
}

// DecodeEnvelope is useful for the Mock Controller and contract tests. The
// normalized P0 envelope holds one telemetry sample; a multi-sample wire batch
// must be handled explicitly by a future Backend instead of silently truncated.
func DecodeEnvelope(message *pb.AgentEnvelope) (model.Envelope, error) {
	if message == nil {
		return model.Envelope{}, errors.New("agent envelope is nil")
	}
	sentAt, err := requiredTime(message.SentAt, "sent_at")
	if err != nil {
		return model.Envelope{}, err
	}
	e := model.Envelope{
		MessageID: message.MessageId, AgentID: message.AgentId,
		SessionID: message.SessionId, Sequence: message.Sequence, SentAt: sentAt,
	}
	switch payload := message.Payload.(type) {
	case *pb.AgentEnvelope_Hello:
		if payload == nil || payload.Hello == nil || payload.Hello.Device == nil {
			return e, errors.New("hello or local device is nil")
		}
		h := payload.Hello
		startedAt, err := requiredTime(h.StartedAt, "hello.started_at")
		if err != nil {
			return e, err
		}
		device, err := deviceFromProto(h.Device)
		if err != nil {
			return e, err
		}
		e.Hello = &model.AgentHello{
			AgentID: h.AgentId, SessionID: h.SessionId, Name: h.Name,
			Build: buildFromProto(h.Build), StartedAt: startedAt,
			Device: device, ProtocolVersion: h.ProtocolVersion,
		}
	case *pb.AgentEnvelope_Heartbeat:
		if payload == nil || payload.Heartbeat == nil || payload.Heartbeat.Queue == nil {
			return e, errors.New("heartbeat or queue state is nil")
		}
		h := payload.Heartbeat
		at, err := requiredTime(h.Timestamp, "heartbeat.timestamp")
		if err != nil {
			return e, err
		}
		if h.Queue.Messages > uint64(math.MaxInt) || h.Queue.Bytes > uint64(math.MaxInt) || uint64(h.RemoteDeviceCount) > uint64(math.MaxInt) {
			return e, errors.New("heartbeat counts overflow native integer")
		}
		e.Heartbeat = &model.Heartbeat{
			Timestamp: at, UptimeSeconds: h.UptimeSeconds, Build: buildFromProto(h.Build),
			Health: h.Health, RemoteDeviceCount: int(h.RemoteDeviceCount),
			Queue: model.QueueState{Messages: int(h.Queue.Messages), Bytes: int(h.Queue.Bytes),
				DroppedTelemetry: h.Queue.DroppedTelemetry, DroppedOther: h.Queue.DroppedOther},
		}
	case *pb.AgentEnvelope_Telemetry:
		if payload == nil || payload.Telemetry == nil || len(payload.Telemetry.Samples) != 1 {
			return e, errors.New("P0 telemetry envelope requires exactly one sample")
		}
		sample, err := sampleFromProto(payload.Telemetry.Samples[0])
		if err != nil {
			return e, err
		}
		e.Telemetry = &sample
	case *pb.AgentEnvelope_Inventory:
		if payload == nil || payload.Inventory == nil {
			return e, errors.New("inventory payload is nil")
		}
		i := payload.Inventory
		at, err := requiredTime(i.ObservedAt, "inventory.observed_at")
		if err != nil {
			return e, err
		}
		e.Inventory = &model.InventoryUpdate{ObservedAt: at}
		for _, item := range i.Devices {
			device, err := deviceFromProto(item)
			if err != nil {
				return e, err
			}
			e.Inventory.Devices = append(e.Inventory.Devices, device)
		}
	case *pb.AgentEnvelope_Events:
		if payload == nil || payload.Events == nil || len(payload.Events.Events) == 0 {
			return e, errors.New("event batch is nil or empty")
		}
		for _, event := range payload.Events.Events {
			if event == nil {
				return e, errors.New("event is nil")
			}
			at, err := requiredTime(event.Timestamp, "event.timestamp")
			if err != nil {
				return e, err
			}
			e.Events = append(e.Events, model.Event{
				ID: event.Id, Type: event.Type, DeviceID: event.DeviceId,
				Timestamp: at, Severity: event.Severity, Message: event.Message, Source: event.Source,
			})
		}
	case *pb.AgentEnvelope_CommandResult:
		if payload == nil || payload.CommandResult == nil {
			return e, errors.New("command result is nil")
		}
		r := payload.CommandResult
		status, ok := reverseLookup(commandStatuses, r.Status)
		if !ok {
			return e, errors.New("command result has unknown status")
		}
		startedAt, err := optionalTime(r.StartedAt, "command_result.started_at")
		if err != nil {
			return e, err
		}
		finishedAt, err := optionalTime(r.FinishedAt, "command_result.finished_at")
		if err != nil {
			return e, err
		}
		e.CommandResult = &model.CommandResult{
			CommandID: r.CommandId, Status: status, StartedAt: startedAt, FinishedAt: finishedAt,
			Changed: r.Changed, ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage,
		}
	case *pb.AgentEnvelope_Error:
		if payload == nil || payload.Error == nil {
			return e, errors.New("error payload is nil")
		}
		report := payload.Error
		at, err := requiredTime(report.Timestamp, "error.timestamp")
		if err != nil {
			return e, err
		}
		e.Error = &model.ErrorReport{Component: report.Component, Code: report.Code, Message: report.Message, Timestamp: at}
	default:
		return e, errors.New("agent payload is missing or unsupported")
	}
	if err := validateEnvelope(e); err != nil {
		return model.Envelope{}, err
	}
	return e, nil
}

func EncodeCommand(command model.Command) (*pb.Command, error) {
	if command.ID == "" || command.TargetDeviceID == "" {
		return nil, errors.New("command and target IDs are required")
	}
	if err := validateTime(command.RequestedAt, "command.requested_at"); err != nil {
		return nil, err
	}
	if command.Timeout <= 0 {
		return nil, errors.New("command timeout must be positive")
	}
	initiator := map[string]pb.CommandInitiator{
		"human":        pb.CommandInitiator_COMMAND_INITIATOR_HUMAN,
		"ai_suggested": pb.CommandInitiator_COMMAND_INITIATOR_AI_SUGGESTED,
		"automation":   pb.CommandInitiator_COMMAND_INITIATOR_AUTOMATION,
	}[command.Initiator]
	if initiator == pb.CommandInitiator_COMMAND_INITIATOR_UNSPECIFIED {
		return nil, errors.New("command initiator is unsupported")
	}
	if (command.Interface == nil) == (command.Service == nil) {
		return nil, errors.New("command requires exactly one typed action")
	}
	message := &pb.Command{
		Id: command.ID, TargetDeviceId: command.TargetDeviceID,
		RequestedAt: timestamp(command.RequestedAt), Timeout: durationpb.New(command.Timeout),
		Initiator: initiator, IdempotencyKey: command.IdempotencyKey,
	}
	if command.Interface != nil {
		action := command.Interface
		operation := map[string]pb.InterfaceOperation{"enable": pb.InterfaceOperation_INTERFACE_OPERATION_ENABLE, "disable": pb.InterfaceOperation_INTERFACE_OPERATION_DISABLE}[action.Operation]
		if action.InterfaceID == "" || operation == pb.InterfaceOperation_INTERFACE_OPERATION_UNSPECIFIED {
			return nil, errors.New("interface action has missing resource or unknown operation")
		}
		message.Action = &pb.Command_Interface{Interface: &pb.InterfaceAction{InterfaceId: action.InterfaceID, Operation: operation}}
	} else {
		action := command.Service
		operation := map[string]pb.ServiceOperation{"start": pb.ServiceOperation_SERVICE_OPERATION_START, "stop": pb.ServiceOperation_SERVICE_OPERATION_STOP, "restart": pb.ServiceOperation_SERVICE_OPERATION_RESTART}[action.Operation]
		if action.Service == "" || operation == pb.ServiceOperation_SERVICE_OPERATION_UNSPECIFIED {
			return nil, errors.New("service action has missing resource or unknown operation")
		}
		message.Action = &pb.Command_Service{Service: &pb.ServiceAction{Service: action.Service, Operation: operation}}
	}
	return message, nil
}

// DecodeCommand only performs wire/structural validation. Target capability,
// policy, protected resources, expiry and idempotency remain core checks.
func DecodeCommand(message *pb.Command) (model.Command, error) {
	if message == nil {
		return model.Command{}, errors.New("command is nil")
	}
	at, err := requiredTime(message.RequestedAt, "command.requested_at")
	if err != nil {
		return model.Command{}, err
	}
	if message.Timeout == nil || message.Timeout.CheckValid() != nil || message.Timeout.Seconds < 0 || message.Timeout.Nanos < 0 {
		return model.Command{}, errors.New("command timeout is missing or invalid")
	}
	if message.Timeout.Seconds > math.MaxInt64/int64(time.Second) || (message.Timeout.Seconds == math.MaxInt64/int64(time.Second) && int64(message.Timeout.Nanos) > math.MaxInt64%int64(time.Second)) {
		return model.Command{}, errors.New("command timeout overflows Go duration")
	}
	initiators := map[pb.CommandInitiator]string{
		pb.CommandInitiator_COMMAND_INITIATOR_HUMAN:        "human",
		pb.CommandInitiator_COMMAND_INITIATOR_AI_SUGGESTED: "ai_suggested",
		pb.CommandInitiator_COMMAND_INITIATOR_AUTOMATION:   "automation",
	}
	initiator, ok := initiators[message.Initiator]
	if !ok {
		return model.Command{}, errors.New("command initiator is unknown or unspecified")
	}
	command := model.Command{
		ID: message.Id, TargetDeviceID: message.TargetDeviceId, RequestedAt: at,
		Timeout: message.Timeout.AsDuration(), Initiator: initiator, IdempotencyKey: message.IdempotencyKey,
	}
	switch action := message.Action.(type) {
	case *pb.Command_Interface:
		if action == nil || action.Interface == nil {
			return command, errors.New("interface action is nil")
		}
		operations := map[pb.InterfaceOperation]string{pb.InterfaceOperation_INTERFACE_OPERATION_ENABLE: "enable", pb.InterfaceOperation_INTERFACE_OPERATION_DISABLE: "disable"}
		operation, ok := operations[action.Interface.Operation]
		if !ok {
			return command, errors.New("interface operation is unknown or unspecified")
		}
		command.Interface = &model.InterfaceAction{InterfaceID: action.Interface.InterfaceId, Operation: operation}
	case *pb.Command_Service:
		if action == nil || action.Service == nil {
			return command, errors.New("service action is nil")
		}
		operations := map[pb.ServiceOperation]string{pb.ServiceOperation_SERVICE_OPERATION_START: "start", pb.ServiceOperation_SERVICE_OPERATION_STOP: "stop", pb.ServiceOperation_SERVICE_OPERATION_RESTART: "restart"}
		operation, ok := operations[action.Service.Operation]
		if !ok {
			return command, errors.New("service operation is unknown or unspecified")
		}
		command.Service = &model.ServiceAction{Service: action.Service.Service, Operation: operation}
	default:
		return command, errors.New("command action is missing or unsupported")
	}
	// Reuse the outbound structural checks, keeping mappings symmetric.
	if _, err := EncodeCommand(command); err != nil {
		return model.Command{}, err
	}
	return command, nil
}

func validateEnvelope(e model.Envelope) error {
	if e.MessageID == "" || e.AgentID == "" || e.SessionID == "" {
		return errors.New("envelope message, agent and session IDs are required")
	}
	if err := validateTime(e.SentAt, "sent_at"); err != nil {
		return err
	}
	payloads := 0
	for _, present := range []bool{e.Hello != nil, e.Heartbeat != nil, e.Telemetry != nil, e.Inventory != nil, len(e.Events) > 0, e.CommandResult != nil, e.Error != nil} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return errors.New("envelope requires exactly one payload")
	}
	switch {
	case e.Hello != nil:
		if e.Hello.AgentID != e.AgentID || e.Hello.SessionID != e.SessionID {
			return errors.New("hello identity does not match envelope")
		}
		if e.Hello.ProtocolVersion == 0 {
			return errors.New("hello protocol version is required")
		}
		if err := validateTime(e.Hello.StartedAt, "hello.started_at"); err != nil {
			return err
		}
		return validateDevice(e.Hello.Device)
	case e.Heartbeat != nil:
		h := e.Heartbeat
		if err := validateTime(h.Timestamp, "heartbeat.timestamp"); err != nil {
			return err
		}
		if h.RemoteDeviceCount < 0 || uint64(h.RemoteDeviceCount) > math.MaxUint32 || h.Queue.Messages < 0 || h.Queue.Bytes < 0 {
			return errors.New("heartbeat contains invalid counts")
		}
		return finite(h.UptimeSeconds)
	case e.Telemetry != nil:
		s := e.Telemetry
		if s.DeviceID == "" || s.Source == "" {
			return errors.New("telemetry device ID and source are required")
		}
		if err := validateTime(s.ObservedAt, "telemetry.observed_at"); err != nil {
			return err
		}
		if err := validateTime(s.CollectedAt, "telemetry.collected_at"); err != nil {
			return err
		}
		if err := finite(s.UptimeSeconds); err != nil {
			return err
		}
		if s.CPU != nil {
			c := s.CPU
			if c.LogicalCPUs < 0 || c.LogicalCPUs > math.MaxInt32 || c.PhysicalCores < 0 || c.PhysicalCores > math.MaxInt32 {
				return errors.New("CPU count is invalid")
			}
			if err := finite(c.Load1, c.Load5, c.Load15); err != nil {
				return err
			}
			if c.UsagePercent != nil {
				if err := finite(*c.UsagePercent); err != nil {
					return err
				}
			}
		}
		if s.Memory != nil && s.Memory.PressureSomeAvg10 != nil {
			if err := finite(*s.Memory.PressureSomeAvg10); err != nil {
				return err
			}
		}
		for _, iface := range s.Interfaces {
			if iface.Index < 0 || iface.Index > math.MaxInt32 || iface.MTU < 0 || iface.MTU > math.MaxInt32 {
				return errors.New("interface index or MTU is invalid")
			}
			if iface.Rates != nil {
				r := iface.Rates
				if err := finite(r.RXBytesPerSecond, r.TXBytesPerSecond, r.RXPacketsPerSecond, r.TXPacketsPerSecond, r.RXErrorsPerSecond, r.TXErrorsPerSecond, r.RXDroppedPerSecond, r.TXDroppedPerSecond); err != nil {
					return err
				}
			}
		}
		for _, metric := range s.Metrics {
			if err := finite(metric.Value); err != nil {
				return err
			}
		}
	case e.Inventory != nil:
		if err := validateTime(e.Inventory.ObservedAt, "inventory.observed_at"); err != nil {
			return err
		}
		for _, device := range e.Inventory.Devices {
			if err := validateDevice(device); err != nil {
				return err
			}
		}
	case len(e.Events) > 0:
		for _, event := range e.Events {
			if err := validateTime(event.Timestamp, "event.timestamp"); err != nil {
				return err
			}
		}
	case e.CommandResult != nil:
		r := e.CommandResult
		if r.CommandID == "" {
			return errors.New("command result ID is required")
		}
		if _, ok := commandStatuses[r.Status]; !ok {
			return errors.New("command result status is unsupported")
		}
		if !r.StartedAt.IsZero() {
			if err := validateTime(r.StartedAt, "command_result.started_at"); err != nil {
				return err
			}
		}
		if !r.FinishedAt.IsZero() {
			if err := validateTime(r.FinishedAt, "command_result.finished_at"); err != nil {
				return err
			}
		}
	case e.Error != nil:
		return validateTime(e.Error.Timestamp, "error.timestamp")
	}
	return nil
}

func validateDevice(device model.Device) error {
	if device.ID == "" {
		return errors.New("device ID is required")
	}
	if _, ok := deviceTypes[device.Type]; !ok {
		return errors.New("device type is unsupported")
	}
	if device.Location.RackUnit < 0 || device.Location.RackUnit > math.MaxInt32 {
		return errors.New("rack unit is invalid")
	}
	if !device.LastSeen.IsZero() {
		if err := validateTime(device.LastSeen, "device.last_seen"); err != nil {
			return err
		}
	}
	return finite(device.UptimeSeconds)
}

func timestamp(at time.Time) *timestamppb.Timestamp { return timestamppb.New(at.UTC()) }

func optionalTimestamp(at time.Time) *timestamppb.Timestamp {
	if at.IsZero() {
		return nil
	}
	return timestamp(at)
}

func validateTime(at time.Time, field string) error {
	if at.IsZero() {
		return fmt.Errorf("%s is required", field)
	}
	if err := timestamp(at).CheckValid(); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func requiredTime(at *timestamppb.Timestamp, field string) (time.Time, error) {
	if at == nil {
		return time.Time{}, fmt.Errorf("%s is required", field)
	}
	if err := at.CheckValid(); err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", field, err)
	}
	value := at.AsTime().UTC()
	if value.IsZero() {
		return time.Time{}, fmt.Errorf("%s is required", field)
	}
	return value, nil
}

func optionalTime(at *timestamppb.Timestamp, field string) (time.Time, error) {
	if at == nil {
		return time.Time{}, nil
	}
	return requiredTime(at, field)
}

func finite(values ...float64) error {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("metric is not finite")
		}
	}
	return nil
}

func reverseLookup[K comparable](values map[string]K, value K) (string, bool) {
	for text, item := range values {
		if item == value {
			return text, true
		}
	}
	return "", false
}

func buildToProto(build model.BuildInfo) *pb.BuildInfo {
	return &pb.BuildInfo{Version: build.Version, Commit: build.Commit, BuildTime: build.BuildTime}
}

func buildFromProto(build *pb.BuildInfo) model.BuildInfo {
	if build == nil {
		return model.BuildInfo{}
	}
	return model.BuildInfo{Version: build.Version, Commit: build.Commit, BuildTime: build.BuildTime}
}

func deviceToProto(d model.Device) *pb.Device {
	location := d.Location
	result := &pb.Device{
		Id: d.ID, Name: d.Name, Hostname: d.Hostname, ManagementAddress: d.ManagementAddress,
		Vendor: d.Vendor, Model: d.Model, Type: deviceTypes[d.Type], SerialNumber: d.SerialNumber,
		HardwareRevision: d.HardwareRevision, SoftwareVersion: d.SoftwareVersion, FirmwareVersion: d.FirmwareVersion,
		Os: d.OS, Distribution: d.Distribution, Kernel: d.Kernel, Architecture: d.Architecture,
		ChassisId: d.ChassisID, Driver: d.Driver, UptimeSeconds: d.UptimeSeconds, BootId: d.BootID,
		Online: d.Online, LastSeen: optionalTimestamp(d.LastSeen),
		Location: &pb.PhysicalLocation{Site: location.Site, Datacenter: location.Datacenter, Building: location.Building,
			Room: location.Room, Rack: location.Rack, RackUnit: int32(location.RackUnit), Position: location.Position, Comment: location.Comment},
	}
	for _, capability := range d.Capabilities {
		result.Capabilities = append(result.Capabilities, &pb.Capability{Name: capability.Name, Supported: capability.Supported, Reason: capability.Reason})
	}
	return result
}

func deviceFromProto(d *pb.Device) (model.Device, error) {
	if d == nil {
		return model.Device{}, errors.New("device is nil")
	}
	typeName, ok := reverseLookup(deviceTypes, d.Type)
	if !ok {
		return model.Device{}, errors.New("device type is unknown")
	}
	lastSeen, err := optionalTime(d.LastSeen, "device.last_seen")
	if err != nil {
		return model.Device{}, err
	}
	result := model.Device{
		ID: d.Id, Name: d.Name, Hostname: d.Hostname, ManagementAddress: d.ManagementAddress,
		Vendor: d.Vendor, Model: d.Model, Type: typeName, SerialNumber: d.SerialNumber,
		HardwareRevision: d.HardwareRevision, SoftwareVersion: d.SoftwareVersion, FirmwareVersion: d.FirmwareVersion,
		OS: d.Os, Distribution: d.Distribution, Kernel: d.Kernel, Architecture: d.Architecture,
		ChassisID: d.ChassisId, Driver: d.Driver, UptimeSeconds: d.UptimeSeconds, BootID: d.BootId,
		Online: d.Online, LastSeen: lastSeen,
	}
	if location := d.Location; location != nil {
		result.Location = model.PhysicalLocation{Site: location.Site, Datacenter: location.Datacenter, Building: location.Building,
			Room: location.Room, Rack: location.Rack, RackUnit: int(location.RackUnit), Position: location.Position, Comment: location.Comment}
	}
	for _, capability := range d.Capabilities {
		if capability == nil {
			return model.Device{}, errors.New("device capability is nil")
		}
		result.Capabilities = append(result.Capabilities, model.Capability{Name: capability.Name, Supported: capability.Supported, Reason: capability.Reason})
	}
	return result, nil
}

func sampleToProto(s model.TelemetrySample) *pb.TelemetrySample {
	result := &pb.TelemetrySample{
		DeviceId: s.DeviceID, ObservedAt: timestamp(s.ObservedAt), CollectedAt: timestamp(s.CollectedAt),
		Source: s.Source, BootId: s.BootID, UptimeSeconds: s.UptimeSeconds,
	}
	if c := s.CPU; c != nil {
		result.Cpu = &pb.CPUStats{LogicalCpus: int32(c.LogicalCPUs), PhysicalCores: int32(c.PhysicalCores),
			Model: c.Model, UsagePercent: c.UsagePercent, Load1: c.Load1, Load5: c.Load5, Load15: c.Load15}
	}
	if m := s.Memory; m != nil {
		result.Memory = &pb.MemoryStats{TotalBytes: m.TotalBytes, UsedBytes: m.UsedBytes, AvailableBytes: m.AvailableBytes,
			FreeBytes: m.FreeBytes, SwapTotalBytes: m.SwapTotalBytes, SwapUsedBytes: m.SwapUsedBytes, PressureSomeAvg10: m.PressureSomeAvg10}
	}
	for _, iface := range s.Interfaces {
		c := iface.Counters
		item := &pb.Interface{Id: iface.ID, Index: int32(iface.Index), Name: iface.Name, Mac: iface.MAC, Mtu: int32(iface.MTU),
			AdminUp: iface.AdminUp, OperState: iface.OperState, SpeedBitsPerSecond: iface.SpeedBitsPerSecond,
			Addresses: append([]string(nil), iface.Addresses...), Counters: &pb.InterfaceCounters{
				RxBytes: c.RXBytes, TxBytes: c.TXBytes, RxPackets: c.RXPackets, TxPackets: c.TXPackets,
				RxErrors: c.RXErrors, TxErrors: c.TXErrors, RxDropped: c.RXDropped, TxDropped: c.TXDropped,
			}}
		if r := iface.Rates; r != nil {
			item.Rates = &pb.InterfaceRates{RxBytesPerSecond: r.RXBytesPerSecond, TxBytesPerSecond: r.TXBytesPerSecond,
				RxPacketsPerSecond: r.RXPacketsPerSecond, TxPacketsPerSecond: r.TXPacketsPerSecond,
				RxErrorsPerSecond: r.RXErrorsPerSecond, TxErrorsPerSecond: r.TXErrorsPerSecond,
				RxDroppedPerSecond: r.RXDroppedPerSecond, TxDroppedPerSecond: r.TXDroppedPerSecond}
		}
		result.Interfaces = append(result.Interfaces, item)
	}
	for _, m := range s.Metrics {
		result.Metrics = append(result.Metrics, &pb.Metric{Name: m.Name, Value: m.Value, Unit: m.Unit})
	}
	return result
}

func sampleFromProto(s *pb.TelemetrySample) (model.TelemetrySample, error) {
	if s == nil {
		return model.TelemetrySample{}, errors.New("telemetry sample is nil")
	}
	observedAt, err := requiredTime(s.ObservedAt, "telemetry.observed_at")
	if err != nil {
		return model.TelemetrySample{}, err
	}
	collectedAt, err := requiredTime(s.CollectedAt, "telemetry.collected_at")
	if err != nil {
		return model.TelemetrySample{}, err
	}
	result := model.TelemetrySample{DeviceID: s.DeviceId, ObservedAt: observedAt, CollectedAt: collectedAt,
		Source: s.Source, BootID: s.BootId, UptimeSeconds: s.UptimeSeconds}
	if c := s.Cpu; c != nil {
		result.CPU = &model.CPUStats{LogicalCPUs: int(c.LogicalCpus), PhysicalCores: int(c.PhysicalCores),
			Model: c.Model, UsagePercent: c.UsagePercent, Load1: c.Load1, Load5: c.Load5, Load15: c.Load15}
	}
	if m := s.Memory; m != nil {
		result.Memory = &model.MemoryStats{TotalBytes: m.TotalBytes, UsedBytes: m.UsedBytes, AvailableBytes: m.AvailableBytes,
			FreeBytes: m.FreeBytes, SwapTotalBytes: m.SwapTotalBytes, SwapUsedBytes: m.SwapUsedBytes, PressureSomeAvg10: m.PressureSomeAvg10}
	}
	for _, iface := range s.Interfaces {
		if iface == nil || iface.Counters == nil {
			return model.TelemetrySample{}, errors.New("interface or counters are nil")
		}
		c := iface.Counters
		item := model.Interface{ID: iface.Id, Index: int(iface.Index), Name: iface.Name, MAC: iface.Mac, MTU: int(iface.Mtu),
			AdminUp: iface.AdminUp, OperState: iface.OperState, SpeedBitsPerSecond: iface.SpeedBitsPerSecond,
			Addresses: append([]string(nil), iface.Addresses...), Counters: model.Counters{RXBytes: c.RxBytes, TXBytes: c.TxBytes,
				RXPackets: c.RxPackets, TXPackets: c.TxPackets, RXErrors: c.RxErrors, TXErrors: c.TxErrors, RXDropped: c.RxDropped, TXDropped: c.TxDropped}}
		if r := iface.Rates; r != nil {
			item.Rates = &model.Rates{RXBytesPerSecond: r.RxBytesPerSecond, TXBytesPerSecond: r.TxBytesPerSecond,
				RXPacketsPerSecond: r.RxPacketsPerSecond, TXPacketsPerSecond: r.TxPacketsPerSecond,
				RXErrorsPerSecond: r.RxErrorsPerSecond, TXErrorsPerSecond: r.TxErrorsPerSecond,
				RXDroppedPerSecond: r.RxDroppedPerSecond, TXDroppedPerSecond: r.TxDroppedPerSecond}
		}
		result.Interfaces = append(result.Interfaces, item)
	}
	for _, m := range s.Metrics {
		if m == nil {
			return model.TelemetrySample{}, errors.New("metric is nil")
		}
		result.Metrics = append(result.Metrics, model.Metric{Name: m.Name, Value: m.Value, Unit: m.Unit})
	}
	return result, nil
}
