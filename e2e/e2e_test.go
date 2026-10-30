//go:build e2e

// End-to-end gate: builds the REAL vdb-companion and vdb binaries, runs
// them against the fakes, drives the full agent loop through the CLI, and
// asserts process hygiene (every spawned PID exits; navigate wall-clock
// bound). This is the strongest local signal that the shipped product
// works, not just its packages.
package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/testkit"
)

const serial = "AFTVE2E001"

const pageResume = `<root><app id="com.example.app"><window x="0" y="0" width="1920" height="1080">` +
	`<child x="100" y="200" width="500" height="100" role="button" test_id="resume" focusable="true" focused="true"/>` +
	`<child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="false"/>` +
	`</window></app></root>`

const pageEpisodes = `<root><app id="com.example.app"><window x="0" y="0" width="1920" height="1080">` +
	`<child x="100" y="200" width="500" height="100" role="button" test_id="resume" focusable="true" focused="false"/>` +
	`<child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="true"/>` +
	`</window></app></root>`

type env struct {
	companionBin string
	cliBin       string
	bind         string
	stateDir     string
}

func buildBinaries(t *testing.T) (companion, cli string) {
	t.Helper()
	dir := t.TempDir()
	companion = filepath.Join(dir, "vdb-companion")
	cli = filepath.Join(dir, "vdb")
	for _, b := range []struct{ path, pkg string }{
		{companion, "github.com/nitishagar/vdb/cmd/vdb-companion"},
		{cli, "github.com/nitishagar/vdb/cmd/vdb"},
	} {
		cmd := exec.Command("go", "build", "-o", b.path, b.pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", b.pkg, err, out)
		}
	}
	return companion, cli
}

func TestFullAgentLoop(t *testing.T) {
	tk := faketoolkit.New(t)
	// Script the whole journey with generous step counts.
	pages := make([]faketoolkit.Step, 0, 64)
	pages = append(pages, faketoolkit.XMLStep(pageResume)) // describe
	pages = append(pages, faketoolkit.XMLStep(pageResume)) // focused
	for i := 0; i < 40; i++ {
		pages = append(pages, faketoolkit.XMLStep(pageEpisodes))
	}
	tk.Script("getPageSource", pages...)
	injects := make([]faketoolkit.Step, 16)
	for i := range injects {
		injects[i] = faketoolkit.ObjectStep(nil)
	}
	tk.Script("injectInputKeyEvent", injects...)
	rects := make([]faketoolkit.Step, 4)
	for i := range rects {
		rects[i] = faketoolkit.ObjectStep(map[string]any{"x": 0, "y": 0, "width": 1920, "height": 1080})
	}
	tk.Script("getWindowRect", rects...)
	tk.Script("getScreenContext", faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"}), faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"}), faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"}), faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"}))
	tk.Script("takeScreenshot", faketoolkit.PNGStep(faketoolkit.ValidPNG), faketoolkit.PNGStep(faketoolkit.ValidPNG), faketoolkit.PNGStep(faketoolkit.ValidPNG))

	vdaBin, state := testkit.FakeVDAEnv(t, serial)
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", strings.TrimPrefix(tk.URL(), "http://"))

	// A vega CLI fake: install ok, apps listed, log lines.
	vegaBin := filepath.Join(t.TempDir(), "vega")
	script := "#!/bin/sh\ncase \"$1:$2\" in\n" +
		"\"device:install-app\") echo installed; exit 0 ;;\n" +
		"\"device:launch-app\") echo launched; exit 0 ;;\n" +
		"\"device:installed-apps\") printf 'com.example.app\\tExample App\\n'; exit 0 ;;\n" +
		"\"device:start-log-stream\") for i in 1 2 3; do echo \"log$i\"; done; sleep 30 ;;\n" +
		"esac\necho ok; exit 0\n"
	if err := os.WriteFile(vegaBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// A real (dummy) package file — /dev/null reads as empty and the
	// companion correctly rejects zero-payload installs.
	pkgPath := filepath.Join(t.TempDir(), "app.vpkg")
	if err := os.WriteFile(pkgPath, []byte("VPKG-DUMMY-PAYLOAD"), 0o644); err != nil {
		t.Fatal(err)
	}

	companionBin, cliBin := buildBinaries(t)

	// Start the companion with both binaries resolvable.
	bind := "127.0.0.1:0"
	// Pick an actual free port by listening and closing.
	lnFree, err := listenFree()
	if err != nil {
		t.Fatal(err)
	}
	bind = lnFree
	comp := exec.Command(companionBin, "--bind", bind)
	comp.Env = append(os.Environ(),
		"VDB_VDA_PATH="+vdaBin,
		"VDB_VEGA_PATH="+vegaBin,
		"FAKEVDA_STATE_DIR="+state,
		"FAKEVDA_TOOLKIT_ADDR="+strings.TrimPrefix(tk.URL(), "http://"),
	)
	comp.Stderr = os.Stderr
	if err := comp.Start(); err != nil {
		t.Fatal(err)
	}
	// Reap the companion and record its exit.
	compDone := make(chan error, 1)
	go func() { compDone <- comp.Wait() }()

	vdb := func(args ...string) (string, error) {
		// Flags go directly after the verb (Go's flag package stops at the first
		// positional argument).
		full := append([]string{args[0], "--bind", bind, "--target", serial}, args[1:]...)
		cmd := exec.Command(cliBin, full...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("vdb %v failed: %v\n%s", args, err, out)
		}
		return string(out), err
	}

	// Wait for the companion to accept connections.
	deadline := time.Now().Add(15 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out, err = vdb("list-targets")
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("companion never came up: %v\n%s", err, out)
	}
	if !strings.Contains(out, serial) {
		t.Fatalf("list-targets output missing serial: %q", out)
	}

	steps := []struct {
		name string
		args []string
		want string
	}{
		{"install", []string{"install", pkgPath}, "done"},
		{"launch", []string{"launch", "com.example.app"}, ""},
		{"list-apps", []string{"list-apps"}, "com.example.app"},
		{"describe", []string{"describe"}, "episodes"},
		{"focused", []string{"focused"}, "test_id:resume"},
		{"keys", []string{"keys", "DOWN"}, "test_id:episodes"},
		{"screenshot", []string{"screenshot"}, "digest"},
		{"wait-idle", []string{"wait-idle", "--stable-ms", "100", "--timeout-ms", "3000"}, "stable=true"},
	}
	// Navigate gets a wall-clock budget (hang detection, not a perf SLO).
	navigateStart := time.Now()
	for _, s := range steps {
		out, err := vdb(s.args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", s.name, err, out)
		}
		if s.want != "" && !strings.Contains(out, s.want) {
			t.Fatalf("%s: output %q missing %q", s.name, out, s.want)
		}
	}
	if d := time.Since(navigateStart); d > 5*time.Second {
		t.Errorf("core loop took %v (>5s budget)", d)
	}

	navigateOut, err := vdb("navigate", "test_id=episodes")
	if err != nil {
		t.Fatalf("navigate: %v\n%s", err, navigateOut)
	}
	if !strings.Contains(navigateOut, "reached") || time.Since(navigateStart) > 10*time.Second {
		t.Fatalf("navigate output: %q", navigateOut)
	}

	// Shutdown: SIGTERM the companion (graceful stop releases sessions);
	// escalate to SIGKILL after a bound.
	_ = comp.Process.Signal(os.Interrupt)
	select {
	case <-compDone:
	case <-time.After(3 * time.Second):
		_ = comp.Process.Kill()
		select {
		case <-compDone:
		case <-time.After(2 * time.Second):
			t.Fatal("companion did not exit even after SIGKILL")
		}
	}

	// Process sweep: no forward children survive.
	sweepDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(sweepDeadline) {
		pids, _ := filepath.Glob(filepath.Join(state, "forward-*.pid"))
		if len(pids) == 0 {
			return // clean
		}
		time.Sleep(100 * time.Millisecond)
	}
	pids, _ := filepath.Glob(filepath.Join(state, "forward-*.pid"))
	if len(pids) > 0 {
		// The forward child may outlive the companion by design (it is a
		// detached session) — kill and fail: registry Close should have
		// released them via vda forward --remove.
		for _, p := range pids {
			data, _ := os.ReadFile(p)
			pid := 0
			fmt.Sscanf(string(data), "%d", &pid)
			if pid != 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Kill()
				}
			}
		}
		t.Errorf("forward children left behind after companion exit: %v", pids)
	}
}

func listenFree() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}
