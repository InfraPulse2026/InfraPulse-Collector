// infrapulse-mock-enrollment is an explicit development signing endpoint.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"InfraPulseAgent/internal/pki"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "infrapulse-mock-enrollment:", err)
		os.Exit(1)
	}
}
func run() error {
	address := flag.String("address", "127.0.0.1:9444", "development HTTPS loopback listener")
	certFile := flag.String("cert-file", "", "HTTPS server certificate PEM")
	keyFile := flag.String("key-file", "", "HTTPS server private key PEM")
	caFile := flag.String("ca-file", "", "development signing CA PEM")
	caKeyFile := flag.String("ca-key-file", "", "development signing CA private key PEM")
	agentID := flag.String("agent-id", "", "persistent agent UUID bound to token")
	tokenEnv := flag.String("token-env", "INFRAPULSE_ENROLLMENT_TOKEN", "one-time token environment variable")
	tokenLifetime := flag.Duration("token-lifetime", 15*time.Minute, "token expiration from startup")
	certLifetime := flag.Duration("cert-lifetime", 30*24*time.Hour, "issued client certificate lifetime")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("mock enrollment requires literal loopback listener")
	}
	if *tokenLifetime < time.Second || *tokenLifetime > 24*time.Hour {
		return errors.New("invalid token lifetime")
	}
	ca, err := os.ReadFile(*caFile)
	if err != nil {
		return errors.New("mock signing CA unavailable")
	}
	caKey, err := os.ReadFile(*caKeyFile)
	if err != nil {
		return errors.New("mock signing key unavailable")
	}
	token, ok := os.LookupEnv(*tokenEnv)
	if !ok || token == "" {
		return errors.New("mock token unavailable")
	}
	handler, err := pki.NewMockEnrollment(ca, caKey, []pki.Token{{Value: token, AgentID: *agentID, ExpiresAt: time.Now().Add(*tokenLifetime)}}, *certLifetime)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return errors.New("mock HTTPS certificate unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("invalid mock CA")
	}
	// Enrollment authenticates a token; renewal additionally requires a verified
	// client certificate. Requests without a client certificate can reach /enroll.
	server := &http.Server{Addr: *address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS("", "") }()
	select {
	case err = <-done:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
