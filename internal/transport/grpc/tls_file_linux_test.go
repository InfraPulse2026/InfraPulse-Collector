//go:build linux

package grpc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestManualTLSMaterialRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	ca := makeCA(t, dir, "trust")
	cert, key := makeCertificate(t, dir, "client", ca, false)
	fifo := filepath.Join(dir, "trust.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "trust-fifo-link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"ca", "cert", "key"} {
		for _, path := range []string{fifo, link} {
			t.Run(field+filepath.Base(path), func(t *testing.T) {
				caPath, certPath, keyPath := ca.path, cert, key
				switch field {
				case "ca":
					caPath = path
				case "cert":
					certPath = path
				case "key":
					keyPath = path
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualTLSMaterialSpecialFileHelper$", "-test.count=1")
				cmd.Env = append(os.Environ(), "INFRAPULSE_GRPC_TLS_CA="+caPath, "INFRAPULSE_GRPC_TLS_CERT="+certPath, "INFRAPULSE_GRPC_TLS_KEY="+keyPath)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("special manual TLS file blocked or was accepted: %v %s", err, output)
				}
			})
		}
	}
	paths := make([]string, 3)
	for i, target := range []string{ca.path, cert, key} {
		paths[i] = filepath.Join(dir, "regular-link-"+filepath.Base(target))
		if err := os.Symlink(target, paths[i]); err != nil {
			t.Fatal(err)
		}
	}
	options := Options{Mode: "production", CAFile: paths[0], CertFile: paths[1], KeyFile: paths[2]}
	for _, server := range []bool{false, true} {
		if _, err := TLSConfig(options, server); err != nil {
			t.Fatal("administrator regular TLS symlinks rejected", err)
		}
	}
}

func TestManualTLSMaterialSpecialFileHelper(t *testing.T) {
	ca := os.Getenv("INFRAPULSE_GRPC_TLS_CA")
	if ca == "" {
		return
	}
	options := Options{Mode: "production", CAFile: ca, CertFile: os.Getenv("INFRAPULSE_GRPC_TLS_CERT"), KeyFile: os.Getenv("INFRAPULSE_GRPC_TLS_KEY")}
	if _, err := TLSConfig(options, false); err == nil {
		t.Fatal("special manual TLS material accepted")
	}
}
