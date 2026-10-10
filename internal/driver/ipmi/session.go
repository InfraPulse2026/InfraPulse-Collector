package ipmi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"sync"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	ipmicrypto "github.com/bougou/go-ipmi/pkg/crypto"
	"github.com/bougou/go-ipmi/pkg/rmcpplus"
	"github.com/bougou/go-ipmi/pkg/types"
)

const maxDiscardedDatagrams = 16

var errDiscardDatagram = errors.New("unrelated or repeated IPMI response")

// The pinned library owns LANplus framing and authentication. This small gate
// covers checks missing from its client receive path: negotiated algorithms,
// HMAC, session/sequence correlation, AES framing and IPMB checksums. It builds
// no protocol requests and never exposes or formats the transcript or keys.
type sessionGuard struct {
	mu                                                 sync.Mutex
	credential                                         credentials.Credential
	maximum, cipher                                    int
	maxOperations, maxPollBytes, operations, pollBytes int
	cleanupSent, cleanupReceived                       bool
	privilege                                          uint8
	auth                                               types.AuthAlg
	integrity                                          types.IntegrityAlg
	crypt                                              types.CryptAlg
	consoleID, bmcID, inboundSequence                  uint32
	consoleRandom, bmcRandom, guid                     [16]byte
	role                                               uint8
	sik, k1, k2                                        []byte
	expected                                           uint8
	pending                                            requestIdentity
	active                                             bool
	terminal                                           error
	lastError                                          error
}

type requestIdentity struct{ responder, requester, netfn, responderLUN, requesterLUN, sequence, command uint8 }

func newSessionGuard(credential credentials.Credential, opts SessionOptions, privilege uint8) *sessionGuard {
	a, i, c, _ := types.GetCipherSuiteAlgorithms(types.CipherSuiteID(opts.CipherSuite))
	g := &sessionGuard{credential: credential, maximum: opts.MaxResponseBytes, cipher: opts.CipherSuite, maxOperations: opts.MaxOperations, maxPollBytes: opts.MaxPollBytes, privilege: privilege, auth: a, integrity: i, crypt: c}
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		g.terminal = ErrSecurity
	}
	g.consoleID = binary.LittleEndian.Uint32(id[:])
	if g.consoleID == 0 {
		g.consoleID = 1
	}
	return g
}
func (g *sessionGuard) failure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.terminal != nil {
		return g.terminal
	}
	return g.lastError
}
func (g *sessionGuard) fail(err error) error {
	if g.terminal == nil {
		g.terminal = err
	}
	return err
}
func (g *sessionGuard) erase() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.sik)
	clear(g.k1)
	clear(g.k2)
	clear(g.consoleRandom[:])
	clear(g.bmcRandom[:])
	clear(g.guid[:])
	g.credential = credentials.Credential{}
	g.active = false
}

type guardDialer struct {
	guard *sessionGuard
	dial  func(context.Context, string, string) (net.Conn, error)
}

func (d *guardDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d *guardDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "udp" {
		return nil, ErrSecurity
	}
	c, err := d.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &guardConn{Conn: c, guard: d.guard}, nil
}

type guardConn struct {
	net.Conn
	guard *sessionGuard
}

func (c *guardConn) Write(packet []byte) (int, error) {
	checked, err := c.guard.outbound(packet)
	if err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(checked)
	return n, err
}
func (c *guardConn) Read(out []byte) (int, error) {
	buf := make([]byte, c.guard.maximum+1)
	for discarded := 0; discarded <= maxDiscardedDatagrams; discarded++ {
		n, err := c.Conn.Read(buf)
		if err != nil {
			return 0, err
		}
		if n > c.guard.maximum {
			c.guard.mu.Lock()
			err = c.guard.fail(driver.ErrLimit)
			c.guard.mu.Unlock()
			return 0, err
		}
		err = c.guard.inbound(buf[:n])
		if errors.Is(err, errDiscardDatagram) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n > len(out) {
			return 0, driver.ErrLimit
		}
		copy(out, buf[:n])
		return n, nil
	}
	c.guard.mu.Lock()
	err := c.guard.fail(driver.ErrLimit)
	c.guard.mu.Unlock()
	return 0, err
}

func rmcpHeader(packet []byte) bool {
	return len(packet) >= 4 && bytes.Equal(packet[:4], []byte{6, 0, 255, 7})
}
func parseSession(packet []byte) (id, seq uint32, kind, flags uint8, payload []byte, err error) {
	if !rmcpHeader(packet) || len(packet) < 16 || packet[4] != 6 {
		return 0, 0, 0, 0, nil, ErrSecurity
	}
	flags = packet[5] & 0xc0
	kind = packet[5] & 0x3f
	if kind != 0 && kind != 0x10 && kind != 0x11 && kind != 0x12 && kind != 0x13 && kind != 0x14 && kind != 0x15 {
		return 0, 0, 0, 0, nil, ErrSecurity
	}
	id = binary.LittleEndian.Uint32(packet[6:10])
	seq = binary.LittleEndian.Uint32(packet[10:14])
	length := int(binary.LittleEndian.Uint16(packet[14:16]))
	if length > len(packet)-16 {
		return 0, 0, 0, 0, nil, ErrSecurity
	}
	return id, seq, kind, flags, packet[16 : 16+length], nil
}
func checksum(data []byte) bool {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return sum == 0
}
func parseRequest(plain []byte) (requestIdentity, error) {
	if len(plain) < 7 || !checksum(plain[:3]) || !checksum(plain[3:]) {
		return requestIdentity{}, ErrSecurity
	}
	return requestIdentity{responder: plain[0], requester: plain[3], netfn: plain[1] >> 2, responderLUN: plain[1] & 3, requesterLUN: plain[4] & 3, sequence: plain[4] >> 2, command: plain[5]}, nil
}
func checkResponse(plain []byte, pending requestIdentity) error {
	if len(plain) < 8 || !checksum(plain[:3]) || !checksum(plain[3:]) {
		return ErrSecurity
	}
	if plain[0] != pending.requester || plain[3] != pending.responder || plain[1]>>2 != pending.netfn+1 || plain[1]&3 != pending.requesterLUN || plain[4]&3 != pending.responderLUN || plain[4]>>2 != pending.sequence || plain[5] != pending.command {
		return errDiscardDatagram
	}
	return nil
}
func permittedCommand(r requestIdentity) bool {
	if r.responder != 0x20 {
		return false
	}
	switch r.netfn {
	case 0:
		return r.command == 1 // Get Chassis Status
	case 4:
		return r.command == 0x2d // Get Sensor Reading
	case 6:
		return r.command == 1 || r.command == 0x3b || r.command == 0x3c || r.command == 0x3d // Device ID/session privilege/close/info
	case 10:
		return r.command == 0x10 || r.command == 0x11 || r.command == 0x20 || r.command == 0x22 || r.command == 0x23 // FRU + SDR read/reservation
	}
	return false
}

func (g *sessionGuard) outbound(packet []byte) (out []byte, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	defer func() {
		if err != nil {
			return
		}
		cleanup := g.active && g.pending.netfn == 6 && g.pending.command == 0x3c
		if cleanup && !g.cleanupSent {
			g.cleanupSent = true
			return
		}
		if g.operations >= g.maxOperations || len(packet) > g.maxPollBytes-g.pollBytes {
			out = nil
			err = driver.ErrLimit
			g.lastError = err
			return
		}
		g.operations++
		g.pollBytes += len(packet)
	}()
	if g.terminal != nil && !g.active {
		return nil, g.terminal
	}
	if len(packet) > g.maximum {
		return nil, g.fail(driver.ErrLimit)
	}
	if !rmcpHeader(packet) || len(packet) < 5 {
		return nil, g.fail(ErrSecurity)
	}
	if packet[4] == 0 {
		// §13.15 mandates this unauthenticated IPMI 1.5 *framing* preflight.
		// No v1.5 session activation or operational commands are accepted.
		if g.active || g.bmcID != 0 || len(packet) < 14 || binary.LittleEndian.Uint32(packet[5:9]) != 0 || binary.LittleEndian.Uint32(packet[9:13]) != 0 || int(packet[13]) != len(packet)-14 {
			return nil, g.fail(ErrSecurity)
		}
		r, err := parseRequest(packet[14:])
		if err != nil || r.netfn != 6 || r.command != 0x38 {
			return nil, g.fail(ErrSecurity)
		}
		g.pending = r
		g.expected = 0
		return packet, nil
	}
	id, seq, kind, flags, payload, err := parseSession(packet)
	if err != nil {
		return nil, g.fail(err)
	}
	if kind == 0 {
		if !g.active || id != g.bmcID || seq == 0 {
			return nil, g.fail(ErrSecurity)
		}
		plain, err := g.authenticatedPayload(packet, flags, payload)
		if err != nil {
			return nil, g.fail(err)
		}
		r, err := parseRequest(plain)
		if err != nil || !permittedCommand(r) {
			return nil, g.fail(ErrSecurity)
		}
		if g.terminal != nil && (r.netfn != 6 || r.command != 0x3c) {
			return nil, g.terminal
		}
		if r.netfn == 6 && r.command == 0x3b && (len(plain) != 8 || plain[6]&15 != g.privilege) {
			return nil, g.fail(ErrPermission)
		}
		g.pending = r
		g.expected = 0
		return packet, nil
	}
	if g.active || flags != 0 || id != 0 || seq != 0 || len(packet) != 16+len(payload) {
		return nil, g.fail(ErrSecurity)
	}
	switch kind {
	case 0x10:
		if len(payload) != 32 || payload[12] != uint8(g.auth) || payload[20] != uint8(g.integrity) || payload[28] != uint8(g.crypt) {
			return nil, g.fail(ErrCipherSuite)
		}
		out := append([]byte(nil), packet...)
		binary.LittleEndian.PutUint32(out[20:24], g.consoleID)
		g.expected = 0x11
		return out, nil
	case 0x12:
		if len(payload) < 28 || g.bmcID == 0 || binary.LittleEndian.Uint32(payload[4:8]) != g.bmcID || payload[24] != 0x10|g.privilege || int(payload[27]) != len(g.credential.Username) || len(payload) != 28+len(g.credential.Username) || string(payload[28:]) != g.credential.Username {
			return nil, g.fail(ErrSecurity)
		}
		copy(g.consoleRandom[:], payload[8:24])
		g.role = payload[24]
		g.expected = 0x13
		return packet, nil
	case 0x14:
		mac, err := ipmicrypto.RAKP3AuthCode(g.auth, g.bmcRandom[:], g.consoleID, g.role, g.credential.Username, ipmicrypto.PadPassword20([]byte(g.credential.Password)))
		if err != nil || len(payload) != 8+len(mac) || binary.LittleEndian.Uint32(payload[4:8]) != g.bmcID || !hmac.Equal(payload[8:], mac) {
			return nil, g.fail(ErrSecurity)
		}
		g.expected = 0x15
		return packet, nil
	default:
		return nil, g.fail(ErrSecurity)
	}
}

func (g *sessionGuard) inbound(packet []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cleanup := g.cleanupSent && !g.cleanupReceived && g.pending.netfn == 6 && g.pending.command == 0x3c
	if cleanup {
		g.cleanupReceived = true
	} else {
		if len(packet) > g.maxPollBytes-g.pollBytes {
			g.lastError = driver.ErrLimit
			return driver.ErrLimit
		}
		g.pollBytes += len(packet)
	}
	if g.terminal != nil && !cleanup {
		return g.terminal
	}
	if len(packet) > g.maximum {
		return g.fail(driver.ErrLimit)
	}
	if !rmcpHeader(packet) || len(packet) < 5 {
		return g.fail(ErrSecurity)
	}
	if packet[4] == 0 {
		if g.active || g.bmcID != 0 || g.expected != 0 || len(packet) < 14 || binary.LittleEndian.Uint32(packet[5:9]) != 0 || binary.LittleEndian.Uint32(packet[9:13]) != 0 || int(packet[13]) != len(packet)-14 {
			return g.fail(ErrSecurity)
		}
		plain := packet[14:]
		if err := checkResponse(plain, g.pending); err != nil {
			if errors.Is(err, errDiscardDatagram) {
				return err
			}
			return g.fail(err)
		}
		if plain[6] != 0 {
			return g.fail(ErrPermission)
		}
		if len(plain) != 16 || plain[10]&2 == 0 {
			return g.fail(ErrCipherSuite)
		}
		// Nonzero Kg needs a separate secret API which Phase 5 does not expose.
		if plain[9]&32 != 0 {
			return g.fail(ErrAuthentication)
		}
		return nil
	}
	id, seq, kind, flags, payload, err := parseSession(packet)
	if err != nil {
		return g.fail(err)
	}
	if g.active {
		if kind != 0 || flags != 0xc0 || id != g.consoleID || seq == 0 {
			return g.fail(ErrSecurity)
		}
		plain, err := g.authenticatedPayload(packet, flags, payload)
		if err != nil {
			return g.fail(err)
		}
		if seq <= g.inboundSequence {
			return errDiscardDatagram
		}
		g.inboundSequence = seq
		if err := checkResponse(plain, g.pending); err != nil {
			if errors.Is(err, errDiscardDatagram) {
				return err
			}
			return g.fail(err)
		}
		return nil
	}
	if flags != 0 || id != 0 || seq != 0 || len(packet) != 16+len(payload) {
		return g.fail(ErrSecurity)
	}
	if kind != g.expected {
		return errDiscardDatagram
	}
	if len(payload) < 8 || payload[0] != 0 || binary.LittleEndian.Uint32(payload[4:8]) != g.consoleID {
		return g.fail(ErrSecurity)
	}
	if payload[1] != 0 {
		if payload[1] == 9 || payload[1] == 10 {
			return g.fail(ErrPermission)
		}
		if kind == 0x11 {
			return g.fail(ErrCipherSuite)
		}
		return g.fail(ErrAuthentication)
	}
	switch kind {
	case 0x11:
		if len(payload) != 36 {
			return g.fail(ErrSecurity)
		}
		var response rmcpplus.OpenSessionResponse
		if err := response.Unpack(payload); err != nil {
			return g.fail(ErrSecurity)
		}
		if response.AuthAlg != uint8(g.auth) || response.IntegrityAlg != uint8(g.integrity) || response.CryptAlg != uint8(g.crypt) {
			return g.fail(ErrCipherSuite)
		}
		if response.MaximumPrivilegeLevel&15 < g.privilege {
			return g.fail(ErrPermission)
		}
		if response.ManagedSystemSessionID == 0 {
			return g.fail(ErrSecurity)
		}
		g.bmcID = response.ManagedSystemSessionID
	case 0x13:
		if len(payload) != 40+ipmicrypto.RAKP2AuthCodeLen(g.auth) {
			return g.fail(ErrSecurity)
		}
		copy(g.bmcRandom[:], payload[8:24])
		copy(g.guid[:], payload[24:40])
		mac, err := ipmicrypto.RAKP2AuthCode(g.auth, g.consoleID, g.bmcID, g.consoleRandom[:], g.bmcRandom[:], g.guid[:], g.role, g.credential.Username, ipmicrypto.PadPassword20([]byte(g.credential.Password)))
		if err != nil || !hmac.Equal(payload[40:], mac) {
			return g.fail(ErrAuthentication)
		}
		g.sik, g.k1, g.k2, err = ipmicrypto.DeriveSessionKeys(g.auth, g.consoleRandom[:], g.bmcRandom[:], g.role, g.credential.Username, ipmicrypto.PadPassword20([]byte(g.credential.Password)))
		if err != nil {
			return g.fail(ErrSecurity)
		}
	case 0x15:
		mac, err := ipmicrypto.RAKP4ICV(g.auth, g.consoleRandom[:], g.bmcID, g.guid[:], g.sik)
		if err != nil || len(payload) != 8+len(mac) || !hmac.Equal(payload[8:], mac) {
			return g.fail(ErrAuthentication)
		}
		g.active = true
	default:
		return g.fail(ErrSecurity)
	}
	return nil
}

func (g *sessionGuard) authenticatedPayload(packet []byte, flags uint8, payload []byte) ([]byte, error) {
	if flags != 0xc0 || len(payload) < 32 || (len(payload)-16)%16 != 0 || len(g.k2) < 16 {
		return nil, ErrSecurity
	}
	macLength, ok := ipmicrypto.IntegrityAuthCodeLen(g.integrity)
	if !ok || macLength == 0 {
		return nil, ErrSecurity
	}
	padding := types.IntegrityPadLen(12, len(payload))
	end := 16 + len(payload)
	if len(packet) != end+padding+2+macLength {
		return nil, ErrSecurity
	}
	for _, b := range packet[end : end+padding] {
		if b != 255 {
			return nil, ErrSecurity
		}
	}
	if int(packet[end+padding]) != padding || packet[end+padding+1] != 7 {
		return nil, ErrSecurity
	}
	mac, err := ipmicrypto.SessionIntegrityAuthCode(g.integrity, packet[4:len(packet)-macLength], g.k1, "")
	if err != nil || !hmac.Equal(packet[len(packet)-macLength:], mac) {
		return nil, ErrSecurity
	}
	// The source helper doesn't validate CBC alignment and all padding bytes;
	// check both before allowing its client decoder to see this datagram.
	plain, err := ipmicrypto.DecryptAES(payload[16:], g.k2[:16], payload[:16])
	if err != nil || len(plain) == 0 {
		return nil, ErrSecurity
	}
	pad := int(plain[len(plain)-1])
	if pad > 15 || pad >= len(plain) {
		return nil, ErrSecurity
	}
	for i := 0; i < pad; i++ {
		if plain[len(plain)-1-pad+i] != byte(i+1) {
			return nil, ErrSecurity
		}
	}
	return plain[:len(plain)-pad-1], nil
}
