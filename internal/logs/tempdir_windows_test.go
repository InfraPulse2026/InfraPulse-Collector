//go:build windows

package logs

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

func testTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// Registered before the fixture's Close cleanup, hence executed after it.
	// Go 1.27's testing.TempDir retries access/sharing errors but does not retry
	// ERROR_DIR_NOT_EMPTY. Atomic target replacement followed by deletion can
	// leave an already empty NTFS directory temporarily deletion-pending. This
	// narrow retry mirrors stdlib cleanup and never tolerates files/leaked handles
	// that persist beyond the deadline. Linux uses ordinary t.TempDir directly.
	t.Cleanup(func() {
		if err := removeEmptyPendingTestDirectory(dir); err != nil {
			t.Errorf("log fixture directory cleanup: %v", err)
		}
	})
	return dir
}

func removeEmptyPendingTestDirectory(dir string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.RemoveAll(dir)
		if err == nil || !errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) || time.Now().After(deadline) {
			return err
		}
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return err
		}
		rel, relErr := filepath.Rel(dir, pathErr.Path)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return err
		}
		f, readErr := os.Open(pathErr.Path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		names, readErr := f.Readdirnames(1)
		closeErr := f.Close()
		if len(names) != 0 || !errors.Is(readErr, io.EOF) || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		time.Sleep(time.Millisecond)
	}
}
