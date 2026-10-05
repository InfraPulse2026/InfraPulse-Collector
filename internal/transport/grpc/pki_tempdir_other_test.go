//go:build !windows

package grpc_test

import "testing"

func pkiE2ETempDir(t *testing.T) string { t.Helper(); return t.TempDir() }
