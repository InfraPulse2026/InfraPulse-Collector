package ipmi

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/driver/ipmi/ipmitest"
)

// lostResponseConn sits below the production guard and removes one complete
// authenticated response. No framing, keys or peer validation are changed.
type lostResponseConn struct {
	net.Conn
	mu        sync.Mutex
	armed     bool
	dropped   bool
	sequences []uint32
	closed    bool
}

func (c *lostResponseConn) arm() {
	c.mu.Lock()
	c.armed = true
	c.mu.Unlock()
}

func (c *lostResponseConn) Write(packet []byte) (int, error) {
	c.mu.Lock()
	if c.armed && len(packet) >= 16 && packet[4] == 6 && packet[5] == 0xc0 {
		c.sequences = append(c.sequences, binary.LittleEndian.Uint32(packet[10:14]))
	}
	c.mu.Unlock()
	return c.Conn.Write(packet)
}

func (c *lostResponseConn) Read(out []byte) (int, error) {
	for {
		n, err := c.Conn.Read(out)
		if err != nil {
			return n, err
		}
		c.mu.Lock()
		drop := c.armed && !c.dropped && n >= 16 && out[4] == 6 && out[5] == 0xc0
		if drop {
			c.dropped = true
		}
		c.mu.Unlock()
		if !drop {
			return n, nil
		}
	}
}

func (c *lostResponseConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Conn.Close()
}

func TestLostLANplusResponseRetriesAndCleanup(t *testing.T) {
	for _, retries := range []int{0, 1} {
		t.Run(strconv.Itoa(retries), func(t *testing.T) {
			fixture := ipmitest.Start(t)
			opts := sessionOptions()
			opts.Retries, opts.RequestTimeout = retries, 50*time.Millisecond
			var wire *lostResponseConn
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				wire = &lostResponseConn{Conn: conn}
				return wire, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			client, err := newClientWithDialer(ctx, fixture.Address, fixture.Port, fixture.Credential(), opts, dial)
			if err != nil {
				t.Fatal(err)
			}
			wire.arm()
			started := time.Now()
			_, readErr := client.DeviceID(ctx)
			cleanup, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
			closeErr := client.Close(cleanup)
			cancelCleanup()
			if closeErr != nil {
				t.Errorf("session cleanup failed: %v", closeErr)
			}
			wire.mu.Lock()
			dropped, closed := wire.dropped, wire.closed
			sequences := append([]uint32(nil), wire.sequences...)
			wire.mu.Unlock()
			if !dropped || !closed || fixture.ActiveSessions() != 0 || time.Since(started) > time.Second {
				t.Fatalf("loss/cleanup bound failed: dropped=%v closed=%v active=%d", dropped, closed, fixture.ActiveSessions())
			}
			if len(sequences) != retries+2 { // initial attempt, retries, Close Session
				t.Errorf("wrong finite transmission count: %d", len(sequences))
			}
			if retries == 0 {
				if !errors.Is(readErr, driver.ErrConnectivity) {
					t.Fatalf("lost response without retry did not fail: %v", readErr)
				}
			} else if readErr != nil {
				t.Fatalf("one lost response was not recovered by retry: %v; session sequences=%v", readErr, sequences)
			}
		})
	}
}
