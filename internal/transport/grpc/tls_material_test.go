package grpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTLSMaterialRegularFileAndSizeBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "material.pem")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxTLSFileBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{path, dir, filepath.Join(dir, "missing")} {
		if _, err := readTLSMaterial(invalid); err == nil || strings.Contains(err.Error(), invalid) {
			t.Fatal("invalid material accepted or local path leaked", err)
		}
	}
	if err := os.WriteFile(path, []byte("regular bounded material"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readTLSMaterial(path); err != nil || string(data) != "regular bounded material" {
		t.Fatal("regular material not read exactly", err)
	}
}
