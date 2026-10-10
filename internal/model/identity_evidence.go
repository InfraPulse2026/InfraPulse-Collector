package model

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
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
	IdentityIP            IdentityEvidenceType = "ip"
	IdentityInterfaceName IdentityEvidenceType = "interface_name"
)

// IdentityEvidence reports observed facts for future Backend resolution. Its
// confidence describes evidence quality, not a guarantee of global uniqueness.
// SubjectID optionally scopes evidence to an interface/component within this
// observation. It is neither a Backend asset ID nor an inferred TargetID.
// RawValue/Subtype preserve binary advertised identifiers without interpreting
// them as UUIDs, serials or MACs. Value is base64 when RawValue is present.
type IdentityEvidence struct {
	Type          IdentityEvidenceType `json:"type"`
	Value         string               `json:"value"`
	RawValue      []byte               `json:"raw_value,omitempty"`
	Subtype       *uint32              `json:"subtype,omitempty"`
	Source        SourceRef            `json:"source"`
	Confidence    string               `json:"confidence"`
	SubjectID     string               `json:"subject_id,omitempty"`
	SubjectKind   string               `json:"subject_kind,omitempty"`
	OriginalValue string               `json:"original_value,omitempty"`
	Origin        string               `json:"origin,omitempty"`
	Time          *ObservationTime     `json:"time,omitempty"`
	Normalization string               `json:"normalization,omitempty"`
}

func (e IdentityEvidence) Validate() error {
	switch e.Type {
	case IdentitySerial, IdentityHardwareUUID, IdentityMAC, IdentityChassisID,
		IdentityLLDPChassisID, IdentityManagementIP, IdentityHostname, IdentitySysName,
		IdentitySysObjectID, IdentityManufacturer, IdentityModel, IdentityIP, IdentityInterfaceName:
	default:
		return errors.New("unknown identity evidence type")
	}
	if !safeIdentifierText(e.Value, 4096) || !boundedText(e.OriginalValue, 4096) || !boundedText(e.Origin, 256) || len(e.RawValue) > 1024 || !opaqueIdentifier(e.SubjectID, 256, true) {
		return errors.New("identity evidence value or subject is invalid")
	}
	switch e.SubjectKind {
	case "", "network_device", "network_interface", "computer_system", "chassis", "manager", "component", "remote_neighbor":
	default:
		return errors.New("unknown identity subject kind")
	}
	if e.Time != nil {
		if err := e.Time.Validate(); err != nil {
			return err
		}
	}
	switch e.Normalization {
	case "", "invalid", "rejected":
	case "canonical":
		fact, err := NewIdentityFact(e.Type, e.Value, e.Source, e.Confidence, e.SubjectID, e.SubjectKind, e.Origin, nil)
		if err != nil || fact.Value != e.Value {
			return errors.New("noncanonical identity evidence")
		}
	default:
		return errors.New("unknown identity normalization status")
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
		values[i].Time = CloneObservationTime(values[i].Time)
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
	var accumulator identityAccumulator
	type subjectMetadata struct {
		kind string
		at   *ObservationTime
	}
	subjects := make(map[string]subjectMetadata, len(d.Components))
	chassisCount := 0
	var chassisSerial, chassisModel string
	for _, c := range d.Components {
		kind := IdentitySubjectKind(c.Hardware, "component")
		if c.Hardware == nil && c.Class == "chassis" {
			kind = "chassis"
		}
		if c.Class == "chassis" {
			chassisCount++
			chassisSerial, chassisModel = c.SerialNumber, c.Model
		}
		var at *ObservationTime
		if c.Observation != nil {
			at = &c.Observation.Time
		}
		subjects[c.ID] = subjectMetadata{kind, at}
	}
	add := func(kind IdentityEvidenceType, value, confidence, subject string) {
		if value != "" && !accumulator.full {
			fallback := "network_device"
			if ref.Type == SourceLocal {
				fallback = "computer_system"
			}
			subjectKind := IdentitySubjectKind(d.Hardware, fallback)
			var at *ObservationTime
			if d.Observation != nil {
				at = &d.Observation.Time
			}
			if subject != "" {
				subjectKind = "component"
				if s, ok := subjects[subject]; ok {
					subjectKind = s.kind
					if s.at != nil {
						at = s.at
					}
				}
			}
			fact, _ := NewIdentityFact(kind, value, ref, confidence, subject, subjectKind, IdentityOrigin(ref.Type, kind), at)
			accumulator.add(fact)
		}
	}
	// The existing SNMP inventory copies a sole ENTITY-MIB chassis into
	// legacy Device fields. Keep those fields compatible, but report the
	// underlying identity on its observed chassis subject rather than twice.
	chassisProjection := ref.Type == SourceSNMP && chassisCount == 1
	if !chassisProjection || d.SerialNumber != chassisSerial {
		add(IdentitySerial, d.SerialNumber, "high", "")
	}
	if d.Hardware != nil {
		add(IdentityHardwareUUID, d.Hardware.HardwareUUID, "high", "")
	}
	add(IdentityChassisID, d.ChassisID, "medium", "")
	add(IdentityHostname, d.Hostname, "low", "")
	if ref.Type == SourceSNMP {
		add(IdentitySysName, d.Hostname, "low", "")
	}
	add(IdentitySysObjectID, d.SysObjectID, "medium", "")
	add(IdentityManufacturer, d.Vendor, "medium", "")
	if !chassisProjection || d.Model != chassisModel {
		add(IdentityModel, d.Model, "medium", "")
	}
	if ip := managementIP(d.ManagementAddress); ip != "" {
		add(IdentityManagementIP, ip, "low", "")
	}
	for _, component := range d.Components {
		if accumulator.full {
			break
		}
		if component.ID == "" {
			continue
		}
		// Component facts retain their own resource provenance, rather than
		// attributing a drive serial or CPU UUID to the ComputerSystem URI.
		deviceRef := ref
		if component.Observation != nil {
			ref = component.Observation.Source.ForLegacy(d.Source)
		}
		add(IdentitySerial, component.SerialNumber, "high", component.ID)
		add(IdentityManufacturer, component.Manufacturer, "medium", component.ID)
		add(IdentityModel, component.Model, "medium", component.ID)
		if component.Hardware != nil {
			add(IdentityHardwareUUID, component.Hardware.HardwareUUID, "high", component.ID)
		}
		ref = deviceRef
	}
	return accumulator.values
}

func managementIP(address string) string {
	if ip, _, err := NormalizeIP(address); err == nil {
		return ip
	}
	if u, err := url.Parse(address); err == nil && u.Scheme != "" && u.Host != "" {
		if ip, _, err := NormalizeIP(u.Hostname()); err == nil {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(address); err == nil {
		if ip, _, err := NormalizeIP(host); err == nil {
			return ip
		}
	}
	return ""
}

func interfaceIdentityEvidence(interfaces []Interface, ref SourceRef) []IdentityEvidence {
	var accumulator identityAccumulator
	indexes := make([]int, len(interfaces))
	for i := range indexes {
		indexes[i] = i
	}
	sort.SliceStable(indexes, func(i, j int) bool { return interfaces[indexes[i]].ID < interfaces[indexes[j]].ID })
	for _, index := range indexes {
		if accumulator.full {
			break
		}
		iface := interfaces[index]
		if iface.ID == "" {
			continue
		}
		for _, entry := range []struct {
			kind  IdentityEvidenceType
			value string
		}{{IdentityMAC, iface.MAC}, {IdentityInterfaceName, iface.Name}} {
			if entry.value != "" {
				fact, _ := NewIdentityFact(entry.kind, entry.value, ref, "low", iface.ID, "network_interface", IdentityOrigin(ref.Type, entry.kind), nil)
				accumulator.add(fact)
			}
		}
		for _, address := range iface.Addresses {
			if accumulator.full {
				break
			}
			// Interface inventory contains prefixes, not management endpoint claims.
			if host, _, ok := strings.Cut(address, "/"); ok {
				address = host
			}
			fact, _ := NewIdentityFact(IdentityIP, address, ref, "low", iface.ID, "network_interface", IdentityOrigin(ref.Type, IdentityIP), nil)
			accumulator.add(fact)
		}
	}
	return accumulator.values
}

// InterfaceIdentityEvidence reuses an already-read interface snapshot. It never
// refreshes inventory or treats an interface name/MAC as a global hardware ID.
func InterfaceIdentityEvidence(interfaces []Interface, ref SourceRef, at *ObservationTime) []IdentityEvidence {
	values := interfaceIdentityEvidence(interfaces, ref)
	for i := range values {
		values[i].Time = CloneObservationTime(at)
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
			Subtype: &subtype, Source: ref, Confidence: confidence, SubjectKind: "remote_neighbor", Origin: n.Protocol + ".advertised_chassis_id"})
	}
	if n.RemoteSystemName != "" {
		fact, _ := NewIdentityFact(IdentityHostname, n.RemoteSystemName, ref, "low", "", "remote_neighbor", n.Protocol+".system_name", nil)
		values = append(values, fact)
	}
	for _, address := range n.RemoteManagementAddresses {
		if address.Address != "" {
			fact, _ := NewIdentityFact(IdentityManagementIP, address.Address, ref, "low", "", "remote_neighbor", n.Protocol+".management_address", nil)
			values = append(values, fact)
		}
	}
	for i := range values {
		values[i].SubjectID = "neighbor:" + n.ObservationID
		if n.Observation != nil {
			values[i].Time = CloneObservationTime(&n.Observation.Time)
		}
	}
	return values
}

func IdentitySubjectKind(h *HardwareInventory, fallback string) string {
	if h == nil {
		return fallback
	}
	switch h.ResourceType {
	case "ComputerSystem":
		return "computer_system"
	case "Chassis":
		return "chassis"
	case "Manager":
		return "manager"
	case "EthernetInterface", "NetworkInterface":
		return "network_interface"
	default:
		return fallback
	}
}

func IdentityOrigin(source SourceType, kind IdentityEvidenceType) string {
	if source == SourceRedfish {
		switch kind {
		case IdentityHardwareUUID:
			return "redfish.UUID"
		case IdentitySerial:
			return "redfish.SerialNumber"
		case IdentityManufacturer:
			return "redfish.Manufacturer"
		case IdentityModel:
			return "redfish.Model"
		}
	}
	if source == SourceSNMP {
		switch kind {
		case IdentityHostname, IdentitySysName:
			return "snmp.sysName"
		case IdentitySysObjectID:
			return "snmp.sysObjectID"
		case IdentitySerial:
			return "snmp.entPhysicalSerialNum"
		case IdentityModel:
			return "snmp.entPhysicalModelName"
		case IdentityMAC:
			return "snmp.ifPhysAddress"
		case IdentityInterfaceName:
			return "snmp.ifName"
		}
	}
	if source == SourceRouterOS {
		switch kind {
		case IdentityHostname:
			return "routeros.system.identity.name"
		case IdentitySerial:
			return "routeros.system.routerboard.serial-number"
		case IdentityMAC:
			return "routeros.interface.mac-address"
		case IdentityInterfaceName:
			return "routeros.interface.name"
		}
	}
	if kind == IdentityManagementIP {
		return "configured.management_address"
	}
	return string(source) + "." + string(kind)
}

func identityFactKey(e IdentityEvidence) string {
	// Observation clocks describe freshness, not equality of a source fact.
	e.Time = nil
	data, _ := json.Marshal(e)
	return string(data)
}
