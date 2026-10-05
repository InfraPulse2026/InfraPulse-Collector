//go:build windows

package pki

// Windows is a development build only; file Sync and atomic rename are retained.
func syncDirectory(string) error { return nil }

func verifyPrivatePermissions(string) error { return nil }
