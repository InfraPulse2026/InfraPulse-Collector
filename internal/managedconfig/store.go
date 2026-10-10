package managedconfig

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

	"InfraPulseAgent/internal/model"
)

type diskState struct {
	CollectorID        string                        `json:"collector_id"`
	Applied            *model.DesiredCollectorConfig `json:"applied,omitempty"`
	AppliedAt          model.AppliedConfigState      `json:"applied_state"`
	HighestRevision    uint64                        `json:"highest_revision"`
	HighestFingerprint string                        `json:"highest_fingerprint,omitempty"`
	LastResult         model.ConfigApplyResult       `json:"last_result"`
}
type diskEnvelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Data     json.RawMessage `json:"data"`
}
type store struct {
	dir, path string
	limit     int
	write     func([]byte) error
}

func newStore(dir string, limit int) *store {
	s := &store{dir: dir, path: filepath.Join(dir, "managed-config.json"), limit: limit}
	s.write = s.atomicWrite
	return s
}

func (s *store) load(id string) (diskState, error) {
	state := diskState{CollectorID: id}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > int64(s.limit) || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return state, errors.New("state_unavailable")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return state, errors.New("state_unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(s.limit)+1))
	if err != nil || len(raw) > s.limit {
		return state, errors.New("state_unavailable")
	}
	var env diskEnvelope
	if decodeStrict(raw, &env) != nil || env.Version != 1 {
		return state, errors.New("state_corrupt")
	}
	sum := sha256.Sum256(env.Data)
	if hex.EncodeToString(sum[:]) != env.Checksum || decodeStrict(env.Data, &state) != nil || state.CollectorID != id {
		return diskState{CollectorID: id}, errors.New("state_corrupt")
	}
	if state.Applied != nil {
		digest, e := Digest(*state.Applied)
		if e != nil || state.Applied.CollectorID != id || state.Applied.SessionID != "" || state.Applied.Revision == 0 || state.HighestRevision < state.Applied.Revision || digest != state.Applied.ConfigurationDigest || state.AppliedAt.Revision != state.Applied.Revision || state.AppliedAt.ConfigurationDigest != digest || state.AppliedAt.TargetCount != len(state.Applied.Targets) || state.AppliedAt.Status != model.ConfigStatusApplied || state.AppliedAt.AppliedAt.IsZero() {
			return diskState{CollectorID: id}, errors.New("state_corrupt")
		}
	}
	if state.HighestRevision > 0 {
		fingerprint, e := hex.DecodeString(state.HighestFingerprint)
		if e != nil || len(fingerprint) != 32 || hex.EncodeToString(fingerprint) != state.HighestFingerprint || state.LastResult.CollectorID != id || state.LastResult.Revision != state.HighestRevision ||
			state.LastResult.Status != model.ConfigStatusApplied && state.LastResult.Status != model.ConfigStatusRejected && state.LastResult.Status != model.ConfigStatusFailed ||
			state.LastResult.AppliedRevision != state.AppliedAt.Revision || state.LastResult.AppliedDigest != state.AppliedAt.ConfigurationDigest || len(state.LastResult.ErrorCode) > 64 || len(state.LastResult.ErrorMessage) > 512 {
			return diskState{CollectorID: id}, errors.New("state_corrupt")
		}
	}
	return state, nil
}

func decodeStrict(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra state document")
	}
	return nil
}

func (s *store) save(state diskState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return errors.New("state_write_failed")
	}
	sum := sha256.Sum256(data)
	raw, err := json.Marshal(diskEnvelope{Version: 1, Checksum: hex.EncodeToString(sum[:]), Data: data})
	if err != nil || len(raw) > s.limit {
		return errors.New("state_size_limit")
	}
	if s.write(raw) != nil {
		return errors.New("state_write_failed")
	}
	return nil
}

func (s *store) atomicWrite(raw []byte) error {
	for _, path := range []string{s.path, filepath.Join(s.dir, ".managed-config.tmp")} {
		if info, e := os.Lstat(path); e == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("state_write_failed")
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return errors.New("state_write_failed")
		}
	}
	name := filepath.Join(s.dir, ".managed-config.tmp")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
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
	if err != nil {
		return err
	}
	if err = os.Rename(name, s.path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, e := os.Open(s.dir)
		if e != nil {
			return e
		}
		err = d.Sync()
		_ = d.Close()
	}
	return err
}
