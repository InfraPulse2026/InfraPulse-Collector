package model

import (
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"slices"
)

type IdentityEvidenceType string

const (
	IdentitySerial        IdentityEvidenceType = "serial"
	IdentityHardwareUUID  IdentityEvidenceType = "hardware_uuid"
	IdentityMAC           IdentityEvidenceType = "mac"
	IdentityChassisID     IdentityEvidenceType = "chassis_id"
	IdentityLLDPChassisID IdentityEvidenceType = "lldp_chassis_id"
	IdentityManagementIP  IdentityEvidenceType = "management_ip"
	IdentityHostname      IdentityEvidenceType = "hostname"
	IdentitySysName       IdentityEvidenceType = "sys_name"
	IdentitySysObjectID   IdentityEvidenceType = "sys_object_id"
	IdentityManufacturer  IdentityEvidenceType = "manufacturer"
	IdentityModel         IdentityEvidenceType = "model"
)

// IdentityEvidence reports observed facts for future Backend resolution. Its
// confidence describes evidence quality, not a guarantee of global uniqueness.
// SubjectID optionally scopes evidence to an interface/component within this
// observation. It is neither a Backend asset ID nor an inferred TargetID.
// RawValue/Subtype preserve binary advertised identifiers without interpreting
// them as UUIDs, serials or MACs. Value is base64 when RawValue is present.
type IdentityEvidence struct {
	Type       IdentityEvidenceType `json:"type"`
	Value      string               `json:"value"`
	RawValue   []byte               `json:"raw_value,omitempty"`
	Subtype    *uint32              `json:"subtype,omitempty"`
	Source     SourceRef            `json:"source"`
	Confidence string               `json:"confidence"`
	SubjectID  string               `json:"subject_id,omitempty"`
}

func (e IdentityEvidence) Validate() error {
	switch e.Type {
	case IdentitySerial, IdentityHardwareUUID, IdentityMAC, IdentityChassisID,
		IdentityLLDPChassisID, IdentityManagementIP, IdentityHostname, IdentitySysName,
		IdentitySysObjectID, IdentityManufacturer, IdentityModel:
	default:
		return errors.New("unknown identity evidence type")
	}
	if e.Value == "" || !boundedText(e.Value, 4096) || len(e.RawValue) > 1024 || !opaqueIdentifier(e.SubjectID, 256, true) {
		return errors.New("identity evidence value or subject is invalid")
	}
	if len(e.RawValue) > 0 && e.Value != base64.StdEncoding.EncodeToString(e.RawValue) {
		return errors.New("binary identity evidence value must preserve raw bytes")
	}
	if e.Subtype != nil && (len(e.RawValue) == 0 || *e.Subtype > 7 || e.Type != IdentityChassisID && e.Type != IdentityLLDPChassisID) {
		return errors.New("identity evidence subtype requires an advertised chassis identifier")
	}
	switch e.Confidence {
	case "high", "medium", "low", "unresolved":
	default:
		return errors.New("unknown identity evidence confidence")
	}
	return e.Source.Validate()
}

func cloneIdentityEvidence(values []IdentityEvidence) []IdentityEvidence {
	values = slices.Clone(values)
	for i := range values {
		values[i].RawValue = slices.Clone(values[i].RawValue)
		values[i].Subtype = clonePointer(values[i].Subtype)
	}
	return values
}

func bindIdentityEvidence(values []IdentityEvidence, collectorID, legacySource string) []IdentityEvidence {
	values = cloneIdentityEvidence(values)
	for i := range values {
		if values[i].Source.CollectorID == "" {
			values[i].Source.CollectorID = collectorID
		}
		if legacySource == "simulated" {
			values[i].Source.Provenance = ProvenanceSimulated
		}
	}
	return values
}

// DeviceIdentityEvidence uses only fields already established by a collector.
// Configured VendorHint and presentation Name are deliberately excluded.
func DeviceIdentityEvidence(d Device, ref SourceRef) []IdentityEvidence {
	ref = ref.ForLegacy(d.Source)
	var values []IdentityEvidence
	add := func(kind IdentityEvidenceType, value, confidence, subject string) {
		if value != "" {
			values = append(values, IdentityEvidence{Type: kind, Value: value, Source: ref, Confidence: confidence, SubjectID: subject})
		}
	}
	add(IdentitySerial, d.SerialNumber, "high", "")
	add(IdentityChassisID, d.ChassisID, "medium", "")
	add(IdentityHostname, d.Hostname, "low", "")
	if ref.Type == SourceSNMP {
		add(IdentitySysName, d.Hostname, "low", "")
	}
	add(IdentitySysObjectID, d.SysObjectID, "medium", "")
	add(IdentityManufacturer, d.Vendor, "medium", "")
	add(IdentityModel, d.Model, "medium", "")
	if ip := managementIP(d.ManagementAddress); ip != "" {
		add(IdentityManagementIP, ip, "low", "")
	}
	for _, component := range d.Components {
		if component.ID == "" {
			continue
		}
		add(IdentitySerial, component.SerialNumber, "high", component.ID)
		add(IdentityManufacturer, component.Manufacturer, "medium", component.ID)
		add(IdentityModel, component.Model, "medium", component.ID)
	}
	return values
}

func managementIP(address string) string {
	if ip := net.ParseIP(address); ip != nil {
		return ip.String()
	}
	if u, err := url.Parse(address); err == nil && u.Scheme != "" && u.Host != "" {
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func interfaceIdentityEvidence(interfaces []Interface, ref SourceRef) []IdentityEvidence {
	var values []IdentityEvidence
	for _, iface := range interfaces {
		mac, err := net.ParseMAC(iface.MAC)
		if err == nil && iface.ID != "" {
			values = append(values, IdentityEvidence{Type: IdentityMAC, Value: mac.String(), Source: ref,
				Confidence: "medium", SubjectID: iface.ID})
		}
	}
	return values
}

func neighborIdentityEvidence(n Neighbor, ref SourceRef) []IdentityEvidence {
	var values []IdentityEvidence
	if len(n.RemoteChassisID) > 0 {
		kind, confidence := IdentityChassisID, "low"
		// RouterOS neighbor MAC can have protocol=lldp but unknown subtype;
		// that is not proof that the API returned an LLDP chassis identifier.
		if n.Protocol == "lldp" && n.RemoteChassisSubtype != 0 {
			kind, confidence = IdentityLLDPChassisID, "medium"
		}
		subtype := n.RemoteChassisSubtype
		values = append(values, IdentityEvidence{Type: kind,
			Value: base64.StdEncoding.EncodeToString(n.RemoteChassisID), RawValue: slices.Clone(n.RemoteChassisID),
			Subtype: &subtype, Source: ref, Confidence: confidence})
	}
	if n.RemoteSystemName != "" {
		values = append(values, IdentityEvidence{Type: IdentityHostname, Value: n.RemoteSystemName, Source: ref, Confidence: "low"})
	}
	return values
}
