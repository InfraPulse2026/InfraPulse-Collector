//go:build windows

package agent

import (
	"InfraPulseAgent/internal/testutil"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testTempDir(t *testing.T) string { return testutil.TempDir(t) }
func removeEmptyPendingTestDirectory(dir string) error {
	return testutil.RemoveEmptyPendingDirectory(dir)
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
