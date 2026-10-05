package commands

import (
	"InfraPulseAgent/internal/model"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
)

type cachedResult struct {
	fingerprint [32]byte
	result      model.CommandResult
	complete    bool
	keys        []string
}
type Deduplicator struct {
	mu      sync.Mutex
	max     int
	order   []*cachedResult
	entries map[string]*cachedResult
}

func NewDeduplicator(max int) *Deduplicator {
	if max < 1 {
		max = 1
	}
	return &Deduplicator{max: max, entries: make(map[string]*cachedResult)}
}

// Begin identifies repeats by both command ID and optional idempotency key.
// It never evicts an in-flight command or starts a conflicting payload.
func (d *Deduplicator) Begin(c model.Command) (*model.CommandResult, error) {
	data, _ := json.Marshal(struct {
		Target    string
		Interface *model.InterfaceAction
		Service   *model.ServiceAction
		Initiator string
	}{c.TargetDeviceID, c.Interface, c.Service, c.Initiator})
	fp := sha256.Sum256(data)
	keys := []string{"id:" + c.ID}
	if c.IdempotencyKey != "" {
		keys = append(keys, "key:"+c.IdempotencyKey)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var existing *cachedResult
	for _, key := range keys {
		if e := d.entries[key]; e != nil {
			if e.fingerprint != fp {
				return nil, errors.New("duplicate command identity has different parameters")
			}
			if existing != nil && existing != e {
				return nil, errors.New("conflicting duplicate command identities")
			}
			existing = e
		}
	}
	if existing != nil {
		if !existing.complete {
			return nil, errors.New("command is already in progress")
		}
		if len(existing.keys)+len(keys) > 16 {
			return nil, errors.New("duplicate identity alias limit reached")
		}
		for _, key := range keys {
			if d.entries[key] == nil {
				d.entries[key] = existing
				existing.keys = append(existing.keys, key)
			}
		}
		r := existing.result
		r.CommandID = c.ID
		return &r, nil
	}
	if len(d.order) >= d.max {
		index := -1
		for i, e := range d.order {
			if e.complete {
				index = i
				break
			}
		}
		if index == -1 {
			return nil, errors.New("command admission limit reached")
		}
		e := d.order[index]
		for _, key := range e.keys {
			delete(d.entries, key)
		}
		d.order = append(d.order[:index], d.order[index+1:]...)
	}
	e := &cachedResult{fingerprint: fp, keys: keys}
	for _, key := range keys {
		d.entries[key] = e
	}
	d.order = append(d.order, e)
	return nil, nil
}
func (d *Deduplicator) Complete(c model.Command, r model.CommandResult) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.entries["id:"+c.ID]; e != nil {
		e.result = r
		e.complete = true
	}
}
