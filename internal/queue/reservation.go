package queue

import (
	"InfraPulseAgent/internal/model"
	"encoding/json"
	"errors"
)

// Reservation retains result capacity before a mutation starts. It guarantees
// in-memory admission, not delivery or persistence across restart.
type Reservation struct {
	queue  *Queue
	bytes  int
	active bool
}

func (q *Queue) ReserveResult(bytes int) (*Reservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if bytes < 1 || bytes > q.maxMessageBytes || bytes > q.maxBytes {
		return nil, ErrMessageTooLarge
	}
	removed := make([]bool, len(q.entries))
	count, size := len(q.entries), q.state.Bytes
	for count+q.reservedMessages+1 > q.maxMessages || size+q.reservedBytes+bytes > q.maxBytes {
		candidate := -1
		for i, e := range q.entries {
			if removed[i] || e.priority == 0 {
				continue
			}
			if candidate == -1 || e.priority > q.entries[candidate].priority {
				candidate = i
			}
		}
		if candidate == -1 {
			return nil, ErrFull
		}
		removed[candidate] = true
		count--
		size -= len(q.entries[candidate].data)
	}
	kept := q.entries[:0]
	for i, e := range q.entries {
		if removed[i] {
			q.dropped(e)
		} else {
			kept = append(kept, e)
		}
	}
	clear(q.entries[len(kept):])
	q.entries = kept
	q.state.Messages = count
	q.state.Bytes = size
	q.reservedMessages++
	q.reservedBytes += bytes
	return &Reservation{queue: q, bytes: bytes, active: true}, nil
}
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	q := r.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.active {
		r.active = false
		q.reservedMessages--
		q.reservedBytes -= r.bytes
	}
}
func (r *Reservation) Commit(e model.Envelope) error {
	if r == nil || e.CommandResult == nil {
		return ErrInvalidEnvelope
	}
	p, err := envelopePriority(e)
	if err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	q := r.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if !r.active {
		return errors.New("result reservation is closed")
	}
	if len(data) > r.bytes {
		return ErrMessageTooLarge
	}
	r.active = false
	q.reservedMessages--
	q.reservedBytes -= r.bytes
	q.entries = append(q.entries, entry{data: data, priority: p})
	q.state.Messages = len(q.entries)
	q.state.Bytes += len(data)
	close(q.changed)
	q.changed = make(chan struct{})
	return nil
}
