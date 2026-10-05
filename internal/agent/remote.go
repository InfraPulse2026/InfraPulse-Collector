package agent

import (
	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/driver"
	driverrouteros "InfraPulseAgent/internal/driver/routeros"
	driversnmp "InfraPulseAgent/internal/driver/snmp"
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

func RemoteFactory(cfg config.Config, provider credentials.Provider) remote.Factory {
	return func(dc config.Device) (driver.Driver, error) {
		hint := dc.VendorHint
		if hint == "" {
			hint = dc.VendorProfile
		}
		opts := driver.Options{ID: dc.ID, Name: dc.Name, Address: dc.Address, VendorHint: hint, Location: dc.Location, MaxRows: cfg.Remote.MaxRows, MaxResponseBytes: cfg.Remote.MaxResponseBytes, TopologyTTLSeconds: uint32(cfg.Remote.TopologyTTL / time.Second), EnableMutations: dc.EnableMutations}
		timeout := dc.Polling.Timeout
		if timeout == 0 {
			timeout = cfg.Telemetry.CollectTimeout
		}
		switch dc.Driver {
		case "snmp":
			port := dc.SNMP.Port
			if port == 0 {
				port = 161
			}
			return driversnmp.New(opts, provider, dc.CredentialRef, protocolsnmp.Options{Security: dc.SNMP.Security, Address: net.JoinHostPort(dc.Address, strconv.Itoa(int(port))), Timeout: timeout, Retries: dc.SNMP.Retries, MaxRows: opts.MaxRows, MaxResponseBytes: opts.MaxResponseBytes})
		case "routeros":
			tc := &tls.Config{MinVersion: tls.VersionTLS12}
			if dc.TLS.CAFile != "" {
				file, err := os.Open(dc.TLS.CAFile)
				if err != nil {
					return nil, errors.New("remote CA file unavailable")
				}
				data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
				_ = file.Close()
				if err != nil {
					return nil, errors.New("remote CA file unavailable")
				}
				if len(data) > 1<<20 {
					return nil, errors.New("remote CA file exceeds limit")
				}
				tc.RootCAs = x509.NewCertPool()
				if !tc.RootCAs.AppendCertsFromPEM(data) {
					return nil, errors.New("remote CA file has no certificates")
				}
			}
			return driverrouteros.New(opts, provider, dc.CredentialRef, protocolrouteros.Options{Address: dc.Address, TLSConfig: tc, Timeout: timeout, MaxResponseBytes: opts.MaxResponseBytes, MaxRecords: opts.MaxRows})
		default:
			return nil, errors.New("remote driver unsupported")
		}
	}
}

// ProbeDevice performs a real bounded read without loading local identity or
// connecting to Controller. Credentials are never part of the returned Device.
func ProbeDevice(ctx context.Context, cfg config.Config, id string) (model.Device, error) {
	for _, dc := range cfg.Devices {
		if dc.ID == id {
			d, err := RemoteFactory(cfg, credentials.NewEnvironment(cfg.Credentials))(dc)
			if err != nil {
				return model.Device{}, err
			}
			defer d.Close()
			timeout := dc.Polling.Timeout
			if timeout == 0 {
				timeout = cfg.Telemetry.CollectTimeout
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return d.Probe(ctx)
		}
	}
	return model.Device{}, errors.New("remote device is not configured")
}

func (a *Agent) handleCommand(ctx context.Context, c model.Command) error {
	return a.executePersistentCommand(ctx, c)
}
