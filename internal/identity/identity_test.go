package identity

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPersistence(t *testing.T) {
	d := t.TempDir()
	a, e := LoadOrCreate(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	b, e := LoadOrCreate(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	if a.AgentID != b.AgentID || a.SessionID == b.SessionID {
		t.Fatal("identity/session semantics")
	}
}
func TestCorruptionIsNotReplaced(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "agent-id")
	if e := os.WriteFile(p, []byte("invalid"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadOrCreate(context.Background(), d); e == nil {
		t.Fatal("corruption ignored")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "invalid" {
		t.Fatal("overwritten")
	}
}
func TestConcurrentCreation(t *testing.T) {
	d := t.TempDir()
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := LoadOrCreate(context.Background(), d)
			if e != nil {
				t.Error(e)
				return
			}
			ids <- v.AgentID
		}()
	}
	wg.Wait()
	close(ids)
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if first != id {
			t.Fatal("different persistent IDs")
		}
	}
}
func TestCancellation(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := LoadOrCreate(c, t.TempDir()); e == nil {
		t.Fatal("ignored cancellation")
	}
}
