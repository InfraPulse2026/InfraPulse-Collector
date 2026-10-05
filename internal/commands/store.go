package commands

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/google/uuid"
)

var (
	ErrStorageUnavailable = errors.New("command state storage unavailable")
	ErrStoreFull          = errors.New("command state storage full")
	ErrStoreCorrupt       = errors.New("command state storage corrupted")
	ErrSnapshotConflict   = errors.New("snapshot conflicts with current resource state")
	ErrSnapshotExpired    = errors.New("snapshot expired or unavailable")
)

const resultReservationBytes = 4096

type StoreOptions struct {
	JournalMaxEntries, JournalMaxBytes   int
	JournalRetention                     time.Duration
	SnapshotMaxEntries, SnapshotMaxBytes int
	SnapshotRetention                    time.Duration
}

func DefaultStoreOptions() StoreOptions {
	return StoreOptions{JournalMaxEntries: 256, JournalMaxBytes: 4 << 20, JournalRetention: 7 * 24 * time.Hour,
		SnapshotMaxEntries: 256, SnapshotMaxBytes: 2 << 20, SnapshotRetention: 24 * time.Hour}
}

func (o StoreOptions) Validate() error {
	if o.JournalMaxEntries < 1 || o.JournalMaxEntries > 4096 || o.JournalMaxBytes < 16<<10 || o.JournalMaxBytes > 64<<20 ||
		o.SnapshotMaxEntries < 1 || o.SnapshotMaxEntries > 4096 || o.SnapshotMaxBytes < 1024 || o.SnapshotMaxBytes > 32<<20 ||
		o.JournalRetention < time.Minute || o.JournalRetention > 365*24*time.Hour || o.SnapshotRetention < time.Minute || o.SnapshotRetention > 30*24*time.Hour {
		return errors.New("invalid bounded command store options")
	}
	return nil
}

type JournalRecord struct {
	Command           model.Command        `json:"command"`
	Fingerprint       string               `json:"fingerprint"`
	CommandIDs        []string             `json:"command_ids"`
	IdempotencyKeys   []string             `json:"idempotency_keys,omitempty"`
	TargetDevice      string               `json:"target_device"`
	Operation         string               `json:"operation"`
	Resource          string               `json:"resource"`
	ReceivedAt        time.Time            `json:"received_at"`
	State             string               `json:"state"`
	VerificationState string               `json:"verification_state"`
	DeliveryState     string               `json:"delivery_state"`
	Result            *model.CommandResult `json:"result,omitempty"`
	Envelope          *model.Envelope      `json:"envelope,omitempty"`
	SnapshotID        string               `json:"snapshot_id,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	Reservation       string               `json:"result_capacity_reservation,omitempty"`
}

type storeData struct {
	Version    uint32                        `json:"version"`
	Generation uint64                        `json:"generation"`
	Journal    []JournalRecord               `json:"journal"`
	Snapshots  []model.ConfigurationSnapshot `json:"snapshots"`
}
type diskStore struct {
	Version  uint32          `json:"version"`
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}
type StoreHealth struct {
	Available         bool   `json:"available"`
	JournalEntries    int    `json:"journal_entries"`
	PendingResults    int    `json:"pending_results"`
	UncertainCommands int    `json:"uncertain_commands"`
	Snapshots         int    `json:"snapshots"`
	Bytes             int    `json:"bytes"`
	LastError         string `json:"last_error,omitempty"`
}

// Store persists rare command transitions as a bounded, checksummed atomic
// snapshot. Every admission and pre-mutation transition is synced, unlike the
// telemetry batching strategy. No state is silently reset after corruption.
type Store struct {
	mu         sync.Mutex
	dir, path  string
	opts       StoreOptions
	data       storeData
	lastErr    error
	bytes      int
	lock       *os.File
	writeState func([]byte) error
}

func OpenStore(dir string, opts StoreOptions) (*Store, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, path: filepath.Join(dir, "safety-state.json"), opts: opts, data: storeData{Version: 1}}
	s.writeState = s.atomicWrite
	if err := os.MkdirAll(dir, 0700); err != nil {
		return s, s.degrade(ErrStorageUnavailable)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return s, s.degrade(ErrStorageUnavailable)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0700); err != nil {
			return s, s.degrade(ErrStorageUnavailable)
		}
	}
	s.lock, err = lockCommandStore(filepath.Join(dir, "safety-state.lock"))
	if err != nil {
		return s, s.degrade(ErrStorageUnavailable)
	}
	info, err = os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if err = s.save(s.data); err != nil {
			return s, err
		}
		return s, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return s, s.degrade(ErrStorageUnavailable)
	}
	max := int64(opts.JournalMaxBytes + opts.SnapshotMaxBytes + (64 << 10))
	if info.Size() > max {
		return s, s.degrade(ErrStoreCorrupt)
	}
	f, err := os.Open(s.path)
	if err != nil {
		return s, s.degrade(ErrStorageUnavailable)
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	_ = f.Close()
	if err != nil || int64(len(raw)) > max {
		return s, s.degrade(ErrStoreCorrupt)
	}
	var disk diskStore
	if strictJSON(raw, &disk) != nil || disk.Version != 1 {
		return s, s.degrade(ErrStoreCorrupt)
	}
	sum := sha256.Sum256(disk.Data)
	if disk.Checksum != hex.EncodeToString(sum[:]) || strictJSON(disk.Data, &s.data) != nil || s.validateData(s.data) != nil {
		return s, s.degrade(ErrStoreCorrupt)
	}
	s.bytes = len(raw)
	// A reserved command did not reach the persisted mutation boundary. An
	// executing command may have affected the device; never retry it on startup.
	next := cloneStore(s.data)
	changed := false
	for i := range next.Journal {
		r := &next.Journal[i]
		if r.State != "reserved" && r.State != "executing" {
			continue
		}
		now := time.Now().UTC()
		result := model.CommandResult{CommandID: r.Command.ID, StartedAt: r.CreatedAt, FinishedAt: now, Status: "rejected", ErrorCode: "execution_interrupted", ErrorMessage: "command interrupted before durable execution boundary"}
		if r.State == "executing" {
			result.Status, result.ErrorCode, result.ErrorMessage, result.Uncertain = "timeout", "mutation_uncertain", "agent restarted after execution reservation; device outcome requires operator review", true
		}
		result.SnapshotID = r.SnapshotID
		id, e := uuid.NewRandom()
		if e != nil {
			return s, s.degrade(ErrStorageUnavailable)
		}
		r.Result = &result
		r.Envelope = &model.Envelope{MessageID: id.String(), SentAt: now, CommandResult: &result}
		r.State = executionState(result)
		r.DeliveryState, r.VerificationState = "result_pending_ack", "unverified"
		r.Reservation = ""
		r.UpdatedAt = now
		changed = true
	}
	if changed {
		if err := s.save(next); err != nil {
			return s, err
		}
		s.data = next
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	s.lastErr = ErrStorageUnavailable
	return err
}

func strictJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ErrStoreCorrupt
	}
	return nil
}

func cloneStore(d storeData) storeData {
	b, _ := json.Marshal(d)
	var copy storeData
	_ = json.Unmarshal(b, &copy)
	return copy
}
func (s *Store) degrade(err error) error { s.lastErr = err; return err }

func (s *Store) save(next storeData) error {
	if s.lastErr != nil {
		return ErrStorageUnavailable
	}
	if err := s.validateData(next); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return s.degrade(ErrStorageUnavailable)
	}
	sum := sha256.Sum256(data)
	raw, err := json.Marshal(diskStore{Version: 1, Checksum: hex.EncodeToString(sum[:]), Data: data})
	if err != nil {
		return s.degrade(ErrStorageUnavailable)
	}
	if err := s.writeState(raw); err != nil {
		return s.degrade(ErrStorageUnavailable)
	}
	s.bytes = len(raw)
	return nil
}

func (s *Store) atomicWrite(raw []byte) error {
	// A single staging filename bounds disk growth even after repeated crashes.
	// The state directory is private; reject a replaced/nonregular staging file.
	name := filepath.Join(s.dir, ".safety-state.tmp")
	if info, e := os.Lstat(name); e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return s.degrade(ErrStorageUnavailable)
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return s.degrade(ErrStorageUnavailable)
	}
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, s.path)
	}
	if err == nil && runtime.GOOS != "windows" {
		var directory *os.File
		directory, err = os.Open(s.dir)
		if err == nil {
			err = directory.Sync()
			_ = directory.Close()
		}
	}
	if err != nil {
		return s.degrade(ErrStorageUnavailable)
	}
	return nil
}

func commandFingerprint(c model.Command) string {
	data, _ := json.Marshal(struct {
		Target    string
		Interface *model.InterfaceAction
		Service   *model.ServiceAction
		Rollback  *model.RollbackAction
		Initiator string
	}{c.TargetDeviceID, c.Interface, c.Service, c.Rollback, c.Initiator})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func operationResource(c model.Command) (string, string) {
	if c.Interface != nil {
		return "interface." + c.Interface.Operation, c.Interface.InterfaceID
	}
	if c.Rollback != nil {
		return "resource.rollback", c.Rollback.SnapshotID
	}
	if c.Service != nil {
		return "service." + c.Service.Operation, c.Service.Service
	}
	return "", ""
}
func findCommand(data storeData, c model.Command) (int, error) {
	index := -1
	for i, r := range data.Journal {
		if !contains(r.CommandIDs, c.ID) && (c.IdempotencyKey == "" || !contains(r.IdempotencyKeys, c.IdempotencyKey)) {
			continue
		}
		if index >= 0 && index != i || r.Fingerprint != commandFingerprint(c) {
			return -1, errors.New("duplicate command identity has different parameters")
		}
		index = i
	}
	return index, nil
}

// LookupCommand returns retained historical state without reserving capacity,
// adding aliases or applying current execution policy/freshness. Callers still
// validate the typed shape and must never use lookup alone to start a mutation.
func (s *Store) LookupCommand(c model.Command) (*JournalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return nil, ErrStorageUnavailable
	}
	index, err := findCommand(s.data, c)
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, nil
	}
	r := cloneStore(s.data).Journal[index]
	return &r, nil
}

// Begin persists command admission and a padded future-result allocation. Only
// acknowledged entries older than retention are removed, so bounded dedup has
// an explicit horizon. Pending or uncertain results are never evicted.
func (s *Store) Begin(c model.Command, now time.Time) (*JournalRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return nil, false, ErrStorageUnavailable
	}
	index, err := findCommand(s.data, c)
	if err != nil {
		return nil, false, err
	}
	if index >= 0 {
		next := cloneStore(s.data)
		r := &next.Journal[index]
		changed := false
		if !contains(r.CommandIDs, c.ID) {
			if len(r.CommandIDs) >= 16 {
				return nil, true, errors.New("duplicate identity alias limit reached")
			}
			r.CommandIDs = append(r.CommandIDs, c.ID)
			changed = true
		}
		if c.IdempotencyKey != "" && !contains(r.IdempotencyKeys, c.IdempotencyKey) {
			if len(r.IdempotencyKeys) >= 16 {
				return nil, true, errors.New("duplicate identity alias limit reached")
			}
			r.IdempotencyKeys = append(r.IdempotencyKeys, c.IdempotencyKey)
			changed = true
		}
		if changed {
			if err := s.save(next); err != nil {
				return nil, true, err
			}
			s.data = next
		}
		copy := cloneStore(s.data).Journal[index]
		return &copy, true, nil
	}
	next := cloneStore(s.data)
	s.prune(&next, now)
	if len(next.Journal) >= s.opts.JournalMaxEntries {
		return nil, false, ErrStoreFull
	}
	op, resource := operationResource(c)
	next.Journal = append(next.Journal, JournalRecord{Command: c, Fingerprint: commandFingerprint(c), CommandIDs: []string{c.ID}, TargetDevice: c.TargetDeviceID,
		Operation: op, Resource: resource, ReceivedAt: now.UTC(), State: "reserved", VerificationState: "not_started", DeliveryState: "not_ready", CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Reservation: strings.Repeat(".", 2*resultReservationBytes+128)})
	if c.IdempotencyKey != "" {
		next.Journal[len(next.Journal)-1].IdempotencyKeys = []string{c.IdempotencyKey}
	}
	if err := s.save(next); err != nil {
		return nil, false, err
	}
	s.data = next
	r := cloneStore(next).Journal[len(next.Journal)-1]
	return &r, false, nil
}

func (s *Store) prune(data *storeData, now time.Time) {
	journal := data.Journal[:0]
	for _, r := range data.Journal {
		if r.DeliveryState == "acknowledged" && now.Sub(r.UpdatedAt) > s.opts.JournalRetention {
			continue
		}
		journal = append(journal, r)
	}
	data.Journal = journal
	snapshots := data.Snapshots[:0]
	for _, snap := range data.Snapshots {
		if now.After(snap.ExpiresAt) {
			pending := false
			for _, r := range data.Journal {
				if r.SnapshotID == snap.ID && r.DeliveryState != "acknowledged" {
					pending = true
					break
				}
			}
			if !pending {
				continue
			}
		}
		snapshots = append(snapshots, snap)
	}
	data.Snapshots = snapshots
}

// Executing atomically publishes the snapshot and execution boundary. A crash
// after this point becomes uncertain even if the network write had not begun.
func (s *Store) Executing(c model.Command, target model.Device, resolved driver.ResolvedInterface, expected bool, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return "", ErrStorageUnavailable
	}
	index, err := findCommand(s.data, c)
	if err != nil || index < 0 || s.data.Journal[index].State != "reserved" {
		return "", ErrStorageUnavailable
	}
	if target.ID != c.TargetDeviceID || target.Driver != "routeros-rest" || resolved.Interface.Metadata == nil || !resolved.Interface.Metadata.AdminStatus {
		return "", ErrSnapshotConflict
	}
	next := cloneStore(s.data)
	s.pruneSnapshots(&next, now)
	if len(next.Snapshots) >= s.opts.SnapshotMaxEntries {
		return "", ErrStoreFull
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", ErrStorageUnavailable
	}
	next.Generation++
	iface := resolved.Interface
	// Persist an irreversible superseded marker on older snapshots. A newer
	// snapshot expiring after a retention/config change must not revive an old
	// rollback candidate whose boolean state happens to match again (ABA).
	for i := range next.Snapshots {
		older := &next.Snapshots[i]
		if older.DeviceID == target.ID && older.CanonicalID == iface.ID {
			older.Superseded = true
		}
	}
	snapshot := model.ConfigurationSnapshot{ID: id.String(), DeviceID: target.ID, ResourceType: "interface.admin", CanonicalID: iface.ID, Name: iface.Name, MAC: iface.MAC, Alias: iface.Alias, Description: iface.Description,
		Aliases: append([]string(nil), resolved.Aliases...), PreviousAdminUp: iface.AdminUp, ExpectedAdminUp: expected, ObservedAt: now.UTC(), CreatedAt: now.UTC(), ExpiresAt: now.UTC().Add(s.opts.SnapshotRetention), Source: "routeros.https.interface", RelatedCommandID: c.ID, Driver: target.Driver, SoftwareVersion: target.SoftwareVersion, SchemaVersion: 1, Generation: next.Generation}
	next.Snapshots = append(next.Snapshots, snapshot)
	r := &next.Journal[index]
	r.State, r.Resource, r.SnapshotID, r.UpdatedAt = "executing", iface.ID, snapshot.ID, now.UTC()
	if err := s.save(next); err != nil {
		return "", err
	}
	s.data = next
	return snapshot.ID, nil
}
func (s *Store) pruneSnapshots(data *storeData, now time.Time) {
	kept := data.Snapshots[:0]
	for _, snap := range data.Snapshots {
		if now.After(snap.ExpiresAt) {
			pending := false
			for _, r := range data.Journal {
				if r.SnapshotID == snap.ID && r.DeliveryState != "acknowledged" {
					pending = true
					break
				}
			}
			if !pending {
				continue
			}
		}
		kept = append(kept, snap)
	}
	data.Snapshots = kept
}

func executionState(r model.CommandResult) string {
	if r.Uncertain {
		return "uncertain"
	}
	switch r.Status {
	case "success":
		return "succeeded"
	case "rejected":
		return "rejected"
	case "unsupported":
		return "unsupported"
	default:
		return "failed"
	}
}

// Complete preserves the exact result envelope ID for replay. Acknowledgement
// is independent of execution and never causes an external mutation retry.
func (s *Store) Complete(c model.Command, envelope model.Envelope, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return ErrStorageUnavailable
	}
	if envelope.CommandResult == nil || envelope.CommandResult.CommandID != c.ID || envelope.MessageID == "" {
		return ErrStorageUnavailable
	}
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > resultReservationBytes {
		return ErrStoreFull
	}
	index, err := findCommand(s.data, c)
	if err != nil || index < 0 {
		return ErrStorageUnavailable
	}
	next := cloneStore(s.data)
	r := &next.Journal[index]
	if r.Result != nil {
		return errors.New("command result already completed")
	}
	result := *envelope.CommandResult
	resultRaw, err := json.Marshal(result)
	if err != nil {
		return ErrStoreFull
	}
	r.Result, r.Envelope = &result, &envelope
	r.State, r.DeliveryState, r.UpdatedAt = executionState(result), "result_pending_ack", now.UTC()
	r.Reservation = strings.Repeat(".", 2*resultReservationBytes-len(raw)-len(resultRaw))
	r.VerificationState = "unverified"
	if result.Verification != nil && result.Verification.Verified {
		r.VerificationState = "verified"
	}
	for i := range next.Snapshots {
		snap := &next.Snapshots[i]
		if snap.ID == r.SnapshotID {
			snap.Verified = result.Status == "success" && result.Verification != nil && result.Verification.Verified && !result.Uncertain
		}
		if c.Rollback != nil && snap.ID == c.Rollback.SnapshotID && result.Status == "success" && !result.Uncertain {
			snap.Consumed = true
		}
	}
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *Store) Pending() []model.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return nil
	}
	var pending []model.Envelope
	for _, r := range cloneStore(s.data).Journal {
		if r.Envelope != nil && r.DeliveryState == "result_pending_ack" {
			pending = append(pending, *r.Envelope)
		}
	}
	return pending
}
func (s *Store) Acknowledge(messageID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return ErrStorageUnavailable
	}
	next := cloneStore(s.data)
	for i := range next.Journal {
		r := &next.Journal[i]
		if r.Envelope == nil || r.Envelope.MessageID != messageID || r.DeliveryState == "acknowledged" {
			continue
		}
		r.DeliveryState, r.UpdatedAt = "acknowledged", now.UTC()
		if err := s.save(next); err != nil {
			return err
		}
		s.data = next
		return nil
	}
	return nil
}
func (s *Store) Snapshot(id string, now time.Time) (model.ConfigurationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return model.ConfigurationSnapshot{}, ErrStorageUnavailable
	}
	for _, snap := range cloneStore(s.data).Snapshots {
		if snap.ID != id {
			continue
		}
		if !now.Before(snap.ExpiresAt) {
			return model.ConfigurationSnapshot{}, ErrSnapshotExpired
		}
		if !snap.Verified || snap.Consumed || snap.Superseded {
			return model.ConfigurationSnapshot{}, ErrSnapshotConflict
		}
		for _, other := range s.data.Snapshots {
			if other.DeviceID == snap.DeviceID && other.CanonicalID == snap.CanonicalID && other.Generation > snap.Generation {
				return model.ConfigurationSnapshot{}, ErrSnapshotConflict
			}
		}
		return snap, nil
	}
	return model.ConfigurationSnapshot{}, ErrSnapshotExpired
}
func (s *Store) Health() StoreHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := StoreHealth{Available: s.lastErr == nil, JournalEntries: len(s.data.Journal), Snapshots: len(s.data.Snapshots), Bytes: s.bytes}
	if s.lastErr != nil {
		h.LastError = s.lastErr.Error()
	}
	for _, r := range s.data.Journal {
		if r.DeliveryState == "result_pending_ack" {
			h.PendingResults++
		}
		if r.State == "uncertain" {
			h.UncertainCommands++
		}
	}
	return h
}

func (s *Store) validateData(data storeData) error {
	if data.Version != 1 || len(data.Journal) > s.opts.JournalMaxEntries || len(data.Snapshots) > s.opts.SnapshotMaxEntries {
		return ErrStoreCorrupt
	}
	j, _ := json.Marshal(data.Journal)
	snaps, _ := json.Marshal(data.Snapshots)
	if len(j) > s.opts.JournalMaxBytes || len(snaps) > s.opts.SnapshotMaxBytes {
		return ErrStoreFull
	}
	ids, keys, messageIDs, snapshotIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range data.Journal {
		if !identifier(r.Command.ID) || r.Fingerprint != commandFingerprint(r.Command) || len(r.CommandIDs) < 1 || len(r.CommandIDs) > 16 || len(r.IdempotencyKeys) > 16 || !contains(r.CommandIDs, r.Command.ID) || r.TargetDevice != r.Command.TargetDeviceID || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || len(r.Reservation) > 2*resultReservationBytes+128 {
			return ErrStoreCorrupt
		}
		for _, id := range r.CommandIDs {
			if !identifier(id) || ids[id] {
				return ErrStoreCorrupt
			}
			ids[id] = true
		}
		for _, key := range r.IdempotencyKeys {
			if key == "" || len(key) > 128 || keys[key] {
				return ErrStoreCorrupt
			}
			keys[key] = true
		}
		switch r.State {
		case "reserved", "executing", "succeeded", "failed", "rejected", "unsupported", "uncertain":
		default:
			return ErrStoreCorrupt
		}
		if r.State == "reserved" || r.State == "executing" {
			if r.Result != nil || r.Envelope != nil || r.DeliveryState != "not_ready" {
				return ErrStoreCorrupt
			}
		} else {
			if r.Result == nil || r.Envelope == nil || r.Envelope.CommandResult == nil || r.Envelope.MessageID == "" || messageIDs[r.Envelope.MessageID] || r.Result.CommandID != r.Command.ID || r.Envelope.CommandResult.CommandID != r.Command.ID || (r.DeliveryState != "result_pending_ack" && r.DeliveryState != "acknowledged") {
				return ErrStoreCorrupt
			}
			messageIDs[r.Envelope.MessageID] = true
		}
	}
	for _, snap := range data.Snapshots {
		if !identifier(snap.ID) || snapshotIDs[snap.ID] || !identifier(snap.DeviceID) || snap.Driver != "routeros-rest" || snap.ResourceType != "interface.admin" || snap.SchemaVersion != 1 || snap.Generation == 0 || snap.Generation > data.Generation || snap.CreatedAt.IsZero() || !snap.ExpiresAt.After(snap.CreatedAt) || snap.CanonicalID == "" || len(snap.Aliases) > 4*65536 {
			return ErrStoreCorrupt
		}
		snapshotIDs[snap.ID] = true
	}
	return nil
}

func snapshotMatches(s model.ConfigurationSnapshot, target model.Device, r driver.ResolvedInterface) bool {
	i := r.Interface
	a, b := append([]string(nil), s.Aliases...), append([]string(nil), r.Aliases...)
	sort.Strings(a)
	sort.Strings(b)
	return s.DeviceID == target.ID && s.Driver == target.Driver && s.SoftwareVersion == target.SoftwareVersion && s.SchemaVersion == 1 &&
		s.CanonicalID == i.ID && s.Name == i.Name && s.MAC == i.MAC && s.Alias == i.Alias && s.Description == i.Description &&
		i.Metadata != nil && i.Metadata.AdminStatus && i.AdminUp == s.ExpectedAdminUp && strings.Join(a, "\x00") == strings.Join(b, "\x00")
}
