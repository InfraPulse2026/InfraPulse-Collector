//go:build linux

package logs

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openLog(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func fileIdentity(f *os.File) (string, error) {
	s, err := f.Stat()
	if err != nil {
		return "", err
	}
	v, ok := s.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrRead
	}
	var birth unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &birth); err == nil && birth.Mask&unix.STATX_BTIME != 0 {
		return linuxIdentity(uint64(v.Dev), uint64(v.Ino), &birth), nil
	}
	return linuxIdentity(uint64(v.Dev), uint64(v.Ino), nil), nil
}

// Birth time distinguishes reused inode numbers without treating append/rename
// as a different file. Older kernels/filesystems may not expose STATX_BTIME;
// the fallback is marked in the identity and the offset anchor still detects
// changed content, but equal-content inode reuse cannot be distinguished.
func linuxIdentity(device, inode uint64, birth *unix.Statx_t) string {
	if birth != nil {
		return fmt.Sprintf("linux:%x:%x:b:%x:%x", device, inode, birth.Btime.Sec, birth.Btime.Nsec)
	}
	return fmt.Sprintf("linux:%x:%x:no-btime", device, inode)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
