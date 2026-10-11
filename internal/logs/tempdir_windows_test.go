//go:build windows

package logs

import (
	"testing"

	"InfraPulseAgent/internal/testutil"
)

func testTempDir(t *testing.T) string {
	t.Helper()
	return testutil.TempDir(t)
}
