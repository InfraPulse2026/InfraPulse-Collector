//go:build windows

package commands

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockCommandStore(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, ErrStorageUnavailable
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, ErrStorageUnavailable
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, ErrStorageUnavailable
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = f.Close()
		return nil, ErrStorageUnavailable
	}
	return f, nil
}
