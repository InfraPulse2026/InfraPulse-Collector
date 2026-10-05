//go:build linux

package logs

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxBirthTimeDistinguishesReusedInode(t *testing.T) {
	first := unix.Statx_t{Btime: unix.StatxTimestamp{Sec: 1700000000, Nsec: 1}}
	next := unix.Statx_t{Btime: unix.StatxTimestamp{Sec: 1700000000, Nsec: 2}}
	if linuxIdentity(1, 42, &first) == linuxIdentity(1, 42, &next) {
		t.Fatal("reused inode concealed a replacement")
	}
	if linuxIdentity(1, 42, nil) != linuxIdentity(1, 42, nil) || linuxIdentity(1, 42, nil) == linuxIdentity(1, 43, nil) {
		t.Fatal("fallback does not preserve dev/inode identity")
	}
}

func TestLinuxFileIdentityStableAcrossAppendAndRename(t *testing.T) {
	path := filepath.Join(testTempDir(t), "log")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	first, err := fileIdentity(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("longer append content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	next, err := fileIdentity(f)
	if err != nil || first != next {
		t.Fatal("mutable timestamps treated append/rename as replacement", err)
	}
}
