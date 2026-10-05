//go:build windows

package grpc_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func pkiE2ETempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Registered first, so Agent/server defers and cleanup complete before this
	// narrow NTFS deletion-pending retry. It never tolerates a live leftover file.
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) || time.Now().After(deadline) {
				t.Errorf("PKI E2E directory cleanup: %v", err)
				return
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) {
				t.Errorf("PKI E2E directory cleanup: %v", err)
				return
			}
			rel, relErr := filepath.Rel(dir, pathErr.Path)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Errorf("PKI E2E cleanup escaped fixture directory: %v", err)
				return
			}
			f, readErr := os.Open(pathErr.Path)
			if errors.Is(readErr, os.ErrNotExist) || errors.Is(readErr, windows.ERROR_DELETE_PENDING) {
				continue
			}
			if readErr != nil {
				t.Errorf("PKI E2E cleanup inspection: %v", readErr)
				return
			}
			names, readErr := f.Readdirnames(256)
			closeErr := f.Close()
			if closeErr != nil || readErr != nil && !errors.Is(readErr, io.EOF) || len(names) == 256 {
				t.Errorf("PKI E2E cleanup inspection failed: %v", errors.Join(readErr, closeErr))
				return
			}
			for _, name := range names {
				_, statErr := os.Lstat(filepath.Join(pathErr.Path, name))
				if !errors.Is(statErr, os.ErrNotExist) && !errors.Is(statErr, windows.ERROR_DELETE_PENDING) {
					t.Errorf("PKI E2E cleanup found a live file: %v", err)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	})
	return dir
}
