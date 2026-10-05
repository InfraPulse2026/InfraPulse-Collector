//go:build windows

package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func commandTempDir(t *testing.T) string {
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
			t.Errorf("command fixture directory cleanup: %v", err)
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
		if errors.Is(readErr, os.ErrNotExist) || errors.Is(readErr, windows.ERROR_DELETE_PENDING) {
			time.Sleep(time.Millisecond)
			continue
		}
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		names, readErr := f.Readdirnames(256)
		closeErr := f.Close()
		if closeErr != nil || readErr != nil && !errors.Is(readErr, io.EOF) || len(names) == 256 {
			return fmt.Errorf("inspect pending cleanup directory: %w", errors.Join(err, readErr, closeErr))
		}
		for _, name := range names {
			_, statErr := os.Lstat(filepath.Join(pathErr.Path, name))
			if !errors.Is(statErr, os.ErrNotExist) && !errors.Is(statErr, windows.ERROR_DELETE_PENDING) {
				return fmt.Errorf("live directory entry %q during pending cleanup: %w", name, errors.Join(err, statErr))
			}
		}
		time.Sleep(time.Millisecond)
	}
}
