// Package routeros provides a bounded HTTPS client with a fixed REST operation
// set. It deliberately has no public arbitrary URL, path, method or command API.
package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ErrConnectivity = errors.New("RouterOS connection failed")
	ErrUnavailable  = errors.New("RouterOS operation unavailable")
	ErrMalformed    = errors.New("malformed RouterOS response")
	ErrLimit        = errors.New("RouterOS response exceeds limit")
)

// HTTPError records only a status code. Router-controlled response text is
// intentionally excluded. A 4xx status is a rejected API operation, whereas a
// 5xx response cannot prove that a submitted mutation had no effect.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("RouterOS operation unavailable: HTTP %d", e.StatusCode)
}
func (*HTTPError) Unwrap() error { return ErrUnavailable }

type Record map[string]string
type Authentication struct{ Username, Password string }
type Authenticate func(context.Context) (Authentication, error)
type Options struct {
	Address string
	// DialAddress pins an already policy-checked numeric IP without changing
	// the HTTPS origin, Host header or TLS server identity. Empty uses DNS.
	DialAddress                  string
	TLSConfig                    *tls.Config
	Timeout                      time.Duration
	MaxResponseBytes, MaxRecords int
}
type Client struct {
	origin               *url.URL
	http                 *http.Client
	transport            *http.Transport
	auth                 Authenticate
	maxBytes, maxRecords int
}

func New(opts Options, auth Authenticate) (*Client, error) {
	u, err := url.Parse(opts.Address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("RouterOS requires an HTTPS origin without credentials, path, query or fragment")
	}
	if auth == nil {
		return nil, errors.New("RouterOS authentication provider is required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Timeout < 10*time.Millisecond || opts.Timeout > time.Minute {
		return nil, errors.New("invalid RouterOS timeout")
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = 1 << 20
	}
	if opts.MaxRecords == 0 {
		opts.MaxRecords = 4096
	}
	if opts.MaxResponseBytes < 64 || opts.MaxResponseBytes > 16<<20 || opts.MaxRecords < 1 || opts.MaxRecords > 65536 {
		return nil, errors.New("invalid RouterOS response limits")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSConfig != nil {
		tlsConfig = opts.TLSConfig.Clone()
		if tlsConfig.InsecureSkipVerify || tlsConfig.VerifyConnection != nil || tlsConfig.VerifyPeerCertificate != nil {
			return nil, errors.New("RouterOS TLS requires standard certificate verification")
		}
		if tlsConfig.MinVersion < tls.VersionTLS12 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: opts.Timeout, ResponseHeaderTimeout: opts.Timeout, DisableCompression: true, MaxResponseHeaderBytes: 32 << 10}
	if opts.DialAddress != "" {
		ip, parseErr := netip.ParseAddr(opts.DialAddress)
		if parseErr != nil || ip.Zone() != "" {
			return nil, errors.New("RouterOS dial address must be a numeric IP without a zone")
		}
		port := u.Port()
		if port == "" {
			port = "443"
		}
		pinned := net.JoinHostPort(ip.String(), port)
		dialer := &net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, requestedPort, splitErr := net.SplitHostPort(address)
			if splitErr != nil || !strings.EqualFold(host, u.Hostname()) || requestedPort != port {
				return nil, ErrUnavailable
			}
			return dialer.DialContext(ctx, network, pinned)
		}
	}
	client := &http.Client{Transport: transport, Timeout: opts.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{origin: u, http: client, transport: transport, auth: auth, maxBytes: opts.MaxResponseBytes, maxRecords: opts.MaxRecords}, nil
}

func (c *Client) Close() error { c.transport.CloseIdleConnections(); return nil }
func (c *Client) Identity(ctx context.Context) (Record, error) {
	return c.one(ctx, "system/identity", "name")
}
func (c *Client) Resource(ctx context.Context) (Record, error) {
	return c.one(ctx, "system/resource", "uptime,version,architecture-name,board-name,cpu,cpu-count,cpu-load,free-memory,total-memory")
}
func (c *Client) Routerboard(ctx context.Context) (Record, error) {
	return c.one(ctx, "system/routerboard", "routerboard,model,serial-number,current-firmware,revision")
}
func (c *Client) Interfaces(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "interface", interfaceProperties, nil)
}
func (c *Client) IPv4Addresses(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "ip/address", "address,interface,disabled,invalid", nil)
}
func (c *Client) IPv6Addresses(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "ipv6/address", "address,interface,disabled,invalid", nil)
}
func (c *Client) Neighbors(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "ip/neighbor", "interface,mac-address,identity,interface-name,discovered-by,age,address,address4,address6,platform,board,version,system-description,system-caps,system-caps-enabled", nil)
}
func (c *Client) BridgePorts(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "interface/bridge/port", "interface,bridge", nil)
}
func (c *Client) Bondings(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "interface/bonding", "name,slaves", nil)
}
func (c *Client) VLANs(ctx context.Context) ([]Record, error) {
	return c.list(ctx, "interface/vlan", "name,interface", nil)
}
func (c *Client) Users(ctx context.Context, name string) ([]Record, error) {
	return c.list(ctx, "user", "name,group,disabled", url.Values{"name": {name}})
}
func (c *Client) Groups(ctx context.Context, name string) ([]Record, error) {
	return c.list(ctx, "user/group", "name,policy", url.Values{"name": {name}})
}

const interfaceProperties = ".id,name,default-name,type,actual-mtu,mtu,mac-address,disabled,running,dynamic,rx-byte,tx-byte,rx-packet,tx-packet,rx-drop,tx-drop,rx-error,tx-error"

var recordID = regexp.MustCompile(`^\*[0-9A-Fa-f]{1,16}$`)

func ValidRecordID(id string) bool { return recordID.MatchString(id) }
func (c *Client) Interface(ctx context.Context, id string) (Record, error) {
	if !ValidRecordID(id) {
		return nil, ErrMalformed
	}
	return c.one(ctx, "interface/"+id, interfaceProperties)
}
func (c *Client) SetInterfaceDisabled(ctx context.Context, id string, disabled bool) error {
	if !ValidRecordID(id) {
		return ErrMalformed
	}
	body, _ := json.Marshal(struct {
		Disabled bool `json:"disabled"`
	}{disabled})
	_, err := c.request(ctx, http.MethodPatch, "interface/"+id, nil, body)
	return err
}
func (c *Client) one(ctx context.Context, path, props string) (Record, error) {
	rows, err := c.list(ctx, path, props, nil)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, ErrMalformed
	}
	return rows[0], nil
}
func (c *Client) list(ctx context.Context, path, props string, query url.Values) ([]Record, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set(".proplist", props)
	return c.request(ctx, http.MethodGet, path, query, nil)
}
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body []byte) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auth, err := c.auth(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.Join(ErrUnavailable, errors.New("RouterOS credential unavailable"))
	}
	if auth.Username == "" || auth.Password == "" || strings.Contains(auth.Username, ":") {
		return nil, ErrUnavailable
	}
	u := *c.origin
	u.Path = "/rest/" + path
	// RouterOS explicitly does not decode %2A in record identifiers. Fixed
	// paths contain only literals plus a validated *hex ID, so preserve '*'.
	u.RawPath = u.Path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, ErrMalformed
	}
	req.SetBasicAuth(auth.Username, auth.Password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.Join(ErrConnectivity, ctx.Err())
		}
		var nerr interface{ Timeout() bool }
		if errors.As(err, &nerr) && nerr.Timeout() {
			return nil, errors.Join(ErrConnectivity, context.DeadlineExceeded)
		}
		return nil, ErrConnectivity
	}
	defer resp.Body.Close()
	// Router response/error bodies may contain supplied credentials or other
	// sensitive data. Error text therefore never incorporates a body or URL.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode}
	}
	if resp.ContentLength > int64(c.maxBytes) {
		return nil, ErrLimit
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBytes)+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.Join(ErrConnectivity, ctx.Err())
		}
		return nil, ErrConnectivity
	}
	if len(data) > c.maxBytes {
		return nil, ErrLimit
	}
	trimmed := bytes.TrimSpace(data)
	var rows []Record
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var row Record
		err = json.Unmarshal(trimmed, &row)
		rows = []Record{row}
	} else {
		err = json.Unmarshal(trimmed, &rows)
	}
	if err != nil || rows == nil {
		return nil, ErrMalformed
	}
	if len(rows) > c.maxRecords {
		return nil, ErrLimit
	}
	for _, row := range rows {
		if row == nil || len(row) > 128 {
			return nil, ErrMalformed
		}
		for key, value := range row {
			if len(key) > 128 || len(value) > 4096 {
				return nil, ErrLimit
			}
		}
	}
	return rows, nil
}
