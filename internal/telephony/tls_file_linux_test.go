//go:build linux

package telephony

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTLSCARejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ca.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ca-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTLSCASpecialFileHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "INFRAPULSE_TLS_FILE_FIXTURE="+path)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("special CA file blocked or was accepted: %v %s", err, output)
		}
	}
	_, ca := tlsCertificates(t, false)
	regular := filepath.Join(dir, "regular.pem")
	if err := os.WriteFile(regular, ca, 0600); err != nil {
		t.Fatal(err)
	}
	regularLink := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Fatal(err)
	}
	if _, err := (Endpoint{Address: "localhost", TLS: TLS{CAFile: regularLink}}).tlsConfig(); err != nil {
		t.Fatal("administrator symlink to regular CA rejected", err)
	}
}

func TestTLSCASpecialFileHelper(t *testing.T) {
	path := os.Getenv("INFRAPULSE_TLS_FILE_FIXTURE")
	if path == "" {
		return
	}
	if _, err := (Endpoint{Address: "localhost", TLS: TLS{CAFile: path}}).tlsConfig(); err == nil {
		t.Fatal("special CA file accepted")
	}
}
