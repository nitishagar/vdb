// Package vda adapts the user's installed VDA (Vega Device Adapter) binary —
// an adb-derived bridge shipped with the Vega SDK. vdb never bundles VDA; it
// resolves the user's binary (env → PATH → default SDK location), invokes it
// as a subprocess, and parses its output against golden fixtures.
//
// Output formats are undocumented by Amazon (observed in developer logs);
// every parse rule lives here and is exercised against testdata goldens so a
// format change is corrected in exactly this package.
package vda

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nitishagar/vdb/internal/execrun"
)

// Device is one entry from `vda devices -l`.
type Device struct {
	Serial   string
	Kind     string // "device" | "emulator-5554" style prefixes preserved raw
	Detail   string // rest of the -l line (model:, transport: …)
	Attached bool
}

// ForwardHandle owns one established port-forward for a target serial.
// Release must be called on every exit path; it is idempotent.
type ForwardHandle interface {
	// LocalPort is the local TCP port forwarding to the device port.
	LocalPort() int
	// Release removes the forward. Safe to call more than once.
	Release(ctx context.Context) error
}

// VDA is the device-bridge surface vdb depends on. The fake in
// internal/fakevda implements the same observable behavior.
type VDA interface {
	Devices(ctx context.Context) ([]Device, error)
	Version(ctx context.Context) (string, error)
	Forward(ctx context.Context, serial string, devicePort int) (ForwardHandle, error)
	Shell(ctx context.Context, serial, command string) (string, error)
	Push(ctx context.Context, serial, local, remote string) error
	Pull(ctx context.Context, serial, remote, local string) error
}

// Impl shells out to a resolved VDA binary.
type Impl struct {
	bin string // resolved binary path
}

// New returns an Impl bound to the given binary path (already resolved).
func New(bin string) *Impl { return &Impl{bin: bin} }

// Bin returns the resolved binary path (for diagnostics).
func (i *Impl) Bin() string { return i.bin }

func (i *Impl) Devices(ctx context.Context) ([]Device, error) {
	res, err := execrun.Run(ctx, i.bin, "devices", "-l")
	if err != nil {
		return nil, fmt.Errorf("vda devices: %w", err)
	}
	return ParseDevices(res.Stdout)
}

func (i *Impl) Version(ctx context.Context) (string, error) {
	res, err := execrun.Run(ctx, i.bin, "version")
	if err != nil {
		return "", fmt.Errorf("vda version: %w", err)
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (i *Impl) Forward(ctx context.Context, serial string, devicePort int) (ForwardHandle, error) {
	// adb-derived convention: `forward tcp:0 tcp:<devicePort>` makes the
	// server pick a local port and print it.
	res, err := execrun.Run(ctx, i.bin, "-s", serial, "forward", "tcp:0", "tcp:"+strconv.Itoa(devicePort))
	if err != nil {
		return nil, fmt.Errorf("vda forward %s: %w", serial, err)
	}
	port, err := ParseForwardPort(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("vda forward %s: %w", serial, err)
	}
	return &cmdForward{bin: i.bin, serial: serial, port: port}, nil
}

func (i *Impl) Shell(ctx context.Context, serial, command string) (string, error) {
	res, err := execrun.Run(ctx, i.bin, "-s", serial, "shell", command)
	if err != nil {
		return "", fmt.Errorf("vda shell: %w", err)
	}
	return res.Stdout, nil
}

func (i *Impl) Push(ctx context.Context, serial, local, remote string) error {
	_, err := execrun.Run(ctx, i.bin, "-s", serial, "push", local, remote)
	if err != nil {
		return fmt.Errorf("vda push %s: %w", local, err)
	}
	return nil
}

func (i *Impl) Pull(ctx context.Context, serial, remote, local string) error {
	_, err := execrun.Run(ctx, i.bin, "-s", serial, "pull", remote, local)
	if err != nil {
		return fmt.Errorf("vda pull %s: %w", remote, err)
	}
	return nil
}

type cmdForward struct {
	bin    string
	serial string
	port   int
}

func (f *cmdForward) LocalPort() int { return f.port }

func (f *cmdForward) Release(ctx context.Context) error {
	_, err := execrun.Run(ctx, f.bin, "-s", f.serial, "forward", "--remove", "tcp:"+strconv.Itoa(f.port))
	if err == nil {
		return nil
	}
	// An already-removed forward satisfies the invariant; anything else is
	// a real failure the caller must see.
	blob := ""
	var nz *execrun.ErrNonZero
	if errors.As(err, &nz) {
		blob = nz.Stderr + nz.Stdout
	}
	if containsAnyFold(blob, "not found", "no such", "listener", "does not exist") {
		return nil
	}
	return fmt.Errorf("vda release forward tcp:%d: %w", f.port, err)
}

func containsAnyFold(s string, subs ...string) bool {
	low := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(low, sub) {
			return true
		}
	}
	return false
}

// ParseDevices parses `vda devices -l` output. Golden fixtures:
// internal/vda/testdata/devices_*.golden.
func ParseDevices(out string) ([]Device, error) {
	lines := strings.Split(out, "\n")
	var devs []Device
	started := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, "\r")
		if !started {
			if strings.HasPrefix(ln, "List of devices") {
				started = true
			}
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		devs = append(devs, Device{
			Serial:   fields[0],
			Kind:     fields[1],
			Detail:   strings.Join(fields[2:], " "),
			Attached: fields[1] == "device",
		})
	}
	if !started {
		return nil, fmt.Errorf("vda: malformed devices output: no header line")
	}
	return devs, nil
}

// ParseForwardPort parses the integer local port printed by `vda forward`.
func ParseForwardPort(out string) (int, error) {
	s := strings.TrimSpace(out)
	// Some adb-derived tools print extra lines; take the last all-digit line.
	var last string
	for _, ln := range strings.Split(s, "\n") {
		if ln == "" {
			continue
		}
		last = ln
	}
	port, err := strconv.Atoi(strings.TrimSpace(last))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("vda: malformed forward output %q", out)
	}
	return port, nil
}

// --- binary resolution -----------------------------------------------------

// ResolveError names every location that was searched, so a user (or agent)
// can see exactly how to fix the environment.
type ResolveError struct {
	Env      string
	Searched []string
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("vda binary not found: set %s, install on PATH, or install the Vega SDK (searched: %s)",
		e.Env, strings.Join(e.Searched, ", "))
}

const envVDAPath = "VDB_VDA_PATH"

// ResolveVDA finds the vda binary: env override → PATH → default SDK glob.
func ResolveVDA() (string, error) {
	var searched []string
	if p := os.Getenv(envVDAPath); p != "" {
		searched = append(searched, p)
		if fileExecutable(p) {
			return p, nil
		}
		return "", &ResolveError{Env: envVDAPath, Searched: searched}
	}
	if p, err := execLookPath("vda"); err == nil {
		return p, nil
	}
	searched = append(searched, "PATH:vda")
	home, err := os.UserHomeDir()
	if err == nil {
		glob := filepath.Join(home, "vega", "sdk", "*", "bin", "tools", "vda")
		matches, _ := filepath.Glob(glob)
		for _, m := range matches {
			searched = append(searched, m)
			if fileExecutable(m) {
				return m, nil
			}
		}
		if len(matches) == 0 {
			searched = append(searched, glob)
		}
	}
	return "", &ResolveError{Env: envVDAPath, Searched: searched}
}

func fileExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0111 != 0
}

// execLookPath is a test seam; production resolves via PATH.
var execLookPath = exec.LookPath
