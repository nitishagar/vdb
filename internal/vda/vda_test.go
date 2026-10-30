package vda

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitishagar/vdb/internal/testkit"
)

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(b)
}

func TestParseDevicesBasic(t *testing.T) {
	devs, err := ParseDevices(golden(t, "devices_basic.golden"))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("want 1 device, got %d", len(devs))
	}
	d := devs[0]
	if d.Serial != "AFTVTEST001" || d.Kind != "device" || !d.Attached {
		t.Errorf("parsed = %+v", d)
	}
	if !strings.Contains(d.Detail, "model:Fire_TV_Stick_4K") {
		t.Errorf("detail = %q", d.Detail)
	}
}

func TestParseDevicesMulti(t *testing.T) {
	devs, err := ParseDevices(golden(t, "devices_multi.golden"))
	if err != nil {
		t.Fatalf("ParseDevices: %v", err)
	}
	if len(devs) != 3 {
		t.Fatalf("want 3 devices, got %d", len(devs))
	}
	if devs[1].Serial != "emulator-5554" || devs[1].Kind != "device" {
		t.Errorf("second = %+v", devs[1])
	}
	if devs[2].Attached {
		t.Errorf("offline device must not be Attached: %+v", devs[2])
	}
}

func TestParseDevicesMalformed(t *testing.T) {
	_, err := ParseDevices("garbage with no header\nAFTV\tdevice\n")
	if err == nil {
		t.Fatal("want malformed-output error")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error should name the parse layer: %v", err)
	}
}

func TestParseForwardPort(t *testing.T) {
	p, err := ParseForwardPort(golden(t, "forward_port.golden"))
	if err != nil {
		t.Fatalf("ParseForwardPort: %v", err)
	}
	if p != 42 {
		t.Errorf("port = %d", p)
	}
	if _, err := ParseForwardPort("not a port"); err == nil {
		t.Error("want error for non-numeric output")
	}
	if _, err := ParseForwardPort("0"); err == nil {
		t.Error("want error for port 0")
	}
}

// --- resolution ------------------------------------------------------------

func TestResolveVDAEnvOverrideWins(t *testing.T) {
	t.Setenv("VDB_VDA_PATH", "/bin/true") // executable, exists on Linux
	p, err := ResolveVDA()
	if err != nil || p != "/bin/true" {
		t.Fatalf("want /bin/true, got %q err=%v", p, err)
	}
}

func TestResolveVDAEnvOverrideMissingFailsFast(t *testing.T) {
	t.Setenv("VDB_VDA_PATH", "/definitely/not/here")
	_, err := ResolveVDA()
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("want ResolveError, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "/definitely/not/here") {
		t.Errorf("error must name the searched location: %v", err)
	}
}

func TestResolveVDASearchedPathsInError(t *testing.T) {
	t.Setenv("VDB_VDA_PATH", "")
	// Force PATH miss and a home without SDK. HOME with no vega dir + PATH
	// without vda: error must list PATH and the SDK glob.
	t.Setenv("HOME", t.TempDir())
	orig := execLookPath
	execLookPath = func(string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { execLookPath = orig })

	_, err := ResolveVDA()
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("want ResolveError, got %T %v", err, err)
	}
	joined := err.Error()
	if !strings.Contains(joined, "PATH:vda") {
		t.Errorf("error must mention PATH search: %v", joined)
	}
	if !strings.Contains(joined, "vega") || !strings.Contains(joined, "sdk") {
		t.Errorf("error must mention SDK glob search: %v", joined)
	}
	if !strings.Contains(joined, "VDB_VDA_PATH") {
		t.Errorf("error must tell the user how to fix it: %v", joined)
	}
}

// --- Impl against the fake binary -------------------------------------------

func TestImplDevicesAgainstFake(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	bin, _ := testkit.FakeVDAEnv(t, "AFTVTEST001")
	impl := New(bin)
	devs, err := impl.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 1 || devs[0].Serial != "AFTVTEST001" {
		t.Fatalf("devices = %+v", devs)
	}
}

func TestImplShellAgainstFake(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	bin, _ := testkit.FakeVDAEnv(t, "AFTVTEST001")
	impl := New(bin)
	out, err := impl.Shell(context.Background(), "AFTVTEST001", "echo hi")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if !strings.Contains(out.Stdout, "fake-shell") || !strings.Contains(out.Stdout, "echo hi") {
		t.Errorf("shell output = %q", out.Stdout)
	}
}
