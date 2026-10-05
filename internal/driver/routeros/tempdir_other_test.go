//go:build !windows

package routeros

import "testing"

func snapshotTempDir(t *testing.T) string { t.Helper(); return t.TempDir() }
