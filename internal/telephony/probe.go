package telephony

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"InfraPulseAgent/internal/model"
)

const (
	maxResponseBytes = 32 << 10
	maxHeaderLines   = 256
)

var errMalformed = errors.New("malformed SIP response")
var errUnmatched = errors.New("SIP response belongs to another transaction")

// Probe sends only OPTIONS. A syntactically valid matching response proves SIP
// reachability, while only 2xx proves success of this unauthenticated probe.
func Probe(parent context.Context, endpoint Endpoint) model.TelephonyHealth {
	e := endpoint.WithDefaults()
	h := model.TelephonyHealth{TargetID: e.ID, Address: net.JoinHostPort(e.Address, strconv.Itoa(int(e.Port))), Transport: e.Transport, ObservedAt: time.Now().UTC(), State: "degraded", Source: "sip.options"}
	if err := e.Validate(); err != nil {
		h.ErrorCategory = "configuration"
		return h
	}
	ctx, cancel := context.WithTimeout(parent, e.Timeout)
	defer cancel()
	started := time.Now()
	transport := e.Transport
	var tlsConfig *tls.Config
	if transport == "tls" {
		transport = "tcp"
		var err error
		tlsConfig, err = e.tlsConfig()
		if err != nil {
			h.ErrorCategory = "tls_configuration"
			return h
		}
	}
	dialAddress := h.Address
	if e.DialAddress != "" {
		dialAddress = net.JoinHostPort(e.DialAddress, strconv.Itoa(int(e.Port)))
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, transport, dialAddress)
	if err != nil {
		h.ErrorCategory = category(parent, ctx, err)
		return h
	}
	defer conn.Close()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if tlsConfig != nil {
		secured := tls.Client(conn, tlsConfig)
		if err = secured.HandshakeContext(ctx); err != nil {
			h.ErrorCategory = category(parent, ctx, err)
			return h
		}
		conn = secured
		defer secured.Close()
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		h.ErrorCategory = "internal"
		return h
	}
	transaction := hex.EncodeToString(nonce[:])
	branch := "z9hG4bK" + transaction
	callID := transaction + "@infrapulse"
	domain := e.Domain
	if domain == "" {
		domain = e.Address
	}
	if strings.Contains(domain, ":") {
		domain = "[" + domain + "]"
	}
	to := domain + ":" + strconv.Itoa(int(e.Port))
	if e.ToUser != "" {
		to = e.ToUser + "@" + to
	}
	scheme := "sip"
	if e.Transport == "tls" {
		scheme = "sips"
	}
	request := fmt.Sprintf("OPTIONS %s:%s SIP/2.0\r\nVia: SIP/2.0/%s %s;branch=%s;rport\r\nMax-Forwards: 70\r\nFrom: <%s:%s@%s>;tag=%s\r\nTo: <%s:%s>\r\nCall-ID: %s\r\nCSeq: 1 OPTIONS\r\nUser-Agent: InfraPulse-Agent\r\nAccept: application/sdp\r\nContent-Length: 0\r\n\r\n", scheme, to, strings.ToUpper(e.Transport), conn.LocalAddr().String(), branch, scheme, e.FromUser, domain, transaction, scheme, to, callID)
	if _, err = io.WriteString(conn, request); err != nil {
		h.ErrorCategory = category(parent, ctx, err)
		return h
	}
	var code uint32
	if e.Transport == "udp" {
		code, err = readUDP(ctx, conn, request, callID, branch, deadline)
	} else {
		code, err = readTCP(conn, callID, branch)
	}
	if err != nil {
		h.ErrorCategory = category(parent, ctx, err)
		return h
	}
	latency := float64(time.Since(started)) / float64(time.Millisecond)
	h.Reachable = true
	h.ResponseCode = &code
	h.LatencyMillis = &latency
	switch {
	case code >= 200 && code < 300:
		h.State = "up"
	case code == 401 || code == 407:
		h.ErrorCategory = "authentication_required"
	case code >= 600:
		h.ErrorCategory = "global_failure"
	case code >= 500:
		h.ErrorCategory = "server_error"
	case code >= 300 && code < 400:
		h.ErrorCategory = "redirect"
	default:
		h.ErrorCategory = "request_rejected"
	}
	return h
}

func category(parent, ctx context.Context, err error) string {
	if parent.Err() != nil {
		return "canceled"
	}
	if ctx.Err() != nil {
		return "timeout"
	}
	if errors.Is(err, errMalformed) {
		return "malformed_sip"
	}
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return "tls_certificate"
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "tls_protocol"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsTimeout {
			return "timeout"
		}
		return "dns_failure"
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout"
	}
	return "connection_failure"
}

func readUDP(ctx context.Context, conn net.Conn, request, callID, branch string, deadline time.Time) (uint32, error) {
	buffer := make([]byte, maxResponseBytes+1)
	delay := 500 * time.Millisecond // RFC 3261 non-INVITE Timer E, capped at T2.
	nextSend := time.Now().Add(delay)
	for packets := 0; packets < 64; packets++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		readDeadline := deadline
		if nextSend.Before(readDeadline) {
			readDeadline = nextSend
		}
		_ = conn.SetReadDeadline(readDeadline)
		n, err := conn.Read(buffer)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && time.Now().Before(deadline) && ctx.Err() == nil {
				if _, err = io.WriteString(conn, request); err != nil {
					return 0, err
				}
				delay *= 2
				if delay > 4*time.Second {
					delay = 4 * time.Second
				}
				nextSend = time.Now().Add(delay)
				continue
			}
			return 0, err
		}
		if n > maxResponseBytes {
			return 0, errMalformed
		}
		code, err := parseResponse(buffer[:n], false, callID, branch)
		if errors.Is(err, errUnmatched) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if code < 200 {
			delay = 4 * time.Second
			nextSend = time.Now().Add(delay)
			continue
		}
		return code, nil
	}
	return 0, errMalformed
}

func readTCP(conn net.Conn, callID, branch string) (uint32, error) {
	r := bufio.NewReaderSize(conn, 4096)
	for messages := 0; messages < 8; messages++ {
		var message []byte
		readBytes, lines := 0, 0
		for {
			part, err := r.ReadSlice('\n')
			readBytes += len(part)
			if readBytes > maxResponseBytes {
				return 0, errMalformed
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				lines++
				if lines > maxHeaderLines {
					return 0, errMalformed
				}
			}
			// RFC 3261 section 7.5 requires ignoring CRLF before a stream
			// start-line. Count skipped bytes/lines against the same bounds.
			if len(message) == 0 && err == nil && string(part) == "\r\n" {
				continue
			}
			message = append(message, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				if len(message) > 0 && errors.Is(err, io.EOF) {
					return 0, errMalformed
				}
				return 0, err
			}
			if len(message) >= 4 && strings.HasSuffix(string(message), "\r\n\r\n") {
				break
			}
		}
		_, headers, err := parseHeaders(message)
		if err != nil {
			return 0, err
		}
		length, ok := headers["content-length"]
		if !ok {
			return 0, errMalformed
		}
		count, err := contentLength(length)
		if err != nil || count < 0 || count > maxResponseBytes-readBytes {
			return 0, errMalformed
		}
		body := make([]byte, count)
		if _, err = io.ReadFull(r, body); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, errMalformed
			}
			return 0, err
		}
		message = append(message, body...)
		code, err := parseResponse(message, true, callID, branch)
		if errors.Is(err, errUnmatched) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if code < 200 {
			continue
		}
		return code, nil
	}
	return 0, errMalformed
}

func parseHeaders(data []byte) (uint32, map[string]string, error) {
	if len(data) > maxResponseBytes {
		return 0, nil, errMalformed
	}
	end := strings.Index(string(data), "\r\n\r\n")
	if end < 0 || end > maxResponseBytes {
		return 0, nil, errMalformed
	}
	lines := strings.Split(string(data[:end]), "\r\n")
	if len(lines) > maxHeaderLines {
		return 0, nil, errMalformed
	}
	for _, b := range data[:end] {
		if b < 32 && b != '\r' && b != '\n' && b != '\t' || b == 127 {
			return 0, nil, errMalformed
		}
	}
	status := strings.SplitN(lines[0], " ", 3)
	if len(status) != 3 || status[0] != "SIP/2.0" || len(status[1]) != 3 || strings.ContainsAny(status[2], "\r\n") {
		return 0, nil, errMalformed
	}
	code, err := strconv.ParseUint(status[1], 10, 32)
	if err != nil || code < 100 || code > 699 {
		return 0, nil, errMalformed
	}
	headers := make(map[string]string)
	// Unfold only the RFC-defined CRLF followed by SP/HTAB. Keep rows
	// separate so duplicate transaction headers remain an explicit error.
	rows := make([]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if len(rows) == 0 {
				return 0, nil, errMalformed
			}
			rows[len(rows)-1] += " " + strings.TrimLeft(line, " \t")
		} else {
			rows = append(rows, line)
		}
	}
	for _, line := range rows {
		name, value, ok := strings.Cut(line, ":")
		name = strings.TrimRight(name, " \t")
		if !ok || !headerToken(name) {
			return 0, nil, errMalformed
		}
		name = strings.ToLower(name)
		switch name {
		case "v":
			name = "via"
		case "i":
			name = "call-id"
		case "f":
			name = "from"
		case "t":
			name = "to"
		case "l":
			name = "content-length"
		}
		value = strings.Trim(value, " \t")
		if strings.ContainsAny(value, "\r\n\x00") {
			return 0, nil, errMalformed
		}
		if _, exists := headers[name]; exists {
			if name != "call-id" && name != "cseq" && name != "from" && name != "to" && name != "content-length" {
				continue
			}
			return 0, nil, errMalformed
		}
		headers[name] = value
	}
	return uint32(code), headers, nil
}

func parseResponse(data []byte, stream bool, callID, branch string) (uint32, error) {
	code, h, err := parseHeaders(data)
	if err != nil {
		return 0, err
	}
	for _, key := range []string{"via", "from", "to", "call-id", "cseq"} {
		if h[key] == "" {
			return 0, errMalformed
		}
	}
	cseq := strings.Fields(h["cseq"])
	if len(cseq) != 2 {
		return 0, errMalformed
	}
	if h["call-id"] != callID || cseq[0] != "1" || cseq[1] != "OPTIONS" {
		return 0, errUnmatched
	}
	matchedBranch := false
	topVia := strings.SplitN(h["via"], ",", 2)[0]
	branchSeen := false
	for _, param := range strings.Split(topVia, ";")[1:] {
		name, value, ok := strings.Cut(param, "=")
		if ok && strings.EqualFold(strings.Trim(name, " \t"), "branch") {
			if branchSeen {
				return 0, errMalformed
			}
			branchSeen = true
			matchedBranch = strings.Trim(value, " \t") == branch
		}
	}
	if !matchedBranch {
		return 0, errUnmatched
	}
	via := strings.Fields(strings.SplitN(topVia, ";", 2)[0])
	if len(via) != 2 || !strings.EqualFold(via[0], "SIP/2.0/UDP") && !strings.EqualFold(via[0], "SIP/2.0/TCP") && !strings.EqualFold(via[0], "SIP/2.0/TLS") {
		return 0, errMalformed
	}
	if length, ok := h["content-length"]; ok {
		count, err := contentLength(length)
		bodyStart := strings.Index(string(data), "\r\n\r\n") + 4
		if err != nil || count < 0 || count > len(data)-bodyStart {
			return 0, errMalformed
		}
		if stream && count != len(data)-bodyStart {
			return 0, errMalformed
		}
	} else if stream {
		return 0, errMalformed
	}
	return code, nil
}

func (e Endpoint) tlsConfig() (*tls.Config, error) {
	name := e.TLS.ServerName
	if name == "" {
		name = e.Address
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}
	if e.TLS.CAFile == "" {
		return c, nil
	}
	f, err := os.Open(e.TLS.CAFile)
	if err != nil {
		return nil, errors.New("SIP CA unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("SIP CA unavailable or exceeds limit")
	}
	c.RootCAs = x509.NewCertPool()
	if !c.RootCAs.AppendCertsFromPEM(data) {
		return nil, errors.New("SIP CA has no certificates")
	}
	return c, nil
}

func contentLength(value string) (int, error) {
	if value == "" {
		return 0, errMalformed
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errMalformed
		}
	}
	count, err := strconv.Atoi(value)
	if err != nil || count > maxResponseBytes {
		return 0, errMalformed
	}
	return count, nil
}

func headerToken(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-.!%*_+`'~", c)) {
			return false
		}
	}
	return true
}
