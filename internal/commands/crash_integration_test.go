//go:build integration

package commands

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"InfraPulseAgent/internal/model"
)

// Separate subprocess crash fixtures exercise OS lock release and persisted
// process-restart state. The marker models an external mutation; it is not live
// RouterOS validation or a destructive storage/power-loss test.
func TestCommandJournalSubprocessCrashRecovery(t *testing.T) {
	for _, phase := range []string{"admission", "before_mutation", "after_mutation", "after_verification", "result_persisted", "acknowledged", "partial_staging"} {
		t.Run(phase, func(t *testing.T) {
			dir := commandTempDir(t)
			child := exec.Command(os.Args[0], "-test.run=^TestCommandCrashHelper$", "-test.timeout=15s")
			child.Env = append(os.Environ(), "INFRAPULSE_COMMAND_CRASH_CHILD=1", "INFRAPULSE_COMMAND_CRASH_PHASE="+phase, "INFRAPULSE_COMMAND_CRASH_DIR="+dir)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if strings.HasPrefix(scanner.Text(), "READY ") {
						ready <- scanner.Text()
						return
					}
				}
				ready <- "helper exited before durable boundary"
			}()
			select {
			case observed := <-ready:
				if observed != "READY "+phase {
					t.Fatal(observed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("crash helper did not reach boundary")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("helper unexpectedly exited cleanly")
			}
			s, err := OpenStore(dir, DefaultStoreOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			r, duplicate, err := s.Begin(command(), time.Now())
			if err != nil || !duplicate || r.Result == nil {
				t.Fatal(r, duplicate, err)
			}
			switch phase {
			case "admission":
				if r.Result.Uncertain || r.Result.Status != "rejected" {
					t.Fatal(r.Result)
				}
			case "result_persisted", "acknowledged":
				if r.Result.Uncertain || r.Result.Status != "success" || r.Envelope.MessageID != "crash-stable-result" {
					t.Fatal(r)
				}
			default:
				if !r.Result.Uncertain || r.State != "uncertain" {
					t.Fatal(r)
				}
			}
			if phase == "acknowledged" && len(s.Pending()) != 0 {
				t.Fatal("ACK lost on crash")
			}
			if phase != "acknowledged" && len(s.Pending()) != 1 {
				t.Fatal("pending result lost on crash")
			}
			marker, err := os.ReadFile(filepath.Join(dir, "external-mutation-marker"))
			applied := phase == "after_mutation" || phase == "after_verification" || phase == "result_persisted" || phase == "acknowledged"
			if applied && (err != nil || string(marker) != "applied-once") {
				t.Fatal(string(marker), err)
			}
			if !applied && !os.IsNotExist(err) {
				t.Fatal("mutation before selected crash boundary", err)
			}
			// Reopening/duplicate lookup performs no mutation. The marker remains
			// unchanged across another recovery of an uncertain command.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			again, err := OpenStore(dir, DefaultStoreOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = again.Close() })
			if r2, dup, err := again.Begin(command(), time.Now()); err != nil || !dup || r2.Envelope.MessageID != r.Envelope.MessageID {
				t.Fatal(r2, dup, err)
			}
		})
	}
}

func TestCommandCrashHelper(t *testing.T) {
	if os.Getenv("INFRAPULSE_COMMAND_CRASH_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	dir, phase := os.Getenv("INFRAPULSE_COMMAND_CRASH_DIR"), os.Getenv("INFRAPULSE_COMMAND_CRASH_PHASE")
	s, err := OpenStore(dir, DefaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	c := command()
	if _, _, err := s.Begin(c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if phase != "admission" {
		if _, err := s.Executing(c, safetyTarget(), safetyResolved(true), false, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "after_mutation" || phase == "after_verification" || phase == "result_persisted" || phase == "acknowledged" {
		if err := os.WriteFile(filepath.Join(dir, "external-mutation-marker"), []byte("applied-once"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "result_persisted" || phase == "acknowledged" {
		result := model.CommandResult{CommandID: c.ID, Status: "success", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Changed: true, Verification: &model.InterfaceVerification{Verified: true}}
		if err := s.Complete(c, model.Envelope{MessageID: "crash-stable-result", AgentID: "agent", SessionID: "session", SentAt: time.Now().UTC(), CommandResult: &result}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "acknowledged" {
		if err := s.Acknowledge("crash-stable-result", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if phase == "partial_staging" {
		f, err := os.OpenFile(filepath.Join(dir, ".safety-state.tmp"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(`{"version":1,"checksum":"truncated`)); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("READY " + phase)
	<-time.After(10 * time.Second)
	t.Fatal("parent did not terminate crash helper")
}
