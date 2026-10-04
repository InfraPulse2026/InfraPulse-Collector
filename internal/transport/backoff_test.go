package transport

import (
	"math"
	"testing"
	"time"
)

func TestBackoffGrowthCapAndReset(t *testing.T) {
	b := NewBackoff(time.Second, 8*time.Second)
	for _, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		delay := b.Next()
		if delay < base/2 || delay > base {
			t.Fatalf("delay %s outside [%s,%s]", delay, base/2, base)
		}
	}
	b.Reset()
	if delay := b.Next(); delay < 500*time.Millisecond || delay > time.Second {
		t.Fatalf("reset failed: %s", delay)
	}
}

func TestBackoffSmallAndOverflowingDurations(t *testing.T) {
	b := NewBackoff(time.Nanosecond, time.Nanosecond)
	if delay := b.Next(); delay != time.Nanosecond {
		t.Fatalf("busy-loop delay: %s", delay)
	}
	b = NewBackoff(time.Duration(math.MaxInt64-1), time.Duration(math.MaxInt64))
	for range 3 {
		if delay := b.Next(); delay <= 0 {
			t.Fatalf("overflow: %s", delay)
		}
	}
	b = NewBackoff(0, 0)
	if delay := b.Next(); delay <= 0 || delay > time.Second {
		t.Fatalf("invalid defaults: %s", delay)
	}
}
