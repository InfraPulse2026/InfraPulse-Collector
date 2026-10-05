//go:build !windows

package commands

import "testing"

func commandTempDir(t *testing.T) string { t.Helper(); return t.TempDir() }
