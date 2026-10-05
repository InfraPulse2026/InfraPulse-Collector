//go:build !linux && !windows

package commands

import "os"

// Production platforms are Linux/OpenWrt. Other development platforms remain
// fail-closed until a supported single-writer file locking primitive is added.
func lockCommandStore(string) (*os.File, error) { return nil, ErrStorageUnavailable }
