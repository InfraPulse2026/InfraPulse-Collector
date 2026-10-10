// Package ipmi adapts read-only IPMI 2.0 LANplus observations. Protocol-library
// types, credentials and session material remain inside this adapter.
package ipmi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	ipmiclient "github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/app"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	sensorcommand "github.com/bougou/go-ipmi/pkg/command/sensor"
	"github.com/bougou/go-ipmi/pkg/command/storage"
	"github.com/bougou/go-ipmi/pkg/types"
)

var (
	ErrAuthentication     = errors.Join(driver.ErrUnavailable, errors.New("IPMI authentication failed"))
	ErrPermission         = errors.Join(driver.ErrUnavailable, errors.New("IPMI privilege denied"))
	ErrUnsupportedAddress = errors.Join(driver.ErrUnavailable, errors.New("IPMI sensor requires unsupported IPMB bridging"))
	ErrCipherSuite        = errors.Join(driver.ErrUnavailable, errors.New("IPMI requires explicitly selected cipher suite 17 or 3"))
	ErrSecurity           = errors.Join(driver.ErrMalformed, errors.New("IPMI session security validation failed"))
)

type SessionOptions struct {
	RequestTimeout              time.Duration
	Retries, CipherSuite        int
	Privilege                   string
	MaxResponseBytes            int
	MaxOperations, MaxPollBytes int
}

type DeviceInfo struct {
	DeviceID, Revision, FirmwareMajor, FirmwareMinorBCD, IPMIVersion, Support uint8
	ManufacturerID                                                            uint32
	ProductID                                                                 uint16
	Available                                                                 bool
}
type ChassisInfo struct {
	PowerOn, PowerFault, PowerControlFault, PowerOverload bool
	DriveFault, CoolingFault, Intrusion                   bool
}
type RepositoryInfo struct {
	Version          uint8
	Records          uint16
	Added, Erased    uint32
	ReserveSupported bool
}
type FRUAreaInfo struct {
	Bytes      uint16
	WordAccess bool
}
type Reading struct {
	Raw                           uint8
	Unavailable, ScanningDisabled bool
	States                        uint16
	StateBytes                    int
}

// Client exposes only the standard monitoring commands. No raw commands or
// hardware mutation methods are available to collector orchestration.
type Client interface {
	DeviceID(context.Context) (DeviceInfo, error)
	ChassisStatus(context.Context) (ChassisInfo, error)
	RepositoryInfo(context.Context) (RepositoryInfo, error)
	ReserveSDR(context.Context) (uint16, error)
	SDR(context.Context, uint16, uint16, uint8, uint8) (uint16, []byte, error)
	FRUInfo(context.Context, uint8) (FRUAreaInfo, error)
	ReadFRU(context.Context, uint8, uint16, uint8) ([]byte, error)
	SensorReading(context.Context, uint8, uint8, uint8, uint8) (Reading, error)
	Close(context.Context) error
}
type Factory func(context.Context, string, int, credentials.Credential, SessionOptions) (Client, error)

// CompletionError preserves optional-command failure codes without exposing
// library diagnostics or packet data.
type CompletionError struct{ Code uint8 }

func (e *CompletionError) Error() string {
	return fmt.Sprintf("IPMI command completion code 0x%02x", e.Code)
}
func (e *CompletionError) Unwrap() error {
	if e.Code == 0xd4 {
		return ErrPermission
	}
	return driver.ErrUnavailable
}

type nativeClient struct {
	client    *ipmiclient.Client
	guard     *sessionGuard
	closeOnce sync.Once
	closeErr  error
}

func newClient(ctx context.Context, address string, port int, credential credentials.Credential, opts SessionOptions) (Client, error) {
	return newClientWithDialer(ctx, address, port, credential, opts, (&net.Dialer{}).DialContext)
}

func newClientWithDialer(ctx context.Context, address string, port int, credential credentials.Credential, opts SessionOptions, dial func(context.Context, string, string) (net.Conn, error)) (Client, error) {
	if opts.CipherSuite != 17 && opts.CipherSuite != 3 {
		return nil, ErrCipherSuite
	}
	privilege := types.PrivilegeLevelUser
	if opts.Privilege == "operator" {
		privilege = types.PrivilegeLevelOperator
	} else if opts.Privilege != "user" {
		return nil, ErrPermission
	}
	if len(credential.Username) == 0 || len(credential.Username) > 16 || len(credential.Password) == 0 || len(credential.Password) > 20 || strings.ContainsAny(credential.Username, "\x00\r\n") {
		return nil, ErrAuthentication
	}
	if address == "" || port < 1 || port > 65535 || opts.RequestTimeout <= 0 || opts.RequestTimeout > 30*time.Second || opts.Retries < 0 || opts.Retries > 3 || opts.MaxResponseBytes < 128 || opts.MaxResponseBytes > 4096 || opts.MaxOperations < 1 || opts.MaxOperations > 8192 || opts.MaxPollBytes < opts.MaxResponseBytes || opts.MaxPollBytes > 8<<20 {
		return nil, driver.ErrLimit
	}
	c, err := ipmiclient.NewClient(address, port, credential.Username, credential.Password)
	if err != nil {
		return nil, ErrAuthentication
	}
	guard := newSessionGuard(credential, opts, uint8(privilege))
	c.WithInterface(ipmiclient.InterfaceLanplus).WithDebug(false).WithTimeout(opts.RequestTimeout).WithRetry(opts.Retries).WithBufferSize(opts.MaxResponseBytes).WithCipherSuiteID(types.CipherSuiteID(opts.CipherSuite)).WithMaxPrivilegeLevel(privilege).WithUDPProxy(&guardDialer{guard: guard, dial: dial})
	n := &nativeClient{client: c, guard: guard}
	if err := c.Connect(ctx); err != nil {
		mapped := n.mapError(err)
		closeCtx, cancel := context.WithTimeout(context.Background(), min(opts.RequestTimeout, time.Second))
		_ = n.Close(closeCtx)
		cancel()
		return nil, mapped
	}
	return n, nil
}

func (n *nativeClient) mapError(err error) error {
	if err == nil {
		return nil
	}
	if security := n.guard.failure(); security != nil {
		return security
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, driver.ErrLimit) {
		return driver.ErrLimit
	}
	if errors.Is(err, ErrSecurity) {
		return ErrSecurity
	}
	var completion *types.ResponseError
	if errors.As(err, &completion) {
		if completion.CompletionCode() == 0 {
			return driver.ErrMalformed
		}
		return &CompletionError{Code: uint8(completion.CompletionCode())}
	}
	if errors.Is(err, ipmiclient.ErrRAKPAuthentication) || strings.Contains(err.Error(), "rakp") || strings.Contains(err.Error(), "RAKP") {
		return ErrAuthentication
	}
	if strings.Contains(err.Error(), "privilege") || strings.Contains(err.Error(), "Privilege") {
		return ErrPermission
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.Join(driver.ErrConnectivity, context.DeadlineExceeded)
	}
	return driver.ErrConnectivity
}

type boundedResponse struct {
	data    []byte
	maximum int
}

func (r *boundedResponse) Unpack(data []byte) error {
	if len(data) > r.maximum {
		return driver.ErrLimit
	}
	r.data = append([]byte(nil), data...)
	return nil
}
func (*boundedResponse) Format() string { return "[IPMI response redacted]" }
func (n *nativeClient) read(ctx context.Context, request types.Request, maximum int) ([]byte, error) {
	r := &boundedResponse{maximum: maximum}
	if err := n.client.Exchange(ctx, request, r); err != nil {
		return nil, n.mapError(err)
	}
	return r.data, nil
}

func (n *nativeClient) DeviceID(ctx context.Context) (DeviceInfo, error) {
	b, err := n.read(ctx, &app.GetDeviceIDRequest{}, 15)
	if err != nil {
		return DeviceInfo{}, err
	}
	if len(b) != 11 && len(b) != 15 || b[3]&15 > 9 || b[3]>>4 > 9 || b[4]&15 > 9 || b[4]>>4 > 9 || b[8]&0xf0 != 0 {
		return DeviceInfo{}, driver.ErrMalformed
	}
	return DeviceInfo{DeviceID: b[0], Revision: b[1] & 15, FirmwareMajor: b[2] & 127, FirmwareMinorBCD: b[3], IPMIVersion: b[4], Support: b[5], ManufacturerID: uint32(b[6]) | uint32(b[7])<<8 | uint32(b[8])<<16, ProductID: binary.LittleEndian.Uint16(b[9:11]), Available: b[2]&128 == 0}, nil
}
func (n *nativeClient) ChassisStatus(ctx context.Context) (ChassisInfo, error) {
	b, err := n.read(ctx, &chassis.GetChassisStatusRequest{}, 4)
	if err != nil {
		return ChassisInfo{}, err
	}
	if len(b) < 3 {
		return ChassisInfo{}, driver.ErrMalformed
	}
	return ChassisInfo{PowerOn: b[0]&1 != 0, PowerOverload: b[0]&2 != 0, PowerFault: b[0]&8 != 0, PowerControlFault: b[0]&16 != 0, Intrusion: b[2]&1 != 0, DriveFault: b[2]&4 != 0, CoolingFault: b[2]&8 != 0}, nil
}
func (n *nativeClient) RepositoryInfo(ctx context.Context) (RepositoryInfo, error) {
	b, err := n.read(ctx, &storage.GetSDRRepoInfoRequest{}, 14)
	if err != nil {
		return RepositoryInfo{}, err
	}
	if len(b) != 14 {
		return RepositoryInfo{}, driver.ErrMalformed
	}
	return RepositoryInfo{Version: b[0], Records: binary.LittleEndian.Uint16(b[1:3]), Added: binary.LittleEndian.Uint32(b[5:9]), Erased: binary.LittleEndian.Uint32(b[9:13]), ReserveSupported: b[13]&2 != 0}, nil
}
func (n *nativeClient) ReserveSDR(ctx context.Context) (uint16, error) {
	b, err := n.read(ctx, &storage.ReserveSDRRepoRequest{}, 2)
	if err != nil {
		return 0, err
	}
	if len(b) != 2 {
		return 0, driver.ErrMalformed
	}
	return binary.LittleEndian.Uint16(b), nil
}
func (n *nativeClient) SDR(ctx context.Context, reservation, id uint16, offset, count uint8) (uint16, []byte, error) {
	if count == 0 || count > 64 {
		return 0, nil, driver.ErrLimit
	}
	// Conservative chunks also work with BMCs whose Get SDR response cap is
	// 16 bytes. The packet gate counts every exchange against the poll budget.
	data := make([]byte, 0, int(count))
	var next uint16
	for len(data) < int(count) {
		position := int(offset) + len(data)
		if position > 255 {
			return 0, nil, driver.ErrLimit
		}
		chunk := min(16, int(count)-len(data))
		b, err := n.read(ctx, &storage.GetSDRRequest{ReservationID: reservation, RecordID: id, ReadOffset: uint8(position), ReadBytes: uint8(chunk)}, chunk+2)
		if err != nil {
			return 0, nil, err
		}
		if len(b) < 3 {
			return 0, nil, driver.ErrMalformed
		}
		nextID := binary.LittleEndian.Uint16(b)
		if len(data) > 0 && nextID != next {
			return 0, nil, driver.ErrMalformed
		}
		next = nextID
		data = append(data, b[2:]...)
		if len(b)-2 < chunk {
			break
		}
	}
	return next, data, nil
}
func (n *nativeClient) FRUInfo(ctx context.Context, id uint8) (FRUAreaInfo, error) {
	b, err := n.read(ctx, &storage.GetFRUInventoryAreaInfoRequest{FRUDeviceID: id}, 3)
	if err != nil {
		return FRUAreaInfo{}, err
	}
	if len(b) != 3 || b[2]&0xfe != 0 {
		return FRUAreaInfo{}, driver.ErrMalformed
	}
	return FRUAreaInfo{Bytes: binary.LittleEndian.Uint16(b), WordAccess: b[2]&1 != 0}, nil
}
func (n *nativeClient) ReadFRU(ctx context.Context, id uint8, offset uint16, count uint8) ([]byte, error) {
	if count == 0 || count > 64 {
		return nil, driver.ErrLimit
	}
	b, err := n.read(ctx, &storage.ReadFRUDataRequest{FRUDeviceID: id, ReadOffset: offset, ReadCount: count}, int(count)+1)
	if err != nil {
		return nil, err
	}
	if len(b) < 2 || int(b[0]) != len(b)-1 {
		return nil, driver.ErrMalformed
	}
	return b[1:], nil
}
func (n *nativeClient) SensorReading(ctx context.Context, number, owner, lun, channel uint8) (Reading, error) {
	if owner != 0x20 || lun > 3 || channel != 0 {
		return Reading{}, ErrUnsupportedAddress
	}
	ctx = ipmiclient.WithCommandContext(ctx, (&ipmiclient.CommandContext{}).WithResponderAddr(owner).WithResponderLUN(lun))
	b, err := n.read(ctx, &sensorcommand.GetSensorReadingRequest{SensorNumber: number}, 4)
	if err != nil {
		return Reading{}, err
	}
	if len(b) < 2 {
		return Reading{}, driver.ErrMalformed
	}
	r := Reading{Raw: b[0], Unavailable: b[1]&32 != 0, ScanningDisabled: b[1]&64 == 0, StateBytes: len(b) - 2}
	if len(b) >= 3 {
		r.States = uint16(b[2])
	}
	if len(b) == 4 {
		r.States |= uint16(b[3]&127) << 8
	}
	return r, nil
}
func (n *nativeClient) Close(ctx context.Context) error {
	n.closeOnce.Do(func() { n.closeErr = n.mapError(n.client.Close(ctx)); n.guard.erase() })
	return n.closeErr
}
