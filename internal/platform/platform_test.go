package platform

import (
	"errors"
	"runtime"
	"testing"
)

func TestNewSupportsLinuxOnly(t *testing.T) {
	p, err := New()
	if runtime.GOOS == "linux" {
		if err != nil || p == nil {
			t.Fatalf("New = %v, %v", p, err)
		}
		return
	}
	if p != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("New = %v, %v", p, err)
	}
}
