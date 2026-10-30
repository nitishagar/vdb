// Package vegacli adapts the user's installed `vega` CLI for device
// lifecycle operations (install/launch/terminate/apps/logs). Argument
// syntax and output text are undocumented by Amazon; the assumed shapes
// live in this file only, so first contact with a real SDK corrects them
// here and in the golden fixtures.
package vegacli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nitishagar/vdb/internal/execrun"
)

// LifecycleResult distinguishes a real action from an already-so no-op.
// Agents retry; spurious hard failures on idempotent calls are defects.
type LifecycleResult string

const (
	ResultDone             LifecycleResult = "done"
	ResultAlreadyInstalled LifecycleResult = "already-installed"
	ResultNotInstalled     LifecycleResult = "not-installed"
	ResultNotRunning       LifecycleResult = "not-running"
	ResultLaunched         LifecycleResult = "launched"
	ResultTerminated       LifecycleResult = "terminated"
)

// AppInfo is one entry from `vega device installed-apps`.
type AppInfo struct {
	ComponentID string
	Name        string
}

// LogStream is a live `vega device start-log-stream` subprocess. Lines are
// delivered on the channel; Stop kills and waits the subprocess. The channel
// closes when the stream ends (EOF, error, or Stop). A stalled consumer
// never deadlocks Stop: the pump abandons buffered lines when told to stop.
type LogStream struct {
	cmd    *exec.Cmd
	lines  chan string
	done   chan struct{}
	stop   chan struct{}
	stderr string
}

// Lines returns the line channel.
func (s *LogStream) Lines() <-chan string { return s.lines }

// Stop terminates the stream subprocess and waits for full reaping.
func (s *LogStream) Stop() {
	close(s.stop) // unblocks a pump wedged on a full line channel
	if s.cmd.Process != nil {
		// Group kill: the CLI may wrap children that hold our pipes.
		if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = s.cmd.Process.Kill()
		}
	}
	<-s.done
}

// Stderr returns any stderr captured before exit (diagnostics).
func (s *LogStream) Stderr() string { return s.stderr }

// VegaCLI is the lifecycle surface vdb depends on.
type VegaCLI interface {
	Install(ctx context.Context, serial, pkgPath string) (LifecycleResult, error)
	Uninstall(ctx context.Context, serial, componentID string) (LifecycleResult, error)
	Launch(ctx context.Context, serial, componentID string) error
	Terminate(ctx context.Context, serial, componentID string) (LifecycleResult, error)
	ListApps(ctx context.Context, serial string) ([]AppInfo, error)
	StartLogStream(ctx context.Context, serial string) (*LogStream, error)
}

// Impl shells out to a resolved vega binary.
type Impl struct {
	bin string
}

// New binds an Impl to a resolved binary path.
func New(bin string) *Impl { return &Impl{bin: bin} }

// Bin returns the resolved binary path.
func (i *Impl) Bin() string { return i.bin }

func (i *Impl) Install(ctx context.Context, serial, pkgPath string) (LifecycleResult, error) {
	res, err := execrun.Run(ctx, i.bin, "device", "install-app", "-s", serial, pkgPath)
	if err != nil {
		if containsAny(res.Stderr+res.Stdout, "already installed") {
			return ResultAlreadyInstalled, nil
		}
		return "", fmt.Errorf("vega install-app: %w", err)
	}
	return ResultDone, nil
}

func (i *Impl) Uninstall(ctx context.Context, serial, componentID string) (LifecycleResult, error) {
	res, err := execrun.Run(ctx, i.bin, "device", "uninstall-app", "-s", serial, componentID)
	if err != nil {
		blob := res.Stderr + res.Stdout
		if containsAny(blob, "not installed", "no such package", "not found") {
			return ResultNotInstalled, nil
		}
		return "", fmt.Errorf("vega uninstall-app: %w", err)
	}
	return ResultDone, nil
}

func (i *Impl) Launch(ctx context.Context, serial, componentID string) error {
	_, err := execrun.Run(ctx, i.bin, "device", "launch-app", "-s", serial, componentID)
	if err != nil {
		return fmt.Errorf("vega launch-app: %w", err)
	}
	return nil
}

func (i *Impl) Terminate(ctx context.Context, serial, componentID string) (LifecycleResult, error) {
	res, err := execrun.Run(ctx, i.bin, "device", "terminate-app", "-s", serial, componentID)
	if err != nil {
		blob := res.Stderr + res.Stdout
		if containsAny(blob, "not running", "is not running") {
			return ResultNotRunning, nil
		}
		return "", fmt.Errorf("vega terminate-app: %w", err)
	}
	return ResultTerminated, nil
}

func (i *Impl) ListApps(ctx context.Context, serial string) ([]AppInfo, error) {
	res, err := execrun.Run(ctx, i.bin, "device", "installed-apps", "-s", serial)
	if err != nil {
		return nil, fmt.Errorf("vega installed-apps: %w", err)
	}
	return ParseApps(res.Stdout)
}

func (i *Impl) StartLogStream(ctx context.Context, serial string) (*LogStream, error) {
	cmd := exec.Command(i.bin, "device", "start-log-stream", "-s", serial)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("vega log stream: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("vega log stream: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("vega log stream: %w", err)
	}
	s := &LogStream{cmd: cmd, lines: make(chan string, 128), done: make(chan struct{}), stop: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			select {
			case s.lines <- sc.Text():
			case <-s.stop:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return
			case <-ctx.Done():
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				return
			}
		}
		errb := make([]byte, 4096)
		if n, _ := stderr.Read(errb); n > 0 {
			s.stderr = string(errb[:n])
		}
		_ = cmd.Wait()
	}()
	return s, nil
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ParseApps parses `vega device installed-apps` output: one
// "<component_id>\\t<name>" per line. Golden: testdata/apps_*.golden.
func ParseApps(out string) ([]AppInfo, error) {
	var apps []AppInfo
	for n, ln := range strings.Split(out, "\n") {
		ln = strings.TrimRight(ln, "\r")
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, "\t", 2)
		id := strings.TrimSpace(parts[0])
		// Malformed output errors; a silent bogus entry would corrupt
		// every caller's view of installed apps (invariant 3).
		if id == "" || strings.Contains(id, " ") {
			return nil, fmt.Errorf("vega: malformed installed-apps output line %d: %q", n+1, ln)
		}
		app := AppInfo{ComponentID: id}
		if len(parts) == 2 {
			app.Name = strings.TrimSpace(parts[1])
		}
		apps = append(apps, app)
	}
	return apps, nil
}

const envVegaPath = "VDB_VEGA_PATH"

// ResolveError mirrors vda.ResolveError.
type ResolveError struct {
	Env      string
	Searched []string
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("vega binary not found: set %s, install on PATH, or install the Vega SDK (searched: %s)",
		e.Env, strings.Join(e.Searched, ", "))
}

var execLookPath = exec.LookPath

// ResolveVega finds the vega binary: env → PATH → default SDK location.
func ResolveVega() (string, error) {
	var searched []string
	if p := os.Getenv(envVegaPath); p != "" {
		searched = append(searched, p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			return p, nil
		}
		return "", &ResolveError{Env: envVegaPath, Searched: searched}
	}
	if p, err := execLookPath("vega"); err == nil {
		return p, nil
	}
	searched = append(searched, "PATH:vega")
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, "vega", "bin", "vega")
		searched = append(searched, p)
		if st, err := os.Stat(p); err == nil && st.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", &ResolveError{Env: envVegaPath, Searched: searched}
}
