//go:build integration

package queue

import (
	"InfraPulseAgent/internal/model"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func crashQueueOptions(path string) DurableOptions {
	return DurableOptions{Path: path, MaxMessages: 16, MaxBytes: 128 << 10, MaxMessageBytes: 8192, MaxStorageBytes: 2 << 20, Retention: time.Hour, BatchDelay: time.Millisecond, BatchSize: 16, MaxInflight: 4, ACKTimeout: time.Second}
}

// The worker exits without Close or deferred cleanup, testing process-crash
// boundaries separately from the fast ordinary suite. This is not power loss.
func TestDurableCrashWorker(t *testing.T) {
	path := os.Getenv("INFRAPULSE_TEST_CRASH_PATH")
	if path == "" {
		return
	}
	q, err := NewDurable(crashQueueOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Push(model.Envelope{MessageID: "crash-stable-id", CommandResult: &model.CommandResult{CommandID: "executed", Status: "success"}}); err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv("INFRAPULSE_TEST_CRASH_PHASE")
	if phase != "enqueue" {
		if _, err = q.Pop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "ack" {
		if accepted, err := q.Acknowledge("crash-stable-id"); err != nil || !accepted {
			t.Fatalf("ACK: %v %v", accepted, err)
		}
	}
	os.Exit(23)
}
func TestDurableSubprocessCrashRecovery(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"enqueue", "send", "ack"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outbox.db")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDurableCrashWorker$")
			cmd.Env = append(os.Environ(), "INFRAPULSE_TEST_CRASH_PATH="+path, "INFRAPULSE_TEST_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("worker: %v %s", err, output)
			}
			q, err := NewDurable(crashQueueOptions(path))
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			if phase == "ack" {
				if q.State().Messages != 0 {
					t.Fatal("ACK lost after process exit")
				}
				return
			}
			e, err := q.Pop(ctx)
			if err != nil || e.MessageID != "crash-stable-id" || e.CommandResult.CommandID != "executed" {
				t.Fatalf("crash recovery: %+v %v", e, err)
			}
		})
	}
}
