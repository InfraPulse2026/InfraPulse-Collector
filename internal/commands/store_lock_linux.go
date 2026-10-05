//go:build linux

package commands

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockCommandStore(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, ErrStorageUnavailable
	}
	f := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrStorageUnavailable
	}
	return f, nil
}
