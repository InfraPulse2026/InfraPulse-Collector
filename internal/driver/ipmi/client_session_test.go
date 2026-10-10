package ipmi

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
)

func sessionOptions() SessionOptions {
	return SessionOptions{RequestTimeout: time.Second, MaxResponseBytes: 4096, CipherSuite: 17, Privilege: "user", MaxOperations: 1024, MaxPollBytes: 1 << 20}
}

func TestReferenceBMCLANplusReadOnly(t *testing.T) {
	for _, suite := range []int{17, 3} {
		t.Run(strconv.Itoa(suite), func(t *testing.T) {
			fixture := ipmitest.Start(t)
			opts := sessionOptions()
			opts.CipherSuite = suite
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := newClient(ctx, fixture.Address, fixture.Port, fixture.Credential(), opts)
			if err != nil {
				t.Fatalf("%v counts=%v", err, fixture.CommandCounts())
			}
			device, err := c.DeviceID(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if device.DeviceID != 32 || device.Revision != 1 || device.FirmwareMajor != 1 || device.FirmwareMinorBCD != 0x23 || device.IPMIVersion != 2 || device.ManufacturerID != 0x157 || device.ProductID != 1 || !device.Available {
				t.Fatalf("wrong Device ID: %+v", device)
			}
			chassis, err := c.ChassisStatus(ctx)
			if err != nil || chassis.PowerOn || chassis.PowerFault {
				t.Fatalf("chassis off must not be faulty: %+v %v", chassis, err)
			}
			info, err := c.FRUInfo(ctx, 0)
			if err != nil || info.Bytes != uint16(len(ipmitest.FRU(0))) || info.WordAccess {
				t.Fatalf("FRU info: %+v %v", info, err)
			}
			fru, err := c.ReadFRU(ctx, 0, 0, 8)
			if err != nil || len(fru) != 8 || fru[0] != 1 {
				t.Fatalf("FRU header: %x %v", fru, err)
			}
			repo, err := c.RepositoryInfo(ctx)
			if err != nil || repo.Records != 6 {
				t.Fatalf("SDR info: %+v %v", repo, err)
			}
			reservation, err := c.ReserveSDR(ctx)
			if err != nil {
				t.Fatal(err)
			}
			next, data, err := c.SDR(ctx, reservation, 0, 0, 5)
			if err != nil || next != 2 || len(data) != 5 || data[3] != 1 {
				t.Fatalf("SDR header: %d %x %v", next, data, err)
			}
			for id, expected := range ipmitest.SDRs() {
				var record []byte
				for len(record) < len(expected) {
					_, chunk, e := c.SDR(ctx, reservation, id, uint8(len(record)), uint8(min(32, len(expected)-len(record))))
					if e != nil {
						t.Fatalf("SDR %d offset%d: %v", id, len(record), e)
					}
					record = append(record, chunk...)
				}
				if _, e := parseSDR(record, 0xffff); e != nil {
					t.Fatalf("SDR %d malformed fixture: %v", id, e)
				}
			}
			reading, err := c.SensorReading(ctx, 1, 0x20, 0, 0)
			if err != nil || reading.Raw != 42 || reading.Unavailable || reading.ScanningDisabled || reading.StateBytes != 2 {
				t.Fatalf("sensor: %+v %v", reading, err)
			}
			if _, err := c.SensorReading(ctx, 1, 0x30, 0, 0); !errors.Is(err, ErrUnsupportedAddress) {
				t.Fatalf("IPMB bridging permitted: %v", err)
			}
			if err := c.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(ctx); err != nil {
				t.Fatalf("close not idempotent: %v", err)
			}
			if fixture.ActiveSessions() != 0 {
				t.Fatal("session remained after close")
			}
			for command := range fixture.CommandCounts() {
				if command == "00/02" || command == "0a/12" || command == "04/26" {
					t.Fatalf("mutation executed: %s", command)
				}
			}
		})
	}
}

func TestClientSecurityPolicyAndCredentials(t *testing.T) {
	for _, suite := range []int{0, 1, 2, 4, 6, 15, 16, 18, 19, 255} {
		opts := sessionOptions()
		opts.CipherSuite = suite
		_, err := newClient(context.Background(), "127.0.0.1", 623, credentials.Credential{Username: "u", Password: "p"}, opts)
		if !errors.Is(err, ErrCipherSuite) {
			t.Fatalf("insecure cipher %d accepted: %v", suite, err)
		}
	}
	for _, privilege := range []string{"", "administrator", "callback", "oem"} {
		opts := sessionOptions()
		opts.Privilege = privilege
		_, err := newClient(context.Background(), "127.0.0.1", 623, credentials.Credential{Username: "u", Password: "p"}, opts)
		if !errors.Is(err, ErrPermission) {
			t.Fatalf("privilege %q accepted", privilege)
		}
	}
	for _, credential := range []credentials.Credential{{Username: "u"}, {Password: "p"}, {Username: strings.Repeat("u", 17), Password: "p"}, {Username: "u", Password: strings.Repeat("s", 21)}} {
		_, err := newClient(context.Background(), "127.0.0.1", 623, credential, sessionOptions())
		if !errors.Is(err, ErrAuthentication) {
			t.Fatalf("invalid credential accepted: %v", err)
		}
		if strings.Contains(err.Error(), "ssss") {
			t.Fatal("secret in diagnostic")
		}
	}
}

func TestReferenceBMCWrongCredentialsPrivilegesAndNoDowngrade(t *testing.T) {
	t.Run("wrong_password", func(t *testing.T) {
		fixture := ipmitest.Start(t)
		credential := fixture.Credential()
		credential.Password = "wrong-test-secret"
		_, err := newClient(context.Background(), fixture.Address, fixture.Port, credential, sessionOptions())
		if !errors.Is(err, ErrAuthentication) {
			t.Fatalf("wrong password: %v", err)
		}
		if strings.Contains(err.Error(), credential.Password) {
			t.Fatal("secret escaped error")
		}
	})
	t.Run("insufficient_privilege", func(t *testing.T) {
		fixture := ipmitest.StartWith(t, ipmitest.Options{UserPrivilege: 1})
		_, err := newClient(context.Background(), fixture.Address, fixture.Port, fixture.Credential(), sessionOptions())
		if !errors.Is(err, ErrPermission) {
			t.Fatalf("privilege: %v", err)
		}
	})
	t.Run("no_downgrade", func(t *testing.T) {
		fixture := ipmitest.StartWith(t, ipmitest.Options{CipherSuites: []uint8{3}})
		_, err := newClient(context.Background(), fixture.Address, fixture.Port, fixture.Credential(), sessionOptions())
		if !errors.Is(err, ErrCipherSuite) {
			t.Fatalf("suite 17 silently downgraded: %v", err)
		}
	})
	t.Run("unavailable_reading", func(t *testing.T) {
		fixture := ipmitest.StartWith(t, ipmitest.Options{SensorUnavailable: true})
		c, err := newClient(context.Background(), fixture.Address, fixture.Port, fixture.Credential(), sessionOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(context.Background())
		r, err := c.SensorReading(context.Background(), 1, 0x20, 0, 0)
		if err != nil || !r.Unavailable {
			t.Fatalf("unavailable not retained: %+v %v", r, err)
		}
	})
}

type trackingConn struct {
	net.Conn
	mu     sync.Mutex
	closed bool
}

func (c *trackingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Conn.Close()
}
func (c *trackingConn) isClosed() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed }
func TestClientCanceledSessionClosesSocket(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address := listener.LocalAddr().(*net.UDPAddr)
	var tracked *trackingConn
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		tracked = &trackingConn{Conn: c}
		return tracked, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = newClientWithDialer(ctx, "127.0.0.1", address.Port, credentials.Credential{Username: "u", Password: "p"}, sessionOptions(), dial)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > time.Second || tracked == nil || !tracked.isClosed() {
		t.Fatal("cancellation did not promptly clean socket")
	}
}

func TestDatagramBudgetCoversSessionSetup(t *testing.T) {
	fixture := ipmitest.Start(t)
	opts := sessionOptions()
	opts.MaxOperations = 2
	_, err := newClient(context.Background(), fixture.Address, fixture.Port, fixture.Credential(), opts)
	if !errors.Is(err, driver.ErrLimit) {
		t.Fatalf("handshake bypassed packet budget: %v", err)
	}
}
