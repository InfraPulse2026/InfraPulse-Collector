package grpc

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
)

const MaxManagedConfigBytes = 1 << 20
const MaxConfigStatusErrorBytes = 512

var configStatuses = map[string]pb.ConfigStatus{
	"":                           pb.ConfigStatus_CONFIG_STATUS_UNSPECIFIED,
	model.ConfigStatusReceived:   pb.ConfigStatus_CONFIG_STATUS_RECEIVED,
	model.ConfigStatusValidating: pb.ConfigStatus_CONFIG_STATUS_VALIDATING,
	model.ConfigStatusApplied:    pb.ConfigStatus_CONFIG_STATUS_APPLIED,
	model.ConfigStatusRejected:   pb.ConfigStatus_CONFIG_STATUS_REJECTED,
	model.ConfigStatusFailed:     pb.ConfigStatus_CONFIG_STATUS_FAILED,
}

// EncodeDesiredConfig checks wire representability, not local authorization or
// equipment availability. Backend and fixtures may send semantically invalid
// snapshots to exercise the Collector's independent policy boundary.
func EncodeDesiredConfig(c model.DesiredCollectorConfig) (*pb.DesiredConfigSnapshot, error) {
	w := &pb.DesiredConfigSnapshot{CollectorId: c.CollectorID, SessionId: c.SessionID, Revision: c.Revision,
		ConfigurationDigest: c.ConfigurationDigest, IssuedAt: optionalTimestamp(c.IssuedAt)}
	if !c.IssuedAt.IsZero() {
		if err := validateTime(c.IssuedAt, "config.issued_at"); err != nil {
			return nil, err
		}
	}
	if len(c.Targets) > 1024 {
		return nil, errors.New("configuration target count exceeds wire limit")
	}
	for _, t := range c.Targets {
		if !int32Fits(t.Location.RackUnit) {
			return nil, errors.New("location rack unit overflows wire integer")
		}
		p := t.Polling
		v := &pb.ManagedTargetSpec{TargetId: t.ID, Name: t.Name, Protocol: t.Protocol, Enabled: t.Enabled,
			Endpoint: t.Endpoint, CredentialRef: t.CredentialRef, VendorHint: t.VendorHint, CaRef: t.CARef,
			Location: managedLocationToProto(t.Location), Polling: &pb.ManagedTargetPolling{
				FastInterval: durationpb.New(p.FastInterval), MediumInterval: durationpb.New(p.MediumInterval),
				InventoryInterval: durationpb.New(p.InventoryInterval), TopologyInterval: durationpb.New(p.TopologyInterval), Timeout: durationpb.New(p.Timeout)}}
		n := 0
		for _, present := range []bool{t.SNMP != nil, t.SIP != nil, t.Redfish != nil, t.IPMI != nil} {
			if present {
				n++
			}
		}
		if n > 1 {
			return nil, errors.New("configuration target has ambiguous protocol options")
		}
		if s := t.SNMP; s != nil {
			if !int32Fits(s.Retries) {
				return nil, errors.New("SNMP retry count overflows wire integer")
			}
			v.Options = &pb.ManagedTargetSpec_Snmp{Snmp: &pb.ManagedSNMPOptions{Version: s.Version, SecurityLevel: s.SecurityLevel,
				AuthProtocol: s.AuthProtocol, PrivacyProtocol: s.PrivacyProtocol, ContextName: s.ContextName, Port: uint32(s.Port), Retries: int32(s.Retries)}}
		}
		if s := t.SIP; s != nil {
			v.Options = &pb.ManagedTargetSpec_Sip{Sip: &pb.ManagedSIPOptions{Port: uint32(s.Port), Transport: s.Transport, FromUser: s.FromUser,
				ToUser: s.ToUser, Domain: s.Domain, ServerName: s.ServerName, FailureThreshold: s.FailureThreshold, LatencyThreshold: durationpb.New(s.LatencyThreshold)}}
		}
		if r := t.Redfish; r != nil {
			if !int32Fits(r.MaxResources, r.MaxPages, r.MaxRequests) {
				return nil, errors.New("Redfish limits overflow wire integer")
			}
			v.Options = &pb.ManagedTargetSpec_Redfish{Redfish: &pb.ManagedRedfishOptions{Auth: r.Auth, RequestTimeout: durationpb.New(r.RequestTimeout),
				MaxResources: int32(r.MaxResources), MaxPages: int32(r.MaxPages), MaxRequests: int32(r.MaxRequests)}}
		}
		if i := t.IPMI; i != nil {
			if !int32Fits(i.Retries, i.MaxOperations, i.MaxSDRRecords, i.MaxSDRBytes, i.MaxFRURecords, i.MaxFRUBytes, i.MaxPollBytes) {
				return nil, errors.New("IPMI limits overflow wire integer")
			}
			port, err := managedUint32Pointer(i.Port)
			if err != nil {
				return nil, err
			}
			cipher, err := managedUint32Pointer(i.CipherSuite)
			if err != nil {
				return nil, err
			}
			v.Options = &pb.ManagedTargetSpec_Ipmi{Ipmi: &pb.ManagedIPMIOptions{Port: port, CipherSuite: cipher, Privilege: i.Privilege, Retries: int32(i.Retries),
				RequestTimeout: durationpb.New(i.RequestTimeout), MaxSessionDuration: durationpb.New(i.MaxSessionDuration), MaxOperations: int32(i.MaxOperations),
				MaxSdrRecords: int32(i.MaxSDRRecords), MaxSdrBytes: int32(i.MaxSDRBytes), MaxFruRecords: int32(i.MaxFRURecords), MaxFruBytes: int32(i.MaxFRUBytes),
				MaxPollBytes: int32(i.MaxPollBytes), SdrCacheTtl: durationpb.New(i.SDRCacheTTL)}}
		}
		w.Targets = append(w.Targets, v)
	}
	if proto.Size(w) > MaxManagedConfigBytes {
		return nil, errors.New("configuration exceeds wire size limit")
	}
	if _, err := proto.Marshal(w); err != nil {
		return nil, errors.New("configuration cannot be serialized")
	}
	return w, nil
}

// DecodeDesiredConfig rejects unknown fields recursively: the v1 payload can
// never silently carry an administrative setting that the Collector ignores.
func DecodeDesiredConfig(w *pb.DesiredConfigSnapshot) (model.DesiredCollectorConfig, error) {
	var c model.DesiredCollectorConfig
	if w == nil {
		return c, errors.New("configuration payload is nil")
	}
	if proto.Size(w) > MaxManagedConfigBytes || len(w.Targets) > 1024 {
		return c, errors.New("configuration exceeds wire resource limits")
	}
	if hasUnknownFields(w.ProtoReflect()) {
		return c, errors.New("unknown managed configuration field")
	}
	if !safeCorrelation(w.CollectorId, 128) || !safeCorrelation(w.SessionId, 128) || w.ConfigurationDigest != "" && !validConfigDigest(w.ConfigurationDigest) {
		return c, errors.New("invalid managed configuration correlation")
	}
	c.CollectorID, c.SessionID, c.Revision, c.ConfigurationDigest = w.CollectorId, w.SessionId, w.Revision, w.ConfigurationDigest
	var err error
	c.IssuedAt, err = optionalTime(w.IssuedAt, "config.issued_at")
	if err != nil {
		return c, err
	}
	for _, t := range w.Targets {
		if t == nil {
			return c, errors.New("nil managed target")
		}
		v := model.ManagedTargetSpec{ID: t.TargetId, Name: t.Name, Protocol: t.Protocol, Enabled: t.Enabled, Endpoint: t.Endpoint,
			CredentialRef: t.CredentialRef, VendorHint: t.VendorHint, CARef: t.CaRef, Location: managedLocationFromProto(t.Location)}
		if p := t.Polling; p != nil {
			values, e := managedDurations(p.FastInterval, p.MediumInterval, p.InventoryInterval, p.TopologyInterval, p.Timeout)
			if e != nil {
				return c, e
			}
			v.Polling = model.TargetPolling{FastInterval: values[0], MediumInterval: values[1], InventoryInterval: values[2], TopologyInterval: values[3], Timeout: values[4]}
		}
		switch o := t.Options.(type) {
		case *pb.ManagedTargetSpec_Snmp:
			if o == nil || o.Snmp == nil || o.Snmp.Port > math.MaxUint16 {
				return c, errors.New("invalid SNMP wire options")
			}
			s := o.Snmp
			v.SNMP = &model.ManagedSNMPOptions{Version: s.Version, SecurityLevel: s.SecurityLevel, AuthProtocol: s.AuthProtocol, PrivacyProtocol: s.PrivacyProtocol,
				ContextName: s.ContextName, Port: uint16(s.Port), Retries: int(s.Retries)}
		case *pb.ManagedTargetSpec_Sip:
			if o == nil || o.Sip == nil || o.Sip.Port > math.MaxUint16 {
				return c, errors.New("invalid SIP wire options")
			}
			s := o.Sip
			d, e := managedDurations(s.LatencyThreshold)
			if e != nil {
				return c, e
			}
			v.SIP = &model.ManagedSIPOptions{Port: uint16(s.Port), Transport: s.Transport, FromUser: s.FromUser, ToUser: s.ToUser, Domain: s.Domain,
				ServerName: s.ServerName, FailureThreshold: s.FailureThreshold, LatencyThreshold: d[0]}
		case *pb.ManagedTargetSpec_Redfish:
			if o == nil || o.Redfish == nil {
				return c, errors.New("invalid Redfish wire options")
			}
			r := o.Redfish
			d, e := managedDurations(r.RequestTimeout)
			if e != nil {
				return c, e
			}
			v.Redfish = &model.ManagedRedfishOptions{Auth: r.Auth, RequestTimeout: d[0], MaxResources: int(r.MaxResources), MaxPages: int(r.MaxPages), MaxRequests: int(r.MaxRequests)}
		case *pb.ManagedTargetSpec_Ipmi:
			if o == nil || o.Ipmi == nil {
				return c, errors.New("invalid IPMI wire options")
			}
			i := o.Ipmi
			port, e := managedNativePointer(i.Port)
			if e != nil {
				return c, e
			}
			cipher, e := managedNativePointer(i.CipherSuite)
			if e != nil {
				return c, e
			}
			d, e := managedDurations(i.RequestTimeout, i.MaxSessionDuration, i.SdrCacheTtl)
			if e != nil {
				return c, e
			}
			v.IPMI = &model.ManagedIPMIOptions{Port: port, CipherSuite: cipher, Privilege: i.Privilege, Retries: int(i.Retries), RequestTimeout: d[0], MaxSessionDuration: d[1],
				SDRCacheTTL: d[2], MaxOperations: int(i.MaxOperations), MaxSDRRecords: int(i.MaxSdrRecords), MaxSDRBytes: int(i.MaxSdrBytes), MaxFRURecords: int(i.MaxFruRecords), MaxFRUBytes: int(i.MaxFruBytes), MaxPollBytes: int(i.MaxPollBytes)}
		case nil:
		default:
			return c, errors.New("unsupported managed options")
		}
		c.Targets = append(c.Targets, v)
	}
	return c, nil
}

func desiredConfigCorrelation(w *pb.DesiredConfigSnapshot) model.DesiredCollectorConfig {
	var c model.DesiredCollectorConfig
	if w != nil {
		if safeCorrelation(w.CollectorId, 128) {
			c.CollectorID = w.CollectorId
		}
		if safeCorrelation(w.SessionId, 128) {
			c.SessionID = w.SessionId
		}
		c.Revision = w.Revision
		if validConfigDigest(w.ConfigurationDigest) {
			c.ConfigurationDigest = w.ConfigurationDigest
		}
	}
	return c
}

func safeCorrelation(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\r\n\x00")
}
func validConfigDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func configResultToProto(r *model.ConfigApplyResult) *pb.ConfigApplyStatus {
	if r == nil {
		return nil
	}
	return &pb.ConfigApplyStatus{CollectorId: r.CollectorID, Revision: r.Revision, ConfigurationDigest: r.ConfigurationDigest,
		Status: configStatuses[r.Status], AppliedAt: optionalTimestamp(r.AppliedAt), ErrorCode: r.ErrorCode, SanitizedErrorMessage: r.ErrorMessage,
		AppliedRevision: r.AppliedRevision, AppliedDigest: r.AppliedDigest}
}
func configResultFromProto(w *pb.ConfigApplyStatus) (*model.ConfigApplyResult, error) {
	if w == nil {
		return nil, nil
	}
	s, ok := configStatusFromProto(w.Status)
	if !ok || s == "" {
		return nil, errors.New("invalid config status")
	}
	at, err := optionalTime(w.AppliedAt, "config.applied_at")
	if err != nil {
		return nil, err
	}
	r := &model.ConfigApplyResult{CollectorID: w.CollectorId, Revision: w.Revision, ConfigurationDigest: w.ConfigurationDigest, Status: s,
		AppliedAt: at, ErrorCode: w.ErrorCode, ErrorMessage: w.SanitizedErrorMessage, AppliedRevision: w.AppliedRevision, AppliedDigest: w.AppliedDigest}
	return r, validateConfigResult(*r)
}
func validateConfigResult(r model.ConfigApplyResult) error {
	s, ok := configStatuses[r.Status]
	if !ok || s == pb.ConfigStatus_CONFIG_STATUS_UNSPECIFIED {
		return errors.New("invalid config status")
	}
	if r.CollectorID == "" || !safeCorrelation(r.CollectorID, 128) || !safeCorrelation(r.ErrorCode, 128) || !safeCorrelation(r.ErrorMessage, MaxConfigStatusErrorBytes) {
		return errors.New("invalid config status fields")
	}
	for _, d := range []string{r.ConfigurationDigest, r.AppliedDigest} {
		if d != "" && !validConfigDigest(d) {
			return errors.New("invalid config status digest")
		}
	}
	if r.Status == model.ConfigStatusApplied && (r.Revision == 0 || !validConfigDigest(r.ConfigurationDigest) || r.AppliedAt.IsZero()) {
		return errors.New("applied config status lacks state")
	}
	if !r.AppliedAt.IsZero() {
		return validateTime(r.AppliedAt, "config.applied_at")
	}
	return nil
}
func appliedConfigToProto(s *model.AppliedConfigState) *pb.AppliedConfigState {
	if s == nil {
		return nil
	}
	return &pb.AppliedConfigState{Revision: s.Revision, ConfigurationDigest: s.ConfigurationDigest, Status: configStatuses[s.Status], AppliedAt: optionalTimestamp(s.AppliedAt), TargetCount: uint32(s.TargetCount)}
}
func appliedConfigFromProto(w *pb.AppliedConfigState) (*model.AppliedConfigState, error) {
	if w == nil {
		return nil, nil
	}
	s, ok := configStatusFromProto(w.Status)
	if !ok {
		return nil, errors.New("unknown applied config status")
	}
	at, err := optionalTime(w.AppliedAt, "config.applied_at")
	if err != nil {
		return nil, err
	}
	if uint64(w.TargetCount) > uint64(math.MaxInt) {
		return nil, errors.New("applied target count overflows native integer")
	}
	v := &model.AppliedConfigState{Revision: w.Revision, ConfigurationDigest: w.ConfigurationDigest, Status: s, AppliedAt: at, TargetCount: int(w.TargetCount)}
	return v, validateAppliedConfig(v)
}
func validateAppliedConfig(s *model.AppliedConfigState) error {
	if s == nil {
		return nil
	}
	if _, ok := configStatuses[s.Status]; !ok {
		return errors.New("invalid applied config status")
	}
	if s.TargetCount < 0 || s.TargetCount > 1024 || s.ConfigurationDigest != "" && !validConfigDigest(s.ConfigurationDigest) {
		return errors.New("invalid applied config state")
	}
	if s.Revision > 0 && (!validConfigDigest(s.ConfigurationDigest) || s.AppliedAt.IsZero()) {
		return errors.New("applied config state lacks revision metadata")
	}
	if !s.AppliedAt.IsZero() {
		return validateTime(s.AppliedAt, "config.applied_at")
	}
	return nil
}
func configStatusFromProto(s pb.ConfigStatus) (string, bool) {
	for name, value := range configStatuses {
		if value == s {
			return name, true
		}
	}
	return "", false
}
func int32Fits(values ...int) bool {
	for _, v := range values {
		if int64(v) < math.MinInt32 || int64(v) > math.MaxInt32 {
			return false
		}
	}
	return true
}
func managedUint32Pointer(p *int) (*uint32, error) {
	if p == nil {
		return nil, nil
	}
	if *p < 0 || uint64(*p) > math.MaxUint32 {
		return nil, errors.New("IPMI optional value overflows wire integer")
	}
	v := uint32(*p)
	return &v, nil
}
func managedNativePointer(p *uint32) (*int, error) {
	if p == nil {
		return nil, nil
	}
	if uint64(*p) > uint64(math.MaxInt) {
		return nil, errors.New("IPMI optional value overflows native integer")
	}
	v := int(*p)
	return &v, nil
}
func managedDurations(values ...*durationpb.Duration) ([]time.Duration, error) {
	out := make([]time.Duration, len(values))
	for i, v := range values {
		if v == nil {
			continue
		}
		if v.CheckValid() != nil {
			return nil, errors.New("invalid configuration duration")
		}
		if v.Seconds > int64(math.MaxInt64)/int64(time.Second) || v.Seconds < int64(math.MinInt64)/int64(time.Second) {
			return nil, errors.New("configuration duration overflow")
		}
		n := v.Seconds * int64(time.Second)
		if v.Nanos > 0 && n > math.MaxInt64-int64(v.Nanos) || v.Nanos < 0 && n < math.MinInt64-int64(v.Nanos) {
			return nil, errors.New("configuration duration overflow")
		}
		out[i] = time.Duration(n + int64(v.Nanos))
	}
	return out, nil
}
func managedLocationToProto(l model.PhysicalLocation) *pb.PhysicalLocation {
	return &pb.PhysicalLocation{Site: l.Site, Datacenter: l.Datacenter, Building: l.Building, Room: l.Room, Rack: l.Rack, RackUnit: int32(l.RackUnit), Position: l.Position, Comment: l.Comment}
}
func managedLocationFromProto(l *pb.PhysicalLocation) model.PhysicalLocation {
	if l == nil {
		return model.PhysicalLocation{}
	}
	return model.PhysicalLocation{Site: l.Site, Datacenter: l.Datacenter, Building: l.Building, Room: l.Room, Rack: l.Rack, RackUnit: int(l.RackUnit), Position: l.Position, Comment: l.Comment}
}
func hasUnknownFields(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	unknown := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.IsMap() {
			if f.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					unknown = hasUnknownFields(v.Message())
					return !unknown
				})
			}
		}
		if f.IsList() && f.Kind() == protoreflect.MessageKind {
			list := v.List()
			for i := 0; i < list.Len() && !unknown; i++ {
				unknown = hasUnknownFields(list.Get(i).Message())
			}
		}
		if !f.IsList() && !f.IsMap() && f.Kind() == protoreflect.MessageKind {
			unknown = hasUnknownFields(v.Message())
		}
		return !unknown
	})
	return unknown
}
