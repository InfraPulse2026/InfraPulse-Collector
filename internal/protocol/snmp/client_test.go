package snmp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"github.com/gosnmp/gosnmp"
)

type fixedCredential struct{ community string }

func (p fixedCredential) Get(context.Context, string) (credentials.Credential, error) {
	return credentials.Credential{Community: p.community}, nil
}

// Fixtures exchange actual UDP SNMP messages. All BER encoding/decoding belongs
// to GoSNMP, including on the fixture server; no handwritten SNMP codec is used.
func udpFixture(t *testing.T, handler func(*gosnmp.SnmpPacket) *gosnmp.SnmpPacket) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		decoder := &gosnmp.GoSNMP{Version: gosnmp.Version2c}
		for {
			n, peer, e := conn.ReadFrom(buf)
			if e != nil {
				return
			}
			packet, e := decoder.SnmpDecodePacket(buf[:n])
			if e != nil {
				continue
			}
			response := handler(packet)
			if response == nil {
				continue
			}
			response.Version = packet.Version
			response.Community = packet.Community
			response.RequestID = packet.RequestID
			response.PDUType = gosnmp.GetResponse
			wire, e := response.MarshalMsg()
			if e == nil {
				conn.WriteTo(wire, peer)
			}
		}
	}()
	t.Cleanup(func() { conn.Close(); <-done })
	return conn.LocalAddr().String()
}
func testClient(t *testing.T, address string, edit func(*Options)) *Client {
	t.Helper()
	opts := Options{Address: address, Timeout: time.Second, MaxRows: 50, MaxResponseBytes: 4096, MaxRepetitions: 3}
	if edit != nil {
		edit(&opts)
	}
	client, err := New(opts, fixedCredential{"secret-community"}, "ref")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestUDPGetGetNextAndBulkWalk(t *testing.T) {
	root := ".1.3.6.1.2.1.1"
	oid := root + ".1.0"
	address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		if p.Community != "secret-community" {
			return nil
		}
		switch p.PDUType {
		case gosnmp.GetRequest:
			return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: p.Variables[0].Name, Type: gosnmp.OctetString, Value: []byte("fixture device")}}}
		case gosnmp.GetNextRequest, gosnmp.GetBulkRequest:
			if p.Variables[0].Name == root {
				return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.OctetString, Value: []byte("fixture device")}, {Name: root + ".2.0", Type: gosnmp.ObjectIdentifier, Value: ".1.3.6.1.4.1.14988.1"}}}
			}
			return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: p.Variables[0].Name, Type: gosnmp.EndOfMibView}}}
		}
		return nil
	})
	c := testClient(t, address, nil)
	v, err := c.Get(context.Background(), []string{oid})
	if err != nil || len(v) != 1 || string(v[0].Data.([]byte)) != "fixture device" {
		t.Fatalf("get: %v %v", v, err)
	}
	v, err = c.GetNext(context.Background(), []string{root})
	if err != nil || len(v) != 2 {
		t.Fatalf("getnext: %v %v", v, err)
	}
	v, err = c.BulkWalk(context.Background(), root)
	if err != nil || len(v) != 2 {
		t.Fatalf("bulk: %v %v", v, err)
	}
	v, err = c.Walk(context.Background(), root)
	if err != nil || len(v) != 2 {
		t.Fatalf("walk: %v %v", v, err)
	}
}
func TestPacketErrorIsNotCompleteWalk(t *testing.T) {
	for _, code := range []gosnmp.SNMPError{gosnmp.GenErr, gosnmp.TooBig, gosnmp.NoAccess} {
		t.Run(code.String(), func(t *testing.T) {
			address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
				return &gosnmp.SnmpPacket{Error: code, Variables: p.Variables}
			})
			c := testClient(t, address, nil)
			_, err := c.BulkWalk(context.Background(), ".1.3.6.1.2.1.1")
			if err == nil {
				t.Fatal("packet error swallowed")
			}
			if strings.Contains(err.Error(), "secret-community") {
				t.Fatal("credential leak")
			}
			if code == gosnmp.TooBig && !errors.Is(err, ErrLimit) {
				t.Fatalf("expected limit: %v", err)
			}
		})
	}
}
func TestMissingOptionalOID(t *testing.T) {
	address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: p.Variables[0].Name, Type: gosnmp.NoSuchObject}}}
	})
	c := testClient(t, address, nil)
	values, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"})
	if err != nil || values[0].Type != NoSuchObject {
		t.Fatalf("get: %v %v", values, err)
	}
	_, err = c.BulkWalk(context.Background(), ".1.3.6.1.2.1.1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
func TestTimeoutCancellationAndClose(t *testing.T) {
	received := make(chan struct{}, 8)
	address := udpFixture(t, func(*gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		select {
		case received <- struct{}{}:
		default:
		}
		return nil
	})
	c := testClient(t, address, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	_, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"})
	if !errors.Is(err, ErrConnectivity) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	<-received
	c = testClient(t, address, func(o *Options) { o.Timeout = 5 * time.Second })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, e := c.Get(ctx, []string{".1.3.6.1.2.1.1.1.0"}); done <- e }()
	<-received
	started := time.Now()
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation failed to interrupt UDP read")
	}
	if time.Since(started) > time.Second {
		t.Fatal("slow cancellation")
	}
	ctx = context.Background()
	go func() { _, e := c.Get(ctx, []string{".1.3.6.1.2.1.1.1.0"}); done <- e }()
	<-received
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close failed to interrupt active read")
	}
	if _, err = c.Get(ctx, []string{".1.3.6.1.2.1.1.1.0"}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
func TestResponseAndRowLimits(t *testing.T) {
	address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.1.0", Type: gosnmp.OctetString, Value: []byte(strings.Repeat("x", 1000))}}}
	})
	c := testClient(t, address, func(o *Options) { o.MaxResponseBytes = 256 })
	_, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"})
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	address = udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.1.0", Type: gosnmp.Integer, Value: 1}, {Name: ".1.3.6.1.2.1.1.2.0", Type: gosnmp.Integer, Value: 2}, {Name: ".1.3.6.1.2.1.1.3.0", Type: gosnmp.Integer, Value: 3}}}
	})
	c = testClient(t, address, func(o *Options) { o.MaxRows = 2 })
	_, err = c.BulkWalk(context.Background(), ".1.3.6.1.2.1.1")
	if !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestNonIncreasingOIDIsMalformed(t *testing.T) {
	address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.1.0", Type: gosnmp.Integer, Value: 1}}}
	})
	c := testClient(t, address, nil)
	_, err := c.BulkWalk(context.Background(), ".1.3.6.1.2.1.1")
	if !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
}

func TestGetRejectsMismatchedResponseAndOversizedRequest(t *testing.T) {
	address := udpFixture(t, func(p *gosnmp.SnmpPacket) *gosnmp.SnmpPacket {
		return &gosnmp.SnmpPacket{Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.5.0", Type: gosnmp.OctetString, Value: []byte("wrong instance")}}}
	})
	c := testClient(t, address, nil)
	if _, err := c.Get(context.Background(), []string{".1.3.6.1.2.1.1.1.0"}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("response instance mismatch: %v", err)
	}
	oids := make([]string, 61)
	for i := range oids {
		oids[i] = ".1.3.6.1.2.1.1.1.0"
	}
	if _, err := c.Get(context.Background(), oids); !errors.Is(err, ErrLimit) {
		t.Fatalf("request limit: %v", err)
	}
}
