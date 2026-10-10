package ipmi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	ipmicrypto "github.com/bougou/go-ipmi/pkg/crypto"
	"github.com/bougou/go-ipmi/pkg/protocol"
	"github.com/bougou/go-ipmi/pkg/types"
)

func activeGuard(suite int) *sessionGuard {
	opts := sessionOptions()
	opts.CipherSuite = suite
	g := newSessionGuard(credentials.Credential{Username: "u", Password: "secret"}, opts, 2)
	g.active, g.consoleID, g.bmcID = true, 0x12345678, 0x87654321
	g.k1, g.k2 = bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)
	g.pending = requestIdentity{responder: 0x20, requester: 0x81, netfn: 6, sequence: 9, command: 1}
	return g
}

func packetChecksum(b []byte) byte {
	var sum byte
	for _, v := range b {
		sum += v
	}
	return -sum
}

// Fixed IPMB response layout: requester SWID, response NetFn/LUN, checksum,
// BMC address, request sequence/LUN, Device ID command, completion, checksum.
func guardResponse() []byte {
	plain := []byte{0x81, 7 << 2, 0, 0x20, 9 << 2, 1, 0, 0}
	plain[2], plain[7] = packetChecksum(plain[:2]), packetChecksum(plain[3:7])
	return plain
}

func guardPacket(t testing.TB, g *sessionGuard, id, sequence uint32, plain []byte) []byte {
	t.Helper()
	encrypted, err := ipmicrypto.EncryptAESPayload(plain, g.k2, bytes.Repeat([]byte{0x33}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return guardEncryptedPacket(t, g, id, sequence, encrypted)
}

func guardEncryptedPacket(t testing.TB, g *sessionGuard, id, sequence uint32, encrypted []byte) []byte {
	t.Helper()
	packet := protocol.BuildRMCPPlusPacket(0, 0xc0, id, sequence, encrypted)
	padding := types.IntegrityPadLen(12, len(encrypted))
	packet = append(packet, bytes.Repeat([]byte{255}, padding)...)
	packet = append(packet, byte(padding), 7)
	return guardSign(t, g, packet)
}

func guardSign(t testing.TB, g *sessionGuard, packet []byte) []byte {
	t.Helper()
	mac, err := ipmicrypto.SessionIntegrityAuthCode(g.integrity, packet[4:], g.k1, "")
	if err != nil {
		t.Fatal(err)
	}
	return append(packet, mac...)
}

func TestSessionGuardRejectsUnauthenticatedAndCorruptResponses(t *testing.T) {
	for _, suite := range []int{17, 3} {
		t.Run(strconv.Itoa(suite), func(t *testing.T) {
			for _, scenario := range []string{"integrity", "session", "flags", "zero_sequence", "length", "ipmb_checksum", "aes_padding", "integrity_padding"} {
				t.Run(scenario, func(t *testing.T) {
					g := activeGuard(suite)
					packet := guardPacket(t, g, g.consoleID, 1, guardResponse())
					macLength, _ := ipmicrypto.IntegrityAuthCodeLen(g.integrity)
					switch scenario {
					case "integrity":
						packet[len(packet)-1] ^= 1
					case "session":
						packet = guardPacket(t, g, g.consoleID+1, 1, guardResponse())
					case "flags":
						packet[5] &^= 0x80
					case "zero_sequence":
						packet = guardPacket(t, g, g.consoleID, 0, guardResponse())
					case "length":
						binary.LittleEndian.PutUint16(packet[14:16], 65535)
					case "ipmb_checksum":
						plain := guardResponse()
						plain[2] ^= 1
						packet = guardPacket(t, g, g.consoleID, 1, plain)
					case "aes_padding":
						// Valid HMAC over CBC containing invalid IPMI padding bytes.
						plain := append(guardResponse(), []byte{9, 2, 3, 4, 5, 6, 7, 7}...)
						iv := bytes.Repeat([]byte{0x33}, 16)
						encrypted, err := ipmicrypto.EncryptAES(plain, g.k2[:16], iv)
						if err != nil {
							t.Fatal(err)
						}
						packet = guardEncryptedPacket(t, g, g.consoleID, 1, append(iv, encrypted...))
					case "integrity_padding":
						packet = packet[:len(packet)-macLength]
						packet[len(packet)-3] = 0
						packet = guardSign(t, g, packet)
					}
					if err := g.inbound(packet); !errors.Is(err, ErrSecurity) {
						t.Fatalf("corrupt response accepted: %v", err)
					}
					if !errors.Is(g.failure(), ErrSecurity) {
						t.Fatal("security rejection did not terminate session")
					}
				})
			}
		})
	}
}

func TestSessionGuardReplayAndRequestCorrelation(t *testing.T) {
	g := activeGuard(17)
	packet := guardPacket(t, g, g.consoleID, 1, guardResponse())
	if err := g.inbound(packet); err != nil {
		t.Fatal(err)
	}
	if err := g.inbound(packet); !errors.Is(err, errDiscardDatagram) {
		t.Fatalf("replayed authenticated response accepted: %v", err)
	}
	plain := guardResponse()
	plain[4] = 8 << 2
	plain[7] = packetChecksum(plain[3:7])
	if err := g.inbound(guardPacket(t, g, g.consoleID, 2, plain)); !errors.Is(err, errDiscardDatagram) {
		t.Fatalf("response for another request accepted: %v", err)
	}
	if err := g.inbound(guardPacket(t, g, g.consoleID, 3, guardResponse())); err != nil {
		t.Fatalf("valid response after unrelated datagram rejected: %v", err)
	}
}

func TestSessionGuardReadOnlyGate(t *testing.T) {
	for _, command := range []struct{ netfn, command byte }{{0, 2}, {0, 4}, {4, 0x26}, {10, 0x12}, {10, 0x24}, {10, 0x27}, {6, 0x47}} {
		g := activeGuard(17)
		plain := []byte{0x20, command.netfn << 2, 0, 0x81, 9 << 2, command.command, 0}
		plain[2], plain[6] = packetChecksum(plain[:2]), packetChecksum(plain[3:6])
		if _, err := g.outbound(guardPacket(t, g, g.bmcID, 1, plain)); !errors.Is(err, ErrSecurity) {
			t.Fatalf("mutation %02x/%02x passed wire gate: %v", command.netfn, command.command, err)
		}
	}
}

func TestSessionGuardRAKPAuthenticationAndAlgorithms(t *testing.T) {
	for _, kind := range []byte{0x11, 0x13, 0x15} {
		g := newSessionGuard(credentials.Credential{Username: "u", Password: "secret"}, sessionOptions(), 2)
		g.expected, g.bmcID, g.role = kind, 42, 0x12
		g.sik = bytes.Repeat([]byte{1}, 32)
		length := map[byte]int{0x11: 36, 0x13: 72, 0x15: 24}[kind]
		payload := make([]byte, length)
		binary.LittleEndian.PutUint32(payload[4:8], g.consoleID)
		err := g.inbound(protocol.BuildRMCPPlusPacket(kind, 0, 0, 0, payload))
		if kind == 0x11 && !errors.Is(err, ErrCipherSuite) || kind != 0x11 && !errors.Is(err, ErrAuthentication) {
			t.Fatalf("invalid handshake kind %02x accepted: %v", kind, err)
		}
	}
}

type datagramFixture struct {
	net.Conn
	packets [][]byte
	reads   int
}

func (c *datagramFixture) Read(out []byte) (int, error) {
	if c.reads >= len(c.packets) {
		return 0, io.EOF
	}
	packet := c.packets[c.reads]
	c.reads++
	return copy(out, packet), nil
}

func TestSessionGuardBoundsDiscardAndResponseSize(t *testing.T) {
	g := activeGuard(17)
	packet := guardPacket(t, g, g.consoleID, 1, guardResponse())
	if err := g.inbound(packet); err != nil {
		t.Fatal(err)
	}
	f := &datagramFixture{packets: make([][]byte, maxDiscardedDatagrams+2)}
	for i := range f.packets {
		f.packets[i] = packet
	}
	c := &guardConn{Conn: f, guard: g}
	if _, err := c.Read(make([]byte, 4096)); !errors.Is(err, driver.ErrLimit) || f.reads > maxDiscardedDatagrams+1 {
		t.Fatalf("unbounded replay filtering: reads=%d err=%v", f.reads, err)
	}
	g = activeGuard(17)
	f = &datagramFixture{packets: [][]byte{make([]byte, g.maximum+1)}}
	c = &guardConn{Conn: f, guard: g}
	if _, err := c.Read(make([]byte, 4096)); !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("oversized datagram accepted: %v", err)
	}
}
