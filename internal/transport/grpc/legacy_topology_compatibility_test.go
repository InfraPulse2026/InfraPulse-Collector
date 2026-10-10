package grpc

import (
	"strconv"
	"strings"
	"testing"
)

func TestLegacyTopologyNamesRemainWireCompatibleWithoutAdditiveFields(t *testing.T) {
	for _, length := range []int{256, 4096} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			u := remoteTopology()
			u.DeviceID = strings.Repeat("d", 300)
			n := &u.Observations[0]
			n.LocalDeviceID = u.DeviceID
			n.LocalInterfaceID = strings.Repeat("i", 300)
			n.LocalInterfaceName = strings.Repeat("l", length)
			n.RemoteSystemName = strings.Repeat("r", length)
			if u.Observation != nil || n.Observation != nil || n.ObservationID != "" || len(n.IdentityEvidence) != 0 {
				t.Fatal("fixture must represent an old retained envelope")
			}
			e := testEnvelope()
			e.Topology = &u
			roundTripRemote(t, e)
		})
	}
}
