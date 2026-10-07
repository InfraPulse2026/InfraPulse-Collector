// Package logs implements bounded file collection with persistent read cursors.
// Cursor admission is deliberately separate from transport delivery: an accepted
// RAM-outbox batch advances the cursor, even though a crash may lose that batch.
package logs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"InfraPulseAgent/internal/model"
	"InfraPulseAgent/internal/sanitize"
)

var (
	ErrSourceUnavailable = errors.New("log source unavailable")
	ErrPermission        = errors.New("log source permission denied")
	ErrRead              = errors.New("log source read failed")
	ErrNotRegular        = errors.New("log source is not a regular file")
	ErrRotationLimit     = errors.New("log rotation tracking limit exceeded")
	ErrRotationGap       = errors.New("log rotation gap: previous file no longer available")
	ErrCursorCorrupt     = errors.New("log cursor invalid or corrupted; explicit recovery required")
	ErrStatePersistence  = errors.New("log cursor persistence failed")
	ErrUnsupported       = errors.New("file log collection unsupported on this platform")
	ErrClosed            = errors.New("log pipeline closed")
)

type Source struct {
	ID     string
	Path   string
	Format string // syslog, auditd, or raw
}

type Options struct {
	CollectorID     string
	StateDir        string
	DeviceID        string
	Sources         []Source
	MaxLineBytes    int
	MaxBatchRecords int
	MaxBatchBytes   int
	MaxReadBytes    int
	Sanitize        func(string) string
	Emit            func(model.Envelope) error
}

type State struct {
	SourceID         string    `json:"source_id"`
	Status           string    `json:"status"`
	FileIdentity     string    `json:"file_identity,omitempty"`
	Offset           int64     `json:"offset"`
	PendingFiles     int       `json:"pending_files"`
	Records          uint64    `json:"records"`
	TruncatedRecords uint64    `json:"truncated_records"`
	Rotations        uint64    `json:"rotations"`
	Truncations      uint64    `json:"truncations"`
	LastObservedAt   time.Time `json:"last_observed_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

type sourceState struct {
	source  Source
	cursor  cursor
	handles map[string]*os.File
	state   State
	dirty   bool
}

type Pipeline struct {
	mu      sync.Mutex
	options Options
	dir     string
	sources map[string]*sourceState
	order   []string
	closed  bool
	open    func(string) (*os.File, error)
	save    func(string, cursor) error
	now     func() time.Time
}

func New(o Options) (*Pipeline, error) {
	if o.StateDir == "" || o.DeviceID == "" || o.Emit == nil || len(o.Sources) > 64 || o.MaxLineBytes < 128 || o.MaxLineBytes > 64<<10 || o.MaxBatchRecords < 1 || o.MaxBatchRecords > 1024 || o.MaxBatchBytes < 2*o.MaxLineBytes+4096 || o.MaxBatchBytes > 4<<20 || o.MaxReadBytes < o.MaxLineBytes+1 || o.MaxReadBytes > 16<<20 {
		return nil, errors.New("invalid bounded log pipeline options")
	}
	if o.Sanitize == nil {
		o.Sanitize = sanitize.Text
	}
	dir := filepath.Join(o.StateDir, "log-cursors")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrStatePersistence
	}
	p := &Pipeline{options: o, dir: dir, sources: make(map[string]*sourceState, len(o.Sources)), open: openLog, save: saveCursor, now: time.Now}
	for _, s := range o.Sources {
		if s.ID == "" || len(s.ID) > 128 || p.sources[s.ID] != nil || s.Path == "" || len(s.Path) > 4096 || strings.ContainsAny(s.ID, "\r\n\x00") || strings.ContainsRune(s.Path, '\x00') || s.Format != "syslog" && s.Format != "auditd" && s.Format != "raw" {
			_ = p.Close()
			return nil, errors.New("invalid log source configuration")
		}
		path, err := filepath.Abs(s.Path)
		if err != nil {
			_ = p.Close()
			return nil, errors.New("invalid log source path")
		}
		s.Path = filepath.Clean(path)
		c, err := loadCursor(dir, s)
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("source %s: %w", s.ID, err)
		}
		st := &sourceState{source: s, cursor: c, handles: make(map[string]*os.File), state: State{SourceID: s.ID, Status: "unknown"}}
		p.sources[s.ID], p.order = st, append(p.order, s.ID)
	}
	return p, nil
}

// Poll performs at most one bounded batch. Sources share a serial collector,
// never an unbounded goroutine pool. No read cursor advances for a rejected
// batch. Persistence failure after admission blocks further reads until save
// succeeds; a restart can replay the previously admitted records.
func (p *Pipeline) Poll(ctx context.Context, sourceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	s := p.sources[sourceID]
	if s == nil {
		return errors.New("unknown log source ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.dirty {
		if err := p.save(p.dir, s.cursor); err != nil {
			return p.failure(s, err)
		}
		s.dirty = false
	}
	c := s.cursor
	c.Files, c.Seen = append([]fileCursor(nil), c.Files...), append([]string(nil), c.Seen...)
	rotation, gap, unavailable, err := p.discover(ctx, s, &c)
	limited := errors.Is(err, ErrRotationLimit) && len(c.Files) > 0
	if err != nil && !limited {
		p.closeUnused(s, s.cursor)
		return p.failure(s, err)
	}
	if limited {
		// Drain already tracked files to release capacity. The remaining
		// identities are rediscovered on a later poll instead of wedging the
		// collector permanently at its limit or silently dropping them.
		for _, fc := range c.Files {
			if s.handles[fc.Identity] == nil {
				p.closeUnused(s, s.cursor)
				return p.failure(s, ErrRotationLimit)
			}
		}
	}
	if unavailable {
		for _, fc := range c.Files {
			if s.handles[fc.Identity] == nil {
				return p.failure(s, ErrSourceUnavailable)
			}
		}
	}
	observed := p.now().UTC()
	// File discovery must survive a rejected first batch too. Otherwise a
	// subsequent rotation/restart could forget the unread original file.
	// Discovery persists identities at their existing admitted offsets only.
	if !sameCursor(c, s.cursor) {
		s.cursor, s.dirty = cloneCursor(c), true
		if err := p.save(p.dir, s.cursor); err != nil {
			return p.failure(s, err)
		}
		s.dirty = false
	}
	if rotation {
		s.state.Rotations++
		p.event(s, "log.rotation", "info", "log source file identity changed", c.CurrentIdentity, observed)
	}
	if gap {
		p.event(s, "log.source.gap", "warn", "previous log file disappeared before its unread records could be collected", c.CurrentIdentity, observed)
		s.state.Status, s.state.LastError = "error", ErrRotationGap.Error()
	}
	batch := model.LogBatch{ObservedAt: observed, Source: "linux-file"}
	if s.source.Format == "auditd" {
		batch.Source = "auditd"
	}
	ref := p.sourceRef(s)
	batch.Observation = model.NewObservation(ref, batch.Source, observed, observed, nil)
	// Account for domain metadata in the existing batch budget, before admission
	// and cursor advancement. Record metadata is included in each encoded size.
	emptyBatch, err := json.Marshal(batch)
	if err != nil {
		return p.failure(s, ErrRead)
	}
	readBudget, batchBytes, truncated, truncations := p.options.MaxReadBytes, len(emptyBatch)+64, uint64(0), uint64(0)
	for i := 0; i < len(c.Files) && readBudget > 0 && len(batch.Records) < p.options.MaxBatchRecords; {
		if err := ctx.Err(); err != nil {
			return err
		}
		fc := &c.Files[i]
		f := s.handles[fc.Identity]
		if f == nil {
			return p.failure(s, ErrRead)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return p.failure(s, ErrRead)
		}
		if fc.Offset > info.Size() {
			resetFile(fc)
			truncations++
		} else if fc.Offset > 0 {
			actual, err := anchor(f, fc.Offset)
			if err != nil || actual != fc.Anchor {
				resetFile(fc)
				truncations++
			}
		}
		finished := false
		for readBudget > 0 && len(batch.Records) < p.options.MaxBatchRecords {
			if err := ctx.Err(); err != nil {
				return err
			}
			if fc.Discard {
				n, newline, err := discardLine(ctx, f, fc.Offset, readBudget)
				if err != nil {
					return p.failure(s, err)
				}
				fc.Offset += int64(n)
				readBudget -= n
				fc.Discard = !newline
				if n == 0 {
					finished = true
					break
				}
				if !newline {
					continue
				}
			}
			old := *fc
			line, consumed, tooLong, complete, err := readLine(ctx, f, fc.Offset, p.options.MaxLineBytes, readBudget)
			if err != nil {
				return p.failure(s, err)
			}
			readBudget -= consumed
			if consumed == 0 {
				finished = true
				break
			}
			// An unfinished short line on the active file is retained until a
			// newline arrives. A renamed/draining file can emit its final line.
			if !complete && !tooLong && (fc.Identity == c.CurrentIdentity || int64(consumed)+fc.Offset < info.Size()) {
				break
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			privateBefore := c.PrivateKey
			line, expanded := p.redactLine(line, &c.PrivateKey)
			if tooLong {
				// A truncated credential may not match its configured complete
				// value. Never export an oversized line's partial raw prefix.
				line = p.safeMarker("[OVERSIZED LOG LINE REDACTED]")
			}
			r := Parse(line, s.source.Format, observed)
			r.ID = recordID(s.source.ID, fc.Identity, fc.Generation, fc.Offset)
			r.DeviceID, r.Source, r.SourceID, r.FileIdentity, r.Offset = p.options.DeviceID, batch.Source, s.source.ID, fc.Identity, fc.Offset
			r.Truncated = tooLong || expanded
			r.Observation = model.NewObservation(ref, r.Source, r.ObservedAt, observed, r.OriginalTimestamp)
			encoded, err := json.Marshal(r)
			if err != nil {
				return p.failure(s, ErrRead)
			}
			// CollectedAt is finalized after reading. RFC3339Nano can grow by
			// ten bytes when completion has fractional seconds and observation
			// does not; reserve that growth for every record, plus the comma.
			if batchBytes+len(encoded)+11 > p.options.MaxBatchBytes {
				if len(batch.Records) > 0 {
					*fc = old
					c.PrivateKey = privateBefore
					readBudget = 0
					break
				}
				// JSON escaping can expand a source line up to sixfold. A
				// single record exceeding the batch cap must not permanently
				// stall this source even though its source bytes were bounded.
				marker := p.safeMarker("[LOG RECORD EXCEEDS BATCH LIMIT; REDACTED]")
				r.Message, r.Raw = marker, marker
				r.Host, r.Process, r.ProcessID, r.Audit = "", "", "", nil
				r.Truncated = true
				encoded, err = json.Marshal(r)
				if err != nil || batchBytes+len(encoded)+11 > p.options.MaxBatchBytes {
					return p.failure(s, ErrRead)
				}
			}
			batchBytes += len(encoded) + 11
			batch.Records = append(batch.Records, r)
			fc.Offset += int64(consumed)
			fc.Discard = tooLong && !complete
			if r.Truncated {
				truncated++
			}
		}
		if fc.Offset > 0 {
			fc.Anchor, err = anchor(f, fc.Offset)
			if err != nil {
				return p.failure(s, ErrRead)
			}
		}
		if finished && fc.Identity != c.CurrentIdentity {
			c.Seen = append(c.Seen, fc.Identity)
			c.Files = append(c.Files[:i], c.Files[i+1:]...)
			continue
		}
		i++
	}
	if len(batch.Records) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.source.Format == "auditd" {
			batch.Records = correlateAuditBatch(batch.Records)
		}
		completed := p.now().UTC()
		batch.Observation.Time.CollectedAt = completed
		for i := range batch.Records {
			batch.Records[i].Observation.Time.CollectedAt = completed
		}
		if err := p.options.Emit(model.Envelope{Logs: &batch}); err != nil {
			p.closeUnused(s, s.cursor)
			return err
		}
	}
	// Persist discovery and discard progress too. Those offsets discard only
	// bytes of an already admitted oversized line, never a new log record.
	if !sameCursor(c, s.cursor) {
		s.cursor, s.dirty = cloneCursor(c), true
		if err := p.save(p.dir, c); err != nil {
			p.updateState(s, observed)
			return p.failure(s, err)
		}
		s.dirty = false
	}
	p.closeUnused(s, c)
	s.state.Records += uint64(len(batch.Records))
	s.state.TruncatedRecords += truncated
	s.state.Truncations += truncations
	if truncations > 0 {
		p.event(s, "log.truncate", "warn", "log source truncated or overwritten; cursor restarted", c.CurrentIdentity, observed)
	}
	p.updateState(s, observed)
	if limited {
		return p.failure(s, ErrRotationLimit)
	}
	if gap {
		return p.failure(s, ErrRotationGap)
	}
	if unavailable {
		return p.failure(s, ErrSourceUnavailable)
	}
	if s.state.Status == "error" || s.state.Status == "unavailable" {
		p.event(s, "log.source.recovered", "info", "log source available", c.CurrentIdentity, observed)
	}
	s.state.Status, s.state.LastError = "available", ""
	return nil
}

func (p *Pipeline) redactLine(line string, privateKey *bool) (string, bool) {
	upper := strings.ToUpper(line)
	if strings.Contains(upper, "-----BEGIN ") && strings.Contains(upper, "PRIVATE KEY-----") {
		*privateKey = true
	}
	if *privateKey {
		if strings.Contains(upper, "-----END ") && strings.Contains(upper, "PRIVATE KEY-----") {
			*privateKey = false
		}
		return p.safeMarker("[REDACTED PRIVATE KEY]"), false
	}
	cleaned := p.options.Sanitize(strings.ToValidUTF8(line, "\ufffd"))
	if len(cleaned) > p.options.MaxLineBytes {
		return p.safeMarker("[SANITIZED LOG LINE EXCEEDS LIMIT; REDACTED]"), true
	}
	return cleaned, false
}

func (p *Pipeline) safeMarker(marker string) string {
	cleaned := p.options.Sanitize(marker)
	if len(cleaned) > p.options.MaxLineBytes || !utf8.ValidString(cleaned) {
		return ""
	}
	return cleaned
}

func resetFile(fc *fileCursor) {
	fc.Offset, fc.Anchor, fc.Discard = 0, "", false
	fc.Generation++
}

func sameCursor(a, b cursor) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func cloneCursor(c cursor) cursor {
	c.Files = append([]fileCursor(nil), c.Files...)
	c.Seen = append([]string(nil), c.Seen...)
	return c
}

func recordID(source, identity string, generation uint64, offset int64) string {
	v := fmt.Sprintf("%s\x00%s\x00%d\x00%d", source, identity, generation, offset)
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

func (p *Pipeline) updateState(s *sourceState, observed time.Time) {
	s.state.FileIdentity, s.state.PendingFiles, s.state.LastObservedAt = s.cursor.CurrentIdentity, len(s.cursor.Files), observed
	for _, fc := range s.cursor.Files {
		if fc.Identity == s.cursor.CurrentIdentity {
			s.state.Offset = fc.Offset
		}
	}
}

func (p *Pipeline) failure(s *sourceState, err error) error {
	category := err.Error()
	if s.state.LastError != category {
		p.event(s, "log.source.error", "warn", category, s.cursor.CurrentIdentity, time.Now().UTC())
	}
	s.state.Status, s.state.LastError = "error", category
	if errors.Is(err, ErrSourceUnavailable) {
		s.state.Status = "unavailable"
	}
	return err
}

func (p *Pipeline) event(s *sourceState, kind, severity, message, identity string, at time.Time) {
	id := recordID(s.source.ID, kind+identity, uint64(at.UnixNano()), 0)
	e := model.Envelope{Events: []model.Event{{ID: id, Type: kind, DeviceID: p.options.DeviceID, Timestamp: at, Severity: severity, Message: message, Source: "linux-file"}}}
	_ = p.options.Emit(model.ObserveEnvelope(e, p.sourceRef(s), time.Now().UTC()))
}

func (p *Pipeline) Snapshot() []State {
	p.mu.Lock()
	defer p.mu.Unlock()
	states := make([]State, 0, len(p.order))
	for _, id := range p.order {
		states = append(states, p.sources[id].state)
	}
	return states
}

func (p *Pipeline) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var errs []error
	for _, s := range p.sources {
		if s.dirty {
			errs = append(errs, p.save(p.dir, s.cursor))
		}
		for _, f := range s.handles {
			if err := f.Close(); err != nil {
				errs = append(errs, ErrRead)
			}
		}
		clear(s.handles)
	}
	return errors.Join(errs...)
}

func (p *Pipeline) closeUnused(s *sourceState, c cursor) {
	keep := make(map[string]bool, len(c.Files))
	for _, fc := range c.Files {
		keep[fc.Identity] = true
	}
	for id, f := range s.handles {
		if !keep[id] {
			_ = f.Close()
			delete(s.handles, id)
		}
	}
}

type candidate struct {
	path     string
	identity string
	modified time.Time
}

func (p *Pipeline) discover(ctx context.Context, s *sourceState, c *cursor) (rotation, gap, unavailable bool, err error) {
	paths, err := rotationPaths(s.source.Path)
	if err != nil {
		return false, false, false, err
	}
	candidates := make([]candidate, 0, len(paths))
	byID := map[string]candidate{}
	current := ""
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return false, false, false, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, false, false, classifyIO(err)
		}
		if !info.Mode().IsRegular() {
			if path == s.source.Path {
				return false, false, false, ErrNotRegular
			}
			continue
		}
		f, err := p.open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return false, false, false, classifyIO(err)
		}
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return false, false, false, ErrNotRegular
		}
		id, err := fileIdentity(f)
		if err != nil {
			_ = f.Close()
			return false, false, false, ErrRead
		}
		v := candidate{path: path, identity: id, modified: info.ModTime()}
		if _, known := byID[id]; !known {
			candidates, byID[id] = append(candidates, v), v
		}
		if path == s.source.Path {
			current = id
		}
		if s.handles[id] == nil && (path == s.source.Path || containsCursor(c.Files, id)) {
			s.handles[id] = f
		} else {
			_ = f.Close()
		}
	}
	unavailable = current == ""
	if unavailable && len(c.Files) == 0 {
		return false, false, true, nil
	}
	initial := c.CurrentIdentity == ""
	if initial {
		for _, v := range candidates {
			if v.identity != current {
				c.Seen = append(c.Seen, v.identity)
			}
		}
	} else {
		// Remember only rotated identities still on disk, keeping history
		// bounded. Removed files cannot be rediscovered by the scanner.
		kept := c.Seen[:0]
		for _, id := range c.Seen {
			if _, exists := byID[id]; exists {
				kept = append(kept, id)
			}
		}
		c.Seen = kept
	}
	kept := c.Files[:0]
	for _, fc := range c.Files {
		if _, exists := byID[fc.Identity]; exists {
			fc.Path = byID[fc.Identity].path
		}
		if s.handles[fc.Identity] == nil {
			// A missing configured path is reversible; preserve its cursor
			// until a replacement is available or the old file reappears.
			if unavailable {
				return false, false, true, nil
			}
			gap = true
			continue
		}
		kept = append(kept, fc)
	}
	c.Files = kept
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].identity == current {
			return false
		}
		if candidates[j].identity == current {
			return true
		}
		if candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].modified.Before(candidates[j].modified)
	})
	for _, v := range candidates {
		if containsCursor(c.Files, v.identity) || containsID(c.Seen, v.identity) || initial && v.identity != current {
			continue
		}
		if len(c.Files) >= maxTrackedFiles {
			return false, false, false, ErrRotationLimit
		}
		if s.handles[v.identity] == nil {
			f, err := p.open(v.path)
			if err != nil {
				return false, false, false, classifyIO(err)
			}
			actual, err := fileIdentity(f)
			if err != nil || actual != v.identity {
				_ = f.Close()
				return false, false, false, ErrRead
			}
			s.handles[v.identity] = f
		}
		c.Files = append(c.Files, fileCursor{Identity: v.identity, Path: v.path})
	}
	if current != "" {
		rotation = !initial && current != c.CurrentIdentity
		c.CurrentIdentity = current
	}
	return rotation, gap, unavailable, nil
}

func containsCursor(files []fileCursor, id string) bool {
	for _, fc := range files {
		if fc.Identity == id {
			return true
		}
	}
	return false
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func rotationPaths(path string) ([]string, error) {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, classifyIO(err)
	}
	defer f.Close()
	paths, scanned := []string{path}, 0
	base := filepath.Base(path)
	for {
		entries, err := f.ReadDir(32)
		for _, entry := range entries {
			scanned++
			if scanned > maxDirectoryFiles {
				return nil, ErrRotationLimit
			}
			name := entry.Name()
			if !entry.IsDir() && strings.HasPrefix(name, base+".") && !strings.HasSuffix(name, ".gz") && !strings.HasSuffix(name, ".bz2") && !strings.HasSuffix(name, ".xz") {
				paths = append(paths, filepath.Join(filepath.Dir(path), name))
				if len(paths) > maxRotatedFiles+1 {
					return nil, ErrRotationLimit
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return paths, nil
		}
		if err != nil {
			return nil, classifyIO(err)
		}
	}
}

func classifyIO(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return ErrSourceUnavailable
	}
	if errors.Is(err, os.ErrPermission) {
		return ErrPermission
	}
	return ErrRead
}

func readLine(ctx context.Context, f *os.File, offset int64, maxLine, budget int) (line string, consumed int, tooLong, complete bool, err error) {
	limit := maxLine + 1
	if budget < limit {
		limit = budget
	}
	data := make([]byte, 0, limit)
	chunk := make([]byte, min(4096, limit))
	for len(data) < limit {
		if err := ctx.Err(); err != nil {
			return "", 0, false, false, err
		}
		n, err := f.ReadAt(chunk[:min(len(chunk), limit-len(data))], offset+int64(len(data)))
		if n > 0 {
			if at := bytes.IndexByte(chunk[:n], '\n'); at >= 0 {
				data = append(data, chunk[:at+1]...)
				return string(data), len(data), len(data) > maxLine, true, nil
			}
			data = append(data, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", 0, false, false, ErrRead
		}
	}
	tooLong = len(data) > maxLine
	if tooLong {
		return string(data[:maxLine]), maxLine, true, false, nil
	}
	return string(data), len(data), false, false, nil
}

func discardLine(ctx context.Context, f *os.File, offset int64, budget int) (int, bool, error) {
	chunk := make([]byte, min(4096, budget))
	total := 0
	for total < budget {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		n, err := f.ReadAt(chunk[:min(len(chunk), budget-total)], offset+int64(total))
		if n > 0 {
			if at := bytes.IndexByte(chunk[:n], '\n'); at >= 0 {
				return total + at + 1, true, nil
			}
			total += n
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, false, nil
			}
			return 0, false, ErrRead
		}
	}
	return total, false, nil
}
