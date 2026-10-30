// Package testkit holds shared test helpers: building the fakevda test
// binary once per test process and setting up its state directory.
package testkit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// FakeVDA builds (once per process) and returns the path to the fakevda
// test binary. Fails the test on build error.
func FakeVDA(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := os.CreateTemp("", "fakevda-*.bin")
		if err != nil {
			buildErr = err
			return
		}
		_ = out.Close()
		cmd := exec.Command("go", "build", "-o", out.Name(), "github.com/nitishagar/vdb/internal/fakevda")
		cmd.Env = os.Environ()
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build fakevda: %v: %s", err, b)
			return
		}
		binPath = out.Name()
	})
	if buildErr != nil {
		t.Fatalf("testkit: %v", buildErr)
	}
	t.Cleanup(func() {
		// Best-effort: binary is process-wide; removed when tests exit.
	})
	return binPath
}

// FakeVDAEnv returns (binPath, stateDir) with a devices.txt listing one
// attached device serial, and sets FAKEVDA_STATE_DIR for the test.
func FakeVDAEnv(t testing.TB, serial string) (string, string) {
	t.Helper()
	bin := FakeVDA(t)
	dir := t.TempDir()
	WriteDevices(t, dir, serial)
	t.Setenv("FAKEVDA_STATE_DIR", dir)
	return bin, dir
}

// WriteDevices writes the devices.txt listing for the state dir.
func WriteDevices(t testing.TB, dir, serial string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "devices.txt"), []byte(
		fmt.Sprintf("%s\tdevice\tproduct:fake\tmodel:Fake_TV\ttransport:usb\n", serial)), 0o644); err != nil {
		t.Fatalf("write devices.txt: %v", err)
	}
}
