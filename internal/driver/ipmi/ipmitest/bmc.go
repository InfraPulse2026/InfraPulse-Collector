// Package ipmitest supplies an isolated reference BMC for tests. Production
// adapters never import this package. Sensor command responses supplement the
// reference server's native Device ID, chassis, FRU and SDR command handlers.
package ipmitest

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/credentials"
	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/clock"
	"github.com/bougou/go-ipmi/pkg/hal/mock"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/server"
	"github.com/bougou/go-ipmi/pkg/transport/udp"
	"github.com/bougou/go-ipmi/pkg/types"
)

const Username = "fixture-user"
const Password = "fixture-secret"

type Options struct {
	CipherSuites      []uint8
	UserPrivilege     uint8
	SensorUnavailable bool
}
type BMC struct {
	Address   string
	Port      int
	state     *bmc.BMC
	server    *server.Server
	cancel    context.CancelFunc
	done      chan error
	mu        sync.Mutex
	counts    map[string]int
	closeOnce sync.Once
}

func Start(t testing.TB) *BMC { return StartWith(t, Options{}) }
func StartWith(t testing.TB, options Options) *BMC {
	t.Helper()
	hardware := mock.New()
	ctx := context.Background()
	for id := uint8(0); id < 2; id++ {
		if err := hardware.Storage().FRU().Write(ctx, id, FRU(id)); err != nil {
			t.Fatal(err)
		}
	}
	for id, data := range SDRs() {
		if err := hardware.Storage().SDR().Write(ctx, id, data); err != nil {
			t.Fatal(err)
		}
	}
	// Reference SDRRepository.Info uses Now as its Add/Erase timestamps.
	// A fixed fixture clock makes the unchanged repository deterministic.
	fixtureClock := stableClock{Clock: clock.Real, now: time.Now()}
	state := bmc.New(bmc.DeviceInfo{DeviceID: 32, DeviceRevision: 1, FirmwareMajor: 1, FirmwareMinor: 0x23, IPMIVersion: 0x02, ManufacturerID: 0x157, ProductID: 1, AdditionalDeviceSupport: 0x8b}, [16]byte{1, 2, 3}, hardware, bmc.WithV15Disabled(), bmc.WithClock(fixtureClock))
	user, err := state.Users.Add(2, Username)
	if err != nil {
		t.Fatal(err)
	}
	user.SetPassword([]byte(Password))
	user.Enabled = true
	if options.UserPrivilege == 0 {
		options.UserPrivilege = 2
	}
	user.ChannelAccess[1] = bmc.UserChannelAccess{MaxPrivilege: bmc.PrivilegeLevel(options.UserPrivilege), Enabled: true}
	conn, err := udp.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &BMC{Address: "127.0.0.1", Port: conn.LocalAddr().(*net.UDPAddr).Port, state: state, done: make(chan error, 1), counts: make(map[string]int)}
	registry := handlers.NewRegistry()
	registry.Use(func(next handlers.Handler) handlers.Handler {
		return handlers.HandlerFunc(func(ctx context.Context, hctx *handlers.HandlerContext, request []byte) ([]byte, types.CompletionCode, error) {
			fixture.mu.Lock()
			fixture.counts[fmt.Sprintf("%02x/%02x", hctx.Command.NetFn, hctx.Command.ID)]++
			fixture.mu.Unlock()
			return next.Handle(ctx, hctx, request)
		})
	})
	handlers.RegisterAllHandlers(registry)
	registry.RegisterFunc(types.CommandGetSensorReading, func(_ context.Context, _ *handlers.HandlerContext, request []byte) ([]byte, types.CompletionCode, error) {
		if len(request) != 1 {
			return nil, types.CodeRequestDataTruncated, nil
		}
		reading, ok := map[uint8]uint8{1: 42, 2: 120, 3: 120, 4: 150, 5: 0}[request[0]]
		if !ok {
			return nil, types.CodeRequestedDataNotPresent, nil
		}
		flags := byte(0xc0)
		if options.SensorUnavailable {
			flags |= 0x20
		}
		states := byte(0)
		if request[0] == 5 {
			states = 1
		} // PSU presence, not failure.
		return []byte{reading, flags, states, 0}, types.CodeOK, nil
	})
	suites := []types.CipherSuiteID{17, 3}
	if len(options.CipherSuites) > 0 {
		suites = nil
		for _, id := range options.CipherSuites {
			suites = append(suites, types.CipherSuiteID(id))
		}
	}
	fixture.server = server.NewServer(state, &responseFraming{PacketConn: conn, state: state, requests: make(map[string]responseIdentity)}, server.WithHandlerRegistry(registry), server.WithV15Disabled(), server.WithCipherSuites(suites))
	serveCtx, cancel := context.WithCancel(context.Background())
	fixture.cancel = cancel
	go func() { fixture.done <- fixture.server.Serve(serveCtx) }()
	t.Cleanup(fixture.Close)
	return fixture
}

type stableClock struct {
	clock.Clock
	now time.Time
}

func (c stableClock) Now() time.Time { return c.now }
func (*BMC) Credential() credentials.Credential {
	return credentials.Credential{Username: Username, Password: Password}
}
func (b *BMC) ActiveSessions() int { return b.state.Sessions.Count() }
func (b *BMC) CommandCounts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.counts))
	for k, v := range b.counts {
		out[k] = v
	}
	return out
}
func (b *BMC) Close() {
	b.closeOnce.Do(func() {
		b.cancel()
		_ = b.server.Close()
		select {
		case <-b.done:
		case <-time.After(time.Second):
		}
	})
}

func sum(data []byte) byte {
	var s byte
	for _, b := range data {
		s += b
	}
	return -s
}
func area(prefix []byte, fields ...string) []byte {
	out := append([]byte(nil), prefix...)
	for _, field := range fields {
		out = append(out, byte(0xc0|len(field)))
		out = append(out, field...)
	}
	out = append(out, 0xc1)
	for (len(out)+1)%8 != 0 {
		out = append(out, 0)
	}
	out = append(out, 0)
	out[1] = byte(len(out) / 8)
	out[len(out)-1] = sum(out[:len(out)-1])
	return out
}

// FRU emits independent byte fixtures using the standard FRU area layout,
// rather than serializing the library's structs through its own decoder.
func FRU(id uint8) []byte {
	chassis := area([]byte{1, 0, 0x17}, "CHASSIS-PART", "CHASSIS-SERIAL")
	board := area([]byte{1, 0, 0, 0, 0, 0}, "Fixture Board Mfg", "Fixture Board", "BOARD-SERIAL", "BOARD-PART", "")
	product := area([]byte{1, 0, 0}, "Fixture Product Mfg", "Fixture Server", "SYSTEM-PART", "1.2", "SYSTEM-SERIAL", "", "")
	if id == 1 {
		chassis = nil
		board = nil
		product = area([]byte{1, 0, 0}, "Fixture PSU Mfg", "Fixture PSU", "PSU-PART", "1.0", "PSU-SERIAL", "", "")
	}
	header := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	out := append([]byte(nil), header...)
	for _, a := range []struct {
		index int
		data  []byte
	}{{2, chassis}, {3, board}, {4, product}} {
		if len(a.data) > 0 {
			out[a.index] = byte(len(out) / 8)
			out = append(out, a.data...)
		}
	}
	out[7] = sum(out[:7])
	return out
}
func full(record uint16, number, sensorType, unit, entity, m uint8, rexp int8, label string) []byte {
	out := make([]byte, 48+len(label))
	binary.LittleEndian.PutUint16(out[:2], record)
	out[2] = 0x51
	out[3] = 1
	out[4] = byte(len(out) - 5)
	out[5] = 0x20
	out[7] = number
	out[8] = entity
	out[9] = 1
	out[10] = 0x03
	out[11] = 0x04
	out[12] = sensorType
	out[13] = 1
	out[18] = 0x3f
	out[15] = 0x70
	out[17] = 0x70
	out[21] = unit
	out[24] = m
	out[29] = byte(rexp&15) << 4
	out[34] = 255
	out[36] = 95
	out[37] = 85
	out[38] = 75
	out[47] = byte(0xc0 | len(label))
	copy(out[48:], label)
	return out
}
func compactPSU() []byte {
	label := "PSU status"
	out := make([]byte, 32+len(label))
	binary.LittleEndian.PutUint16(out[:2], 5)
	out[2] = 0x51
	out[3] = 2
	out[4] = byte(len(out) - 5)
	out[5] = 0x20
	out[7] = 5
	out[8] = 10
	out[9] = 1
	out[10] = 3
	out[12] = 8
	out[13] = 0x6f
	out[18] = 0x7f
	out[20] = 0xc0
	out[23] = 1
	out[31] = byte(0xc0 | len(label))
	copy(out[32:], label)
	return out
}
func fruLocator() []byte {
	label := "PSU FRU"
	out := make([]byte, 16+len(label))
	binary.LittleEndian.PutUint16(out[:2], 6)
	out[2] = 0x51
	out[3] = 0x11
	out[4] = byte(len(out) - 5)
	out[5] = 0x20
	out[6] = 1
	out[7] = 0x80
	out[10] = 0x10
	out[12] = 10
	out[13] = 1
	out[15] = byte(0xc0 | len(label))
	copy(out[16:], label)
	return out
}
func SDRs() map[uint16][]byte {
	return map[uint16][]byte{
		1: full(1, 1, 1, 1, 3, 1, 0, "CPU temperature"),
		2: full(2, 2, 4, 18, 29, 10, 0, "Fan speed"),
		3: full(3, 3, 2, 4, 10, 1, -1, "PSU voltage"),
		4: full(4, 4, 0x0b, 6, 10, 1, 0, "PSU power"),
		5: compactPSU(), 6: fruLocator(),
	}
}
