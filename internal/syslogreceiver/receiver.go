package syslogreceiver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"InfraPulseAgent/internal/logs"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/queue"
	"github.com/google/uuid"
)

type Options struct {
	CollectorID   string
	MaxBatchBytes int
	Sanitize      func(string) string
	Emit          func(context.Context, model.Envelope) error
}

// Stats contains categories and aggregate counts only, never peer payloads,
// certificate contents or arbitrary client-supplied diagnostic text.
type Stats struct {
	Listening         []string `json:"listening,omitempty"`
	ActiveConnections int      `json:"active_connections"`
	PeakConnections   int      `json:"peak_connections"`
	Accepted          uint64   `json:"accepted_connections"`
	Rejected          uint64   `json:"rejected_connections"`
	Frames            uint64   `json:"frames"`
	Admitted          uint64   `json:"admitted"`
	ParseFallbacks    uint64   `json:"parse_fallbacks"`
	AdmissionFailures uint64   `json:"admission_failures"`
	LastError         string   `json:"last_error,omitempty"`
}

type Receiver struct {
	settings    Settings
	options     Options
	tlsConfig   *tls.Config
	mu          sync.Mutex
	stats       Stats
	running     bool
	connections map[net.Conn]string
	peers       map[string]int
	sources     map[string]int
}

func New(settings Settings, options Options) (*Receiver, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if options.Emit == nil || options.Sanitize == nil || options.CollectorID == "" || options.MaxBatchBytes < 14*settings.Limits.MaxMessageBytes+8192 {
		return nil, errors.New("invalid syslog receiver pipeline options")
	}
	r := &Receiver{settings: settings, options: options, connections: map[net.Conn]string{}, peers: map[string]int{}, sources: map[string]int{}}
	if settings.Enabled && settings.TLS.Enabled {
		var err error
		r.tlsConfig, err = LoadTLS(settings.TLS)
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// LoadTLS checks the separate listener certificate/key pair and trust pool.
// The outbound Collector identity is never implicitly reused.
func LoadTLS(settings TLS) (*tls.Config, error) {
	certPEM, certErr := readPEM(settings.CertFile)
	keyPEM, keyErr := readPEM(settings.KeyFile)
	if certErr != nil || keyErr != nil {
		return nil, errors.New("syslog_tls_invalid_server_key_pair")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, errors.New("syslog_tls_invalid_server_key_pair")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("syslog_tls_invalid_server_certificate")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if settings.ClientAuth == "require_and_verify" {
		data, err := readPEM(settings.ClientCAFile)
		if err != nil {
			return nil, errors.New("syslog_tls_invalid_client_ca")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("syslog_tls_invalid_client_ca")
		}
		cfg.ClientAuth, cfg.ClientCAs = tls.RequireAndVerifyClientCert, roots
	} else if settings.ClientAuth != "none" {
		return nil, errors.New("syslog_tls_invalid_client_auth")
	}
	return cfg, nil
}

func readPEM(path string) ([]byte, error) {
	// Check the final file before Open: opening a FIFO can block without a
	// writer, outside the listener's network deadlines. Administrator-managed
	// symlinks to regular certificate files remain supported.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("syslog_tls_invalid_file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("syslog_tls_invalid_file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("syslog_tls_file_exceeds_limit")
	}
	return data, nil
}

// Run binds enabled listeners as one startup operation, rolling back all sockets
// if any bind fails. Each bounded connection owns one sequential processor.
func (r *Receiver) Run(ctx context.Context) error {
	if !r.settings.Enabled {
		return nil
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errors.New("syslog_receiver_already_running")
	}
	r.running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.running = false; r.stats.Listening = nil; r.mu.Unlock() }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type listener struct {
		socket             net.Listener
		transport, framing string
	}
	var listeners []listener
	defer func() {
		for _, l := range listeners {
			_ = l.socket.Close()
		}
	}()
	for _, spec := range []struct {
		enabled                     bool
		address, transport, framing string
	}{
		{r.settings.TLS.Enabled, r.settings.TLS.Listen, "tls", "octet_counting"},
		{r.settings.TCP.Enabled, r.settings.TCP.Listen, "tcp", r.settings.TCP.Framing},
	} {
		if !spec.enabled {
			continue
		}
		l, err := (&net.ListenConfig{}).Listen(runCtx, "tcp", spec.address)
		if err != nil {
			r.failure("syslog_listener_bind_failed")
			return errors.New("syslog_listener_bind_failed")
		}
		listeners = append(listeners, listener{l, spec.transport, spec.framing})
	}
	r.mu.Lock()
	for _, l := range listeners {
		r.stats.Listening = append(r.stats.Listening, l.socket.Addr().String())
	}
	r.mu.Unlock()
	var accepts, workers sync.WaitGroup
	failed := make(chan error, len(listeners))
	for _, l := range listeners {
		accepts.Add(1)
		go func() {
			defer accepts.Done()
			for runCtx.Err() == nil {
				conn, err := l.socket.Accept()
				if err != nil {
					if runCtx.Err() == nil {
						failed <- errors.New("syslog_listener_accept_failed")
					}
					return
				}
				peer, ok := peerIP(conn)
				if !ok || !r.reserve(conn, peer) {
					_ = conn.Close()
					continue
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer conn.Close()
					defer r.release(conn, peer)
					r.serve(runCtx, conn, peer, l.transport, l.framing)
				}()
			}
		}()
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-failed:
		r.failure("syslog_listener_accept_failed")
	}
	cancel()
	for _, l := range listeners {
		_ = l.socket.Close()
	}
	// Wait for Add calls to stop before waiting on the connection workers.
	accepts.Wait()
	r.mu.Lock()
	for conn := range r.connections {
		_ = conn.Close()
	}
	r.mu.Unlock()
	workers.Wait()
	return result
}

func peerIP(conn net.Conn) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	ip, parseErr := netip.ParseAddr(host)
	return ip.Unmap(), err == nil && parseErr == nil
}

func matchesCIDRs(ip netip.Addr, cidrs []string) bool {
	for _, value := range cidrs {
		prefix, _ := netip.ParsePrefix(value)
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func (r *Receiver) reserve(conn net.Conn, peer netip.Addr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.connections) >= r.settings.Limits.MaxConnections || r.peers[peer.String()] >= r.settings.Limits.MaxConnectionsPerSource || len(r.settings.AllowedPeers) > 0 && !matchesCIDRs(peer, r.settings.AllowedPeers) {
		r.stats.Rejected++
		r.stats.LastError = "syslog_connection_rejected"
		return false
	}
	r.connections[conn] = ""
	r.peers[peer.String()]++
	r.stats.Accepted++
	r.stats.ActiveConnections = len(r.connections)
	r.stats.PeakConnections = max(r.stats.PeakConnections, len(r.connections))
	return true
}

func (r *Receiver) release(conn net.Conn, peer netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if source := r.connections[conn]; source != "" {
		r.sources[source]--
		if r.sources[source] == 0 {
			delete(r.sources, source)
		}
	}
	delete(r.connections, conn)
	r.peers[peer.String()]--
	if r.peers[peer.String()] == 0 {
		delete(r.peers, peer.String())
	}
	r.stats.ActiveConnections = len(r.connections)
}

func (r *Receiver) attribute(peer netip.Addr, state *tls.ConnectionState) (*Source, string, bool, error) {
	authenticated := state != nil && len(state.VerifiedChains) > 0 && len(state.PeerCertificates) > 0
	identity := ""
	if authenticated {
		digest := sha256.Sum256(state.PeerCertificates[0].Raw)
		identity = "sha256:" + hex.EncodeToString(digest[:])
	}
	var selected *Source
	for i := range r.settings.Sources {
		src := &r.settings.Sources[i]
		if len(src.PeerCIDRs) > 0 && !matchesCIDRs(peer, src.PeerCIDRs) {
			continue
		}
		if src.TLSIdentity != "" {
			if !authenticated || !matchesIdentity(state.PeerCertificates[0], src.TLSIdentity) {
				continue
			}
		}
		if selected != nil {
			return nil, "", authenticated, errors.New("syslog_ambiguous_source")
		}
		selected = src
	}
	if selected == nil && r.settings.UnknownSourcePolicy == "reject" {
		return nil, "", authenticated, errors.New("syslog_unknown_source")
	}
	if selected != nil && selected.TLSIdentity != "" {
		identity = selected.TLSIdentity
	}
	return selected, identity, authenticated, nil
}

func matchesIdentity(cert *x509.Certificate, identity string) bool {
	if strings.HasPrefix(identity, "dns:") {
		for _, name := range cert.DNSNames {
			if strings.EqualFold("dns:"+name, identity) {
				return true
			}
		}
	}
	if strings.HasPrefix(identity, "uri:") {
		for _, uri := range cert.URIs {
			if "uri:"+uri.String() == identity {
				return true
			}
		}
	}
	return false
}

func (r *Receiver) serve(ctx context.Context, raw net.Conn, peer netip.Addr, transport, framing string) {
	conn := raw
	var state *tls.ConnectionState
	if transport == "tls" {
		tlsConn := tls.Server(raw, r.tlsConfig)
		_ = raw.SetDeadline(time.Now().Add(r.settings.Limits.ReadTimeout))
		handshakeCtx, cancel := context.WithTimeout(ctx, r.settings.Limits.ReadTimeout)
		err := tlsConn.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			r.mu.Lock()
			r.stats.Rejected++
			r.mu.Unlock()
			r.failure("syslog_tls_handshake_failed")
			return
		}
		_ = raw.SetDeadline(time.Time{})
		value := tlsConn.ConnectionState()
		conn, state = tlsConn, &value
	}
	source, identity, authenticated, err := r.attribute(peer, state)
	if err != nil {
		r.mu.Lock()
		r.stats.Rejected++
		r.mu.Unlock()
		r.failure(err.Error())
		return
	}
	if source != nil {
		r.mu.Lock()
		if r.sources[source.ID] >= r.settings.Limits.MaxConnectionsPerSource {
			r.stats.Rejected++
			r.stats.LastError = "syslog_source_connection_limit"
			r.mu.Unlock()
			return
		}
		r.sources[source.ID]++
		r.connections[raw] = source.ID
		r.mu.Unlock()
	}
	reader := bufio.NewReaderSize(conn, 4096)
	privateKey := false
	for ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(r.settings.Limits.IdleTimeout))
		if _, err := reader.Peek(1); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				r.failure("syslog_idle_or_read_timeout")
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(r.settings.Limits.ReadTimeout))
		frame, err := ReadFrame(reader, framing, r.settings.Limits.MaxMessageBytes)
		if err != nil {
			if ctx.Err() == nil {
				category := "syslog_incomplete_or_invalid_frame"
				if errors.Is(err, ErrOversized) {
					category = ErrOversized.Error()
				}
				r.failure(category)
			}
			return
		}
		observed := time.Now().UTC()
		r.mu.Lock()
		r.stats.Frames++
		r.mu.Unlock()
		envelope, err := r.normalize(frame, source, peer, transport, identity, authenticated, observed, &privateKey)
		if err != nil {
			r.failure("syslog_normalization_failed")
			return
		}
		admitCtx, cancel := context.WithTimeout(ctx, r.settings.Limits.AdmissionTimeout)
		err = r.admit(admitCtx, envelope)
		cancel()
		if err != nil {
			r.mu.Lock()
			r.stats.AdmissionFailures++
			r.stats.LastError = "syslog_durable_admission_failed"
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		r.stats.Admitted++
		r.stats.LastError = ""
		r.mu.Unlock()
	}
}

func (r *Receiver) normalize(frame []byte, source *Source, peer netip.Addr, transport, identity string, authenticated bool, observed time.Time, privateKey *bool) (model.Envelope, error) {
	text := string(frame)
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "-----BEGIN ") && strings.Contains(upper, "PRIVATE KEY-----") {
		*privateKey = true
	}
	if *privateKey {
		if strings.Contains(upper, "-----END ") && strings.Contains(upper, "PRIVATE KEY-----") {
			*privateKey = false
		}
		text = "[REDACTED PRIVATE KEY]"
	}
	text = r.options.Sanitize(text)
	if len(text) > r.settings.Limits.MaxMessageBytes {
		return model.Envelope{}, ErrOversized
	}
	record := logs.Parse(text, "syslog", observed)
	if record.Syslog == nil {
		record.Syslog = &model.SyslogMetadata{Format: "unknown", ParseStatus: "fallback"}
	}
	if record.Syslog.ParseStatus == "fallback" {
		r.mu.Lock()
		r.stats.ParseFallbacks++
		r.mu.Unlock()
	}
	if !utf8.ValidString(text) {
		record.Syslog.RawPayload = []byte(text)
		record.Raw = strings.ToValidUTF8(record.Raw, "\ufffd")
		record.Message = strings.ToValidUTF8(record.Message, "\ufffd")
	}
	if len(record.Raw) > 65536 || len(record.Message) > 65536 {
		return model.Envelope{}, ErrOversized
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return model.Envelope{}, err
	}
	record.ID = id.String()
	record.Source, record.SourceID, record.DeviceID = "syslog", "unmanaged", "syslog:unmanaged"
	ref := model.SourceRef{Type: model.SourceSyslog, CollectorID: r.options.CollectorID, Protocol: "syslog-" + transport, InstanceID: "peer:" + peer.String(), Provenance: model.ProvenanceReal}
	if source != nil {
		record.SourceID, record.DeviceID = source.ID, "syslog:"+source.ID
		ref.TargetID, ref.InstanceID = source.Target(true).ID, source.ID
		if source.Provenance != "" {
			ref.Provenance = source.Provenance
		}
		for _, app := range source.AuditApps {
			if record.Process == app {
				if logs.AttachAudit(&record) {
					ref.Type = model.SourceAuditd
				}
				break
			}
		}
	}
	if ref.Provenance == model.ProvenanceSimulated {
		record.Source = "simulated"
	}
	record.Syslog.PeerIP, record.Syslog.Transport, record.Syslog.TLSIdentity = peer.String(), transport, identity
	record.Syslog.Authenticated, record.Syslog.Managed = authenticated, source != nil
	collected := time.Now().UTC()
	record.Observation = model.NewObservation(ref, record.Source, observed, collected, record.OriginalTimestamp)
	batch := model.LogBatch{ObservedAt: observed, Source: record.Source, Records: []model.LogRecord{record}, Observation: model.NewObservation(ref, record.Source, observed, collected, nil)}
	encoded, err := json.Marshal(batch)
	if err != nil || len(encoded) > r.options.MaxBatchBytes {
		return model.Envelope{}, errors.New("syslog_batch_exceeds_limit")
	}
	messageID, err := uuid.NewRandom()
	if err != nil {
		return model.Envelope{}, err
	}
	return model.Envelope{MessageID: messageID.String(), Logs: &batch}, nil
}

func (r *Receiver) admit(ctx context.Context, envelope model.Envelope) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.options.Emit(ctx, envelope)
		if err == nil {
			return nil
		}
		if !errors.Is(err, queue.ErrFull) {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *Receiver) failure(category string) { r.mu.Lock(); r.stats.LastError = category; r.mu.Unlock() }

func (r *Receiver) Snapshot() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.stats
	stats.Listening = append([]string(nil), stats.Listening...)
	return stats
}
