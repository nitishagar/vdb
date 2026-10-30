package vegacli

import (
	"context"
	execrun "github.com/nitishagar/vdb/internal/execrun"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return string(b)
}

func TestParseApps(t *testing.T) {
	apps, err := ParseApps(golden(t, "apps.golden"))
	if err != nil {
		t.Fatalf("ParseApps: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("want 2 apps, got %d", len(apps))
	}
	if apps[0].ComponentID != "com.example.app" || apps[0].Name != "Example App" {
		t.Errorf("apps[0] = %+v", apps[0])
	}
}

// Idempotent lifecycle semantics (invariant 6): already-so outcomes are
// typed results, not errors.
func TestParseAlreadySo(t *testing.T) {
	cases := []struct {
		stderr string
		want   LifecycleResult
	}{
		{"Package com.example.app is already installed", ResultAlreadyInstalled},
		{"Package com.example.app is not installed", ResultNotInstalled},
		{"no such package: com.example.app", ResultNotInstalled},
		{"App com.example.app is not running", ResultNotRunning},
		{"some unrelated hard failure", ""},
	}
	for _, tc := range cases {
		got, ok := parseAlreadySo(execrun.Result{Stderr: tc.stderr}, nil)
		if tc.want == "" {
			if ok {
				t.Errorf("stderr %q: unexpected result %s", tc.stderr, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("stderr %q: want %s got %s ok=%v", tc.stderr, tc.want, got, ok)
		}
	}
}

// --- Impl against a scriptable fake vega binary ------------------------------

// fakeVega is a tiny shell script standing in for the vega CLI: behavior is
// selected per test by the FAKEVEGA_MODE env var.
func fakeVega(t *testing.T, mode string) *Impl {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "vega")
	body := `#!/bin/sh
case "$FAKEVEGA_MODE" in
  ok) echo ok; exit 0 ;;
  already) echo "Package is already installed" >&2; exit 1 ;;
  notrunning) echo "App is not running" >&2; exit 1 ;;
  notinstalled) echo "no such package" >&2; exit 1 ;;
  hard) echo "segfault" >&2; exit 139 ;;
  apps) printf 'com.example.app\tExample App\ncom.example.other\tOther App\n'; exit 0 ;;
  logs) for i in 1 2 3 4 5; do echo "line$i"; done; sleep 30 ;;
esac
echo unhandled >&2; exit 9
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEVEGA_MODE", mode)
	return New(script)
}

func TestInstallAlreadyInstalledIsNoOp(t *testing.T) {
	impl := fakeVega(t, "already")
	res, err := impl.Install(context.Background(), "S", "pkg.vpkg")
	if err != nil {
		t.Fatalf("Install already-installed must not error: %v", err)
	}
	if res != ResultAlreadyInstalled {
		t.Errorf("res = %s", res)
	}
}

func TestTerminateNotRunningIsNoOp(t *testing.T) {
	impl := fakeVega(t, "notrunning")
	res, err := impl.Terminate(context.Background(), "S", "com.example.app")
	if err != nil {
		t.Fatalf("Terminate not-running must not error: %v", err)
	}
	if res != ResultNotRunning {
		t.Errorf("res = %s", res)
	}
}

func TestUninstallAbsentIsNoOp(t *testing.T) {
	impl := fakeVega(t, "notinstalled")
	res, err := impl.Uninstall(context.Background(), "S", "com.example.app")
	if err != nil {
		t.Fatalf("Uninstall absent must not error: %v", err)
	}
	if res != ResultNotInstalled {
		t.Errorf("res = %s", res)
	}
}

func TestInstallHardFailureIsError(t *testing.T) {
	impl := fakeVega(t, "hard")
	_, err := impl.Install(context.Background(), "S", "pkg.vpkg")
	if err == nil {
		t.Fatal("hard failure must error")
	}
	if !strings.Contains(err.Error(), "vega install-app") {
		t.Errorf("error must name the layer: %v", err)
	}
}

func TestInstallSuccess(t *testing.T) {
	impl := fakeVega(t, "ok")
	res, err := impl.Install(context.Background(), "S", "pkg.vpkg")
	if err != nil || res != ResultDone {
		t.Fatalf("res=%s err=%v", res, err)
	}
}

func TestListAppsAgainstFake(t *testing.T) {
	impl := fakeVega(t, "apps")
	apps, err := impl.ListApps(context.Background(), "S")
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0].ComponentID != "com.example.app" {
		t.Fatalf("apps = %+v", apps)
	}
}

func TestLogStreamLinesAndStop(t *testing.T) {
	impl := fakeVega(t, "logs")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := impl.StartLogStream(ctx, "S")
	if err != nil {
		t.Fatalf("StartLogStream: %v", err)
	}
	got := 0
	timeout := time.After(5 * time.Second)
	for got < 5 {
		select {
		case line, ok := <-stream.Lines():
			if !ok {
				t.Fatalf("stream closed early after %d lines", got)
			}
			if !strings.HasPrefix(line, "line") {
				t.Errorf("line = %q", line)
			}
			got++
		case <-timeout:
			t.Fatalf("timed out with %d lines", got)
		}
	}
	stream.Stop() // kills `sleep 30`; must not hang
	if _, stillOpen := <-stream.Lines(); stillOpen {
		t.Error("channel must be closed after Stop")
	}
}
