//go:build linux

package pki

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPKIMaterialRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "trust.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "trust-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPKIMaterialSpecialFileHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "INFRAPULSE_TLS_FILE_FIXTURE="+path)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("special PKI file blocked or was accepted: %v %s", err, output)
		}
	}
	regular := filepath.Join(dir, "regular.pem")
	if err := os.WriteFile(regular, []byte("local administrator trust"), 0600); err != nil {
		t.Fatal(err)
	}
	regularLink := filepath.Join(dir, "regular-link")
	if err := os.Symlink(regular, regularLink); err != nil {
		t.Fatal(err)
	}
	if data, err := readBounded(regularLink); err != nil || string(data) != "local administrator trust" {
		t.Fatal("administrator symlink to regular trust rejected", err)
	}
}

func TestPKIMaterialSpecialFileHelper(t *testing.T) {
	path := os.Getenv("INFRAPULSE_TLS_FILE_FIXTURE")
	if path == "" {
		return
	}
	if _, err := readBounded(path); err == nil {
		t.Fatal("special PKI file accepted")
	}
}

func TestManualCertificateRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "manual.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(dir, "other.pem")
	if err := os.WriteFile(regular, []byte("invalid but regular material"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []bool{false, true} {
		certPath, keyPath := fifo, regular
		if key {
			certPath, keyPath = regular, fifo
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualCertificateSpecialFileHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "INFRAPULSE_MANUAL_CERT="+certPath, "INFRAPULSE_MANUAL_KEY="+keyPath)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("manual certificate/key FIFO blocked or was accepted: %v %s", err, output)
		}
	}
}

func TestManualCertificateSpecialFileHelper(t *testing.T) {
	certPath := os.Getenv("INFRAPULSE_MANUAL_CERT")
	if certPath == "" {
		return
	}
	if err := VerifyCertificateFile(certPath, os.Getenv("INFRAPULSE_MANUAL_KEY"), "11111111-1111-4111-8111-111111111111"); err == nil {
		t.Fatal("special manual certificate/key accepted")
	}
}

func TestManualCertificateAdministratorSymlinksAndIdentity(t *testing.T) {
	f := newFixture(t, time.Now().Add(time.Hour), nil)
	m, err := New(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.path())
	if err != nil {
		t.Fatal(err)
	}
	var b bundle
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, material := range map[string][]byte{certPath: b.Certificate, keyPath: b.PrivateKey} {
		if err := os.WriteFile(path, material, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, path+".link"); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyCertificateFile(certPath+".link", keyPath+".link", f.agentID); err != nil {
		t.Fatal("administrator regular certificate/key links rejected", err)
	}
	if err := VerifyCertificateFile(certPath+".link", keyPath+".link", "11111111-1111-4111-8111-111111111111"); err == nil {
		t.Fatal("manual certificate authenticated another Collector")
	}
	file, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	truncateErr := file.Truncate(maxPKIBytes + 1)
	closeErr := file.Close()
	if truncateErr != nil || closeErr != nil {
		t.Fatal(truncateErr, closeErr)
	}
	if err := VerifyCertificateFile(certPath+".link", keyPath+".link", f.agentID); err == nil {
		t.Fatal("oversized linked certificate accepted")
	}
}
