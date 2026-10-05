// Package queue provides a bounded, in-memory priority queue for outbound data.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"InfraPulseAgent/internal/model"
)

var (
	ErrFull            = errors.New("outbound queue is full")
	ErrMessageTooLarge = errors.New("outbound message exceeds size limit")
	ErrInvalidEnvelope = errors.New("envelope must contain exactly one payload")
)

type entry struct {
	data      []byte
	priority  int
	telemetry bool
	logs      bool
}

type Queue struct {
	mu               sync.Mutex
	entries          []entry
	changed          chan struct{}
	maxMessages      int
	maxBytes         int
	maxMessageBytes  int
	state            model.QueueState
	reservedMessages int
	reservedBytes    int
}

func New(maxMessages, maxBytes, maxMessageBytes int) (*Queue, error) {
	if maxMessages < 1 || maxBytes < 1 || maxMessageBytes < 1 || maxMessageBytes > maxBytes {
		return nil, errors.New("queue limits must be positive and message limit cannot exceed byte limit")
	}
	return &Queue{maxMessages: maxMessages, maxBytes: maxBytes, maxMessageBytes: maxMessageBytes, changed: make(chan struct{})}, nil
}

// Push stores an immutable serialized copy. Limits include the complete JSON
// envelope, not only its payload. Lowest priority, oldest entries are evicted
// first, provided they have lower or equal priority to the incoming message.
// Highest-priority hello/results are never evicted: an all-results queue rejects
// new results instead of silently destroying a previously accepted result.
// In-memory delivery is best effort; this queue is not a persistent outbox.
func (q *Queue) Push(envelope model.Envelope) error {
	priority, err := envelopePriority(envelope)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode outbound envelope: %w", err)
	}
	item := entry{data: data, priority: priority, telemetry: envelope.Telemetry != nil, logs: envelope.Logs != nil}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(data) > q.maxMessageBytes || len(data) > q.maxBytes {
		q.dropped(item)
		return ErrMessageTooLarge
	}
	// Plan all removals before mutation: a rejected push preserves existing data.
	removed := make([]bool, len(q.entries))
	count, size := len(q.entries), q.state.Bytes
	for count+q.reservedMessages+1 > q.maxMessages || len(data)+q.reservedBytes > q.maxBytes-size {
		candidate := -1
		for i, existing := range q.entries {
			if removed[i] || existing.priority == 0 || existing.priority < priority {
				continue
			}
			if candidate == -1 || existing.priority > q.entries[candidate].priority {
				candidate = i
			}
		}
		if candidate == -1 {
			q.dropped(item)
			return ErrFull
		}
		removed[candidate] = true
		count--
		size -= len(q.entries[candidate].data)
	}
	kept := q.entries[:0]
	for i, existing := range q.entries {
		if removed[i] {
			q.dropped(existing)
		} else {
			kept = append(kept, existing)
		}
	}
	// Release references to evicted buffers in the unused slice tail.
	clear(q.entries[len(kept):])
	q.entries = append(kept, item)
	q.state.Messages = len(q.entries)
	q.state.Bytes = size + len(data)
	close(q.changed)
	q.changed = make(chan struct{})
	return nil
}

func (q *Queue) Pop(ctx context.Context) (model.Envelope, error) {
	for {
		if err := ctx.Err(); err != nil {
			return model.Envelope{}, err
		}
		q.mu.Lock()
		if len(q.entries) > 0 {
			index := 0
			for i := 1; i < len(q.entries); i++ {
				if q.entries[i].priority < q.entries[index].priority {
					index = i
				}
			}
			item := q.entries[index]
			copy(q.entries[index:], q.entries[index+1:])
			q.entries[len(q.entries)-1] = entry{}
			q.entries = q.entries[:len(q.entries)-1]
			q.state.Messages = len(q.entries)
			q.state.Bytes -= len(item.data)
			q.mu.Unlock()
			var envelope model.Envelope
			if err := json.Unmarshal(item.data, &envelope); err != nil {
				return model.Envelope{}, fmt.Errorf("decode queued envelope: %w", err)
			}
			return envelope, nil
		}
		changed := q.changed
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return model.Envelope{}, ctx.Err()
		case <-changed:
		}
	}
}

func (q *Queue) State() model.QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.state
}

func (q *Queue) dropped(item entry) {
	if item.logs {
		q.state.DroppedLogs++
	}
	if item.telemetry {
		q.state.DroppedTelemetry++
	} else {
		q.state.DroppedOther++
	}
}

func envelopePriority(e model.Envelope) (int, error) {
	count, priority := 0, 5
	if e.Logs != nil {
		count++
		priority = 6
	}
	if e.Telephony != nil {
		count++
		priority = 4
	}
	if e.Hello != nil {
		count++
		priority = 0
	}
	if e.CommandResult != nil {
		count++
		priority = 0
	}
	if len(e.Events) > 0 {
		count++
		priority = 1
	}
	if e.Error != nil {
		count++
		priority = 1
	}
	if e.Inventory != nil {
		count++
		priority = 3
	}
	if e.Topology != nil {
		count++
		priority = 3
	}
	if e.Heartbeat != nil {
		count++
		priority = 4
	}
	if e.Telemetry != nil {
		count++
		priority = 5
	}
	if count != 1 {
		return 0, ErrInvalidEnvelope
	}
	return priority, nil
}
