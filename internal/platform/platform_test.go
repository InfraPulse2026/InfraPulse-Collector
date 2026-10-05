package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"InfraPulseAgent/internal/platform/linux"
	"InfraPulseAgent/internal/platform/openwrt"
)

func TestNewSupportsLinuxOnly(t *testing.T) {
	p, err := New()
	if runtime.GOOS == "linux" {
		if err != nil || p == nil {
			t.Fatalf("New = %v, %v", p, err)
		}
		return
	}
	if p != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("New = %v, %v", p, err)
	}
}

func TestModeSelectionAndLinuxFallback(t *testing.T) {
	etc := t.TempDir()
	for _, mode := range []string{"auto", "linux", ""} {
		p, err := newMode(mode, "linux", "test-proc", "test-sys", etc)
		if _, ok := p.(*linux.Platform); !ok || err != nil {
			t.Fatalf("Linux fallback %q = %T, %v", mode, p, err)
		}
	}
	if _, err := newMode("openwrt", "linux", "test-proc", "test-sys", etc); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unconfirmed OpenWrt selected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(etc, "openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "openwrt"} {
		p, err := newMode(mode, "linux", "test-proc", "test-sys", etc)
		if _, ok := p.(*openwrt.Platform); !ok || err != nil {
			t.Fatalf("OpenWrt selection %q = %T, %v", mode, p, err)
		}
	}
	p, err := newMode("linux", "linux", "test-proc", "test-sys", etc)
	if _, ok := p.(*linux.Platform); !ok || err != nil {
		t.Fatal("explicit Linux mode ignored")
	}
	for _, mode := range []string{"auto", "linux", "openwrt"} {
		if _, err := newMode(mode, "windows", "test-proc", "test-sys", etc); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("Windows LocalPlatform unexpectedly implemented: %v", err)
		}
	}
	if _, err := newMode("arbitrary", "linux", "test-proc", "test-sys", etc); err == nil {
		t.Fatal("invalid platform mode accepted")
	}
}
