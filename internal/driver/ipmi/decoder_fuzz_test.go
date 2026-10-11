package ipmi

import (
	"context"
	"encoding/json"
	"testing"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
	"InfraPulseAgent/internal/model"
)

func FuzzIPMIStandardDecoders(f *testing.F) {
	for _, data := range [][]byte{nil, {1, 0, 0, 0, 0, 0, 0, 255}, ipmitest.FRU(0), ipmitest.FRU(1)} {
		f.Add(data)
	}
	for _, record := range ipmitest.SDRs() {
		f.Add(record)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		p := &poll{ctx: context.Background(), a: &Adapter{opts: driver.Options{ID: "decoder-fixture"}, limits: Options{Provenance: model.ProvenanceSimulated}}}
		components, _ := p.parseFRU(0, "fixture-parent", data)
		if len(components) > 3 {
			t.Fatal("FRU exceeded standard area bound")
		}
		if _, err := json.Marshal(components); err != nil {
			t.Fatal("FRU produced unencodable model", err)
		}
		parsed, err := parseSDR(data, 0xffff)
		if err == nil {
			observations := sensors(parsed, data)
			if len(observations) > 15 {
				t.Fatal("compact SDR exceeded sharing bound")
			}
		}
	})
}

func FuzzIPMILANplusPacketGuard(f *testing.F) {
	g := activeGuard(17)
	f.Add(guardPacket(f, g, g.consoleID, 1, guardResponse()), false)
	f.Add([]byte{6, 0, 255, 7, 6}, false)
	f.Add([]byte{}, true)
	f.Fuzz(func(t *testing.T, packet []byte, compatibility bool) {
		if len(packet) > 8192 {
			t.Skip()
		}
		suite := 17
		if compatibility {
			suite = 3
		}
		g := activeGuard(suite)
		if err := g.inbound(packet); err == nil {
			if g.inboundSequence == 0 {
				t.Fatal("operational packet accepted without a sequence")
			}
			if err := g.inbound(packet); err == nil {
				t.Fatal("operational replay accepted")
			}
		}
		g.erase()
		if g.active || len(g.credential.Password) != 0 {
			t.Fatal("session erase retained credentials or active state")
		}
	})
}
