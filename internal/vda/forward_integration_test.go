package vda

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/testkit"
	"github.com/nitishagar/vdb/internal/toolkit"
)

// Forward lifecycle through the real path: vda.Impl spawns fakevda, which
// runs a real TCP proxy to the fake toolkit; the toolkit client talks JSON-
// RPC through it; Release removes the forward on every path and is
// idempotent (invariant 4).
func TestForwardLifecycleEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	srv := faketoolkit.New(t)
	srv.Script("getPageSource", faketoolkit.XMLStep("<root><app id=\"com.example.app\"/></root>"))

	bin, state := testkit.FakeVDAEnv(t, "AFTVTEST001")
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", stripScheme(srv.URL()))
	impl := New(bin)

	ctx := context.Background()
	h, err := impl.Forward(ctx, "AFTVTEST001", 8383)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	port := h.LocalPort()
	if port <= 0 || port > 65535 {
		t.Fatalf("port = %d", port)
	}

	client := toolkit.NewClient("http://127.0.0.1:" + strconv.Itoa(port))
	if _, err := client.GetPageSource(ctx); err != nil {
		t.Fatalf("GetPageSource through forward: %v", err)
	}

	// Release removes the forward; a second Release is a no-op.
	if err := h.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := h.Release(ctx); err != nil {
		t.Fatalf("second Release must not error: %v", err)
	}

	// After release, the proxy port is gone: the toolkit call must fail as
	// transport, promptly.
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.GetPageSource(cctx)
	if err == nil {
		t.Fatal("post-release call must fail")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("post-release failure took %v", time.Since(start))
	}

	// No forward children left behind: the pid registry must be empty.
	entries, _ := filepath.Glob(filepath.Join(state, "forward-*.pid"))
	if len(entries) != 0 {
		t.Errorf("forward pid files left behind: %v", entries)
	}
}

// Forward-death mid-call → typed transport error < 5 s; the NEXT call
// re-forwards and succeeds (the caller policy from the plan, exercised at
// adapter level).
func TestForwardDeathAndReestablish(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	srv := faketoolkit.New(t)
	srv.Script("getPageSource",
		faketoolkit.XMLStep("<root/>"),
		faketoolkit.XMLStep("<root/>"),
		faketoolkit.XMLStep("<root/>"),
	)
	bin, _ := testkit.FakeVDAEnv(t, "AFTVTEST001")
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", stripScheme(srv.URL()))
	impl := New(bin)
	ctx := context.Background()

	h1, err := impl.Forward(ctx, "AFTVTEST001", 8383)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	c1 := toolkit.NewClient("http://127.0.0.1:" + strconv.Itoa(h1.LocalPort()))
	if _, err := c1.GetPageSource(ctx); err != nil {
		t.Fatalf("baseline call: %v", err)
	}

	// Kill the forward's owning child (simulates VDA forward death).
	killForwardChild(t, h1.LocalPort())

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c1.GetPageSource(cctx); err == nil {
		t.Fatal("call through dead forward must fail")
	} else if !toolkit.IsTransport(err) {
		t.Fatalf("want transport error, got %T %v", err, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("dead-forward failure took %v", time.Since(start))
	}

	// Caller policy: re-forward once, retry once → success.
	h2, err := impl.Forward(ctx, "AFTVTEST001", 8383)
	if err != nil {
		t.Fatalf("re-Forward: %v", err)
	}
	defer func() { _ = h2.Release(ctx) }()
	c2 := toolkit.NewClient("http://127.0.0.1:" + strconv.Itoa(h2.LocalPort()))
	if _, err := c2.GetPageSource(ctx); err != nil {
		t.Fatalf("retry after re-forward: %v", err)
	}
}

func killForwardChild(t *testing.T, port int) {
	t.Helper()
	dir := os.Getenv("FAKEVDA_STATE_DIR")
	data, err := os.ReadFile(filepath.Join(dir, "forward-"+strconv.Itoa(port)+".pid"))
	if err != nil {
		t.Fatalf("read forward pid: %v", err)
	}
	pid := 0
	for _, c := range string(data) {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid == 0 {
		t.Fatal("bad pid file")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill forward child: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "forward-"+strconv.Itoa(port)+".pid"))
}

func stripScheme(url string) string {
	if len(url) > 7 && url[:7] == "http://" {
		return url[7:]
	}
	return url
}
