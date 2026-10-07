package openwrt

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/platform/timesync"
)

var corporateTimeSync = timesync.Settings{Enabled: true, Servers: []string{"ntp1.company.local", "10.10.0.5", "2001:db8::5"}}

func timeSyncConfigFixture(t *testing.T, servers []string, flags map[string]any) []byte {
	t.Helper()
	entry := map[string]any{".type": "timeserver", ".name": "ntp", "enabled": "1", "use_dhcp": "0", "server": servers}
	for key, value := range flags {
		entry[key] = value
	}
	data, err := json.Marshal(map[string]any{"values": entry})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func timeSyncRunner(config, services []byte, calls *[]Operation) Runner {
	return runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if calls != nil {
			*calls = append(*calls, op)
		}
		switch op {
		case NTPConfig:
			return config, nil
		case Services:
			return services, nil
		default:
			return nil, errors.New("unexpected operation")
		}
	})
}

const activeSysntpd = `{"sysntpd":{"instances":{"instance1":{"running":true,"pid":100,"command":["do-not-forward-command-secret"],"env":{"TOKEN":"do-not-forward-env-secret"}}}}}`

func TestTimeSyncDisabledPerformsNoSystemReads(t *testing.T) {
	provider := NewTimeSyncWithRunner(runnerFunc(func(context.Context, Operation) ([]byte, error) {
		t.Fatal("disabled time synchronization touched the OS")
		return nil, nil
	}))
	for _, operation := range []func(context.Context, timesync.Settings) (timesync.Status, error){provider.Status, provider.Apply} {
		status, err := operation(context.Background(), timesync.Settings{})
		if err != nil || status.Enabled || status.Changed || status.Synchronized != nil {
			t.Fatalf("disabled = %+v, %v", status, err)
		}
	}
}

func TestTimeSyncObservesCorporateUCIAndProcdWithoutInferringSynchronization(t *testing.T) {
	// Server order and duplicate list entries do not alter the effective set.
	actual := []string{"2001:db8::5", "10.10.0.5", "ntp1.company.local", "10.10.0.5"}
	provider := NewTimeSyncWithRunner(timeSyncRunner(timeSyncConfigFixture(t, actual, nil), []byte(activeSysntpd), nil))
	status, err := provider.Status(context.Background(), corporateTimeSync)
	if err != nil || status.Provider != "openwrt-sysntpd" || !status.Available || !status.Active || !status.Configured ||
		status.Synchronized != nil || status.Changed || status.ManagedFile != "" || status.CheckedAt.IsZero() || len(status.ConfiguredServers) != 3 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	data, err := json.Marshal(status)
	if err != nil || strings.Contains(string(data), "do-not-forward") {
		t.Fatalf("status leaked process fields: %s, %v", data, err)
	}
	status.DesiredServers[0], status.ConfiguredServers[0] = "mutated", "mutated"
	second, err := provider.Status(context.Background(), corporateTimeSync)
	if err != nil || !second.Configured || corporateTimeSync.Servers[0] != "ntp1.company.local" {
		t.Fatalf("returned status owns configuration memory: %+v, %v", second, err)
	}
}

func TestTimeSyncApplyNeverMutatesSharedAdministratorUCI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		servers []string
		matches bool
	}{
		{"unchanged", corporateTimeSync.Servers, true},
		{"different", []string{"ntp2.company.local"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []Operation
			provider := NewTimeSyncWithRunner(timeSyncRunner(timeSyncConfigFixture(t, tc.servers, nil), []byte(activeSysntpd), &calls))
			for range 2 {
				status, err := provider.Apply(context.Background(), corporateTimeSync)
				if err == nil || !strings.Contains(status.Diagnostic, "setup_required") || !strings.Contains(status.Diagnostic, "unsupported") ||
					status.Configured != tc.matches || status.Changed || status.ManagedFile != "" {
					t.Fatalf("apply = %+v, %v", status, err)
				}
			}
			for _, op := range calls {
				if op != Services && op != NTPConfig {
					t.Fatalf("apply escaped read-only operations: %v", calls)
				}
			}
		})
	}
}

func TestTimeSyncConfigurationNeedsEnabledAndCorporateOnlySources(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags map[string]any
	}{
		{"disabled", map[string]any{"enabled": "0"}},
		{"dhcp additional sources", map[string]any{"use_dhcp": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := NewTimeSyncWithRunner(timeSyncRunner(timeSyncConfigFixture(t, corporateTimeSync.Servers, tc.flags), []byte(activeSysntpd), nil))
			status, err := provider.Status(context.Background(), corporateTimeSync)
			if err != nil || status.Configured || !strings.Contains(status.Diagnostic, "setup_required") {
				t.Fatalf("configuration = %+v, %v", status, err)
			}
		})
	}
	data := []byte(`{"values":{".type":"timeserver",".name":"ntp","server":["ntp1.company.local"]}}`)
	_, enabled, useDHCP, err := parseTimeSyncConfig(data)
	if err != nil || !enabled || !useDHCP {
		t.Fatalf("standard sysntpd defaults = %v, %v, %v", enabled, useDHCP, err)
	}
}

func TestTimeSyncInactiveAndAbsentServices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		services  string
		available bool
		wantError bool
	}{
		{"inactive", `{"sysntpd":{"instances":{"main":{"running":false}}}}`, true, false},
		{"unknown", `{"sysntpd":{"instances":{"main":{}}}}`, true, false},
		{"missing", `{"dnsmasq":{"instances":{"main":{"running":true}}}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := NewTimeSyncWithRunner(timeSyncRunner(timeSyncConfigFixture(t, corporateTimeSync.Servers, nil), []byte(tc.services), nil))
			status, err := provider.Status(context.Background(), corporateTimeSync)
			if (err != nil) != tc.wantError || status.Available != tc.available || status.Active || status.Synchronized != nil || status.Diagnostic == "" {
				t.Fatalf("service = %+v, %v", status, err)
			}
		})
	}
}

func TestTimeSyncReadFailuresAreBoundedAndSafe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   error
		category  string
		operation Operation
	}{
		{"ubus missing", os.ErrNotExist, "unavailable", Services},
		{"service permission", os.ErrPermission, "permission denied", Services},
		{"UCI permission", os.ErrPermission, "permission denied", NTPConfig},
		{"secret error", errors.New("password=do-not-forward-error-secret"), "unavailable", NTPConfig},
		{"timeout", context.DeadlineExceeded, "timeout", Services},
		{"cancellation", errors.Join(context.Canceled, errors.New("token=do-not-forward")), "canceled", NTPConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			good := timeSyncRunner(timeSyncConfigFixture(t, corporateTimeSync.Servers, nil), []byte(activeSysntpd), nil)
			provider := NewTimeSyncWithRunner(runnerFunc(func(ctx context.Context, op Operation) ([]byte, error) {
				if op == tc.operation {
					return nil, tc.failure
				}
				return good.Run(ctx, op)
			}))
			status, err := provider.Status(context.Background(), corporateTimeSync)
			if err == nil || !strings.Contains(status.Diagnostic, tc.category) || strings.Contains(err.Error(), "do-not-forward") || strings.Contains(status.Diagnostic, "do-not-forward") {
				t.Fatalf("failure = %+v, %v", status, err)
			}
			if errors.Is(tc.failure, os.ErrPermission) && !errors.Is(err, os.ErrPermission) {
				t.Fatalf("permission category lost: %v", err)
			}
		})
	}
	for _, op := range []Operation{Services, NTPConfig} {
		t.Run("oversized "+string(rune('0'+op)), func(t *testing.T) {
			good := timeSyncRunner(timeSyncConfigFixture(t, corporateTimeSync.Servers, nil), []byte(activeSysntpd), nil)
			provider := NewTimeSyncWithRunner(runnerFunc(func(ctx context.Context, operation Operation) ([]byte, error) {
				if operation == op {
					return make([]byte, maxResponseBytes+1), nil
				}
				return good.Run(ctx, operation)
			}))
			status, err := provider.Status(context.Background(), corporateTimeSync)
			if err == nil || !strings.Contains(status.Diagnostic, "response exceeds limit") {
				t.Fatalf("oversized = %+v, %v", status, err)
			}
		})
	}
}

func TestTimeSyncMalformedUCI(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`null`), []byte(`{}`), []byte(`{"values":[]}`),
		timeSyncConfigFixture(t, corporateTimeSync.Servers, map[string]any{".type": "system"}),
		timeSyncConfigFixture(t, corporateTimeSync.Servers, map[string]any{".name": "another"}),
		timeSyncConfigFixture(t, corporateTimeSync.Servers, map[string]any{"enabled": true}),
		timeSyncConfigFixture(t, corporateTimeSync.Servers, map[string]any{"use_dhcp": "maybe"}),
		timeSyncConfigFixture(t, []string{"invalid/server"}, nil),
		timeSyncConfigFixture(t, corporateTimeSync.Servers, map[string]any{"server": []any{1}}),
		timeSyncConfigFixture(t, []string{strings.Repeat("x", maxFieldBytes+1)}, nil),
	} {
		provider := NewTimeSyncWithRunner(timeSyncRunner(data, []byte(activeSysntpd), nil))
		status, err := provider.Status(context.Background(), corporateTimeSync)
		if err == nil || status.Configured || status.Synchronized != nil {
			t.Fatalf("malformed %s = %+v, %v", data, status, err)
		}
	}
}

func TestTimeSyncCancellationAndTimeoutUseExistingBoundedRead(t *testing.T) {
	provider := NewTimeSyncWithRunner(runnerFunc(func(ctx context.Context, _ Operation) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := provider.Status(ctx, corporateTimeSync); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Status(ctx, corporateTimeSync); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestTimeSyncInvalidDesiredConfigurationDoesNotReadOS(t *testing.T) {
	provider := NewTimeSyncWithRunner(runnerFunc(func(context.Context, Operation) ([]byte, error) {
		t.Fatal("invalid desired configuration touched the OS")
		return nil, nil
	}))
	if _, err := provider.Apply(context.Background(), timesync.Settings{Enabled: true, Servers: []string{"invalid/server"}}); err == nil {
		t.Fatal("invalid desired server accepted")
	}
}

func TestTimeSyncUCIReadHasFixedSectionArguments(t *testing.T) {
	args, err := arguments(NTPConfig)
	if err != nil || !reflect.DeepEqual(args, []string{"-S", "call", "uci", "get", `{"config":"system","section":"ntp"}`}) {
		t.Fatalf("NTP arguments = %v, %v", args, err)
	}
}
