package config

import (
	"errors"
	"net"
	"strings"
	"time"

	"InfraPulseAgent/internal/credentials"
)

// IPMI configures only v2.0 LANplus reads. A single selected cipher avoids
// implicit negotiation or downgrade; credentials remain provider references.
type IPMI struct {
	Port               *int          `yaml:"port"`
	CipherSuite        *int          `yaml:"cipher_suite"`
	Privilege          string        `yaml:"privilege"`
	Retries            int           `yaml:"retries"`
	RequestTimeout     time.Duration `yaml:"request_timeout"`
	MaxSessionDuration time.Duration `yaml:"max_session_duration"`
	MaxOperations      int           `yaml:"max_operations"`
	MaxSDRRecords      int           `yaml:"max_sdr_records"`
	MaxSDRBytes        int           `yaml:"max_sdr_bytes"`
	MaxFRURecords      int           `yaml:"max_fru_records"`
	MaxFRUBytes        int           `yaml:"max_fru_bytes"`
	MaxPollBytes       int           `yaml:"max_poll_bytes"`
	SDRCacheTTL        time.Duration `yaml:"sdr_cache_ttl"`
	Provenance         string        `yaml:"provenance"`
}

func (i IPMI) WithDefaults() IPMI {
	if i.Port == nil {
		value := 623
		i.Port = &value
	}
	if i.CipherSuite == nil {
		value := 17
		i.CipherSuite = &value
	}
	if i.Privilege == "" {
		i.Privilege = "user"
	}
	if i.RequestTimeout == 0 {
		i.RequestTimeout = 2 * time.Second
	}
	if i.MaxSessionDuration == 0 {
		i.MaxSessionDuration = 30 * time.Second
	}
	if i.MaxOperations == 0 {
		i.MaxOperations = 1024
	}
	if i.MaxSDRRecords == 0 {
		i.MaxSDRRecords = 128
	}
	if i.MaxSDRBytes == 0 {
		i.MaxSDRBytes = 128 << 10
	}
	if i.MaxFRURecords == 0 {
		i.MaxFRURecords = 16
	}
	if i.MaxFRUBytes == 0 {
		i.MaxFRUBytes = 64 << 10
	}
	if i.MaxPollBytes == 0 {
		i.MaxPollBytes = 1 << 20
	}
	if i.SDRCacheTTL == 0 {
		i.SDRCacheTTL = 5 * time.Minute
	}
	if i.Provenance == "" {
		i.Provenance = "real"
	}
	return i
}

func validateIPMI(d Device, credential credentials.Reference, maxRows, maxResponseBytes int, mode string) error {
	if d.Driver != "ipmi" {
		if d.IPMI != (IPMI{}) {
			return errors.New("ipmi options require the ipmi driver")
		}
		return nil
	}
	i := d.IPMI.WithDefaults()
	var problems []error
	check := func(ok bool, field string) {
		if !ok {
			problems = append(problems, errors.New("invalid ipmi "+field))
		}
	}
	check(validIPMIHost(d.Address), "address (host or IP; use ipmi.port, no URL)")
	check(d.Target(Polling{}).Validate() == nil, "target identity and metadata")
	check(maxRows >= 2, "remote.max_rows (at least BMC and chassis)")
	check(*i.Port >= 1 && *i.Port <= 65535, "port")
	check(*i.CipherSuite == 17 || *i.CipherSuite == 3, "cipher_suite (only 17 or explicit 3)")
	check(i.Privilege == "user" || i.Privilege == "operator", "privilege (user or explicit operator)")
	check(credential.UsernameEnv != "" && credential.PasswordEnv != "" && credential.CommunityEnv == "" && credential.AuthPasswordEnv == "" && credential.PrivacyPasswordEnv == "", "credential_ref kind")
	check(!d.EnableMutations && d.TLS.CAFile == "" && d.SNMP == (SNMP{}), "read-only LANplus options")
	check(i.Retries >= 0 && i.Retries <= 3, "retries")
	check(i.RequestTimeout >= 10*time.Millisecond && i.RequestTimeout <= 30*time.Second, "request_timeout")
	check(i.MaxSessionDuration >= 10*time.Millisecond && i.MaxSessionDuration <= 5*time.Minute, "max_session_duration")
	check(i.MaxOperations >= 1 && i.MaxOperations <= 8192, "max_operations")
	check(i.MaxSDRRecords >= 1 && i.MaxSDRRecords <= 1024 && i.MaxSDRRecords <= maxRows, "max_sdr_records")
	check(i.MaxSDRBytes >= 260 && i.MaxSDRBytes <= 1<<20, "max_sdr_bytes")
	check(i.MaxFRURecords >= 1 && i.MaxFRURecords <= 128 && i.MaxFRURecords <= maxRows, "max_fru_records")
	check(i.MaxFRUBytes >= 8 && i.MaxFRUBytes <= 64<<10, "max_fru_bytes")
	check(i.MaxPollBytes >= i.MaxFRUBytes && i.MaxPollBytes >= i.MaxSDRBytes && i.MaxPollBytes >= min(maxResponseBytes, 4096) && i.MaxPollBytes <= 8<<20, "max_poll_bytes")
	check(i.SDRCacheTTL >= time.Second && i.SDRCacheTTL <= time.Hour, "sdr_cache_ttl")
	check(i.Provenance == "real" || i.Provenance == "simulated" && mode == "development", "provenance (simulated requires development)")
	check(d.Polling.FastInterval == 0 && d.Polling.TopologyInterval == 0, "polling (use medium/inventory intervals)")
	check(d.Polling.Timeout == 0 || d.Polling.Timeout <= 5*time.Minute, "polling.timeout (maximum 5m)")
	return errors.Join(problems...)
}

func validIPMIHost(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/@?#\\ \t\r\n\x00[]%") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if strings.Contains(host, ":") {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
