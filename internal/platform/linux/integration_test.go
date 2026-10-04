//go:build integration && linux

package linux

import (
	"context"
	"testing"
	"time"
)

// Run explicitly on Linux: go test -tags=integration ./internal/platform/linux.
// This test needs no root privileges and does not require remote equipment.
func TestRealLinuxCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := New("/proc", "/sys")
	device, err := p.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if device.Hostname == "" || device.OS != "linux" || device.Kernel == "" {
		t.Fatalf("incomplete real identity: %+v", device)
	}
	sample, err := p.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPU.LogicalCPUs < 1 || sample.Memory.TotalBytes == 0 || len(sample.Interfaces) == 0 {
		t.Fatalf("missing real metrics: %+v", sample)
	}
	for _, iface := range sample.Interfaces {
		if iface.Index < 1 || iface.MTU < 1 {
			t.Fatalf("missing real interface metadata: %+v", iface)
		}
	}
}
