package grpc

import (
	"reflect"
	"testing"
	"time"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestP2WirePresenceAndCompatibility(t *testing.T) {
	now := time.Now().UTC()
	zero := uint32(0)
	no := false
	latency := 0.0
	code := uint32(401)
	logs := model.LogBatch{ObservedAt: now, Source: "linux.file", Records: []model.LogRecord{{ID: "r", DeviceID: "d", Source: "linux.audit", SourceID: "audit", ObservedAt: now, OriginalTimestamp: &now, Severity: &zero, Facility: &zero, Message: "raw sanitized", FileIdentity: "device:inode", Offset: 12, Audit: &model.AuditRecord{EventID: "1.2:3", RecordType: "SYSCALL", UID: &zero, Success: &no}}}}
	sip := model.TelephonyUpdate{ObservedAt: now, Source: "sip.options", Targets: []model.TelephonyHealth{{TargetID: "pbx", Address: "127.0.0.1:5060", Transport: "udp", Reachable: true, ResponseCode: &code, LatencyMillis: &latency, ObservedAt: now, State: "degraded", Source: "sip.options", ErrorCategory: "authentication_required"}}}
	d := model.Device{ID: "d", Type: "router", Source: "openwrt", Services: []model.Service{{Name: "dnsmasq", State: "stopped", Enabled: &no, Source: "ubus.procd", ObservedAt: now, Instances: []model.ServiceInstance{{Name: "main", Running: &no, PID: &zero}}}}, Networks: []model.NetworkState{{Name: "wan", Source: "ubus.netifd", ObservedAt: now, Up: &no}}, Firewall: &model.FirewallSnapshot{ObservedAt: now, Source: "uci.firewall", Completeness: "complete", Zones: []model.FirewallZone{{ID: "lan", Name: "lan", Enabled: &no}}, Rules: []model.FirewallRule{{ID: "rule", Enabled: &no}}, Redirects: []model.FirewallRedirect{{FirewallRule: model.FirewallRule{ID: "redirect"}, ExternalPort: "5060"}}}}
	for _, e := range []model.Envelope{{Logs: &logs}, {Telephony: &sip}, {Inventory: &model.InventoryUpdate{ObservedAt: now, Devices: []model.Device{d}}}} {
		e.MessageID, e.AgentID, e.SessionID, e.SentAt = "m", "a", "s", now
		p, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		bytes, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		wire := new(pb.AgentEnvelope)
		if err = proto.Unmarshal(bytes, wire); err != nil {
			t.Fatal(err)
		}
		back, err := DecodeEnvelope(wire)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(e, back) {
			t.Fatalf("presence lost: %#v / %#v", e, back)
		}
	}
	fields := (&pb.AgentEnvelope{}).ProtoReflect().Descriptor().Fields()
	for name, number := range map[string]int{"hello": 10, "telemetry": 12, "inventory": 13, "topology": 14, "logs": 15, "events": 16, "telephony": 17, "command_result": 18, "error": 20} {
		if f := fields.ByName(protoreflect.Name(name)); f == nil || int(f.Number()) != number {
			t.Fatalf("field %s renumbered", name)
		}
	}
}

func TestP2RejectsMalformedWire(t *testing.T) {
	now := time.Now().UTC()
	for _, e := range []model.Envelope{{Logs: &model.LogBatch{ObservedAt: now, Source: "x"}}, {Telephony: &model.TelephonyUpdate{ObservedAt: now, Source: "x", Targets: []model.TelephonyHealth{{TargetID: "x", Address: "x", Transport: "udp", ObservedAt: now, State: "healthy", Source: "x", ResponseCode: ptr(uint32(200))}}}}, {Inventory: &model.InventoryUpdate{ObservedAt: now, Devices: []model.Device{{ID: "x", Type: "router", Services: []model.Service{{Name: "x", State: "running", Source: "x"}}}}}}} {
		e.MessageID, e.AgentID, e.SessionID, e.SentAt = "m", "a", "s", now
		if _, err := Encode(e); err == nil {
			t.Fatal("malformed P2 accepted")
		}
	}
}
func ptr[T any](v T) *T { return &v }
