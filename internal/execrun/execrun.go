// Package execrun runs subprocesses with strict lifecycle hygiene: the
// process is always waited for, killed on context cancellation, and never
// orphaned. It is the single choke-point through which vdb spawns vda/vega.
package execrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Result holds the captured output of a finished process.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ErrNonZero is returned when the process exits non-zero; it always carries
// the captured stderr so callers can name the failing layer.
type ErrNonZero struct {
	Cmd      string
	ExitCode int
	Stderr   string
	Stdout   string
}

func (e *ErrNonZero) Error() string {
	return fmt.Sprintf("%s exited %d: %s", e.Cmd, e.ExitCode, truncate(e.Stderr, 400))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Run executes path with args and waits for it to finish. On context
// cancellation the process is killed and waited for — Run never returns
// leaving a child behind. A 10s default kill grace applies after ctx done.
func Run(ctx context.Context, path string, args ...string) (Result, error) {
	return run(ctx, path, args, 10*time.Second)
}

// RunWithEnv is Run with extra environment variables appended to os.Environ.
func RunWithEnv(ctx context.Context, env []string, path string, args ...string) (Result, error) {
	return run(ctx, path, args, 10*time.Second, env...)
}

func run(ctx context.Context, path string, args []string, grace time.Duration, env ...string) (Result, error) {
	cmd := exec.Command(path, args...)
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
	}
	// Own process group: children of the child (shell wrappers, daemons)
	// die with it — a lone child kill lets grandchildren hold our pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("execrun: start %s: %w", path, err)
	}

	// Wait in a goroutine so ctx cancellation can interject with a kill.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err := <-waitCh:
		res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				res.ExitCode = exitErr.ExitCode()
				return res, &ErrNonZero{Cmd: fmt.Sprintf("%s %v", path, args), ExitCode: exitErr.ExitCode(), Stderr: res.Stderr, Stdout: res.Stdout}
			}
			return res, fmt.Errorf("execrun: wait %s: %w", path, err)
		}
		return res, nil
	case <-ctx.Done():
		// Kill the whole group and — critically — still wait, so no orphan
		// remains. SIGKILL always terminates eventually; the grace timer
		// only bounds our own wait in pathological cases.
		killGroup(cmd)
		select {
		case <-waitCh:
		case <-time.After(grace):
		}
		return Result{}, fmt.Errorf("execrun: %s canceled: %w", path, ctx.Err())
	}
}

// killGroup sends SIGKILL to the process group of cmd (negative pid).
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill() // fall back to direct kill
	}
}
