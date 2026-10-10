//go:build !windows

package testutil

import "testing"

func TempDir(t testing.TB) string {
	t.Helper()
	return t.TempDir()
}
