package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/credentials"
	"InfraPulseAgent/internal/telephony"
)

func TestRemoteTrustFilesMustBeRegularAndBounded(t *testing.T) {
	dir := t.TempDir()
	oversized := filepath.Join(dir, "oversized.pem")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate((1 << 20) + 1)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{dir, oversized, filepath.Join(dir, "missing.pem")}
	if runtime.GOOS != "windows" {
		paths = append(paths, "/dev/null")
	}
	for _, path := range paths {
		if _, err := readRemoteCA(path); err == nil {
			t.Fatalf("invalid trust file accepted: %s", path)
		}
		if _, err := redfishTLS(path); err == nil {
			t.Fatalf("Redfish accepted invalid trust file: %s", path)
		}
	}
}

func TestLocalTrustSymlinksPreserveStaticAndManagedAdapters(t *testing.T) {
	authority := newManagedTLSAuthority(t)
	path := filepath.Join(testTempDir(t), "trusted-ca-link.pem")
	if err := os.Symlink(authority.ca, path); err != nil {
		if runtime.GOOS == "windows" && (os.IsPermission(err) || errors.Is(err, syscall.Errno(1314))) {
			t.Skip("Windows account lacks symlink privilege; Linux suite covers this check")
		}
		t.Fatal(err)
	}
	c := config.Defaults()
	if c.RemoteManagement.Enabled {
		t.Fatal("management unexpectedly enabled")
	}
	c.Credentials = map[string]credentials.Reference{"monitor": {UsernameEnv: "CA_FIXTURE_USER", PasswordEnv: "CA_FIXTURE_PASSWORD"}}
	t.Setenv("CA_FIXTURE_USER", "fixture")
	t.Setenv("CA_FIXTURE_PASSWORD", "fixture-password")
	factory := RemoteAdapterFactory(c, credentials.NewEnvironment(c.Credentials))
	for _, protocol := range []string{"routeros", "redfish", "sip"} {
		d := config.Device{ID: "ca-fixture", Driver: protocol, Address: "https://localhost", CredentialRef: "monitor", TLS: config.RemoteTLS{CAFile: path}}
		if protocol == "sip" {
			d.SIP = &telephony.Endpoint{ID: d.ID, Address: "localhost", Transport: "tls", TLS: telephony.TLS{CAFile: path}}
		}
		adapter, err := factory(d)
		if err != nil {
			t.Fatalf("%s rejected administrator CA symlink: %v", protocol, err)
		}
		if err := adapter.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
