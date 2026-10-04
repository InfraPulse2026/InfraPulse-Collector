// Package identity persists a stable UUID independently of hostnames.
package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

type Identity struct {
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
}

func LoadOrCreate(ctx context.Context, dir string) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Identity{}, fmt.Errorf("create identity directory: %w", err)
	}
	path := filepath.Join(dir, "agent-id")
	id, err := read(path)
	if errors.Is(err, os.ErrNotExist) {
		newID, e := uuid.NewRandom()
		if e != nil {
			return Identity{}, fmt.Errorf("generate agent identity: %w", e)
		}
		f, e := os.CreateTemp(dir, ".identity-*")
		if e != nil {
			return Identity{}, fmt.Errorf("create identity temporary file: %w", e)
		}
		temp := f.Name()
		defer os.Remove(temp)
		e = f.Chmod(0600)
		if e == nil {
			_, e = f.WriteString(newID.String() + "\n")
		}
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil {
			return Identity{}, fmt.Errorf("persist identity: %w", e)
		}
		// Hard-link publishes a complete synced file and cannot overwrite an
		// identity concurrently created by another process on the same filesystem.
		if e = os.Link(temp, path); e != nil && !errors.Is(e, os.ErrExist) {
			return Identity{}, fmt.Errorf("publish persistent identity: %w", e)
		}
		id, err = read(path)
	}
	if err != nil {
		return Identity{}, err
	}
	session, err := uuid.NewRandom()
	if err != nil {
		return Identity{}, fmt.Errorf("generate session identity: %w", err)
	}
	return Identity{AgentID: id, SessionID: session.String()}, nil
}

func read(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read persistent identity: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 129))
	if err != nil {
		return "", fmt.Errorf("read persistent identity: %w", err)
	}
	if len(b) > 128 {
		return "", errors.New("invalid persistent identity: too large")
	}
	id, err := uuid.Parse(strings.TrimSpace(string(b)))
	if err != nil || id == uuid.Nil {
		return "", errors.New("invalid persistent identity: refusing to silently replace it")
	}
	return id.String(), nil
}
