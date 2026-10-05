package config

import (
	"InfraPulseAgent/internal/queue"
	"errors"
	"path/filepath"
	"time"
)

func (q Queue) WithDefaults() Queue {
	if q.Mode == "" {
		q.Mode = "memory"
	}
	if q.MaxStorageBytes == 0 {
		q.MaxStorageBytes = min(512<<20, max(64<<20, 4*q.MaxBytes))
	}
	if q.Retention == 0 {
		q.Retention = 24 * time.Hour
	}
	if q.BatchDelay == 0 {
		q.BatchDelay = 50 * time.Millisecond
	}
	if q.BatchSize == 0 {
		q.BatchSize = 64
	}
	if q.MaxInflight == 0 {
		q.MaxInflight = min(16, q.MaxMessages)
	}
	if q.ACKTimeout == 0 {
		q.ACKTimeout = 30 * time.Second
	}
	if q.ACKFallback == "" {
		q.ACKFallback = "reject"
	}
	return q
}
func (q Queue) ValidateDurable() error {
	q = q.WithDefaults()
	if (q.Mode != "memory" && q.Mode != "durable") || q.MaxStorageBytes < 1<<20 || q.MaxStorageBytes > 512<<20 || q.MaxStorageBytes < 2*q.MaxBytes || q.Retention < time.Minute || q.Retention > 30*24*time.Hour || q.BatchDelay < 0 || q.BatchDelay > time.Second || q.BatchSize < 1 || q.BatchSize > 1024 || q.MaxInflight < 1 || q.MaxInflight > q.MaxMessages || q.ACKTimeout < time.Second || q.ACKTimeout > time.Hour || (q.ACKFallback != "reject" && q.ACKFallback != "send") {
		return errors.New("invalid queue durable limits or ACK fallback")
	}
	return nil
}
func (q Queue) DurableOptions(stateDir string) queue.DurableOptions {
	q = q.WithDefaults()
	return queue.DurableOptions{Path: filepath.Join(stateDir, "outbox", "outbox.db"), MaxMessages: q.MaxMessages, MaxBytes: q.MaxBytes, MaxMessageBytes: q.MaxMessageBytes, MaxStorageBytes: q.MaxStorageBytes, Retention: q.Retention, BatchDelay: q.BatchDelay, BatchSize: q.BatchSize, MaxInflight: q.MaxInflight, ACKTimeout: q.ACKTimeout}
}
