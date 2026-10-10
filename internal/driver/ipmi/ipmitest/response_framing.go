package ipmitest

import (
	"net"
	"sync"

	"github.com/bougou/go-ipmi/pkg/bmc"
	ipmicrypto "github.com/bougou/go-ipmi/pkg/crypto"
	"github.com/bougou/go-ipmi/pkg/protocol"
	"github.com/bougou/go-ipmi/pkg/transport"
	"github.com/bougou/go-ipmi/pkg/types"
)

// The pinned reference BMC hardcodes response requester address 0x20 in
// protocol.BuildIPMIResponse, instead of echoing the LAN request's 0x81 SWID.
// Correct only that reference-fixture framing defect; production validation
// keeps checking addresses, LUNs, checksums and authenticated session packets.
type responseFraming struct {
	transport.PacketConn
	state    *bmc.BMC
	mu       sync.Mutex
	requests map[string]responseIdentity
}
type responseIdentity struct {
	requester, requesterLUN, responderLUN uint8
	k1, k2                                []byte
	integrity                             types.IntegrityAlg
}

func (f *responseFraming) ReadFrom(out []byte) (int, net.Addr, error) {
	n, addr, err := f.PacketConn.ReadFrom(out)
	if err != nil {
		return n, addr, err
	}
	pkt := out[:n]
	var plain []byte
	identity := responseIdentity{}
	if n >= 14 && pkt[4] == 0 {
		plain = pkt[14:]
	} else if id, _, kind, flags, payload, ok := protocol.ParseRMCPPlusHeader(pkt); ok && kind == 0 && flags&0xc0 == 0xc0 {
		if session, e := f.state.Sessions.Get(id); e == nil {
			session.ProcMu.Lock()
			identity.k1 = append([]byte(nil), session.K1...)
			identity.k2 = append([]byte(nil), session.K2...)
			identity.integrity = session.IntegrityAlg
			session.ProcMu.Unlock()
			if len(payload) >= 32 && (len(payload)-16)%16 == 0 {
				plain, _ = ipmicrypto.DecryptAESPayload(payload, identity.k2)
			}
		}
	}
	if len(plain) >= 7 {
		identity.requester = plain[3]
		identity.requesterLUN = plain[4] & 3
		identity.responderLUN = plain[1] & 3
		key := addr.String() + string([]byte{plain[4] >> 2})
		f.mu.Lock()
		if len(f.requests) >= 256 {
			clear(f.requests)
		}
		f.requests[key] = identity
		f.mu.Unlock()
	}
	return n, addr, nil
}
func (f *responseFraming) WriteTo(pkt []byte, addr net.Addr) (int, error) {
	originalLength := len(pkt)
	var plain, payload []byte
	var id, seq uint32
	var kind, flags uint8
	var ok bool
	if len(pkt) >= 14 && pkt[4] == 0 {
		plain = append([]byte(nil), pkt[14:]...)
	} else {
		id, seq, kind, flags, payload, ok = protocol.ParseRMCPPlusHeader(pkt)
		if !ok || kind != 0 || flags&0xc0 != 0xc0 {
			return f.PacketConn.WriteTo(pkt, addr)
		}
	}
	f.mu.Lock()
	// All reference fixtures use one pending exchange per connection. The
	// IPMB request sequence identifies its immutable framing/key snapshot.
	var identity responseIdentity
	if plain != nil && len(plain) >= 8 {
		identity = f.requests[addr.String()+string([]byte{plain[4] >> 2})]
	} else {
		for key, entry := range f.requests {
			if len(key) > 0 && key[:len(key)-1] == addr.String() && len(entry.k2) >= 16 {
				decoded, err := ipmicrypto.DecryptAESPayload(payload, entry.k2)
				if err == nil && len(decoded) >= 8 && key[len(key)-1] == decoded[4]>>2 {
					identity = entry
					plain = decoded
					break
				}
			}
		}
	}
	f.mu.Unlock()
	if len(plain) < 8 || identity.requester == 0 {
		return f.PacketConn.WriteTo(pkt, addr)
	}
	plain[0] = identity.requester
	plain[1] = plain[1]&0xfc | identity.requesterLUN
	plain[4] = plain[4]&0xfc | identity.responderLUN
	plain[2] = sum(plain[:2])
	plain[len(plain)-1] = sum(plain[3 : len(plain)-1])
	if payload == nil {
		pkt = append(append([]byte(nil), pkt[:14]...), plain...)
	} else {
		encrypted, err := ipmicrypto.EncryptAESPayload(plain, identity.k2, payload[:16])
		if err != nil {
			return 0, err
		}
		pkt = protocol.BuildRMCPPlusPacket(kind, flags&0xc0, id, seq, encrypted)
		padding := types.IntegrityPadLen(12, len(encrypted))
		for i := 0; i < padding; i++ {
			pkt = append(pkt, 255)
		}
		pkt = append(pkt, byte(padding), 7)
		mac, err := ipmicrypto.SessionIntegrityAuthCode(identity.integrity, pkt[4:], identity.k1, "")
		if err != nil {
			return 0, err
		}
		pkt = append(pkt, mac...)
	}
	n, err := f.PacketConn.WriteTo(pkt, addr)
	if err == nil && n == len(pkt) {
		return originalLength, nil
	}
	return n, err
}
func (f *responseFraming) LocalAddr() net.Addr {
	return f.PacketConn.(interface{ LocalAddr() net.Addr }).LocalAddr()
}
func (f *responseFraming) Close() error {
	err := f.PacketConn.Close()
	f.mu.Lock()
	for key, entry := range f.requests {
		clear(entry.k1)
		clear(entry.k2)
		delete(f.requests, key)
	}
	f.mu.Unlock()
	return err
}
