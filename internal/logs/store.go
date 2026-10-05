package logs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const (
	maxTrackedFiles   = 8
	maxDirectoryFiles = 1024
	maxRotatedFiles   = 64
	maxCursorBytes    = 128 << 10
	anchorBytes       = 64
)

type fileCursor struct {
	Identity   string `json:"identity"`
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	Generation uint64 `json:"generation"`
	Anchor     string `json:"anchor,omitempty"`
	Discard    bool   `json:"discard,omitempty"`
}

type cursor struct {
	Version         int          `json:"version"`
	SourceID        string       `json:"source_id"`
	Path            string       `json:"path"`
	Format          string       `json:"format"`
	CurrentIdentity string       `json:"current_identity,omitempty"`
	Files           []fileCursor `json:"files"`
	Seen            []string     `json:"seen,omitempty"`
	PrivateKey      bool         `json:"private_key,omitempty"`
}

func cursorPath(dir, id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(dir, hex.EncodeToString(h[:])+".json")
}

func loadCursor(dir string, source Source) (cursor, error) {
	c := cursor{Version: 1, SourceID: source.ID, Path: source.Path, Format: source.Format}
	path := cursorPath(dir, source.ID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxCursorBytes {
		return cursor{}, ErrCursorCorrupt
	}
	f, err := os.Open(path)
	if err != nil {
		return cursor{}, ErrStatePersistence
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, maxCursorBytes+1))
	d.DisallowUnknownFields()
	c = cursor{}
	if d.Decode(&c) != nil {
		return cursor{}, ErrCursorCorrupt
	}
	var extra any
	if d.Decode(&extra) != io.EOF || c.Version != 1 || c.SourceID != source.ID || c.Path != source.Path || c.Format != source.Format || len(c.Files) > maxTrackedFiles || len(c.Seen) > maxRotatedFiles {
		return cursor{}, ErrCursorCorrupt
	}
	identities := make(map[string]bool, len(c.Files))
	for _, v := range c.Files {
		if v.Identity == "" || len(v.Identity) > 128 || identities[v.Identity] || v.Offset < 0 || !withinSourceDirectory(source.Path, v.Path) || v.Offset > 0 && len(v.Anchor) != 64 || v.Offset == 0 && v.Anchor != "" {
			return cursor{}, ErrCursorCorrupt
		}
		if v.Anchor != "" {
			if _, err := hex.DecodeString(v.Anchor); err != nil {
				return cursor{}, ErrCursorCorrupt
			}
		}
		identities[v.Identity] = true
	}
	for _, v := range c.Seen {
		if v == "" || len(v) > 128 || identities[v] {
			return cursor{}, ErrCursorCorrupt
		}
		identities[v] = true
	}
	if len(c.Files) > 0 && !containsCursor(c.Files, c.CurrentIdentity) || len(c.Files) == 0 && c.CurrentIdentity != "" {
		return cursor{}, ErrCursorCorrupt
	}
	return c, nil
}

func saveCursor(dir string, c cursor) error {
	data, err := json.Marshal(c)
	if err != nil || len(data) > maxCursorBytes {
		return ErrStatePersistence
	}
	f, err := os.CreateTemp(dir, ".cursor-*.tmp")
	if err != nil {
		return ErrStatePersistence
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = io.Copy(f, bytes.NewReader(data))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrStatePersistence
	}
	if os.Rename(temp, cursorPath(dir, c.SourceID)) != nil || syncDirectory(dir) != nil {
		return ErrStatePersistence
	}
	return nil
}

func withinSourceDirectory(source, path string) bool {
	if filepath.Dir(source) != filepath.Dir(path) {
		return false
	}
	base, name := filepath.Base(source), filepath.Base(path)
	return name == base || len(name) > len(base)+1 && name[:len(base)+1] == base+"."
}

func anchor(f *os.File, offset int64) (string, error) {
	if offset == 0 {
		return "", nil
	}
	n := int64(anchorBytes)
	if offset < n {
		n = offset
	}
	b := make([]byte, n)
	if _, err := f.ReadAt(b, offset-n); err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
