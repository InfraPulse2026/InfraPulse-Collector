package transport

import (
	"math/rand/v2"
	"sync"
	"time"
)

type Backoff struct {
	mu      sync.Mutex
	initial time.Duration
	maximum time.Duration
	next    time.Duration
}

func NewBackoff(initial, maximum time.Duration) *Backoff {
	if initial <= 0 {
		initial = time.Second
	}
	if maximum <= 0 {
		maximum = 30 * time.Second
	}
	if maximum < initial {
		maximum = initial
	}
	return &Backoff{initial: initial, maximum: maximum, next: initial}
}

// Next returns equal jitter in [base/2, base], where base doubles up to maximum.
// The strictly positive result prevents an outage from producing a busy loop.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	base := b.next
	if base >= b.maximum-base {
		b.next = b.maximum
	} else {
		b.next = base * 2
	}
	minimum := base / 2
	if minimum < 1 {
		minimum = 1
	}
	return minimum + time.Duration(rand.Int64N(int64(base-minimum)+1))
}

func (b *Backoff) Reset() {
	b.mu.Lock()
	b.next = b.initial
	b.mu.Unlock()
}
