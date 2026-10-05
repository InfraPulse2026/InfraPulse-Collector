//go:build !windows

package logs

import "testing"

func testTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
