package snmp

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"github.com/gosnmp/gosnmp"
)

type v3Credential struct{ credentials.Credential }

func (p v3Credential) Get(context.Context, string) (credentials.Credential, error) {
	return p.Credential, nil
}

var fixtureV3Credential = credentials.Credential{Username: "fixture-user", AuthPassword: "private-auth-password", PrivacyPassword: "private-privacy-password"}

// The fixture uses the pinned library's BER/USM encoder, decoder, key
// localization and encryption. No synthetic result bypasses the UDP adapter.
func v3Fixture(t *testing.T, security Security, response func(*gosnmp.SnmpPacket) *gosnmp.SnmpPacket, modify ...func(*gosnmp.SnmpPacket)) (string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	security = security.WithDefaults()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	engine := string([]byte{0x80, 0, 0, 0, 1, 2, 3, 4})
	level, privacy := gosnmp.AuthNoPriv, gosnmp.NoPriv
	if security.SecurityLevel == "authPriv" {
		level, privacy = gosnmp.AuthPriv, privacyProtocols[security.PrivacyProtocol]
	}
	parameters := func() *gosnmp.UsmSecurityParameters {
		p := &gosnmp.UsmSecurityParameters{AuthoritativeEngineID: engine, AuthoritativeEngineBoots: 1, AuthoritativeEngineTime: 10, UserName: fixtureV3Credential.Username, AuthenticationProtocol: authProtocols[security.AuthProtocol], AuthenticationPassphrase: fixtureV3Credential.AuthPassword, PrivacyProtocol: privacy, PrivacyPassphrase: fixtureV3Credential.PrivacyPassword}
		if err := p.InitSecurityKeys(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	decoder := &gosnmp.GoSNMP{Version: gosnmp.Version3, MsgFlags: level, SecurityModel: gosnmp.UserSecurityModel, SecurityParameters: parameters()}
	var discoveries, requests atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 65535)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			request, err := decoder.SnmpDecodePacket(buffer[:n])
			if err != nil {
				continue
			}
			incoming := request.SecurityParameters.(*gosnmp.UsmSecurityParameters)
			var packet *gosnmp.SnmpPacket
			if incoming.AuthoritativeEngineID == "" {
				discoveries.Add(1)
				packet = &gosnmp.SnmpPacket{Version: gosnmp.Version3, SecurityModel: gosnmp.UserSecurityModel, MsgFlags: gosnmp.NoAuthNoPriv, PDUType: gosnmp.Report, MsgID: request.MsgID, RequestID: request.RequestID, SecurityParameters: &gosnmp.UsmSecurityParameters{AuthoritativeEngineID: engine, AuthoritativeEngineBoots: 1, AuthoritativeEngineTime: 10}, Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.6.3.15.1.1.4.0", Type: gosnmp.Counter32, Value: uint32(1)}}}
			} else {
				requests.Add(1)
				if incoming.UserName != fixtureV3Credential.Username {
					continue
				}
				packet = &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: request.Variables[0].Name, Type: gosnmp.OctetString, Value: []byte("USM fixture")}}}
				if response != nil {
					packet = response(request)
				}
				if packet == nil {
					continue
				}
				packet.Version, packet.SecurityModel, packet.MsgFlags, packet.PDUType = gosnmp.Version3, gosnmp.UserSecurityModel, level, gosnmp.GetResponse
				packet.MsgID, packet.RequestID, packet.ContextEngineID, packet.ContextName = request.MsgID, request.RequestID, engine, request.ContextName
				packet.SecurityParameters = parameters()
				for _, apply := range modify {
					apply(packet)
				}
				if err := packet.SecurityParameters.InitPacket(packet); err != nil {
					continue
				}
			}
			wire, err := packet.MarshalMsg()
			if err == nil {
				_, _ = conn.WriteTo(wire, peer)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); <-done })
	return conn.LocalAddr().String(), &discoveries, &requests
}

func newV3Client(t *testing.T, address string, security Security, credential credentials.Credential) *Client {
	t.Helper()
	c, err := New(Options{Address: address, Security: security, Timeout: 250 * time.Millisecond, Retries: 0, MaxRows: 50, MaxResponseBytes: 4096}, v3Credential{credential}, "ref")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestV3USMEngineDiscoveryReconnectAndAlgorithms(t *testing.T) {
	for _, security := range []Security{
		{Version: "v3"}, {Version: "v3", SecurityLevel: "authNoPriv"},
		{Version: "v3", AuthProtocol: "SHA512", PrivacyProtocol: "AES256C"},
		{Version: "v3", AuthProtocol: "SHA", PrivacyProtocol: "DES"},
	} {
		t.Run(security.WithDefaults().SecurityLevel+security.WithDefaults().AuthProtocol+security.WithDefaults().PrivacyProtocol, func(t *testing.T) {
			address, discovery, requests := v3Fixture(t, security, nil)
			c := newV3Client(t, address, security, fixtureV3Credential)
			for range 2 {
				v, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"})
				if err != nil || len(v) != 1 || string(v[0].Data.([]byte)) != "USM fixture" {
					t.Fatalf("get: %v %v", v, err)
				}
			}
			if discovery.Load() != 2 || requests.Load() != 2 {
				t.Fatalf("discovery/reconnect: %d %d", discovery.Load(), requests.Load())
			}
		})
	}
}

func TestV3InvalidCredentialsRedactionAndCancellation(t *testing.T) {
	security := Security{Version: "v3"}
	address, _, _ := v3Fixture(t, security, nil)
	for _, field := range []string{"username", "auth", "privacy"} {
		t.Run(field, func(t *testing.T) {
			credential := fixtureV3Credential
			switch field {
			case "username":
				credential.Username = "invalid-user"
			case "auth":
				credential.AuthPassword = "invalid-auth-secret"
			case "privacy":
				credential.PrivacyPassword = "invalid-privacy-secret"
			}
			c := newV3Client(t, address, security, credential)
			_, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"})
			if err == nil {
				t.Fatal("invalid credential accepted")
			}
			for _, secret := range []string{credential.Username, credential.AuthPassword, credential.PrivacyPassword} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("credential leak")
				}
			}
		})
	}
	address, _, requests := v3Fixture(t, security, func(*gosnmp.SnmpPacket) *gosnmp.SnmpPacket { return nil })
	c := newV3Client(t, address, security, fixtureV3Credential)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Get(ctx, []string{".1.3.6.1.2.1.1.1.0"}); done <- err }()
	deadline := time.Now().Add(time.Second)
	for requests.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt USM request")
	}
	if _, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
}

func TestV3RejectsMalformedAndDowngradedResponse(t *testing.T) {
	security := Security{Version: "v3"}
	address, _, _ := v3Fixture(t, security, func(*gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.5.0", Type: gosnmp.OctetString, Value: []byte("wrong instance")}}}
	})
	c := newV3Client(t, address, security, fixtureV3Credential)
	if _, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"}); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	for _, level := range []gosnmp.SnmpV3MsgFlags{gosnmp.NoAuthNoPriv, gosnmp.AuthNoPriv} {
		_, err := c.values(&gosnmp.SnmpPacket{Version: gosnmp.Version3, SecurityModel: gosnmp.UserSecurityModel, MsgFlags: level, PDUType: gosnmp.GetResponse})
		if !errors.Is(err, ErrAuthentication) {
			t.Fatalf("downgrade accepted: %v", err)
		}
	}
	for _, bad := range []Security{{Version: "v3", SecurityLevel: "noAuthNoPriv"}, {Version: "v3", AuthProtocol: "SHA1"}, {Version: "v3", PrivacyProtocol: "AES512"}, {Version: "v3", SecurityLevel: "authNoPriv", PrivacyProtocol: "AES"}, {Version: "v2c", AuthProtocol: "SHA256"}} {
		if _, err := New(Options{Address: address, Security: bad}, v3Credential{fixtureV3Credential}, "ref"); err == nil {
			t.Fatalf("unsupported setting accepted: %+v", bad)
		}
	}
	for _, modify := range []func(*gosnmp.SnmpPacket){func(p *gosnmp.SnmpPacket) { p.MsgFlags = gosnmp.AuthNoPriv }, func(p *gosnmp.SnmpPacket) {
		p.SecurityParameters.(*gosnmp.UsmSecurityParameters).UserName = "different-user"
	}} {
		address, _, _ := v3Fixture(t, security, nil, modify)
		client := newV3Client(t, address, security, fixtureV3Credential)
		if _, err := client.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"}); err == nil || !errors.Is(err, ErrAuthentication) && !errors.Is(err, ErrConnectivity) {
			t.Fatalf("authenticated downgrade/identity mismatch accepted: %v", err)
		}
	}
}
