//go:build windows

package logs

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openLog(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func fileIdentity(f *os.File) (string, error) {
	var s windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &s); err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%x:%x:%x:%x:%x", s.VolumeSerialNumber, s.FileIndexHigh, s.FileIndexLow, s.CreationTime.HighDateTime, s.CreationTime.LowDateTime), nil
}

// Windows does not support fsync on a directory handle through os.File.Sync.
// The cursor file is synced before the rename; crash guarantees depend on the
// filesystem, as on Linux. Production file collection is supported on Linux.
func syncDirectory(string) error { return nil }
