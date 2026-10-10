// Package telephony adapts the existing fixed SIP OPTIONS collector to the
// shared remote lifecycle. It adds no listener, authentication or execution DSL.
package telephony

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	sip "InfraPulseAgent/internal/telephony"
)

type Adapter struct {
	collector *sip.Collector
	lifecycle context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func New(endpoint sip.Endpoint, source model.SourceRef) (*Adapter, error) {
	endpoint = endpoint.WithDefaults()
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if endpoint.TLS.CAFile != "" {
		info, err := os.Stat(endpoint.TLS.CAFile)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("SIP CA is not a regular file")
		}
		f, err := os.Open(endpoint.TLS.CAFile)
		if err != nil {
			return nil, errors.New("SIP CA unavailable")
		}
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, errors.New("SIP CA is not a regular file")
		}
		data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("SIP CA unavailable or exceeds limit")
		}
		if !x509.NewCertPool().AppendCertsFromPEM(data) {
			return nil, errors.New("SIP CA has no certificates")
		}
	}
	c, err := sip.New(endpoint, source)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Adapter{collector: c, lifecycle: ctx, cancel: cancel}, nil
}

func (a *Adapter) Close() error { a.closeOnce.Do(a.cancel); return nil }
func (a *Adapter) Capabilities() []model.Capability {
	return []model.Capability{{Name: "telephony.options", Supported: true}, {Name: "telephony.health", Supported: true}}
}
func (a *Adapter) State() model.TelephonyHealth { return a.collector.State() }
func (a *Adapter) CollectTelephony(parent context.Context) (model.TelephonyUpdate, []model.Event, error) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(a.lifecycle, cancel)
	defer func() { stop(); cancel() }()
	if err := a.lifecycle.Err(); err != nil {
		return model.TelephonyUpdate{}, nil, err
	}
	h, events, err := a.collector.Collect(ctx)
	return model.TelephonyUpdate{ObservedAt: time.Now().UTC(), Source: "sip.options", Targets: []model.TelephonyHealth{h}}, events, err
}

var _ driver.TelephonyReader = (*Adapter)(nil)
var _ driver.Adapter = (*Adapter)(nil)
