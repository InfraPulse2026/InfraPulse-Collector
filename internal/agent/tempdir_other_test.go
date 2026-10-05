//go:build !windows

package agent

import "testing"

func testTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
