package queue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"InfraPulseAgent/internal/model"
	"github.com/google/uuid"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrStorage     = errors.New("durable outbox storage unavailable")
	ErrCorrupt     = errors.New("durable outbox integrity check failed")
	ErrACKTimeout  = errors.New("controller acknowledgement timeout")
	ErrDuplicateID = errors.New("message ID reused with different content")
)

type DurableOptions struct {
	Path                                                    string
	MaxMessages, MaxBytes, MaxMessageBytes, MaxStorageBytes int
	Retention, BatchDelay, ACKTimeout                       time.Duration
	BatchSize, MaxInflight                                  int
}
type diskRecord struct {
	ID            string    `json:"id"`
	Data          []byte    `json:"data,omitempty"`
	Checksum      string    `json:"checksum"`
	Priority      int       `json:"priority"`
	CreatedAt     time.Time `json:"created_at"`
	ReservedBytes int       `json:"reserved_bytes,omitempty"`
}
type diskQueue struct {
	db        *bolt.DB
	opts      DurableOptions
	mu        sync.Mutex
	changed   chan struct{}
	leased    map[string]time.Time
	lastError string
	closed    bool
}

var recordsBucket = []byte("records-v1")
var idsBucket = []byte("ids-v1")
var metaBucket = []byte("meta-v1")

// NewDurable opens a single-writer, bounded bbolt database. Successful admission
// waits for the batch transaction's fsync. NoSync is deliberately never enabled.
func NewDurable(o DurableOptions) (q *Queue, err error) {
	if o.MaxMessages < 1 || o.MaxBytes < 1 || o.MaxMessageBytes < 1 || o.MaxMessageBytes > o.MaxBytes || o.MaxStorageBytes < 1<<20 || o.MaxStorageBytes < 2*o.MaxBytes || o.Retention <= 0 || o.BatchDelay < 0 || o.BatchDelay > time.Second || o.BatchSize < 1 || o.MaxInflight < 1 || o.MaxInflight > o.MaxMessages || o.ACKTimeout <= 0 {
		return nil, errors.New("invalid durable queue limits")
	}
	if err = os.MkdirAll(filepath.Dir(o.Path), 0700); err != nil {
		return nil, ErrStorage
	}
	if fi, e := os.Lstat(filepath.Dir(o.Path)); e != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return nil, ErrStorage
	}
	if runtime.GOOS != "windows" {
		if os.Chmod(filepath.Dir(o.Path), 0700) != nil {
			return nil, ErrStorage
		}
	}
	existing := false
	if fi, e := os.Lstat(o.Path); e == nil {
		existing = true
		if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, ErrStorage
		}
		if runtime.GOOS != "windows" {
			if os.Chmod(o.Path, 0600) != nil {
				return nil, ErrStorage
			}
		}
		if fi.Size() == 0 {
			return nil, ErrCorrupt
		}
		if fi.Size() > int64(o.MaxStorageBytes) {
			return nil, ErrFull
		}
	}
	var db *bolt.DB
	defer func() {
		if recover() != nil {
			if db != nil {
				_ = db.Close()
			}
			q = nil
			err = ErrCorrupt
		}
	}()
	// Process exit can precede the filesystem's observable lock release (WSL
	// demonstrated this). Bound the startup wait without retrying any mutation.
	db, err = bolt.Open(o.Path, 0600, &bolt.Options{Timeout: time.Second, MaxSize: o.MaxStorageBytes})
	if err != nil {
		return nil, ErrStorage
	}
	db.AllocSize = 64 << 10
	db.MaxBatchDelay = o.BatchDelay
	db.MaxBatchSize = o.BatchSize
	failed := true
	defer func() {
		if failed {
			_ = db.Close()
		}
	}()
	if existing {
		if e := db.View(func(tx *bolt.Tx) error {
			for _, name := range [][]byte{recordsBucket, idsBucket, metaBucket} {
				if tx.Bucket(name) == nil {
					return ErrCorrupt
				}
			}
			if !bytes.Equal(tx.Bucket(metaBucket).Get([]byte("version")), []byte("1")) {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return nil, ErrCorrupt
		}
	}
	if !existing {
		if err = db.Update(func(tx *bolt.Tx) error {
			for _, name := range [][]byte{recordsBucket, idsBucket, metaBucket} {
				if _, e := tx.CreateBucketIfNotExists(name); e != nil {
					return e
				}
			}
			m := tx.Bucket(metaBucket)
			if v := m.Get([]byte("version")); v != nil && !bytes.Equal(v, []byte("1")) {
				return ErrCorrupt
			}
			return m.Put([]byte("version"), []byte("1"))
		}); err != nil {
			return nil, ErrStorage
		}
	}
	d := &diskQueue{db: db, opts: o, changed: make(chan struct{}), leased: map[string]time.Time{}}
	if err = db.View(func(tx *bolt.Tx) error {
		invalid := false
		for e := range tx.Check() {
			if e != nil {
				invalid = true
			}
		}
		if invalid {
			return ErrCorrupt
		}
		if e := tx.Bucket(metaBucket).ForEach(func(k, v []byte) error {
			if string(k) == "version" {
				if !bytes.Equal(v, []byte("1")) {
					return ErrCorrupt
				}
				return nil
			}
			if string(k) != "telemetry" && string(k) != "other" && string(k) != "logs" || len(v) != 8 {
				return ErrCorrupt
			}
			return nil
		}); e != nil {
			return e
		}
		rb, ib := tx.Bucket(recordsBucket), tx.Bucket(idsBucket)
		count, size := 0, 0
		e := rb.ForEach(func(k, v []byte) error {
			r, e := decodeRecord(v)
			if e != nil {
				return e
			}
			if len(k) != 8 || !bytes.Equal(ib.Get([]byte(r.ID)), k) {
				return ErrCorrupt
			}
			count++
			size += recordSize(r)
			if recordSize(r) > o.MaxMessageBytes || count > o.MaxMessages || size > o.MaxBytes {
				return ErrFull
			}
			return nil
		})
		if e != nil {
			return e
		}
		if ib.Stats().KeyN != count {
			return ErrCorrupt
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrFull) {
			return nil, ErrFull
		}
		return nil, ErrCorrupt
	}
	// In-flight reservations belong to a process. Journal recovery restores a
	// result independently; orphan capacity placeholders never represent writes.
	if err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(recordsBucket)
		var remove [][]byte
		if e := b.ForEach(func(k, v []byte) error {
			r, e := decodeRecord(v)
			if e != nil {
				return e
			}
			if r.ReservedBytes > 0 {
				remove = append(remove, bytes.Clone(k))
			}
			return nil
		}); e != nil {
			return e
		}
		for _, k := range remove {
			if e := deleteRecord(tx, k, false); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		return nil, ErrStorage
	}
	if !existing && runtime.GOOS != "windows" {
		for _, dir := range []string{filepath.Dir(o.Path), filepath.Dir(filepath.Dir(o.Path))} {
			f, e := os.Open(dir)
			if e != nil {
				return nil, ErrStorage
			}
			e = f.Sync()
			_ = f.Close()
			if e != nil {
				return nil, ErrStorage
			}
		}
	}
	failed = false
	return &Queue{disk: d}, nil
}

func encodeRecord(r diskRecord) ([]byte, error) { return json.Marshal(r) }
func decodeRecord(v []byte) (diskRecord, error) {
	var r diskRecord
	decoder := json.NewDecoder(bytes.NewReader(v))
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&r)
	var extra any
	endErr := decoder.Decode(&extra)
	if decodeErr != nil || endErr != io.EOF || r.ID == "" || len(r.ID) > 128 || r.CreatedAt.IsZero() || r.Priority < 0 || r.Priority > 6 || r.ReservedBytes < 0 {
		return r, ErrCorrupt
	}
	if r.Checksum != recordChecksum(r) {
		return r, ErrCorrupt
	}
	if r.ReservedBytes > 0 {
		if len(r.Data) != 0 || r.Priority != 0 {
			return r, ErrCorrupt
		}
		return r, nil
	}
	var e model.Envelope
	if json.Unmarshal(r.Data, &e) != nil || e.MessageID != r.ID {
		return r, ErrCorrupt
	}
	p, err := durablePriority(e)
	if err != nil || p != r.Priority {
		return r, ErrCorrupt
	}
	return r, nil
}
func recordChecksum(r diskRecord) string {
	r.Checksum = ""
	data, _ := json.Marshal(r)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
func durablePriority(e model.Envelope) (int, error) {
	p, err := envelopePriority(e)
	if err == nil && e.Logs != nil && p == 6 {
		p = 3
	}
	return p, err
}
func newRecord(e model.Envelope) (diskRecord, error) {
	if e.MessageID == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return diskRecord{}, err
		}
		e.MessageID = id.String()
	}
	if len(e.MessageID) > 128 {
		return diskRecord{}, ErrInvalidEnvelope
	}
	p, err := durablePriority(e)
	if err != nil {
		return diskRecord{}, err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return diskRecord{}, err
	}
	r := diskRecord{ID: e.MessageID, Data: data, Priority: p, CreatedAt: time.Now().UTC()}
	r.Checksum = recordChecksum(r)
	return r, nil
}
func recordSize(r diskRecord) int {
	if r.ReservedBytes > 0 {
		return r.ReservedBytes
	}
	return len(r.Data)
}
func increment(tx *bolt.Tx, key string) {
	b := tx.Bucket(metaBucket)
	var n uint64
	if v := b.Get([]byte(key)); len(v) == 8 {
		n = binary.BigEndian.Uint64(v)
	}
	v := make([]byte, 8)
	binary.BigEndian.PutUint64(v, n+1)
	_ = b.Put([]byte(key), v)
}
func countDrop(tx *bolt.Tx, r diskRecord) {
	var e model.Envelope
	_ = json.Unmarshal(r.Data, &e)
	if e.Telemetry != nil {
		increment(tx, "telemetry")
	} else {
		increment(tx, "other")
	}
	if e.Logs != nil {
		increment(tx, "logs")
	}
}
func deleteRecord(tx *bolt.Tx, k []byte, drop bool) error {
	b := tx.Bucket(recordsBucket)
	v := b.Get(k)
	if v == nil {
		return nil
	}
	r, e := decodeRecord(v)
	if e != nil {
		return e
	}
	if drop {
		countDrop(tx, r)
	}
	if e = tx.Bucket(idsBucket).Delete([]byte(r.ID)); e != nil {
		return e
	}
	return b.Delete(k)
}

type keyedRecord struct {
	key []byte
	r   diskRecord
}

func (d *diskQueue) admit(tx *bolt.Tx, r diskRecord) error {
	b, ids := tx.Bucket(recordsBucket), tx.Bucket(idsBucket)
	if k := ids.Get([]byte(r.ID)); k != nil {
		old, e := decodeRecord(b.Get(k))
		if e != nil {
			return e
		}
		if bytes.Equal(old.Data, r.Data) && old.ReservedBytes == r.ReservedBytes {
			return nil
		}
		return ErrDuplicateID
	}
	if recordSize(r) > d.opts.MaxMessageBytes {
		return ErrMessageTooLarge
	}
	var items []keyedRecord
	size := 0
	if e := b.ForEach(func(k, v []byte) error {
		x, e := decodeRecord(v)
		if e != nil {
			return e
		}
		items = append(items, keyedRecord{bytes.Clone(k), x})
		size += recordSize(x)
		return nil
	}); e != nil {
		return e
	}
	// Expiration/eviction never removes a pending command result/reservation.
	now := time.Now()
	count := len(items)
	removed := make([]bool, len(items))
	for i, item := range items {
		if item.r.Priority != 0 && now.Sub(item.r.CreatedAt) > d.opts.Retention {
			removed[i] = true
			count--
			size -= recordSize(item.r)
		}
	}
	for count+1 > d.opts.MaxMessages || size+recordSize(r) > d.opts.MaxBytes {
		candidate := -1
		for i, item := range items {
			if removed[i] || item.r.Priority == 0 || item.r.Priority < r.Priority {
				continue
			}
			if candidate < 0 || item.r.Priority > items[candidate].r.Priority {
				candidate = i
			}
		}
		if candidate < 0 {
			return ErrFull
		}
		removed[candidate] = true
		count--
		size -= recordSize(items[candidate].r)
	}
	for i, item := range items {
		if removed[i] {
			if e := deleteRecord(tx, item.key, true); e != nil {
				return e
			}
		}
	}
	seq, e := b.NextSequence()
	if e != nil {
		return e
	}
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	data, e := encodeRecord(r)
	if e != nil {
		return e
	}
	if e = b.Put(key, data); e != nil {
		return e
	}
	return ids.Put([]byte(r.ID), key)
}
func (d *diskQueue) signal() {
	d.mu.Lock()
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
}
func (d *diskQueue) write(fn func(*bolt.Tx) error) error {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return ErrStorage
	}
	err := d.db.Batch(fn)
	d.mu.Lock()
	if err != nil {
		d.lastError = "storage_unavailable"
	} else {
		d.lastError = ""
	}
	d.mu.Unlock()
	if err != nil {
		if errors.Is(err, ErrCorrupt) {
			return ErrCorrupt
		}
		if errors.Is(err, ErrFull) || errors.Is(err, ErrMessageTooLarge) || errors.Is(err, ErrDuplicateID) {
			return err
		}
		return ErrStorage
	}
	d.cleanLeases()
	d.signal()
	return nil
}

func (d *diskQueue) cleanLeases() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.leased))
	for id := range d.leased {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	var gone []string
	if err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(idsBucket)
		for _, id := range ids {
			if b.Get([]byte(id)) == nil {
				gone = append(gone, id)
			}
		}
		return nil
	}); err != nil {
		return
	}
	d.mu.Lock()
	for _, id := range gone {
		delete(d.leased, id)
	}
	d.mu.Unlock()
}
func (d *diskQueue) push(e model.Envelope) error {
	r, err := newRecord(e)
	if err != nil {
		return err
	}
	var rejected error
	err = d.write(func(tx *bolt.Tx) error {
		rejected = d.admit(tx, r)
		if errors.Is(rejected, ErrFull) || errors.Is(rejected, ErrMessageTooLarge) {
			countDrop(tx, r)
			return nil
		}
		return rejected
	})
	if err != nil {
		return err
	}
	return rejected
}
func (d *diskQueue) reserve(n int) (*Reservation, error) {
	if n < 1 || n > d.opts.MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	r := diskRecord{ID: "reserve:" + id.String(), Priority: 0, CreatedAt: time.Now().UTC(), ReservedBytes: n}
	r.Checksum = recordChecksum(r)
	if err = d.write(func(tx *bolt.Tx) error { return d.admit(tx, r) }); err != nil {
		return nil, err
	}
	return &Reservation{disk: d, id: r.ID, bytes: n, active: true}, nil
}
func (d *diskQueue) release(r *Reservation) {
	if !r.active {
		return
	}
	_ = d.write(func(tx *bolt.Tx) error {
		k := tx.Bucket(idsBucket).Get([]byte(r.id))
		if k != nil {
			return deleteRecord(tx, k, false)
		}
		return nil
	})
	r.active = false
}
func (d *diskQueue) commit(res *Reservation, e model.Envelope) error {
	if !res.active {
		return errors.New("result reservation is closed")
	}
	r, err := newRecord(e)
	if err != nil {
		return err
	}
	if len(r.Data) > res.bytes {
		return ErrMessageTooLarge
	}
	err = d.write(func(tx *bolt.Tx) error {
		b, ids := tx.Bucket(recordsBucket), tx.Bucket(idsBucket)
		k := ids.Get([]byte(res.id))
		if k == nil {
			return ErrStorage
		}
		if existing := ids.Get([]byte(r.ID)); existing != nil {
			old, e := decodeRecord(b.Get(existing))
			if e != nil {
				return e
			}
			if !bytes.Equal(old.Data, r.Data) {
				return ErrDuplicateID
			}
			return deleteRecord(tx, k, false)
		}
		key := bytes.Clone(k)
		data, e := encodeRecord(r)
		if e != nil {
			return e
		}
		if e = ids.Delete([]byte(res.id)); e != nil {
			return e
		}
		if e = b.Put(key, data); e != nil {
			return e
		}
		return ids.Put([]byte(r.ID), key)
	})
	if err == nil {
		res.active = false
	}
	return err
}
func (d *diskQueue) pop(ctx context.Context) (model.Envelope, error) {
	for {
		if err := ctx.Err(); err != nil {
			return model.Envelope{}, err
		}
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return model.Envelope{}, ErrStorage
		}
		leased := make(map[string]time.Time, len(d.leased))
		for k, v := range d.leased {
			leased[k] = v
		}
		changed := d.changed
		d.mu.Unlock()
		d.cleanLeases()
		d.mu.Lock()
		for id := range leased {
			if _, ok := d.leased[id]; !ok {
				delete(leased, id)
			}
		}
		d.mu.Unlock()
		var selected *diskRecord
		for _, at := range leased {
			if time.Since(at) >= d.opts.ACKTimeout {
				return model.Envelope{}, ErrACKTimeout
			}
		}
		err := d.db.View(func(tx *bolt.Tx) error {
			return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
				r, e := decodeRecord(v)
				if e != nil {
					return e
				}
				if r.ReservedBytes > 0 {
					return nil
				}
				if _, ok := leased[r.ID]; ok {
					return nil
				}
				if selected == nil || r.Priority < selected.Priority {
					x := r
					selected = &x
				}
				return nil
			})
		})
		if err != nil {
			return model.Envelope{}, ErrCorrupt
		}
		if selected != nil && len(leased) < d.opts.MaxInflight {
			d.mu.Lock()
			d.leased[selected.ID] = time.Now()
			d.mu.Unlock()
			var e model.Envelope
			if json.Unmarshal(selected.Data, &e) != nil {
				return e, ErrCorrupt
			}
			return e, nil
		}
		wait := d.opts.ACKTimeout
		for _, at := range leased {
			remaining := time.Until(at.Add(d.opts.ACKTimeout))
			if remaining <= 0 {
				return model.Envelope{}, ErrACKTimeout
			}
			if remaining < wait {
				wait = remaining
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return model.Envelope{}, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (q *Queue) Acknowledge(id string) (bool, error) {
	if q.disk == nil {
		return false, nil
	}
	d := q.disk
	if len(id) == 0 || len(id) > 128 {
		return false, ErrInvalidEnvelope
	}
	d.mu.Lock()
	_, sent := d.leased[id]
	d.mu.Unlock()
	if !sent {
		return false, nil
	}
	found := false
	err := d.write(func(tx *bolt.Tx) error {
		k := tx.Bucket(idsBucket).Get([]byte(id))
		found = k != nil
		if k != nil {
			return deleteRecord(tx, k, false)
		}
		return nil
	})
	if err == nil {
		d.mu.Lock()
		delete(d.leased, id)
		d.mu.Unlock()
		d.signal()
	}
	return found, err
}
func (q *Queue) ResetInflight() {
	if q.disk != nil {
		d := q.disk
		d.mu.Lock()
		clear(d.leased)
		d.mu.Unlock()
		d.signal()
	}
}
func (q *Queue) IsDurable() bool { return q.disk != nil }
func (q *Queue) Contains(id string) bool {
	if q.disk == nil {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, r := range q.entries {
			var e model.Envelope
			_ = json.Unmarshal(r.data, &e)
			if e.MessageID == id {
				return true
			}
		}
		return false
	}
	found := false
	_ = q.disk.db.View(func(tx *bolt.Tx) error { found = tx.Bucket(idsBucket).Get([]byte(id)) != nil; return nil })
	return found
}
func (q *Queue) Close() error {
	if q.disk == nil {
		return nil
	}
	d := q.disk
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
	return d.db.Close()
}
func (d *diskQueue) state() model.QueueState {
	s := model.QueueState{Mode: "durable"}
	d.mu.Lock()
	s.Unacked = len(d.leased)
	s.StorageError = d.lastError
	closed := d.closed
	d.mu.Unlock()
	if closed {
		s.StorageError = "closed"
		return s
	}
	err := d.db.View(func(tx *bolt.Tx) error {
		m := tx.Bucket(metaBucket)
		for key, p := range map[string]*uint64{"telemetry": &s.DroppedTelemetry, "other": &s.DroppedOther, "logs": &s.DroppedLogs} {
			if v := m.Get([]byte(key)); len(v) == 8 {
				*p = binary.BigEndian.Uint64(v)
			}
		}
		return tx.Bucket(recordsBucket).ForEach(func(_, v []byte) error {
			r, e := decodeRecord(v)
			if e != nil {
				return e
			}
			s.Bytes += recordSize(r)
			s.Messages++
			if r.ReservedBytes == 0 {
				if s.OldestPending.IsZero() || r.CreatedAt.Before(s.OldestPending) {
					s.OldestPending = r.CreatedAt
				}
				if r.Priority == 0 {
					s.PendingResults++
				}
			}
			return nil
		})
	})
	if err != nil {
		s.StorageError = "integrity_failure"
	}
	if fi, e := os.Stat(d.opts.Path); e == nil {
		s.StorageBytes = fi.Size()
	}
	return s
}
