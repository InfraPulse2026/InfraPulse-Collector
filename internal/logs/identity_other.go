//go:build !linux && !windows

package logs

import "os"

func openLog(string) (*os.File, error)      { return nil, ErrUnsupported }
func fileIdentity(*os.File) (string, error) { return "", ErrUnsupported }
func syncDirectory(string) error            { return ErrUnsupported }
