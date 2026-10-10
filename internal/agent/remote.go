package agent

import (
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	driveripmi "InfraPulseAgent/internal/driver/ipmi"
	driverredfish "InfraPulseAgent/internal/driver/redfish"
	driverrouteros "InfraPulseAgent/internal/driver/routeros"
	driversnmp "InfraPulseAgent/internal/driver/snmp"
	driversip "InfraPulseAgent/internal/driver/telephony"
	"InfraPulseAgent/internal/model"
	protocolrouteros "InfraPulseAgent/internal/protocol/routeros"
	protocolsnmp "InfraPulseAgent/internal/protocol/snmp"
	"InfraPulseAgent/internal/remote"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// RemoteAdapterFactory is the compile-time protocol registration point. New
// adapters implement only their supported readers and return normalized models.
func RemoteAdapterFactory(cfg config.Config, provider credentials.Provider, collectorIDs ...string) remote.AdapterFactory {
	registry := driver.NewRegistry[config.Device]()
	options := func(dc config.Device) (driver.Options, time.Duration) {
		hint := dc.VendorHint
		if hint == "" {
			hint = dc.VendorProfile
		}
		opts := driver.Options{ID: dc.ID, Name: dc.Name, Address: dc.Address, VendorHint: hint, Location: dc.Location, MaxRows: cfg.Remote.MaxRows, MaxResponseBytes: cfg.Remote.MaxResponseBytes, TopologyTTLSeconds: uint32(cfg.Remote.TopologyTTL / time.Second), EnableMutations: dc.EnableMutations}
		timeout := dc.Polling.Timeout
		if timeout == 0 {
			timeout = cfg.Telemetry.CollectTimeout
		}
		return opts, timeout
	}
	snmpRegistration := registry.Register("snmp", func(dc config.Device) (driver.Adapter, error) {
		opts, timeout := options(dc)
		port := dc.SNMP.Port
		if port == 0 {
			port = 161
		}
		host := dc.Address
		if dc.DialAddress != "" {
			host = dc.DialAddress
		}
		return driversnmp.New(opts, provider, dc.CredentialRef, protocolsnmp.Options{Security: dc.SNMP.Security, Address: net.JoinHostPort(host, strconv.Itoa(int(port))), Timeout: timeout, Retries: dc.SNMP.Retries, MaxRows: opts.MaxRows, MaxResponseBytes: opts.MaxResponseBytes})
	})
	routerosRegistration := registry.Register("routeros", func(dc config.Device) (driver.Adapter, error) {
		opts, timeout := options(dc)
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if dc.TLS.CAFile != "" {
			data, err := readRemoteCA(dc.TLS.CAFile)
			if err != nil {
				return nil, err
			}
			tc.RootCAs = x509.NewCertPool()
			if !tc.RootCAs.AppendCertsFromPEM(data) {
				return nil, errors.New("remote CA file has no certificates")
			}
		}
		return driverrouteros.New(opts, provider, dc.CredentialRef, protocolrouteros.Options{Address: dc.Address, DialAddress: dc.DialAddress, TLSConfig: tc, Timeout: timeout, MaxResponseBytes: opts.MaxResponseBytes, MaxRecords: opts.MaxRows})
	})
	redfishRegistration := registry.Register("redfish", func(dc config.Device) (driver.Adapter, error) {
		opts, _ := options(dc)
		target := dc.Target(config.Polling{})
		tc, err := redfishTLS(dc.TLS.CAFile)
		if err != nil {
			return nil, err
		}
		rf := dc.Redfish.WithDefaults()
		return driverredfish.New(opts, provider, dc.CredentialRef, driverredfish.Options{
			DialAddress: dc.DialAddress,
			TLSConfig:   tc, Timeout: target.Polling.Timeout, RequestTimeout: rf.RequestTimeout,
			Auth: rf.Auth, MaxResources: rf.MaxResources, MaxPages: rf.MaxPages, MaxRequests: rf.MaxRequests,
			MaxResponseBytes: opts.MaxResponseBytes, Provenance: model.Provenance(rf.Provenance),
		})
	})
	ipmiRegistration := registry.Register("ipmi", func(dc config.Device) (driver.Adapter, error) {
		opts, _ := options(dc)
		target := dc.Target(config.Polling{})
		i := dc.IPMI.WithDefaults()
		return driveripmi.New(opts, provider, dc.CredentialRef, driveripmi.Options{
			DialAddress: dc.DialAddress,
			Port:        *i.Port, CipherSuite: *i.CipherSuite, Privilege: i.Privilege, Retries: i.Retries,
			Timeout: target.Polling.Timeout, RequestTimeout: i.RequestTimeout,
			MaxSessionDuration: i.MaxSessionDuration, MaxOperations: i.MaxOperations,
			MaxSDRRecords: i.MaxSDRRecords, MaxSDRBytes: i.MaxSDRBytes,
			MaxFRURecords: i.MaxFRURecords, MaxFRUBytes: i.MaxFRUBytes,
			MaxPollBytes: i.MaxPollBytes, SDRCacheTTL: i.SDRCacheTTL,
			MaxResponseBytes: min(opts.MaxResponseBytes, 4096), Provenance: model.Provenance(i.Provenance),
		})
	})
	sipRegistration := registry.Register("sip", func(dc config.Device) (driver.Adapter, error) {
		if dc.SIP == nil {
			return nil, errors.New("SIP settings required")
		}
		ep := *dc.SIP
		ep.DialAddress = dc.DialAddress
		ref := model.SourceRef{Type: model.SourceSIP, TargetID: "remote:" + dc.ID, Protocol: "sip", InstanceID: dc.ID, Provenance: model.ProvenanceReal}
		if len(collectorIDs) > 0 {
			ref.CollectorID = collectorIDs[0]
		}
		return driversip.New(ep, ref)
	})
	registrationErr := errors.Join(snmpRegistration, routerosRegistration, redfishRegistration, ipmiRegistration, sipRegistration)
	return func(dc config.Device) (driver.Adapter, error) {
		if registrationErr != nil {
			return nil, registrationErr
		}
		return registry.Create(dc.Driver, dc)
	}
}

func redfishTLS(caFile string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return tc, nil
	}
	data, err := readRemoteCA(caFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("Redfish CA file has no certificates")
	}
	tc.RootCAs = pool
	return tc, nil
}

// Trust files are local administrator-owned regular files. Reject pipes and
// devices before opening them so preparation cannot wait for a FIFO writer.
func readRemoteCA(path string) ([]byte, error) {
	// Administrator trust paths may be symlinks used by certificate rotation.
	// Inspect their final target, preserving existing static-target behavior.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("remote CA file must be regular and bounded")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("remote CA file unavailable")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("remote CA file must be regular and bounded")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("remote CA file unavailable or exceeds limit")
	}
	return data, nil
}

// RemoteFactory keeps the historical full-driver API for bounded probe callers.
// The Collector runtime uses RemoteAdapterFactory directly.
func RemoteFactory(cfg config.Config, provider credentials.Provider) remote.Factory {
	factory := RemoteAdapterFactory(cfg, provider)
	return func(dc config.Device) (driver.Driver, error) {
		adapter, err := factory(dc)
		if err != nil {
			return nil, err
		}
		d, ok := adapter.(driver.Driver)
		if !ok {
			_ = adapter.Close()
			return nil, errors.New("remote adapter does not provide the legacy driver contract")
		}
		return d, nil
	}
}

// ProbeDevice performs a real bounded read without loading local identity or
// connecting to Controller. Credentials are never part of the returned Device.
func ProbeDevice(ctx context.Context, cfg config.Config, id string) (model.Device, error) {
	for _, dc := range cfg.Devices {
		if dc.ID == id {
			d, err := RemoteAdapterFactory(cfg, credentials.NewEnvironment(cfg.Credentials))(dc)
			if err != nil {
				return model.Device{}, err
			}
			defer d.Close()
			reader, ok := d.(driver.ProbeReader)
			if !ok {
				return model.Device{}, driver.ErrUnavailable
			}
			timeout := dc.Polling.Timeout
			if timeout == 0 {
				timeout = cfg.Telemetry.CollectTimeout
			}
			if dc.Driver == "ipmi" {
				timeout = dc.Target(config.Polling{}).Polling.Timeout
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			observed := time.Now().UTC()
			device, err := reader.Probe(ctx)
			if err != nil {
				return device, err
			}
			target := dc.Target(config.Polling{Timeout: timeout})
			ref := model.SourceRef{Type: target.Type, TargetID: target.ID, Protocol: target.Protocol, Provenance: model.ProvenanceReal}
			// Standalone probes intentionally have no loaded Collector identity.
			return model.ObserveDevice(device, ref, observed, time.Now().UTC()), nil
		}
	}
	return model.Device{}, errors.New("remote device is not configured")
}

func (a *Agent) handleCommand(ctx context.Context, c model.Command) error {
	return a.executePersistentCommand(ctx, c)
}
