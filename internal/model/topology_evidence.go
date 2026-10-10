package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

const TopologyObservedL2Neighbor = "observed_l2_neighbor"

// CloneNeighbor makes metadata and raw evidence independent of collector buffers.
func CloneNeighbor(n Neighbor) Neighbor {
	n.Observation = CloneObservation(n.Observation)
	n.IdentityEvidence = cloneIdentityEvidence(n.IdentityEvidence)
	n.LocalInterfaceIndex = clonePointer(n.LocalInterfaceIndex)
	n.LocalPortID = slices.Clone(n.LocalPortID)
	n.RemoteChassisID = slices.Clone(n.RemoteChassisID)
	n.RemotePortID = slices.Clone(n.RemotePortID)
	n.Evidence = slices.Clone(n.Evidence)
	n.AgeSeconds = clonePointer(n.AgeSeconds)
	n.RemoteCapabilitiesSupported = slices.Clone(n.RemoteCapabilitiesSupported)
	n.RemoteCapabilitiesEnabled = slices.Clone(n.RemoteCapabilitiesEnabled)
	n.RemoteManagementAddresses = slices.Clone(n.RemoteManagementAddresses)
	for i := range n.RemoteManagementAddresses {
		a := &n.RemoteManagementAddresses[i]
		a.RawAddress = slices.Clone(a.RawAddress)
		a.InterfaceSubtype = clonePointer(a.InterfaceSubtype)
		a.InterfaceID = clonePointer(a.InterfaceID)
	}
	return n
}

// EnrichNeighbor records a directed, unresolved L2 observation identity. It does
// not resolve remote assets, deduplicate other sources or infer physical cables.
// Bind SourceRef before calling: different Collector/Target scopes must differ.
func EnrichNeighbor(n Neighbor) Neighbor {
	n = CloneNeighbor(n)
	if n.Kind == "" {
		n.Kind = TopologyObservedL2Neighbor
	}
	slices.Sort(n.RemoteCapabilitiesSupported)
	n.RemoteCapabilitiesSupported = slices.Compact(n.RemoteCapabilitiesSupported)
	slices.Sort(n.RemoteCapabilitiesEnabled)
	n.RemoteCapabilitiesEnabled = slices.Compact(n.RemoteCapabilitiesEnabled)
	sort.Slice(n.RemoteManagementAddresses, func(i, j int) bool {
		a, b := n.RemoteManagementAddresses[i], n.RemoteManagementAddresses[j]
		if a.AddressFamily != b.AddressFamily {
			return a.AddressFamily < b.AddressFamily
		}
		if string(a.RawAddress) != string(b.RawAddress) {
			return strings.Compare(string(a.RawAddress), string(b.RawAddress)) < 0
		}
		optionalKey := func(v *uint32) uint64 {
			if v == nil {
				return 0
			}
			return uint64(*v) + 1
		}
		if optionalKey(a.InterfaceSubtype) != optionalKey(b.InterfaceSubtype) {
			return optionalKey(a.InterfaceSubtype) < optionalKey(b.InterfaceSubtype)
		}
		return optionalKey(a.InterfaceID) < optionalKey(b.InterfaceID)
	})
	previousID := n.ObservationID
	{
		ref := SourceRef{}
		if n.Observation != nil {
			ref = n.Observation.Source
		}
		// ResourceID is excluded: after enrichment it can contain this very hash.
		// TimeMark/remIndex are poll-instance indexes, not stable remote identity.
		key := struct {
			Version                     string
			Collector, Target           string
			Type                        SourceType
			Carrier, Instance, Source   string
			LocalDevice, LocalInterface string
			LocalPort                   uint32
			LocalSubtype                uint32
			LocalRaw                    []byte
			RemoteSubtype               uint32
			RemoteRaw                   []byte
			PortSubtype                 uint32
			PortRaw                     []byte
			Protocol, FallbackName      string
			FallbackAddresses           []string
		}{"infrapulse.topology.observation.v1", ref.CollectorID, ref.TargetID, ref.Type,
			ref.Protocol, ref.InstanceID, n.Source, n.LocalDeviceID, n.LocalInterfaceID,
			n.LocalPortNumber, n.LocalPortSubtype, n.LocalPortID, n.RemoteChassisSubtype,
			n.RemoteChassisID, n.RemotePortSubtype, n.RemotePortID, n.Protocol, "", nil}
		if len(n.RemoteChassisID) == 0 {
			key.FallbackName = n.RemoteSystemName
			for _, address := range n.RemoteManagementAddresses {
				data, _ := json.Marshal(struct {
					Family uint32
					Raw    []byte
				}{address.AddressFamily, address.RawAddress})
				key.FallbackAddresses = append(key.FallbackAddresses, string(data))
			}
		}
		data, _ := json.Marshal(key)
		digest := sha256.Sum256(data)
		n.ObservationID = hex.EncodeToString(digest[:])
	}
	if n.Observation != nil && (n.Observation.Source.ResourceID == "" || previousID != "" && n.Observation.Source.ResourceID == "/topology/"+previousID) {
		n.Observation.Source.ResourceID = "/topology/" + n.ObservationID
	}
	return n
}

func topologyText(s string, max int) bool {
	return utf8.ValidString(s) && boundedText(s, max)
}

func (a ManagementAddress) Validate() error {
	if a.AddressFamily == 0 || len(a.RawAddress) == 0 || len(a.RawAddress) > 31 {
		return errors.New("topology management address family or raw length is invalid")
	}
	if a.InterfaceSubtype != nil && (*a.InterfaceSubtype < 1 || *a.InterfaceSubtype > 3) {
		return errors.New("topology management address interface subtype is invalid")
	}
	switch a.AddressFamily {
	case 1, 2:
		length := 4
		if a.AddressFamily == 2 {
			length = 16
		}
		ip, valid := netip.AddrFromSlice(a.RawAddress)
		if len(a.RawAddress) != length || !valid || ip.String() != a.Address {
			return errors.New("topology management IP does not match its raw address or family")
		}
	default:
		if a.Address != "" {
			return errors.New("unknown topology address family must retain raw bytes without invented IP")
		}
	}
	return nil
}

// ValidateNeighborEnrichment accepts absent additive fields from legacy peers.
func ValidateNeighborEnrichment(n Neighbor) error {
	// Legacy fields keep their historical wire validation limits. Stronger
	// collection-time limits must not reject previously admitted durable records.
	if n.ObservationID != "" {
		if len(n.ObservationID) != sha256.Size*2 {
			return errors.New("topology observation ID must be a SHA256 hexadecimal digest")
		}
		if _, err := hex.DecodeString(n.ObservationID); err != nil {
			return errors.New("topology observation ID is invalid")
		}
	}
	if n.Kind != "" && n.Kind != TopologyObservedL2Neighbor {
		return errors.New("topology observation kind is unsupported")
	}
	if n.LocalInterfaceIndex != nil && *n.LocalInterfaceIndex == 0 {
		return errors.New("topology interface index must be positive when available")
	}
	if n.LocalInterfaceMAC != "" {
		mac, err := NormalizeMAC(n.LocalInterfaceMAC)
		if err != nil || mac != n.LocalInterfaceMAC {
			return errors.New("topology interface MAC must be canonical")
		}
	}
	for _, value := range []string{n.LocalInterfaceAlias, n.RemotePlatform, n.RemoteModel, n.RemoteSoftwareVersion} {
		if !topologyText(value, 255) {
			return errors.New("topology identity text is malformed or oversized")
		}
	}
	for _, value := range []string{n.LocalInterfaceDescription, n.RemoteSystemDescription} {
		if !topologyText(value, 4096) {
			return errors.New("topology description is malformed or oversized")
		}
	}
	if len(n.RemoteManagementAddresses) > 16 {
		return errors.New("topology management address count exceeds limit")
	}
	for _, address := range n.RemoteManagementAddresses {
		if err := address.Validate(); err != nil {
			return err
		}
	}
	for _, values := range [][]string{n.RemoteCapabilitiesSupported, n.RemoteCapabilitiesEnabled} {
		if len(values) > 16 {
			return errors.New("topology capability count exceeds limit")
		}
		for _, value := range values {
			if value == "" || !topologyText(value, 64) {
				return errors.New("topology capability is malformed or oversized")
			}
		}
	}
	if n.AgeSeconds != nil && *n.AgeSeconds > 365*24*60*60 {
		return errors.New("topology neighbor age exceeds limit")
	}
	return nil
}
