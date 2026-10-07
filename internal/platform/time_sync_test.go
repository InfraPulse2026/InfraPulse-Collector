package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"InfraPulseAgent/internal/platform/timesync"
)

func TestTimeSyncPlatformSelection(t *testing.T) {
	etc := t.TempDir()
	if _, err := newTimeSync("auto", "windows", etc); !errors.Is(err, ErrUnsupported) {
		t.Fatal("Windows time provider unexpectedly supported", err)
	}
	if _, err := newTimeSync("openwrt", "linux", etc); !errors.Is(err, ErrUnsupported) {
		t.Fatal("OpenWrt selected without distribution evidence", err)
	}
	if _, err := newTimeSync("invalid", "linux", etc); err == nil {
		t.Fatal("unknown platform accepted")
	}
	p, err := newTimeSync("auto", "linux", etc)
	if err != nil || !strings.Contains(fmt.Sprintf("%T", p), "linux.") {
		t.Fatalf("Linux selection: %T %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(etc, "openwrt_release"), []byte("DISTRIB_ID='OpenWrt'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"auto", "openwrt", "linux"} {
		p, err := newTimeSync(mode, "linux", etc)
		if err != nil || !strings.Contains(fmt.Sprintf("%T", p), "openwrt.") {
			t.Fatalf("OpenWrt OS setup selection: %T %v", p, err)
		}
	}
}

func TestDisabledStatusDoesNotProbeOS(t *testing.T) {
	s := TimeSyncStatus(context.Background(), "invalid-should-not-be-used", timesync.Settings{})
	if s.Enabled || s.Available || s.Diagnostic != "" {
		t.Fatal(s)
	}
}
