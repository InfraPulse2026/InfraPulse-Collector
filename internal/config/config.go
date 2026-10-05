// Package config loads strict YAML and validates operational resource limits.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/telephony"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Logs        Logs                             `yaml:"logs"`
	Telephony   TelephonyHealth                  `yaml:"telephony"`
	Agent       Agent                            `yaml:"agent"`
	Controller  Controller                       `yaml:"controller"`
	Security    Security                         `yaml:"security"`
	Local       Local                            `yaml:"local"`
	Telemetry   Telemetry                        `yaml:"telemetry"`
	Queue       Queue                            `yaml:"queue"`
	Scheduler   Scheduler                        `yaml:"scheduler"`
	Logging     Logging                          `yaml:"logging"`
	Location    model.PhysicalLocation           `yaml:"location"`
	Devices     []Device                         `yaml:"devices"`
	Commands    Commands                         `yaml:"commands"`
	Remote      Remote                           `yaml:"remote"`
	Credentials map[string]credentials.Reference `yaml:"credentials" json:"-"`
}
type Agent struct {
	Name     string `yaml:"name"`
	StateDir string `yaml:"state_dir"`
}
type Controller struct {
	Address          string        `yaml:"address"`
	ConnectTimeout   time.Duration `yaml:"connect_timeout"`
	SendTimeout      time.Duration `yaml:"send_timeout"`
	ReconnectInitial time.Duration `yaml:"reconnect_initial"`
	ReconnectMax     time.Duration `yaml:"reconnect_max"`
}
type Security struct {
	Mode       string `yaml:"mode"`
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
}
type Local struct {
	Enabled  bool   `yaml:"enabled"`
	Platform string `yaml:"platform"`
}
type LogSource struct {
	ID     string `yaml:"id"`
	Path   string `yaml:"path"`
	Format string `yaml:"format"`
}
type Logs struct {
	Sources         []LogSource   `yaml:"sources"`
	Interval        time.Duration `yaml:"interval"`
	Timeout         time.Duration `yaml:"timeout"`
	MaxLineBytes    int           `yaml:"max_line_bytes"`
	MaxBatchRecords int           `yaml:"max_batch_records"`
	MaxBatchBytes   int           `yaml:"max_batch_bytes"`
	MaxReadBytes    int           `yaml:"max_read_bytes"`
}
type TelephonyHealth struct {
	Endpoints   []telephony.Endpoint `yaml:"endpoints"`
	Concurrency int                  `yaml:"concurrency"`
}
type Telemetry struct {
	FastInterval      time.Duration `yaml:"fast_interval"`
	MediumInterval    time.Duration `yaml:"medium_interval"`
	InventoryInterval time.Duration `yaml:"inventory_interval"`
	TopologyInterval  time.Duration `yaml:"topology_interval"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	CollectTimeout    time.Duration `yaml:"collect_timeout"`
}
type Queue struct {
	MaxMessages     int `yaml:"max_messages"`
	MaxBytes        int `yaml:"max_bytes"`
	MaxMessageBytes int `yaml:"max_message_bytes"`
}
type Scheduler struct {
	Concurrency int `yaml:"concurrency"`
}
type Logging struct {
	Level string `yaml:"level"`
}
type Device struct {
	ID              string                 `yaml:"id"`
	Name            string                 `yaml:"name"`
	Driver          string                 `yaml:"driver"`
	Address         string                 `yaml:"address"`
	CredentialRef   string                 `yaml:"credential_ref"`
	VendorProfile   string                 `yaml:"vendor_profile"`
	VendorHint      string                 `yaml:"vendor_hint"`
	Location        model.PhysicalLocation `yaml:"location"`
	Polling         Polling                `yaml:"polling"`
	SNMP            SNMP                   `yaml:"snmp"`
	TLS             RemoteTLS              `yaml:"tls"`
	EnableMutations bool                   `yaml:"enable_mutations"`
}
type Remote struct {
	Concurrency      int           `yaml:"concurrency"`
	MaxRows          int           `yaml:"max_rows"`
	MaxResponseBytes int           `yaml:"max_response_bytes"`
	TopologyTTL      time.Duration `yaml:"topology_ttl"`
}
type Polling struct {
	FastInterval      time.Duration `yaml:"fast_interval"`
	MediumInterval    time.Duration `yaml:"medium_interval"`
	InventoryInterval time.Duration `yaml:"inventory_interval"`
	TopologyInterval  time.Duration `yaml:"topology_interval"`
	Timeout           time.Duration `yaml:"timeout"`
}
type SNMP struct {
	Port    uint16 `yaml:"port"`
	Retries int    `yaml:"retries"`
}
type RemoteTLS struct {
	CAFile string `yaml:"ca_file"`
}
type Commands struct {
	Enabled             bool     `yaml:"enabled"`
	ProtectedDevices    []string `yaml:"protected_devices"`
	ProtectedInterfaces []string `yaml:"protected_interfaces"`
	ProtectedServices   []string `yaml:"protected_services"`
}

func Defaults() Config {
	return Config{
		Agent:      Agent{Name: "infrapulse-agent", StateDir: "../state"},
		Controller: Controller{Address: "localhost:9443", ConnectTimeout: 5 * time.Second, SendTimeout: 10 * time.Second, ReconnectInitial: time.Second, ReconnectMax: 30 * time.Second},
		Security:   Security{Mode: "production"}, Local: Local{Enabled: true},
		Telemetry: Telemetry{FastInterval: 3 * time.Second, MediumInterval: 30 * time.Second, InventoryInterval: 5 * time.Minute, TopologyInterval: 5 * time.Minute, HeartbeatInterval: 10 * time.Second, CollectTimeout: 2 * time.Second},
		Queue:     Queue{MaxMessages: 256, MaxBytes: 8 << 20, MaxMessageBytes: 1 << 20}, Scheduler: Scheduler{Concurrency: 4}, Logging: Logging{Level: "info"},
		Remote:    Remote{Concurrency: 4, MaxRows: 4096, MaxResponseBytes: 1 << 20, TopologyTTL: 15 * time.Minute},
		Logs:      Logs{Interval: time.Second, Timeout: 2 * time.Second, MaxLineBytes: 8192, MaxBatchRecords: 64, MaxBatchBytes: 128 << 10, MaxReadBytes: 256 << 10},
		Telephony: TelephonyHealth{Concurrency: 2},
	}
}

// Paths are resolved relative to the YAML file. Environment overrides are limited
// to explicitly supported path fields, not arbitrary expansion of all strings.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("configuration exceeds 1 MiB")
	}
	c, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	for _, v := range []struct {
		name string
		dest *string
	}{{"INFRAPULSE_STATE_DIR", &c.Agent.StateDir}, {"INFRAPULSE_CA_FILE", &c.Security.CAFile}, {"INFRAPULSE_CERT_FILE", &c.Security.CertFile}, {"INFRAPULSE_KEY_FILE", &c.Security.KeyFile}} {
		if val, ok := os.LookupEnv(v.name); ok {
			*v.dest = val
		}
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("resolve configuration directory: %w", err)
	}
	for _, p := range []*string{&c.Agent.StateDir, &c.Security.CAFile, &c.Security.CertFile, &c.Security.KeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	for i := range c.Devices {
		p := &c.Devices[i].TLS.CAFile
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	for i := range c.Logs.Sources {
		p := &c.Logs.Sources[i].Path
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, c.Validate()
}

func Parse(data []byte) (Config, error) {
	c := Defaults()
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return Config{}, errors.New("invalid YAML configuration: check field names, types and durations")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("configuration must contain exactly one YAML document")
	}
	return c, nil
}

func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Config) Validate() error {
	var problems []error
	add := func(ok bool, field string) {
		if !ok {
			problems = append(problems, fmt.Errorf("invalid %s", field))
		}
	}
	add(strings.TrimSpace(c.Agent.Name) != "" && len(c.Agent.Name) <= 128, "agent.name")
	add(c.Agent.StateDir != "", "agent.state_dir")
	add(c.Local.Platform == "" || c.Local.Platform == "auto" || c.Local.Platform == "linux" || c.Local.Platform == "openwrt", "local.platform")
	add(len(c.Logs.Sources) <= 32, "logs.sources (maximum 32)")
	add(c.Logs.Interval >= 10*time.Millisecond && c.Logs.Interval <= 24*time.Hour, "logs.interval")
	add(c.Logs.Timeout >= 10*time.Millisecond && c.Logs.Timeout <= time.Minute, "logs.timeout")
	add(c.Logs.MaxLineBytes >= 128 && c.Logs.MaxLineBytes <= 32768, "logs.max_line_bytes")
	add(c.Logs.MaxBatchRecords >= 1 && c.Logs.MaxBatchRecords <= 1024, "logs.max_batch_records")
	add(c.Logs.MaxBatchBytes >= 2*c.Logs.MaxLineBytes+4096 && c.Logs.MaxBatchBytes <= 1<<20 && (len(c.Logs.Sources) == 0 || c.Logs.MaxBatchBytes+1024 <= c.Queue.MaxMessageBytes), "logs.max_batch_bytes")
	add(c.Logs.MaxReadBytes > c.Logs.MaxLineBytes && c.Logs.MaxReadBytes <= 4<<20, "logs.max_read_bytes")
	logIDs := map[string]bool{}
	for _, s := range c.Logs.Sources {
		add(s.ID != "" && len(s.ID) <= 128 && !logIDs[s.ID] && !strings.ContainsAny(s.ID, "/\\\r\n\x00"), "logs.source.id")
		logIDs[s.ID] = true
		add(s.Path != "" && len(s.Path) <= 4096 && !strings.ContainsAny(s.Path, "\r\n\x00"), "logs.source.path")
		add(s.Format == "syslog" || s.Format == "auditd" || s.Format == "raw", "logs.source.format")
	}
	add(len(c.Telephony.Endpoints) <= 128, "telephony.endpoints")
	add(c.Telephony.Concurrency >= 1 && c.Telephony.Concurrency <= 16, "telephony.concurrency")
	sipIDs := map[string]bool{}
	for _, ep := range c.Telephony.Endpoints {
		add(!sipIDs[ep.ID], "telephony.endpoint.id (duplicate)")
		sipIDs[ep.ID] = true
		if err := ep.WithDefaults().Validate(); err != nil {
			problems = append(problems, fmt.Errorf("invalid telephony endpoint: %w", err))
		}
	}
	host, port, err := net.SplitHostPort(c.Controller.Address)
	add(err == nil && host != "" && port != "", "controller.address (host:port required)")
	if err == nil {
		number, parseErr := strconv.Atoi(port)
		add(parseErr == nil && number >= 1 && number <= 65535, "controller.address port")
		u, e := url.Parse("http://" + c.Controller.Address)
		add(e == nil && u.User == nil && u.Path == "" && u.RawQuery == "", "controller.address")
	}
	add(c.Security.Mode == "development" || c.Security.Mode == "production", "security.mode")
	if c.Security.Mode == "development" {
		add(IsLoopback(host), "controller.address (development requires loopback)")
	}
	if c.Security.Mode == "production" {
		add(c.Security.CAFile != "" && c.Security.CertFile != "" && c.Security.KeyFile != "", "security mTLS files")
	}
	for _, v := range []struct {
		name string
		d    time.Duration
	}{{"controller.connect_timeout", c.Controller.ConnectTimeout}, {"controller.send_timeout", c.Controller.SendTimeout}, {"controller.reconnect_initial", c.Controller.ReconnectInitial}, {"controller.reconnect_max", c.Controller.ReconnectMax}, {"telemetry.fast_interval", c.Telemetry.FastInterval}, {"telemetry.medium_interval", c.Telemetry.MediumInterval}, {"telemetry.inventory_interval", c.Telemetry.InventoryInterval}, {"telemetry.topology_interval", c.Telemetry.TopologyInterval}, {"telemetry.heartbeat_interval", c.Telemetry.HeartbeatInterval}, {"telemetry.collect_timeout", c.Telemetry.CollectTimeout}} {
		add(v.d >= 10*time.Millisecond && v.d <= 24*time.Hour, v.name)
	}
	add(c.Controller.ReconnectInitial <= c.Controller.ReconnectMax, "controller.reconnect_max")
	add(c.Queue.MaxMessages >= 4 && c.Queue.MaxMessages <= 65536, "queue.max_messages")
	add(c.Queue.MaxBytes >= 1024 && c.Queue.MaxBytes <= 256<<20, "queue.max_bytes")
	add(c.Queue.MaxMessageBytes >= 512 && c.Queue.MaxMessageBytes <= 4<<20 && c.Queue.MaxMessageBytes <= c.Queue.MaxBytes, "queue.max_message_bytes")
	add(c.Scheduler.Concurrency >= 1 && c.Scheduler.Concurrency <= 64, "scheduler.concurrency")
	add(c.Remote.Concurrency >= 1 && c.Remote.Concurrency <= 64, "remote.concurrency")
	add(c.Remote.MaxRows >= 1 && c.Remote.MaxRows <= 16384, "remote.max_rows")
	add(c.Remote.MaxResponseBytes >= 512 && c.Remote.MaxResponseBytes <= 4<<20, "remote.max_response_bytes")
	add(c.Remote.TopologyTTL >= time.Second && c.Remote.TopologyTTL <= 24*time.Hour, "remote.topology_ttl")
	add(len(c.Credentials) <= 1024, "credentials count")
	validRef := func(s string) bool {
		if len(s) == 0 || len(s) > 128 {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				return false
			}
		}
		return true
	}
	for ref, cr := range c.Credentials {
		add(validRef(ref), "credential reference")
		add((cr.CommunityEnv != "") != (cr.UsernameEnv != "" || cr.PasswordEnv != ""), "credential kind")
		if cr.CommunityEnv != "" {
			add(validRef(cr.CommunityEnv), "credential community_env")
		} else {
			add(validRef(cr.UsernameEnv) && validRef(cr.PasswordEnv), "credential username_env/password_env")
		}
	}
	add(c.Logging.Level == "debug" || c.Logging.Level == "info" || c.Logging.Level == "warn" || c.Logging.Level == "error", "logging.level")
	add(c.Location.RackUnit >= 0, "location.rack_unit")
	for _, value := range []string{c.Location.Site, c.Location.Datacenter, c.Location.Building, c.Location.Room, c.Location.Rack, c.Location.Position, c.Location.Comment} {
		add(len(value) <= 512, "location field length (maximum 512 bytes)")
	}
	add(len(c.Devices) <= 1024, "devices (maximum 1024)")
	seen := map[string]bool{}
	for i, d := range c.Devices {
		prefix := fmt.Sprintf("devices[%d]", i)
		add(d.ID != "" && len(d.ID) <= 128 && !seen[d.ID], prefix+".id")
		seen[d.ID] = true
		add(d.Driver == "snmp" || d.Driver == "routeros", prefix+".driver")
		add(d.Address != "", prefix+".address")
		add(d.CredentialRef != "", prefix+".credential_ref")
		cr, exists := c.Credentials[d.CredentialRef]
		add(exists, prefix+".credential_ref (not configured)")
		add(!strings.HasPrefix(d.ID, "local:"), prefix+".id (reserved local namespace)")
		add(len(d.Name) <= 128 && len(d.VendorHint) <= 128 && len(d.VendorProfile) <= 128, prefix+" metadata length")
		add(d.Location.RackUnit >= 0, prefix+".location.rack_unit")
		for _, v := range []string{d.Location.Site, d.Location.Datacenter, d.Location.Building, d.Location.Room, d.Location.Rack, d.Location.Position, d.Location.Comment} {
			add(len(v) <= 512, prefix+".location")
		}
		for _, v := range []time.Duration{d.Polling.FastInterval, d.Polling.MediumInterval, d.Polling.InventoryInterval, d.Polling.TopologyInterval, d.Polling.Timeout} {
			add(v == 0 || v >= 10*time.Millisecond && v <= 24*time.Hour, prefix+".polling")
		}
		add(d.SNMP.Retries >= 0 && d.SNMP.Retries <= 3, prefix+".snmp.retries")
		if d.Driver == "snmp" {
			add(cr.CommunityEnv != "", prefix+".credential_ref kind")
			add(!d.EnableMutations && d.TLS.CAFile == "", prefix+" SNMP read-only options")
			add(!strings.ContainsAny(d.Address, "/@?#\\ \t\r\n") && len(d.Address) <= 253, prefix+".address (host or IP)")
			add(net.ParseIP(d.Address) != nil || !strings.Contains(d.Address, ":"), prefix+".address (port is snmp.port)")
		}
		if d.Driver == "routeros" {
			timeout := d.Polling.Timeout
			if timeout == 0 {
				timeout = c.Telemetry.CollectTimeout
			}
			add(timeout <= time.Minute, prefix+".polling.timeout (RouterOS maximum 1m)")
			add(cr.UsernameEnv != "" && cr.PasswordEnv != "", prefix+".credential_ref kind")
			u, e := url.Parse(d.Address)
			add(e == nil && u != nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/"), prefix+".address (HTTPS origin without embedded credentials)")
		}
	}
	return errors.Join(problems...)
}
