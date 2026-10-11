//go:build linux

package syslogreceiver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTLSMaterialRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "certificate.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "certificate-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTLSMaterialSpecialFileHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "INFRAPULSE_TLS_FILE_FIXTURE="+path)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("special TLS file blocked or was accepted: %v %s", err, output)
		}
	}
	regular := filepath.Join(dir, "regular.pem")
	if err := os.WriteFile(regular, []byte("local administrator material"), 0600); err != nil {
		t.Fatal(err)
	}
	regularLink := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Fatal(err)
	}
	if data, err := readPEM(regularLink); err != nil || string(data) != "local administrator material" {
		t.Fatal("administrator symlink to regular material rejected", err)
	}
}

func TestTLSMaterialSpecialFileHelper(t *testing.T) {
	path := os.Getenv("INFRAPULSE_TLS_FILE_FIXTURE")
	if path == "" {
		return
	}
	if _, err := readPEM(path); err == nil {
		t.Fatal("special TLS file accepted")
	}
}
