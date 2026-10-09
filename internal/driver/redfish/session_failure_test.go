package redfish

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/driver"
)

func TestFailedSessionBodyStillCleansCreatedSession(t *testing.T) {
	for _, body := range []string{"invalid json", strings.Repeat("x", 8192)} {
		t.Run(body[:min(len(body), 12)], func(t *testing.T) {
			f := newFixture(t)
			f.override["/redfish/v1/SessionService/Sessions"] = func(w http.ResponseWriter, r *http.Request) {
				const uri = "/redfish/v1/SessionService/Sessions/badbody"
				const token = "private-fixture-badbody-token"
				f.mu.Lock()
				f.created++
				f.tokens[uri] = token
				f.mu.Unlock()
				w.Header().Set("Location", uri)
				w.Header().Set("X-Auth-Token", token)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}
			a := f.adapter(Options{MaxResponseBytes: 4096})
			for i := 0; i < 3; i++ {
				if _, err := a.InventoryBatch(context.Background()); err == nil {
					t.Fatal("invalid login body accepted")
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.created != 3 || f.deleted != 3 || len(f.tokens) != 0 {
				t.Fatalf("leaked sessions created=%d deleted=%d pending=%d", f.created, f.deleted, len(f.tokens))
			}
		})
	}
}

func TestRetryResponseUpdatesCooldown(t *testing.T) {
	f := newFixture(t)
	attempt := 0
	f.override["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) {
		attempt++
		wait := "0"
		if attempt > 1 {
			wait = "120"
		}
		w.Header().Set("Retry-After", wait)
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	a := f.adapter(Options{})
	_, err := a.InventoryBatch(context.Background())
	if !errors.Is(err, driver.ErrConnectivity) || a.cooldown.Before(time.Now().Add(110*time.Second)) {
		t.Fatal("latest Retry-After was lost", err)
	}
	before := f.count("GET", "/redfish/v1/")
	_, err = a.InventoryBatch(context.Background())
	if err == nil || f.count("GET", "/redfish/v1/") != before {
		t.Fatal("poll ignored second response cooldown")
	}
}
