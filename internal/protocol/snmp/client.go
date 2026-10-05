// Package snmp provides bounded, read-only SNMP v2c/v3 exchange. It does not
// interpret MIBs or depend on InfraPulse domain models.
package snmp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/credentials"
	"github.com/gosnmp/gosnmp"
)

var (
	ErrConnectivity   = errors.New("SNMP connectivity failure")
	ErrUnavailable    = errors.New("SNMP object or operation unavailable")
	ErrMalformed      = errors.New("malformed SNMP response")
	ErrLimit          = errors.New("SNMP response exceeds limit")
	ErrAuthentication = errors.New("SNMP authentication or privacy failure")
)

type Type uint8

const (
	Integer          Type = Type(gosnmp.Integer)
	OctetString      Type = Type(gosnmp.OctetString)
	ObjectIdentifier Type = Type(gosnmp.ObjectIdentifier)
	Counter32        Type = Type(gosnmp.Counter32)
	Gauge32          Type = Type(gosnmp.Gauge32)
	TimeTicks        Type = Type(gosnmp.TimeTicks)
	Counter64        Type = Type(gosnmp.Counter64)
	NoSuchObject     Type = Type(gosnmp.NoSuchObject)
	NoSuchInstance   Type = Type(gosnmp.NoSuchInstance)
	EndOfMibView     Type = Type(gosnmp.EndOfMibView)
)

type Value struct {
	OID  string
	Type Type
	Data any
}
type Options struct {
	Security                  Security
	Address                   string
	Timeout                   time.Duration
	Retries                   int
	MaxRows, MaxResponseBytes int
	MaxRepetitions            uint32
}
type Client struct {
	opts      Options
	provider  credentials.Provider
	ref, host string
	port      uint16
	mu        sync.Mutex
	closed    bool
	active    map[net.Conn]struct{}
}

func New(opts Options, provider credentials.Provider, credentialRef string) (*Client, error) {
	if provider == nil || credentialRef == "" || opts.Address == "" {
		return nil, errors.New("SNMP requires address and credential reference")
	}
	opts.Security = opts.Security.WithDefaults()
	if err := opts.Security.Validate(); err != nil {
		return nil, err
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	if opts.MaxRows == 0 {
		opts.MaxRows = 32768
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 4 << 20
	}
	if opts.MaxRepetitions == 0 {
		opts.MaxRepetitions = 20
	}
	if opts.Timeout <= 0 || opts.Retries < 0 || opts.Retries > 5 || opts.MaxRows < 1 || opts.MaxRows > 1<<20 || opts.MaxResponseBytes < 256 || opts.MaxResponseBytes > 64<<20 || opts.MaxRepetitions > 100 {
		return nil, errors.New("invalid SNMP limits")
	}
	host, port := opts.Address, uint16(161)
	if h, p, err := net.SplitHostPort(opts.Address); err == nil {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return nil, errors.New("invalid SNMP port")
		}
		host, port = h, uint16(n)
	} else if strings.Contains(opts.Address, ":") && net.ParseIP(strings.Trim(opts.Address, "[]")) == nil {
		return nil, errors.New("invalid SNMP address")
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.ContainsAny(host, "/\\@ \t\r\n") {
		return nil, errors.New("invalid SNMP host")
	}
	return &Client{opts: opts, provider: provider, ref: credentialRef, host: host, port: port, active: make(map[net.Conn]struct{})}, nil
}

func (c *Client) session(ctx context.Context) (*gosnmp.GoSNMP, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	cred, err := c.provider.Get(ctx, c.ref)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, ErrUnavailable
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, nil, ErrUnavailable
	}
	g := &gosnmp.GoSNMP{Target: c.host, Port: c.port, Transport: "udp", Version: gosnmp.Version2c, Context: ctx, Timeout: c.opts.Timeout, Retries: c.opts.Retries, MaxOids: gosnmp.MaxOids, MaxRepetitions: c.opts.MaxRepetitions}
	if c.opts.Security.Version == "v3" {
		if len(cred.Username) == 0 || len(cred.Username) > 32 || len(cred.AuthPassword) < 8 || len(cred.AuthPassword) > 1024 || c.opts.Security.SecurityLevel == "authPriv" && (len(cred.PrivacyPassword) < 8 || len(cred.PrivacyPassword) > 1024) {
			return nil, nil, ErrUnavailable
		}
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.MsgFlags = gosnmp.AuthNoPriv
		privacy := gosnmp.NoPriv
		if c.opts.Security.SecurityLevel == "authPriv" {
			g.MsgFlags = gosnmp.AuthPriv
			privacy = privacyProtocols[c.opts.Security.PrivacyProtocol]
		}
		g.ContextName = c.opts.Security.ContextName
		g.SecurityParameters = &gosnmp.UsmSecurityParameters{UserName: cred.Username, AuthenticationProtocol: authProtocols[c.opts.Security.AuthProtocol], AuthenticationPassphrase: cred.AuthPassword, PrivacyProtocol: privacy, PrivacyPassphrase: cred.PrivacyPassword}
	} else {
		if cred.Community == "" {
			return nil, nil, ErrUnavailable
		}
		g.Community = cred.Community
	}
	// A zero library logger is intentional: GoSNMP packet debug strings contain
	// the community. Only sanitized error categories leave this package.
	if err := g.Connect(); err != nil {
		return nil, nil, safeError(ctx, err)
	}
	conn := g.Conn
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return nil, nil, ErrUnavailable
	}
	c.active[conn] = struct{}{}
	c.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	cleanup := func() { stop(); conn.Close(); c.mu.Lock(); delete(c.active, conn); c.mu.Unlock() }
	return g, cleanup, nil
}

func safeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrLimit) {
		return ErrLimit
	}
	for _, authentication := range []error{gosnmp.ErrUnknownUsername, gosnmp.ErrWrongDigest, gosnmp.ErrDecryption, gosnmp.ErrUnknownSecurityLevel} {
		if errors.Is(err, authentication) {
			return ErrAuthentication
		}
	}
	if strings.Contains(err.Error(), "not authentic") {
		return ErrAuthentication
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || strings.Contains(err.Error(), "timeout") {
		return fmt.Errorf("%w: %w", ErrConnectivity, context.DeadlineExceeded)
	}
	return ErrConnectivity
}

func (c *Client) Get(ctx context.Context, oids []string) ([]Value, error) {
	return c.get(ctx, oids, false)
}
func (c *Client) GetNext(ctx context.Context, oids []string) ([]Value, error) {
	return c.get(ctx, oids, true)
}
func (c *Client) get(ctx context.Context, oids []string, next bool) ([]Value, error) {
	if len(oids) == 0 || len(oids) > gosnmp.MaxOids {
		return nil, ErrLimit
	}
	for _, oid := range oids {
		if !validOID(oid) {
			return nil, ErrMalformed
		}
	}
	g, cleanup, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var packet *gosnmp.SnmpPacket
	if next {
		packet, err = g.GetNext(oids)
	} else {
		packet, err = g.Get(oids)
	}
	if err != nil {
		return nil, safeError(ctx, err)
	}
	if err := c.validateIdentity(packet, g); err != nil {
		return nil, err
	}
	values, err := c.values(packet)
	if err != nil {
		return nil, err
	}
	if !next {
		if len(values) != len(oids) {
			return nil, ErrMalformed
		}
		for i, v := range values {
			if v.OID != canonicalOID(oids[i]) {
				return nil, ErrMalformed
			}
		}
	}
	return values, nil
}

func (c *Client) validateIdentity(packet *gosnmp.SnmpPacket, session *gosnmp.GoSNMP) error {
	if c.opts.Security.Version != "v3" || packet == nil {
		return nil
	}
	actual, ok := packet.SecurityParameters.(*gosnmp.UsmSecurityParameters)
	expected, expectedOK := session.SecurityParameters.(*gosnmp.UsmSecurityParameters)
	if !ok || !expectedOK || actual.UserName != expected.UserName || actual.AuthoritativeEngineID == "" || actual.AuthoritativeEngineID != expected.AuthoritativeEngineID {
		return ErrAuthentication
	}
	if packet.ContextEngineID != session.ContextEngineID {
		return ErrMalformed
	}
	return nil
}

func (c *Client) values(packet *gosnmp.SnmpPacket) ([]Value, error) {
	if packet == nil {
		return nil, ErrMalformed
	}
	if c.opts.Security.Version == "v3" {
		level := gosnmp.AuthNoPriv
		if c.opts.Security.SecurityLevel == "authPriv" {
			level = gosnmp.AuthPriv
		}
		if packet.Version != gosnmp.Version3 || packet.SecurityModel != gosnmp.UserSecurityModel || packet.MsgFlags&gosnmp.AuthPriv != level {
			return nil, ErrAuthentication
		}
		if packet.PDUType != gosnmp.GetResponse || packet.ContextName != c.opts.Security.ContextName {
			return nil, ErrMalformed
		}
	}
	if packet.Error != gosnmp.NoError {
		if packet.Error == gosnmp.TooBig {
			return nil, ErrLimit
		}
		return nil, ErrUnavailable
	}
	if len(packet.Variables) > c.opts.MaxRows {
		return nil, ErrLimit
	}
	values := make([]Value, 0, len(packet.Variables))
	size := 0
	for _, pdu := range packet.Variables {
		if !validOID(pdu.Name) {
			return nil, ErrMalformed
		}
		v := Value{OID: canonicalOID(pdu.Name), Type: Type(pdu.Type), Data: pdu.Value}
		if b, ok := v.Data.([]byte); ok {
			v.Data = append([]byte(nil), b...)
		}
		size += valueSize(v)
		if size > c.opts.MaxResponseBytes {
			return nil, ErrLimit
		}
		values = append(values, v)
	}
	return values, nil
}

// Walk iteration is implemented over the mature library's GetNext/GetBulk.
// The upstream convenience walks turn packet errors into apparent success;
// here errors and limits are retained so a partial table cannot become complete.
func (c *Client) Walk(ctx context.Context, root string) ([]Value, error) {
	return c.walk(ctx, root, false)
}
func (c *Client) BulkWalk(ctx context.Context, root string) ([]Value, error) {
	return c.walk(ctx, root, true)
}
func (c *Client) walk(ctx context.Context, root string, bulk bool) ([]Value, error) {
	if !validOID(root) {
		return nil, ErrMalformed
	}
	root = canonicalOID(root)
	g, cleanup, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var result []Value
	size := 0
	cursor := root
	for requests := 0; requests <= c.opts.MaxRows; requests++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var packet *gosnmp.SnmpPacket
		if bulk {
			packet, err = g.GetBulk([]string{cursor}, 0, c.opts.MaxRepetitions)
		} else {
			packet, err = g.GetNext([]string{cursor})
		}
		if err != nil {
			return result, safeError(ctx, err)
		}
		if e := c.validateIdentity(packet, g); e != nil {
			return result, e
		}
		values, e := c.values(packet)
		if e != nil {
			return result, e
		}
		if len(values) == 0 {
			return result, ErrMalformed
		}
		for _, v := range values {
			if v.Type == NoSuchObject || v.Type == NoSuchInstance {
				return result, ErrUnavailable
			}
			if v.Type == EndOfMibView {
				return result, nil
			}
			if !strings.HasPrefix(v.OID, root+".") {
				return result, nil
			}
			if compareOID(v.OID, cursor) <= 0 {
				return result, ErrMalformed
			}
			size += valueSize(v)
			if len(result) >= c.opts.MaxRows || size > c.opts.MaxResponseBytes {
				return result, ErrLimit
			}
			result = append(result, v)
			cursor = v.OID
		}
	}
	return result, ErrLimit
}
func valueSize(v Value) int {
	n := len(v.OID) + 32
	switch x := v.Data.(type) {
	case []byte:
		n += len(x)
	case string:
		n += len(x)
	}
	return n
}
func canonicalOID(oid string) string { return "." + strings.Trim(oid, ".") }
func validOID(oid string) bool {
	parts := strings.Split(strings.TrimPrefix(oid, "."), ".")
	if len(parts) < 2 || len(parts) > 128 {
		return false
	}
	for _, p := range parts {
		if _, e := strconv.ParseUint(p, 10, 32); e != nil {
			return false
		}
	}
	return true
}
func compareOID(a, b string) int {
	x, y := strings.Split(strings.Trim(a, "."), "."), strings.Split(strings.Trim(b, "."), ".")
	for i := 0; i < len(x) && i < len(y); i++ {
		u, _ := strconv.ParseUint(x[i], 10, 32)
		v, _ := strconv.ParseUint(y[i], 10, 32)
		if u < v {
			return -1
		}
		if u > v {
			return 1
		}
	}
	if len(x) < len(y) {
		return -1
	}
	if len(x) > len(y) {
		return 1
	}
	return 0
}
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for conn := range c.active {
		conn.Close()
	}
	return nil
}
