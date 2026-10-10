package grpc

import (
	"errors"
	"slices"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
)

func validHardwareStatus(s model.HardwareStatus) error {
	for _, h := range []string{s.Health, s.HealthRollup} {
		if h != "" && h != "ok" && h != "warning" && h != "critical" && h != "unknown" {
			return errors.New("invalid hardware health")
		}
	}
	return observationStringsBound(128, s.State)
}

func validateHardware(h *model.HardwareInventory) error {
	if h == nil {
		return nil
	}
	if h.ResourceType == "" || len(h.Relations) > 1024 || len(h.NetworkAddresses) > 256 {
		return errors.New("invalid hardware inventory bounds")
	}
	if err := observationStringsBound(1024, h.ResourceType, h.HardwareUUID, h.SKU, h.PartNumber, h.BIOSVersion, h.PowerState, h.ChassisType, h.ManagerType, h.AssetTag, h.PhysicalSecurity, h.DeviceLocation, h.MemoryType, h.ErrorCorrection, h.Protocol, h.MediaType); err != nil {
		return err
	}
	for _, a := range h.NetworkAddresses {
		if err := observationStringsBound(1024, a); err != nil {
			return err
		}
	}
	for _, r := range h.Relations {
		if r.Type == "" || len(r.Type) > 128 || r.TargetResourceID == "" {
			return errors.New("invalid hardware relationship")
		}
		if err := (model.SourceRef{Type: model.SourceRedfish, ResourceID: r.TargetResourceID}).Validate(); err != nil {
			return err
		}
	}
	return validHardwareStatus(h.Status)
}

func hardwareStatusToProto(s model.HardwareStatus) *pb.HardwareStatus {
	return &pb.HardwareStatus{Health: s.Health, HealthRollup: s.HealthRollup, State: s.State}
}
func hardwareStatusFromProto(s *pb.HardwareStatus) model.HardwareStatus {
	if s == nil {
		return model.HardwareStatus{Health: "unknown", HealthRollup: "unknown", State: "unknown"}
	}
	return model.HardwareStatus{Health: s.Health, HealthRollup: s.HealthRollup, State: s.State}
}
func hardwareToProto(h *model.HardwareInventory) *pb.HardwareInventory {
	if h == nil {
		return nil
	}
	p := &pb.HardwareInventory{ResourceType: h.ResourceType, HardwareUuid: h.HardwareUUID, Sku: h.SKU, PartNumber: h.PartNumber, BiosVersion: h.BIOSVersion, PowerState: h.PowerState, ChassisType: h.ChassisType, ManagerType: h.ManagerType, AssetTag: h.AssetTag, PhysicalSecurity: h.PhysicalSecurity, DeviceLocation: h.DeviceLocation, MemoryType: h.MemoryType, ErrorCorrection: h.ErrorCorrection, Protocol: h.Protocol, MediaType: h.MediaType, CapacityBytes: h.CapacityBytes, Cores: h.Cores, Threads: h.Threads, NetworkAddresses: slices.Clone(h.NetworkAddresses), Status: hardwareStatusToProto(h.Status)}
	for _, r := range h.Relations {
		p.Relations = append(p.Relations, &pb.ResourceRelationship{Type: r.Type, TargetResourceId: r.TargetResourceID})
	}
	return p
}
func hardwareFromProto(p *pb.HardwareInventory) (*model.HardwareInventory, error) {
	if p == nil {
		return nil, nil
	}
	if len(p.Relations) > 1024 || len(p.NetworkAddresses) > 256 {
		return nil, errors.New("hardware inventory limit exceeded")
	}
	h := &model.HardwareInventory{ResourceType: p.ResourceType, HardwareUUID: p.HardwareUuid, SKU: p.Sku, PartNumber: p.PartNumber, BIOSVersion: p.BiosVersion, PowerState: p.PowerState, ChassisType: p.ChassisType, ManagerType: p.ManagerType, AssetTag: p.AssetTag, PhysicalSecurity: p.PhysicalSecurity, DeviceLocation: p.DeviceLocation, MemoryType: p.MemoryType, ErrorCorrection: p.ErrorCorrection, Protocol: p.Protocol, MediaType: p.MediaType, CapacityBytes: p.CapacityBytes, Cores: p.Cores, Threads: p.Threads, NetworkAddresses: p.NetworkAddresses, Status: hardwareStatusFromProto(p.Status)}
	for _, r := range p.Relations {
		if r == nil {
			return nil, errors.New("nil hardware relationship")
		}
		h.Relations = append(h.Relations, model.ResourceRelationship{Type: r.Type, TargetResourceID: r.TargetResourceId})
	}
	return model.CloneHardware(h), validateHardware(h)
}
func identityToProto(values []model.IdentityEvidence) []*pb.IdentityEvidence {
	var result []*pb.IdentityEvidence
	for _, e := range values {
		result = append(result, identityFactToProto(e))
	}
	return result
}

func identityFactToProto(e model.IdentityEvidence) *pb.IdentityEvidence {
	return &pb.IdentityEvidence{Type: string(e.Type), Value: e.Value, RawValue: slices.Clone(e.RawValue), Subtype: cloneUint32(e.Subtype), Source: sourceToProto(e.Source), Confidence: e.Confidence, SubjectId: e.SubjectID, SubjectKind: e.SubjectKind, OriginalValue: e.OriginalValue, Origin: e.Origin, Time: observationTimeToProto(e.Time), Normalization: e.Normalization}
}

// Legacy peers validate Type against the Phase5 vocabulary, and cannot see the
// Phase6 Normalization marker. Never expose a new type or an unusable diagnostic
// as a legacy identifier. This is a wire partition of one domain evidence set.
func identityToCompatibleProto(values []model.IdentityEvidence) (legacy, extended []*pb.IdentityEvidence) {
	for _, e := range values {
		p := identityFactToProto(e)
		if legacyIdentityType(e.Type) && e.Normalization != "invalid" && e.Normalization != "rejected" {
			legacy = append(legacy, p)
		} else {
			extended = append(extended, p)
		}
	}
	return legacy, extended
}

func legacyIdentityType(kind model.IdentityEvidenceType) bool {
	switch kind {
	case model.IdentitySerial, model.IdentityHardwareUUID, model.IdentityMAC,
		model.IdentityChassisID, model.IdentityLLDPChassisID, model.IdentityManagementIP,
		model.IdentityHostname, model.IdentitySysName, model.IdentitySysObjectID,
		model.IdentityManufacturer, model.IdentityModel:
		return true
	default:
		return false
	}
}

func identityFromProto(groups ...[]*pb.IdentityEvidence) ([]model.IdentityEvidence, error) {
	total := 0
	for _, group := range groups {
		if len(group) > 4096-total {
			return nil, errors.New("identity evidence limit exceeded")
		}
		total += len(group)
	}
	if total == 0 {
		return nil, nil
	}
	result := make([]model.IdentityEvidence, 0, total)
	for _, values := range groups {
		for _, p := range values {
			if p == nil || p.Source == nil {
				return nil, errors.New("missing identity evidence source")
			}
			at, err := observationTimeFromProto(p.Time)
			if err != nil {
				return nil, err
			}
			e := model.IdentityEvidence{Type: model.IdentityEvidenceType(p.Type), Value: p.Value, RawValue: slices.Clone(p.RawValue), Subtype: cloneUint32(p.Subtype), Source: sourceFromProto(p.Source), Confidence: p.Confidence, SubjectID: p.SubjectId, SubjectKind: p.SubjectKind, OriginalValue: p.OriginalValue, Origin: p.Origin, Time: at, Normalization: p.Normalization}
			if err := e.Validate(); err != nil {
				return nil, err
			}
			result = append(result, e)
		}
	}
	return result, validateIdentityEvidence(result)
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func validateIdentityEvidence(values []model.IdentityEvidence) error {
	if len(values) > 4096 {
		return errors.New("identity evidence limit exceeded")
	}
	type subject struct {
		source   model.SourceRef
		id, kind string
	}
	counts := make(map[subject]int)
	for _, e := range values {
		if err := e.Validate(); err != nil {
			return err
		}
		key := subject{e.Source, e.SubjectID, e.SubjectKind}
		counts[key]++
		if counts[key] > 64 {
			return errors.New("identity evidence per-subject limit exceeded")
		}
	}
	return nil
}
func hardwareDeviceToProto(d model.Device, p *pb.Device) {
	p.Hardware = hardwareToProto(d.Hardware)
	p.Observation = observationToProto(d.Observation)
	p.IdentityEvidence, p.ExtendedIdentityEvidence = identityToCompatibleProto(d.IdentityEvidence)
	for i, c := range d.Components {
		p.Components[i].Hardware = hardwareToProto(c.Hardware)
		p.Components[i].Observation = observationToProto(c.Observation)
	}
}
func hardwareDeviceFromProto(p *pb.Device, d *model.Device) error {
	var err error
	d.Hardware, err = hardwareFromProto(p.Hardware)
	if err != nil {
		return err
	}
	d.Observation, err = observationFromProto(p.Observation)
	if err != nil {
		return err
	}
	d.IdentityEvidence, err = identityFromProto(p.IdentityEvidence, p.ExtendedIdentityEvidence)
	if err != nil {
		return err
	}
	for i, c := range p.Components {
		d.Components[i].Hardware, err = hardwareFromProto(c.Hardware)
		if err != nil {
			return err
		}
		d.Components[i].Observation, err = observationFromProto(c.Observation)
		if err != nil {
			return err
		}
	}
	return nil
}
func hardwareSampleToProto(s model.TelemetrySample, p *pb.TelemetrySample) {
	p.Observation = observationToProto(s.Observation)
	p.IdentityEvidence, p.ExtendedIdentityEvidence = identityToCompatibleProto(s.IdentityEvidence)
	for i, m := range s.Metrics {
		p.Metrics[i].ResourceId = m.ResourceID
		p.Metrics[i].ComponentId = m.ComponentID
		p.Metrics[i].Observation = observationToProto(m.Observation)
		if m.Status != nil {
			p.Metrics[i].Status = hardwareStatusToProto(*m.Status)
		}
	}
	for _, h := range s.HardwareHealth {
		p.HardwareHealth = append(p.HardwareHealth, &pb.ComponentHealth{Id: h.ID, ResourceId: h.ResourceID, Name: h.Name, Status: hardwareStatusToProto(h.Status), Observation: observationToProto(h.Observation)})
	}
}
func hardwareSampleFromProto(p *pb.TelemetrySample, s *model.TelemetrySample) error {
	var err error
	s.Observation, err = observationFromProto(p.Observation)
	if err != nil {
		return err
	}
	s.IdentityEvidence, err = identityFromProto(p.IdentityEvidence, p.ExtendedIdentityEvidence)
	if err != nil {
		return err
	}
	if len(p.HardwareHealth) > 4096 {
		return errors.New("hardware health limit exceeded")
	}
	for i, m := range p.Metrics {
		s.Metrics[i].ResourceID = m.ResourceId
		s.Metrics[i].ComponentID = m.ComponentId
		s.Metrics[i].Observation, err = observationFromProto(m.Observation)
		if err != nil {
			return err
		}
		if m.Status != nil {
			v := hardwareStatusFromProto(m.Status)
			s.Metrics[i].Status = &v
		}
	}
	for _, h := range p.HardwareHealth {
		if h == nil {
			return errors.New("nil hardware health")
		}
		o, err := observationFromProto(h.Observation)
		if err != nil {
			return err
		}
		s.HardwareHealth = append(s.HardwareHealth, model.ComponentHealth{ID: h.Id, ResourceID: h.ResourceId, Name: h.Name, Status: hardwareStatusFromProto(h.Status), Observation: o})
	}
	return validateHardwareSample(*s)
}
func validateHardwareSample(s model.TelemetrySample) error {
	if len(s.HardwareHealth) > 4096 || len(s.Metrics) > 16384 {
		return errors.New("hardware sample limit exceeded")
	}
	if err := validateObservation(s.Observation); err != nil {
		return err
	}
	if err := validateIdentityEvidence(s.IdentityEvidence); err != nil {
		return err
	}
	for _, m := range s.Metrics {
		if m.Status != nil {
			if err := validHardwareStatus(*m.Status); err != nil {
				return err
			}
		}
		if err := validateObservation(m.Observation); err != nil {
			return err
		}
		if m.ResourceID != "" {
			if err := (model.SourceRef{Type: model.SourceRedfish, ResourceID: m.ResourceID}).Validate(); err != nil {
				return err
			}
		}
	}
	for _, h := range s.HardwareHealth {
		if h.ID == "" || h.ResourceID == "" {
			return errors.New("missing hardware health identity")
		}
		if err := validHardwareStatus(h.Status); err != nil {
			return err
		}
		if err := validateObservation(h.Observation); err != nil {
			return err
		}
		if err := (model.SourceRef{Type: model.SourceRedfish, ResourceID: h.ResourceID}).Validate(); err != nil {
			return err
		}
	}
	return nil
}
