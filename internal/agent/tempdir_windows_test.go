//go:build windows

package agent

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

func testTempDir(t *testing.T) string {
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
		if err := removeEmptyPendingTestDirectory(dir); err != nil {
			t.Errorf("agent fixture directory cleanup: %v", err)
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

func TestPendingDirectoryCleanupDoesNotMaskLockedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locked-fixture")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "locked.db")
	if err := os.WriteFile(path, []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// A fixture with a leaked handle that disallows deletion must still fail
	// cleanup. This exercises real Windows sharing semantics rather than a mock.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	started := time.Now()
	if err := removeEmptyPendingTestDirectory(dir); err == nil {
		t.Fatal("locked file cleanup was reported as successful")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("locked cleanup exceeded its bounded deadline")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "preserved" {
		t.Fatal("locked file was lost during failed cleanup", err)
	}
}
