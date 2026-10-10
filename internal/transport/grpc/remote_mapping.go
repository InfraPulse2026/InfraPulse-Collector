package grpc

import (
	"errors"
	"math"
	"slices"
	"unicode"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
)

const maxRemoteEntries = 16384

var snapshotCompleteness = map[string]pb.SnapshotCompleteness{
	"complete": pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_COMPLETE,
	"partial":  pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_PARTIAL,
	"failed":   pb.SnapshotCompleteness_SNAPSHOT_COMPLETENESS_FAILED,
}

var discoveryProtocols = map[string]pb.DiscoveryProtocol{
	"lldp":    pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_LLDP,
	"cdp":     pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_CDP,
	"mndp":    pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_MNDP,
	"unknown": pb.DiscoveryProtocol_DISCOVERY_PROTOCOL_UNKNOWN,
}

func validConfidence(value string) bool {
	return value == "high" || value == "medium" || value == "low" || value == "unresolved"
}

func validateEvidence(items []model.Evidence) error {
	if len(items) > 64 {
		return errors.New("evidence limit exceeded")
	}
	for _, item := range items {
		if item.Source == "" || len(item.Source) > 128 || len(item.Value) > 4096 || !validConfidence(item.Confidence) {
			return errors.New("invalid evidence source, value or confidence")
		}
		if err := observationStringsBound(4096, item.Source, item.Value); err != nil {
			return err
		}
		for _, value := range []string{item.Source, item.Value} {
			for _, ch := range value {
				if unicode.IsControl(ch) {
					return errors.New("evidence contains control characters")
				}
			}
		}
	}
	return nil
}

func evidenceToProto(items []model.Evidence) []*pb.Evidence {
	var result []*pb.Evidence
	for _, item := range items {
		result = append(result, &pb.Evidence{Source: item.Source, Value: item.Value, Confidence: item.Confidence})
	}
	return result
}

func evidenceFromProto(items []*pb.Evidence) ([]model.Evidence, error) {
	if len(items) > 64 {
		return nil, errors.New("evidence limit exceeded")
	}
	var result []model.Evidence
	for _, item := range items {
		if item == nil {
			return nil, errors.New("evidence is nil")
		}
		result = append(result, model.Evidence{Source: item.Source, Value: item.Value, Confidence: item.Confidence})
	}
	return result, validateEvidence(result)
}

func validateTopology(update model.TopologyUpdate) error {
	if err := validateObservation(update.Observation); err != nil {
		return err
	}
	if update.DeviceID == "" || update.Source == "" || len(update.Source) > 128 {
		return errors.New("topology device ID and source are required")
	}
	if _, ok := snapshotCompleteness[update.Completeness]; !ok {
		return errors.New("topology completeness is unsupported")
	}
	if update.TTLSeconds > 30*24*60*60 || len(update.ExpiryAssumption) > 4096 || len(update.Observations) > maxRemoteEntries {
		return errors.New("topology TTL, assumption or observation limit exceeded")
	}
	if err := validateTime(update.ObservedAt, "topology.observed_at"); err != nil {
		return err
	}
	if err := validateTime(update.CollectedAt, "topology.collected_at"); err != nil {
		return err
	}
	for _, item := range update.Observations {
		if err := model.ValidateNeighborEnrichment(item); err != nil {
			return err
		}
		if err := validateObservation(item.Observation); err != nil {
			return err
		}
		if err := validateIdentityEvidence(item.IdentityEvidence); err != nil {
			return err
		}
		if item.LocalDeviceID != update.DeviceID || item.Source == "" || len(item.Source) > 128 {
			return errors.New("neighbor local device or source is invalid")
		}
		if _, ok := discoveryProtocols[item.Protocol]; !ok {
			return errors.New("neighbor discovery protocol is unsupported")
		}
		if !validConfidence(item.Confidence) || item.LocalPortSubtype > 7 || item.RemoteChassisSubtype > 7 || item.RemotePortSubtype > 7 {
			return errors.New("neighbor confidence or identifier subtype is invalid")
		}
		if len(item.LocalPortID) > 1024 || len(item.RemoteChassisID) > 1024 || len(item.RemotePortID) > 1024 || len(item.RemoteSystemName) > 4096 || len(item.RemotePortDescription) > 4096 {
			return errors.New("neighbor identifier or description limit exceeded")
		}
		if err := validateTime(item.ObservedAt, "neighbor.observed_at"); err != nil {
			return err
		}
		if !item.ExpiresAt.IsZero() {
			if err := validateTime(item.ExpiresAt, "neighbor.expires_at"); err != nil {
				return err
			}
			if item.ExpiresAt.Before(item.ObservedAt) {
				return errors.New("neighbor expires before observation")
			}
		}
		if err := validateEvidence(item.Evidence); err != nil {
			return err
		}
	}
	return nil
}

func topologyToProto(update model.TopologyUpdate) *pb.TopologyUpdate {
	result := &pb.TopologyUpdate{DeviceId: update.DeviceID, ObservedAt: timestamp(update.ObservedAt), CollectedAt: timestamp(update.CollectedAt),
		Source: update.Source, Completeness: snapshotCompleteness[update.Completeness], TtlSeconds: update.TTLSeconds, ExpiryAssumption: update.ExpiryAssumption, Observation: observationToProto(update.Observation)}
	for _, item := range update.Observations {
		n := &pb.Neighbor{
			LocalDeviceId: item.LocalDeviceID, LocalInterfaceId: item.LocalInterfaceID, LocalInterfaceName: item.LocalInterfaceName,
			LocalPortNumber: item.LocalPortNumber, LocalPortId: append([]byte(nil), item.LocalPortID...), LocalPortSubtype: item.LocalPortSubtype,
			RemoteChassisId: append([]byte(nil), item.RemoteChassisID...), RemoteChassisSubtype: item.RemoteChassisSubtype,
			RemotePortId: append([]byte(nil), item.RemotePortID...), RemotePortSubtype: item.RemotePortSubtype,
			RemoteSystemName: item.RemoteSystemName, RemotePortDescription: item.RemotePortDescription,
			Protocol: discoveryProtocols[item.Protocol], Source: item.Source, ObservedAt: timestamp(item.ObservedAt),
			ExpiresAt: optionalTimestamp(item.ExpiresAt), Evidence: evidenceToProto(item.Evidence), Confidence: item.Confidence,
			Observation: observationToProto(item.Observation), IdentityEvidence: identityToProto(item.IdentityEvidence),
			ObservationId: item.ObservationID, Kind: item.Kind, LocalInterfaceIndex: cloneUint32(item.LocalInterfaceIndex),
			LocalInterfaceMac: item.LocalInterfaceMAC, LocalInterfaceDescription: item.LocalInterfaceDescription, LocalInterfaceAlias: item.LocalInterfaceAlias,
			RemoteSystemDescription: item.RemoteSystemDescription, RemotePlatform: item.RemotePlatform, RemoteModel: item.RemoteModel, RemoteSoftwareVersion: item.RemoteSoftwareVersion,
			RemoteCapabilitiesSupported: slices.Clone(item.RemoteCapabilitiesSupported), RemoteCapabilitiesEnabled: slices.Clone(item.RemoteCapabilitiesEnabled), AgeSeconds: cloneUint32(item.AgeSeconds),
		}
		for _, a := range item.RemoteManagementAddresses {
			n.RemoteManagementAddresses = append(n.RemoteManagementAddresses, &pb.ManagementAddress{AddressFamily: a.AddressFamily, Address: a.Address, RawAddress: slices.Clone(a.RawAddress), InterfaceSubtype: cloneUint32(a.InterfaceSubtype), InterfaceId: cloneUint32(a.InterfaceID)})
		}
		result.Observations = append(result.Observations, n)
	}
	return result
}

func topologyFromProto(update *pb.TopologyUpdate) (model.TopologyUpdate, error) {
	completeness, ok := reverseLookup(snapshotCompleteness, update.Completeness)
	if !ok {
		return model.TopologyUpdate{}, errors.New("topology completeness is unknown or unspecified")
	}
	observed, err := requiredTime(update.ObservedAt, "topology.observed_at")
	if err != nil {
		return model.TopologyUpdate{}, err
	}
	collected, err := requiredTime(update.CollectedAt, "topology.collected_at")
	if err != nil {
		return model.TopologyUpdate{}, err
	}
	if len(update.Observations) > maxRemoteEntries {
		return model.TopologyUpdate{}, errors.New("topology observation limit exceeded")
	}
	result := model.TopologyUpdate{DeviceID: update.DeviceId, ObservedAt: observed, CollectedAt: collected,
		Source: update.Source, Completeness: completeness, TTLSeconds: update.TtlSeconds, ExpiryAssumption: update.ExpiryAssumption}
	result.Observation, err = observationFromProto(update.Observation)
	if err != nil {
		return result, err
	}
	for _, item := range update.Observations {
		if item == nil {
			return model.TopologyUpdate{}, errors.New("neighbor is nil")
		}
		protocol, ok := reverseLookup(discoveryProtocols, item.Protocol)
		if !ok {
			return model.TopologyUpdate{}, errors.New("neighbor discovery protocol is unknown or unspecified")
		}
		observed, err := requiredTime(item.ObservedAt, "neighbor.observed_at")
		if err != nil {
			return model.TopologyUpdate{}, err
		}
		expires, err := optionalTime(item.ExpiresAt, "neighbor.expires_at")
		if err != nil {
			return model.TopologyUpdate{}, err
		}
		evidence, err := evidenceFromProto(item.Evidence)
		if err != nil {
			return model.TopologyUpdate{}, err
		}
		o, err := observationFromProto(item.Observation)
		if err != nil {
			return result, err
		}
		identity, err := identityFromProto(item.IdentityEvidence)
		if err != nil {
			return result, err
		}
		n := model.Neighbor{
			LocalDeviceID: item.LocalDeviceId, LocalInterfaceID: item.LocalInterfaceId, LocalInterfaceName: item.LocalInterfaceName,
			LocalPortNumber: item.LocalPortNumber, LocalPortID: append([]byte(nil), item.LocalPortId...), LocalPortSubtype: item.LocalPortSubtype,
			RemoteChassisID: append([]byte(nil), item.RemoteChassisId...), RemoteChassisSubtype: item.RemoteChassisSubtype,
			RemotePortID: append([]byte(nil), item.RemotePortId...), RemotePortSubtype: item.RemotePortSubtype,
			RemoteSystemName: item.RemoteSystemName, RemotePortDescription: item.RemotePortDescription,
			Protocol: protocol, Source: item.Source, ObservedAt: observed, ExpiresAt: expires, Evidence: evidence, Confidence: item.Confidence,
			Observation: o, IdentityEvidence: identity, ObservationID: item.ObservationId, Kind: item.Kind, LocalInterfaceIndex: cloneUint32(item.LocalInterfaceIndex),
			LocalInterfaceMAC: item.LocalInterfaceMac, LocalInterfaceDescription: item.LocalInterfaceDescription, LocalInterfaceAlias: item.LocalInterfaceAlias,
			RemoteSystemDescription: item.RemoteSystemDescription, RemotePlatform: item.RemotePlatform, RemoteModel: item.RemoteModel, RemoteSoftwareVersion: item.RemoteSoftwareVersion,
			RemoteCapabilitiesSupported: slices.Clone(item.RemoteCapabilitiesSupported), RemoteCapabilitiesEnabled: slices.Clone(item.RemoteCapabilitiesEnabled), AgeSeconds: cloneUint32(item.AgeSeconds),
		}
		if len(item.RemoteManagementAddresses) > 16 {
			return result, errors.New("neighbor management address limit exceeded")
		}
		for _, a := range item.RemoteManagementAddresses {
			if a == nil {
				return result, errors.New("nil neighbor management address")
			}
			n.RemoteManagementAddresses = append(n.RemoteManagementAddresses, model.ManagementAddress{AddressFamily: a.AddressFamily, Address: a.Address, RawAddress: slices.Clone(a.RawAddress), InterfaceSubtype: cloneUint32(a.InterfaceSubtype), InterfaceID: cloneUint32(a.InterfaceId)})
		}
		result.Observations = append(result.Observations, n)
	}
	return result, validateTopology(result)
}

func availabilityToProto(value *model.CounterAvailability) *pb.CounterAvailability {
	if value == nil {
		return nil
	}
	return &pb.CounterAvailability{RxBytes: value.RXBytes, TxBytes: value.TXBytes, RxPackets: value.RXPackets, TxPackets: value.TXPackets,
		RxErrors: value.RXErrors, TxErrors: value.TXErrors, RxDropped: value.RXDropped, TxDropped: value.TXDropped}
}

func availabilityFromProto(value *pb.CounterAvailability) *model.CounterAvailability {
	if value == nil {
		return nil
	}
	return &model.CounterAvailability{RXBytes: value.RxBytes, TXBytes: value.TxBytes, RXPackets: value.RxPackets, TXPackets: value.TxPackets,
		RXErrors: value.RxErrors, TXErrors: value.TxErrors, RXDropped: value.RxDropped, TXDropped: value.TxDropped}
}

func counterMetadataToProto(value *model.CounterMetadata) *pb.CounterMetadata {
	if value == nil {
		return nil
	}
	w := value.Widths
	return &pb.CounterMetadata{Available: availabilityToProto(&value.Available), DiscontinuityTicks: value.DiscontinuityTicks,
		Widths: &pb.CounterWidths{RxBytes: uint32(w.RXBytes), TxBytes: uint32(w.TXBytes), RxPackets: uint32(w.RXPackets), TxPackets: uint32(w.TXPackets),
			RxErrors: uint32(w.RXErrors), TxErrors: uint32(w.TXErrors), RxDropped: uint32(w.RXDropped), TxDropped: uint32(w.TXDropped)}}
}

func counterMetadataFromProto(value *pb.CounterMetadata) (*model.CounterMetadata, error) {
	if value == nil {
		return nil, nil
	}
	if value.Available == nil || value.Widths == nil {
		return nil, errors.New("counter metadata requires availability and widths")
	}
	w := value.Widths
	for _, width := range []uint32{w.RxBytes, w.TxBytes, w.RxPackets, w.TxPackets, w.RxErrors, w.TxErrors, w.RxDropped, w.TxDropped} {
		if width != 0 && width != 32 && width != 64 {
			return nil, errors.New("counter width must be 0, 32 or 64")
		}
	}
	return &model.CounterMetadata{Available: *availabilityFromProto(value.Available), DiscontinuityTicks: value.DiscontinuityTicks,
		Widths: model.CounterWidths{RXBytes: uint8(w.RxBytes), TXBytes: uint8(w.TxBytes), RXPackets: uint8(w.RxPackets), TXPackets: uint8(w.TxPackets),
			RXErrors: uint8(w.RxErrors), TXErrors: uint8(w.TxErrors), RXDropped: uint8(w.RxDropped), TXDropped: uint8(w.TxDropped)}}, nil
}

func interfaceMetadataToProto(value *model.InterfaceMetadata) *pb.InterfaceMetadata {
	if value == nil {
		return nil
	}
	return &pb.InterfaceMetadata{Index: value.Index, Mtu: value.MTU, AdminStatus: value.AdminStatus}
}

func interfaceMetadataFromProto(value *pb.InterfaceMetadata) *model.InterfaceMetadata {
	if value == nil {
		return nil
	}
	return &model.InterfaceMetadata{Index: value.Index, MTU: value.Mtu, AdminStatus: value.AdminStatus}
}

func availabilityValues(value model.CounterAvailability) [8]bool {
	return [8]bool{value.RXBytes, value.TXBytes, value.RXPackets, value.TXPackets, value.RXErrors, value.TXErrors, value.RXDropped, value.TXDropped}
}

func validateCounterMetadata(iface model.Interface) error {
	m := iface.CounterMetadata
	if m == nil {
		return nil
	}
	a, w, c := availabilityValues(m.Available), m.Widths, iface.Counters
	widths := [8]uint8{w.RXBytes, w.TXBytes, w.RXPackets, w.TXPackets, w.RXErrors, w.TXErrors, w.RXDropped, w.TXDropped}
	values := [8]uint64{c.RXBytes, c.TXBytes, c.RXPackets, c.TXPackets, c.RXErrors, c.TXErrors, c.RXDropped, c.TXDropped}
	var rateAvailable [8]bool
	if iface.Rates != nil {
		rateAvailable = [8]bool{true, true, true, true, true, true, true, true}
		if iface.Rates.Available != nil {
			rateAvailable = availabilityValues(*iface.Rates.Available)
		}
	}
	for i, width := range widths {
		if width != 0 && width != 32 && width != 64 {
			return errors.New("counter width must be 0, 32 or 64")
		}
		if a[i] && width == 32 && values[i] > math.MaxUint32 {
			return errors.New("counter exceeds declared 32-bit width")
		}
		if rateAvailable[i] && (!a[i] || width == 0) {
			return errors.New("rate is available for unavailable counter or unknown width")
		}
	}
	return nil
}

func verificationToProto(value *model.InterfaceVerification) *pb.InterfaceVerification {
	if value == nil {
		return nil
	}
	return &pb.InterfaceVerification{InterfaceId: value.InterfaceID, Name: value.Name, ExpectedAdminUp: value.ExpectedAdminUp,
		ObservedAdminUp: value.ObservedAdminUp, ObservedAt: optionalTimestamp(value.ObservedAt), Verified: value.Verified,
		Changed: value.Changed, Uncertain: value.Uncertain, Evidence: value.Evidence}
}

func verificationFromProto(value *pb.InterfaceVerification) (*model.InterfaceVerification, error) {
	if value == nil {
		return nil, nil
	}
	observedAt, err := optionalTime(value.ObservedAt, "verification.observed_at")
	if err != nil {
		return nil, err
	}
	result := &model.InterfaceVerification{InterfaceID: value.InterfaceId, Name: value.Name, ExpectedAdminUp: value.ExpectedAdminUp,
		ObservedAdminUp: value.ObservedAdminUp, ObservedAt: observedAt, Verified: value.Verified, Changed: value.Changed,
		Uncertain: value.Uncertain, Evidence: value.Evidence}
	return result, validateVerification(result)
}

func validateVerification(value *model.InterfaceVerification) error {
	if value == nil {
		return nil
	}
	if value.InterfaceID == "" || len(value.InterfaceID) > 128 || len(value.Name) > 128 || len(value.Evidence) > 4096 {
		return errors.New("verification resource or evidence is invalid")
	}
	if value.ObservedAdminUp != nil || !value.ObservedAt.IsZero() {
		if err := validateTime(value.ObservedAt, "verification.observed_at"); err != nil {
			return err
		}
	}
	if value.Verified && (value.ObservedAdminUp == nil || *value.ObservedAdminUp != value.ExpectedAdminUp || value.Uncertain) {
		return errors.New("verified interface state lacks matching observation")
	}
	return nil
}
