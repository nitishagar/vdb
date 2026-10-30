package execrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCapturesOutput(t *testing.T) {
	res, err := Run(context.Background(), "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "err\n" {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunNonZeroCarriesStderr(t *testing.T) {
	_, err := Run(context.Background(), "sh", "-c", "echo boom >&2; exit 3")
	var e *ErrNonZero
	if !errors.As(err, &e) {
		t.Fatalf("want ErrNonZero, got %T %v", err, err)
	}
	if e.ExitCode != 3 {
		t.Errorf("exit = %d", e.ExitCode)
	}
	if e.Stderr != "boom\n" {
		t.Errorf("stderr = %q", e.Stderr)
	}
}

// Subprocess hygiene: a canceled context must kill AND wait the child — no
// orphan left behind (invariant 5). The child writes its own pid; after Run
// returns, /proc/<pid> must be gone.
func TestRunCancelKillsAndWaits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, "sh", "-c", "echo $$ > "+pidFile+"; sleep 30")
		done <- err
	}()

	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("child never started (pid file missing)")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want cancel error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// The child must be reaped: /proc/<pid>/stat disappears once waited.
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "stat")); !os.IsNotExist(err) {
		t.Errorf("child %d still visible in /proc after cancel (orphan?)", pid)
	}
}

func TestRunStartError(t *testing.T) {
	_, err := Run(context.Background(), "/nonexistent/binary")
	if err == nil {
		t.Fatal("want start error")
	}
}
