// Package redfish adapts standard DMTF Redfish reads. Library schemas and HTTP
// authentication remain confined to this package; no Redfish actions are exposed.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/schemas"
)

var (
	ErrAuthentication = errors.Join(driver.ErrUnavailable, errors.New("Redfish authentication failed"))
	ErrPermission     = errors.Join(driver.ErrUnavailable, errors.New("Redfish access denied"))
)

type Options struct {
	TLSConfig                                             *tls.Config
	Timeout, RequestTimeout                               time.Duration
	Auth                                                  string
	MaxResources, MaxPages, MaxRequests, MaxResponseBytes int
	MaxPollBytes                                          int
	Provenance                                            model.Provenance
}

type Adapter struct {
	opts      driver.Options
	limits    Options
	provider  credentials.Provider
	ref       string
	origin    *url.URL
	http      *http.Client
	transport *safeTransport
	gate      chan struct{}
	lifecycle context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	cooldown  time.Time // protected by the poll gate; no requests before Retry-After
	plan      []readPlan
}

// New performs no network I/O. Credentials are resolved only inside a poll.
func New(opts driver.Options, provider credentials.Provider, ref string, limits Options) (*Adapter, error) {
	u, err := url.Parse(opts.Address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || len(opts.Address) > 2048 || strings.ContainsAny(opts.Address, "\\\r\n\x00") || (u.Path != "" && u.Path != "/") || opts.ID == "" || provider == nil || ref == "" || opts.EnableMutations {
		return nil, errors.New("Redfish requires an HTTPS origin, target ID and credential reference")
	}
	u.Path = ""
	if limits.Timeout == 0 {
		limits.Timeout = 30 * time.Second
	}
	if limits.RequestTimeout == 0 {
		limits.RequestTimeout = 5 * time.Second
	}
	if limits.Auth == "" {
		limits.Auth = "session"
	}
	if limits.MaxResources == 0 {
		limits.MaxResources = 128
	}
	if limits.MaxPages == 0 {
		limits.MaxPages = 8
	}
	if limits.MaxRequests == 0 {
		limits.MaxRequests = 512
	}
	if limits.MaxResponseBytes == 0 {
		limits.MaxResponseBytes = 1 << 20
	}
	if limits.MaxPollBytes == 0 {
		limits.MaxPollBytes = 8 << 20
	}
	if limits.Provenance == "" {
		limits.Provenance = model.ProvenanceReal
	}
	if limits.Timeout <= 0 || limits.Timeout > 5*time.Minute || limits.RequestTimeout < 10*time.Millisecond || limits.RequestTimeout > 30*time.Second || limits.MaxResources < 1 || limits.MaxResources > 1024 || limits.MaxPages < 1 || limits.MaxPages > 64 || limits.MaxRequests < 1 || limits.MaxRequests > 4096 || limits.MaxResponseBytes < 128 || limits.MaxResponseBytes > 4<<20 || limits.MaxPollBytes < limits.MaxResponseBytes || limits.MaxPollBytes > 32<<20 || (limits.Auth != "session" && limits.Auth != "basic") || (limits.Provenance != model.ProvenanceReal && limits.Provenance != model.ProvenanceSimulated) {
		return nil, errors.New("invalid Redfish polling limits, authentication or provenance")
	}
	if limits.RequestTimeout > limits.Timeout {
		limits.RequestTimeout = limits.Timeout
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if limits.TLSConfig != nil {
		tlsConfig = limits.TLSConfig.Clone()
	}
	if tlsConfig.InsecureSkipVerify {
		return nil, errors.New("Redfish requires verified TLS")
	}
	if tlsConfig.MinVersion < tls.VersionTLS12 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil // Do not forward BMC credentials through environment proxies.
	base.TLSClientConfig = tlsConfig
	base.TLSHandshakeTimeout = limits.RequestTimeout
	base.ResponseHeaderTimeout = limits.RequestTimeout
	base.MaxConnsPerHost = 1
	base.MaxResponseHeaderBytes = 64 << 10
	base.MaxIdleConnsPerHost = 1
	base.IdleConnTimeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adapter{opts: opts, limits: limits, provider: provider, ref: ref, origin: u, gate: make(chan struct{}, 1), lifecycle: ctx, cancel: cancel}
	a.transport = &safeTransport{base: base, adapter: a}
	a.http = &http.Client{Transport: a.transport, Timeout: limits.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return driver.ErrUnavailable }}
	return a, nil
}

func (a *Adapter) Close() error {
	a.closeOnce.Do(func() { a.cancel(); a.http.CloseIdleConnections() })
	return nil
}
func (a *Adapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "inventory.read", Supported: true}, {Name: "health.read", Supported: true}, {Name: "telemetry.read", Supported: true}}
}

// canonical accepts only service-local links. Fragments identify embedded
// legacy members and are permitted only for metadata, never for HTTP reads.
func (a *Adapter) canonical(link string, fragment bool) (string, error) {
	if link == "" || len(link) > 2048 || strings.ContainsAny(link, "\\\r\n\x00") {
		return "", driver.ErrMalformed
	}
	u, err := url.Parse(link)
	if err != nil || u.User != nil || u.Opaque != "" || (!fragment && u.Fragment != "") {
		return "", driver.ErrMalformed
	}
	if u.IsAbs() && (u.Scheme != a.origin.Scheme || !strings.EqualFold(u.Host, a.origin.Host)) || u.Host != "" && !strings.EqualFold(u.Host, a.origin.Host) {
		return "", driver.ErrUnavailable
	}
	if !strings.HasPrefix(u.Path, "/redfish/v1/") && u.Path != "/redfish/v1" {
		return "", driver.ErrUnavailable
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." || part == "." {
			return "", driver.ErrUnavailable
		}
	}
	if strings.Contains(u.Path, "\\") || strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") || strings.Contains(strings.ToLower(u.EscapedPath()), "%5c") {
		return "", driver.ErrUnavailable
	}
	result := u.RequestURI()
	if fragment && u.Fragment != "" {
		result += "#" + u.EscapedFragment()
	}
	if len(result) > 2048 {
		return "", driver.ErrLimit
	}
	for key := range u.Query() {
		switch strings.ToLower(key) {
		case "password", "passwd", "token", "authorization", "x-auth-token", "userinfo", "private_key", "cookie":
			return "", driver.ErrUnavailable
		}
	}
	unescaped, e := url.QueryUnescape(result)
	if e != nil || strings.ContainsAny(unescaped, "\r\n\x00") {
		return "", driver.ErrMalformed
	}
	return result, nil
}

type budgetKey struct{}
type pollBudget struct {
	requests          int
	bytes             int
	sessions          map[string]string // validated own session URI -> private token
	sessionCollection string
	secrets           []string
}

// httpFailure deliberately contains only an HTTP status, never BMC body,
// headers, URLs containing untrusted data, or the credential provider's error.
type httpFailure struct{ status int }

func (e *httpFailure) Error() string { return "Redfish HTTP status " + strconv.Itoa(e.status) }
func (e *httpFailure) Unwrap() error {
	if e.status == 401 {
		return ErrAuthentication
	}
	if e.status == 403 {
		return ErrPermission
	}
	if e.status == 404 {
		return driver.ErrUnavailable
	}
	return driver.ErrConnectivity
}

type safeTransport struct {
	base    *http.Transport
	adapter *Adapter
}

func (s *safeTransport) CloseIdleConnections() { s.base.CloseIdleConnections() }
func (s *safeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	a := s.adapter
	if req.URL.Scheme != a.origin.Scheme || !strings.EqualFold(req.URL.Host, a.origin.Host) {
		return nil, driver.ErrUnavailable
	}
	path, err := a.canonical(req.URL.String(), false)
	if err != nil {
		return nil, err
	}
	b, ok := req.Context().Value(budgetKey{}).(*pollBudget)
	if !ok {
		return nil, driver.ErrUnavailable
	}
	for _, secret := range b.secrets {
		decoded, _ := url.QueryUnescape(path)
		if secret != "" && (strings.Contains(path, secret) || strings.Contains(decoded, secret)) {
			return nil, driver.ErrUnavailable
		}
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead && !(req.Method == http.MethodPost && path == b.sessionCollection && path != "") && !(req.Method == http.MethodDelete && b.sessions[path] != "") {
		return nil, driver.ErrUnavailable
	}
	if req.Method != http.MethodDelete && time.Now().Before(a.cooldown) {
		return nil, driver.ErrConnectivity
	}
	for attempt := 0; attempt < 2; attempt++ {
		if b.requests >= a.limits.MaxRequests && req.Method != http.MethodDelete {
			return nil, driver.ErrLimit
		}
		b.requests++
		resp, e := s.base.RoundTrip(req)
		if e != nil {
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			return nil, driver.ErrConnectivity
		}
		// Session creation may succeed even when its body is malformed or too
		// large. Remember only a verified own Location/token before reading the
		// body, so failure cleanup does not leak BMC sessions.
		if req.Method == http.MethodPost && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			location, err := a.canonical(resp.Header.Get("Location"), false)
			token := resp.Header.Get("X-Auth-Token")
			child := strings.TrimPrefix(location, b.sessionCollection+"/")
			if err != nil || !strings.HasPrefix(location, b.sessionCollection+"/") || child == "" || strings.ContainsAny(child, "/?") || token == "" || len(token) > 4096 {
				resp.Body.Close()
				return nil, driver.ErrMalformed
			}
			b.sessions[location] = token
			b.secrets = append(b.secrets, token)
			resp.Header.Set("Location", location)
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, int64(a.limits.MaxResponseBytes)+1))
		resp.Body.Close()
		if e != nil {
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			return nil, driver.ErrConnectivity
		}
		if len(body) > a.limits.MaxResponseBytes {
			return nil, driver.ErrLimit
		}
		b.bytes += len(body)
		if b.bytes > a.limits.MaxPollBytes {
			return nil, driver.ErrLimit
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return nil, driver.ErrUnavailable
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			delay := retryDelay(resp.Header.Get("Retry-After"))
			until := time.Now().Add(delay)
			if until.After(a.cooldown) {
				a.cooldown = until
			}
			if attempt != 0 || req.Method != http.MethodGet || delay > 2*time.Second {
				return nil, &httpFailure{status: resp.StatusCode}
			}
			timer := time.NewTimer(delay)
			select {
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			case <-timer.C:
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, &httpFailure{status: resp.StatusCode}
		}
		if req.Method == http.MethodPost {
			var session map[string]json.RawMessage
			if json.Unmarshal(body, &session) != nil || session == nil {
				return nil, driver.ErrMalformed
			}
		}
		if req.Method == http.MethodGet && (path == "/redfish/v1/" || path == "/redfish/v1") {
			var root struct {
				Links          struct{ Sessions linkRef }
				SessionService linkRef
			}
			if json.Unmarshal(body, &root) != nil {
				return nil, driver.ErrMalformed
			}
			if root.Links.Sessions.ID != "" {
				canonical, e := a.canonical(root.Links.Sessions.ID, false)
				if e != nil || !sessionCollection(canonical) {
					return nil, driver.ErrUnavailable
				}
				b.sessionCollection = canonical
			}
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}
	return nil, driver.ErrConnectivity
}

func retryDelay(value string) time.Duration {
	const capDelay = 5 * time.Minute
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		if seconds > int64(capDelay/time.Second) {
			return capDelay
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := time.Until(at)
		if delay < 0 {
			return 0
		}
		if delay > capDelay {
			return capDelay
		}
		return delay
	}
	return 100 * time.Millisecond
}

type poll struct {
	a                *Adapter
	ctx              context.Context
	client           *gofish.APIClient
	session          *gofish.Session
	budget           *pollBudget
	reauthenticated  bool
	visited          map[string]bool
	cache            map[string]json.RawMessage
	resources        int
	emitted          int
	metrics          int
	partial          bool
	rejectedIdentity map[string][]identityRejection
}

type identityRejection struct {
	kind   model.IdentityEvidenceType
	origin string
}

func (a *Adapter) start(ctx context.Context) (*poll, func(), error) {
	select {
	case a.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-a.lifecycle.Done():
		return nil, nil, driver.ErrUnavailable
	}
	if time.Now().Before(a.cooldown) {
		<-a.gate
		return nil, nil, driver.ErrConnectivity
	}
	ctx, cancel := context.WithTimeout(ctx, a.limits.Timeout)
	stop := context.AfterFunc(a.lifecycle, cancel)
	b := &pollBudget{sessions: map[string]string{}}
	ctx = context.WithValue(ctx, budgetKey{}, b)
	p := &poll{a: a, ctx: ctx, budget: b, visited: map[string]bool{}, cache: map[string]json.RawMessage{}}
	finish := func() {
		p.logout()
		stop()
		cancel()
		if a.lifecycle.Err() != nil {
			a.http.CloseIdleConnections()
		}
		<-a.gate
	}
	if err := p.connect(); err != nil {
		finish()
		return nil, nil, err
	}
	return p, finish, nil
}

func (p *poll) connect() error {
	credential, err := p.a.provider.Get(p.ctx, p.a.ref)
	if err != nil || credential.Username == "" || credential.Password == "" {
		return driver.ErrUnavailable
	}
	p.budget.secrets = append(p.budget.secrets, credential.Password)
	p.budget.secrets = append(p.budget.secrets, base64.StdEncoding.EncodeToString([]byte(credential.Username+":"+credential.Password)))
	config := gofish.ClientConfig{Endpoint: p.a.origin.String(), HTTPClient: p.a.http, NoModifyTransport: true, MaxConcurrentRequests: 1, ReuseConnections: true}
	if p.a.limits.Auth == "basic" {
		config.Username = credential.Username
		config.Password = credential.Password
		config.BasicAuth = true
	}
	client, err := gofish.ConnectContext(p.ctx, config)
	if err != nil {
		return safeError(err)
	}
	p.client = client
	if p.a.limits.Auth == "session" {
		if p.budget.sessionCollection == "" {
			resp, e := client.Get("/redfish/v1/")
			if e != nil {
				return safeError(e)
			}
			var root map[string]json.RawMessage
			decodeErr := json.NewDecoder(resp.Body).Decode(&root)
			resp.Body.Close()
			if decodeErr != nil {
				return driver.ErrMalformed
			}
			service := link(root, "SessionService")
			canonical, e := p.a.canonical(service, false)
			if e != nil {
				return e
			}
			resp, e = client.Get(canonical)
			if e != nil {
				return safeError(e)
			}
			var serviceFields map[string]json.RawMessage
			decodeErr = json.NewDecoder(resp.Body).Decode(&serviceFields)
			resp.Body.Close()
			if decodeErr != nil {
				return driver.ErrMalformed
			}
			collection, e := p.a.canonical(link(serviceFields, "Sessions"), false)
			if e != nil || !sessionCollection(collection) {
				return driver.ErrUnavailable
			}
			p.budget.sessionCollection = collection
		}
		auth, e := schemas.CreateSession(client, p.budget.sessionCollection, credential.Username, credential.Password)
		if e != nil {
			return safeError(e)
		}
		p.session = &gofish.Session{ID: auth.Session, Token: auth.Token}
		config.Session = p.session
		client, e = gofish.ConnectContext(p.ctx, config)
		if e != nil {
			return safeError(e)
		}
		p.client = client
	}
	return nil
}
func (p *poll) logout() {
	if p.client == nil || p.a.limits.Auth == "basic" || len(p.budget.sessions) == 0 {
		return
	}
	cleanupTimeout := min(p.a.limits.RequestTimeout, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, budgetKey{}, p.budget)
	for uri, token := range p.budget.sessions {
		_, _ = p.client.WithContext(ctx).DeleteWithHeaders(uri, map[string]string{"X-Auth-Token": token})
		delete(p.budget.sessions, uri)
	}
	p.session = nil
}
func safeError(err error) error {
	for _, kind := range []error{context.Canceled, context.DeadlineExceeded, ErrAuthentication, ErrPermission, driver.ErrConnectivity, driver.ErrUnavailable, driver.ErrMalformed, driver.ErrLimit} {
		if errors.Is(err, kind) {
			return kind
		}
	}
	return driver.ErrMalformed
}
func (p *poll) get(link string) (json.RawMessage, string, error) {
	uri, err := p.a.canonical(link, false)
	if err != nil {
		return nil, "", err
	}
	for _, secret := range p.budget.secrets {
		decoded, _ := url.QueryUnescape(uri)
		if secret != "" && (strings.Contains(uri, secret) || strings.Contains(decoded, secret)) {
			return nil, uri, driver.ErrUnavailable
		}
	}
	if body, ok := p.cache[uri]; ok {
		return body, uri, nil
	}
	if p.visited[uri] {
		return nil, uri, driver.ErrMalformed
	}
	if p.resources >= p.a.limits.MaxResources {
		return nil, uri, driver.ErrLimit
	}
	p.visited[uri] = true
	p.resources++
	resp, err := p.client.Get(uri)
	var failure *httpFailure
	if errors.As(err, &failure) && failure.status == 401 && !p.reauthenticated && p.a.limits.Auth == "session" {
		p.reauthenticated = true
		p.logout()
		if e := p.connect(); e != nil {
			return nil, uri, e
		}
		resp, err = p.client.Get(uri)
	}
	if err != nil {
		return nil, uri, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, uri, safeError(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, uri, driver.ErrMalformed
	}
	if id := text(fields["@odata.id"]); id != "" {
		canonical, e := p.a.canonical(id, false)
		resourcePath := strings.Split(canonical, "?")[0]
		requestPath := strings.Split(uri, "?")[0]
		if e != nil || strings.TrimSuffix(resourcePath, "/") != strings.TrimSuffix(requestPath, "/") {
			return nil, uri, driver.ErrMalformed
		}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return nil, uri, driver.ErrMalformed
	}
	if p.scrub(value, uri, "") {
		return nil, uri, driver.ErrUnavailable
	}
	body, err = json.Marshal(value)
	if err != nil {
		return nil, uri, driver.ErrMalformed
	}
	p.cache[uri] = body
	return body, uri, nil
}

func (p *poll) scrub(value any, resource, prefix string) bool {
	unsafeLink := false
	clean := func(s string) string {
		if len(s) > 4096 {
			p.partial = true
			return ""
		}
		for _, ch := range s {
			if unicode.IsControl(ch) {
				p.partial = true
				return ""
			}
		}
		for _, secret := range p.budget.secrets {
			if secret != "" {
				decoded, _ := url.QueryUnescape(s)
				if strings.Contains(s, secret) || strings.Contains(decoded, secret) {
					p.partial = true
					return ""
				}
			}
		}
		return s
	}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if s, ok := item.(string); ok {
				cleaned := clean(s)
				v[key] = cleaned
				if cleaned != s && prefix == "" {
					p.rejectIdentityProperty(resource, key)
				}
				if cleaned != s && (key == "@odata.id" || key == "Members@odata.nextLink") {
					unsafeLink = true
				}
			} else {
				unsafeLink = p.scrub(item, resource, prefix+key+".") || unsafeLink
			}
		}
	case []any:
		for i, item := range v {
			if s, ok := item.(string); ok {
				v[i] = clean(s)
			} else {
				unsafeLink = p.scrub(item, resource, prefix) || unsafeLink
			}
		}
	}
	return unsafeLink
}

// Scrubbing removes hostile strings and secrets before typed schema decoding.
// Remember only a known property name/type, never the rejected value itself.
// The map lives for one bounded poll and has no persistent identity state.
func (p *poll) rejectIdentityProperty(resource, property string) {
	var kind model.IdentityEvidenceType
	switch property {
	case "UUID":
		kind = model.IdentityHardwareUUID
	case "SerialNumber":
		kind = model.IdentitySerial
	case "Manufacturer":
		kind = model.IdentityManufacturer
	case "Model":
		kind = model.IdentityModel
	case "MACAddress", "PermanentMACAddress":
		kind = model.IdentityMAC
	default:
		return
	}
	if p.rejectedIdentity == nil {
		p.rejectedIdentity = make(map[string][]identityRejection)
	}
	if len(p.rejectedIdentity) >= p.a.limits.MaxResources && p.rejectedIdentity[resource] == nil {
		return
	}
	rejections := p.rejectedIdentity[resource]
	if len(rejections) >= model.MaxIdentityPerSubject {
		return
	}
	p.rejectedIdentity[resource] = append(rejections, identityRejection{kind: kind, origin: "redfish." + property})
}

type linkRef struct {
	ID string `json:"@odata.id"`
}

func (p *poll) members(link string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	pages := map[string]bool{}
	for page := 0; link != ""; page++ {
		if page >= p.a.limits.MaxPages {
			return out, driver.ErrLimit
		}
		uri, e := p.a.canonical(link, false)
		if e != nil {
			return out, e
		}
		if pages[uri] {
			return out, driver.ErrMalformed
		}
		pages[uri] = true
		body, _, err := p.get(link)
		if err != nil {
			return out, err
		}
		var collection struct {
			Members []linkRef
			Next    string `json:"Members@odata.nextLink"`
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		if fields["Members"] == nil || string(fields["Members"]) == "null" || json.Unmarshal(body, &collection) != nil {
			return out, driver.ErrMalformed
		}
		for _, member := range collection.Members {
			uri, e := p.a.canonical(member.ID, false)
			if e != nil {
				return out, e
			}
			if !seen[uri] {
				if len(out) >= p.a.limits.MaxResources {
					return out, driver.ErrLimit
				}
				seen[uri] = true
				out = append(out, uri)
			}
		}
		link = collection.Next
	}
	return out, nil
}

func text(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	if len(value) > 4096 {
		return ""
	}
	return value
}

func sessionCollection(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/SessionService/Sessions") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if strings.EqualFold(part, "Actions") || strings.EqualFold(part, "Accounts") || strings.EqualFold(part, "AccountService") {
			return false
		}
	}
	return true
}
