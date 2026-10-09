package config

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"InfraPulseAgent/internal/credentials"
)

// Redfish limits are independent of the legacy fast network-counter cadence.
// Authentication changes only session setup; hardware operations stay read-only.
type Redfish struct {
	Auth           string        `yaml:"auth"`
	Provenance     string        `yaml:"provenance"`
	MaxResources   int           `yaml:"max_resources"`
	MaxPages       int           `yaml:"max_pages"`
	MaxRequests    int           `yaml:"max_requests"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

func (r Redfish) WithDefaults() Redfish {
	if r.Auth == "" {
		r.Auth = "session"
	}
	if r.Provenance == "" {
		r.Provenance = "real"
	}
	if r.MaxResources == 0 {
		r.MaxResources = 128
	}
	if r.MaxPages == 0 {
		r.MaxPages = 8
	}
	if r.MaxRequests == 0 {
		r.MaxRequests = 512
	}
	if r.RequestTimeout == 0 {
		r.RequestTimeout = 5 * time.Second
	}
	return r
}

func validateRedfish(d Device, credential credentials.Reference, maxRows int, securityMode string) error {
	if d.Driver != "redfish" {
		if d.Redfish != (Redfish{}) {
			return errors.New("redfish options require the redfish driver")
		}
		return nil
	}
	r := d.Redfish.WithDefaults()
	var problems []error
	check := func(ok bool, field string) {
		if !ok {
			problems = append(problems, errors.New("invalid redfish "+field))
		}
	}
	u, err := url.Parse(d.Address)
	check(err == nil && u != nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && len(d.Address) <= 2048 &&
		u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" &&
		(u.Path == "" || u.Path == "/") && !strings.ContainsAny(d.Address, "\\\r\n\x00"), "address (verified HTTPS origin required)")
	if err == nil && u != nil && u.Port() != "" {
		port, portErr := strconv.Atoi(u.Port())
		check(portErr == nil && port >= 1 && port <= 65535, "address port")
	}
	check(credential.UsernameEnv != "" && credential.PasswordEnv != "" && credential.CommunityEnv == "" &&
		credential.AuthPasswordEnv == "" && credential.PrivacyPasswordEnv == "", "credential_ref kind")
	check(!d.EnableMutations, "read-only options")
	check(r.Auth == "session" || r.Auth == "basic", "auth")
	check(r.Provenance == "real" || r.Provenance == "simulated" && securityMode == "development", "provenance (simulated requires development mode)")
	check(r.MaxResources >= 1 && r.MaxResources <= 1024 && r.MaxResources <= maxRows, "max_resources")
	check(r.MaxPages >= 1 && r.MaxPages <= 64, "max_pages")
	check(r.MaxRequests >= 1 && r.MaxRequests <= 4096, "max_requests")
	check(r.RequestTimeout >= 10*time.Millisecond && r.RequestTimeout <= 30*time.Second, "request_timeout")
	check(d.Polling.Timeout == 0 || d.Polling.Timeout <= 5*time.Minute, "polling.timeout (maximum 5m)")
	check(d.Polling.FastInterval == 0 && d.Polling.TopologyInterval == 0, "polling (use medium/inventory intervals)")
	return errors.Join(problems...)
}
