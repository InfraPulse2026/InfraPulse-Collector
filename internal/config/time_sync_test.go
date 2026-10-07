package config

import (
	"reflect"
	"strings"
	"testing"

	"InfraPulseAgent/internal/platform/timesync"
)

func TestTimeSyncConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		invalid    bool
	}{
		{"default", "", nil, false},
		{"disabled", "time_sync: {enabled: false, servers: []}", nil, false},
		{"IPv4", "time_sync: {enabled: true, servers: [10.10.0.5]}", []string{"10.10.0.5"}, false},
		{"multiple", "time_sync: {enabled: true, servers: [10.10.0.5, 10.10.0.6]}", []string{"10.10.0.5", "10.10.0.6"}, false},
		{"DNS", "time_sync: {enabled: true, servers: [NTP1.Company.Local.]}", []string{"ntp1.company.local"}, false},
		{"IPv6", "time_sync: {enabled: true, servers: ['2001:0db8::5']}", []string{"2001:db8::5"}, false},
		{"deduplicated", "time_sync: {enabled: true, servers: [ntp1.company.local, NTP1.COMPANY.LOCAL., '2001:db8::5', '2001:0db8::5']}", []string{"ntp1.company.local", "2001:db8::5"}, false},
		{"enabled-empty", "time_sync: {enabled: true, servers: []}", nil, true},
		{"enabled-omitted", "time_sync: {enabled: true}", nil, true},
		{"invalid-address", "time_sync: {enabled: true, servers: [10.10.0.999]}", nil, true},
		{"URL", "time_sync: {enabled: true, servers: ['https://ntp.company.local']}", nil, true},
		{"option-injection", "time_sync: {enabled: true, servers: ['ntp.company.local iburst']}", nil, true},
		{"newline", "time_sync: {enabled: true, servers: [\"ntp.company.local\\nserver evil\"]}", nil, true},
		{"unknown-field", "time_sync: {enabled: true, protocol: ntp, servers: [10.10.0.5]}", nil, true},
		{"invalid-type", "time_sync: {enabled: nope, servers: [10.10.0.5]}", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte("security: {mode: development}\n" + tc.body + "\n"))
			if err == nil {
				err = c.Validate()
			}
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v, invalid = %v", err, tc.invalid)
			}
			if !tc.invalid && !reflect.DeepEqual(c.TimeSync.Servers, tc.want) {
				t.Fatalf("servers = %v, want %v", c.TimeSync.Servers, tc.want)
			}
			if tc.name == "default" && c.TimeSync.Enabled {
				t.Fatal("time synchronization management enabled by default")
			}
		})
	}
}

func TestTimeSyncProgrammaticValidation(t *testing.T) {
	for _, server := range []string{"", " ntp.company.local", "ntp.company.local ", "-bad", "bad-", "bad..name", "[2001:db8::5]", "fe80::5%eth0", "0.0.0.0", "::", "224.0.0.1", "ntp:123", "a_b", strings.Repeat("a", 64) + ".local"} {
		c := Defaults()
		c.Security.Mode = "development"
		c.TimeSync = timesync.Settings{Enabled: true, Servers: []string{server}}
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted invalid server %q", server)
		}
	}
	c := Defaults()
	c.Security.Mode = "development"
	c.TimeSync = timesync.Settings{Enabled: true, Servers: make([]string, 17)}
	if err := c.Validate(); err == nil {
		t.Fatal("accepted unbounded server list")
	}
}
