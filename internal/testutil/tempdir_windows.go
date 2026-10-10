//go:build windows

package testutil

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

func TempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	// Registered before the fixture's Close cleanup, hence executed after it.
	// Go 1.27's testing.TempDir retries access/sharing errors but does not retry
	// ERROR_DIR_NOT_EMPTY. Atomic target replacement followed by deletion can
	// leave an NTFS directory temporarily deletion-pending. A renamed entry can
	// also appear between RemoveAll's final enumeration and parent deletion.
	// This narrow retry never tolerates removal errors beyond the deadline or
	// retries access/sharing errors. Linux uses ordinary t.TempDir directly.
	t.Cleanup(func() {
		if err := RemoveEmptyPendingDirectory(dir); err != nil {
			t.Errorf("fixture directory cleanup: %v", err)
		}
	})
	return dir
}

func RemoveEmptyPendingDirectory(dir string) error {
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
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) && !errors.Is(statErr, windows.ERROR_DELETE_PENDING) {
				return fmt.Errorf("live directory entry %q during pending cleanup: %w", name, errors.Join(err, statErr))
			}
		}
		// Restart removal even if the immediately preceding directory scan
		// exposed an extant entry. Renamed/delete-pending entries may briefly
		// look live; continuing is limited to ERROR_DIR_NOT_EMPTY and two seconds.
		time.Sleep(time.Millisecond)
	}
}
