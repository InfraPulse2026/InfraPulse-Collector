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

	"InfraPulseAgent/internal/model"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	Agent      Agent                  `yaml:"agent"`
	Controller Controller             `yaml:"controller"`
	Security   Security               `yaml:"security"`
	Local      Local                  `yaml:"local"`
	Telemetry  Telemetry              `yaml:"telemetry"`
	Queue      Queue                  `yaml:"queue"`
	Scheduler  Scheduler              `yaml:"scheduler"`
	Logging    Logging                `yaml:"logging"`
	Location   model.PhysicalLocation `yaml:"location"`
	Devices    []Device               `yaml:"devices"`
	Commands   Commands               `yaml:"commands"`
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
	Enabled bool `yaml:"enabled"`
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
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	Driver        string `yaml:"driver"`
	Address       string `yaml:"address"`
	CredentialRef string `yaml:"credential_ref"`
	VendorProfile string `yaml:"vendor_profile"`
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
		if d.Driver == "routeros" {
			u, e := url.Parse(d.Address)
			add(e == nil && u != nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "", prefix+".address (HTTPS without embedded credentials)")
		}
	}
	return errors.Join(problems...)
}
