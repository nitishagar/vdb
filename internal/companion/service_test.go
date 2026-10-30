package companion

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/testkit"
	"github.com/nitishagar/vdb/internal/vda"
	"github.com/nitishagar/vdb/internal/vegacli"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
)

// multiAppXML is a two-app page source (launcher + app) with focus on
// resume; used by serialization/navigation/UI tests.
const multiAppXML = `<root>
  <app id="com.amazon.smplighthouse.launcher">
    <window x="0" y="0" width="1920" height="1080" role="window" id="lw">
      <child x="0" y="0" width="1920" height="120" role="menu" test_id="launcher-tiles" focusable="false"/>
    </window>
  </app>
  <app id="com.example.app">
    <window x="0" y="120" width="1920" height="960" role="window" id="aw">
      <child x="100" y="200" width="500" height="100" role="button" test_id="resume" focusable="true" focused="true"/>
      <child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="false"/>
    </window>
  </app>
</root>`

const appXMLFocusedEpisodes = `<root>
  <app id="com.example.app">
    <window x="0" y="120" width="1920" height="960" role="window" id="aw">
      <child x="100" y="200" width="500" height="100" role="button" test_id="resume" focusable="true" focused="false"/>
      <child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="true"/>
    </window>
  </app>
</root>`

const appXMLFocusedResume = `<root>
  <app id="com.example.app">
    <window x="0" y="120" width="1920" height="960" role="window" id="aw">
      <child x="100" y="200" width="500" height="100" role="button" test_id="resume" focusable="true" focused="true"/>
      <child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="false"/>
    </window>
  </app>
</root>`

const serial = "AFTVTEST001"

// harness wires service ↔ fakes over a real gRPC loopback.
type harness struct {
	t      *testing.T
	tk     *faketoolkit.Server
	conn   *grpc.ClientConn
	client vdbv1.CompanionClient
	daemon *Daemon
	tmpDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessCfg(t, Config{})
}

func newHarnessCfg(t *testing.T, cfg Config) *harness {
	t.Helper()
	// Leak check registered FIRST so it runs LAST (after the server, conn,
	// and registry cleanups below). Ignored frames are grpc-internal
	// background pollers, not vdb goroutines.
	t.Cleanup(func() {
		time.Sleep(100 * time.Millisecond) // let grpc observe its stops
		goleak.VerifyNone(t,
			goleak.IgnoreTopFunction("google.golang.org/grpc/internal/resolver/passthrough.(*passthroughBuilder).Build.func1"),
			goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*http2Client).keepalive"),
		)
	})

	tk := faketoolkit.New(t)
	vdaBin, state := testkit.FakeVDAEnv(t, serial)
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", strings.TrimPrefix(tk.URL(), "http://"))

	// A scriptable fake vega CLI.
	vegaBin := filepath.Join(t.TempDir(), "vega")
	if err := os.WriteFile(vegaBin, []byte(vegaScript), 0o755); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry(vda.New(vdaBin), vegacli.New(vegaBin))
	_ = state
	cfg.InstallTmpDir = t.TempDir()
	svc := NewService(reg, cfg)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(
		grpc.MaxSendMsgSize(DefaultMaxMessageBytes),
		grpc.MaxRecvMsgSize(DefaultMaxMessageBytes),
	)
	vdbv1.RegisterCompanionServer(srv, svc)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	d := &Daemon{Bind: ln.Addr().String(), Registry: reg, Service: svc, Server: srv, listener: ln}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reg.Close(ctx)
	})

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(DefaultMaxMessageBytes), grpc.MaxCallRecvMsgSize(DefaultMaxMessageBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &harness{t: t, tk: tk, conn: conn, client: vdbv1.NewCompanionClient(conn), daemon: d, tmpDir: cfg.InstallTmpDir}
}

const vegaScript = `#!/bin/sh
case "$VEGA_MODE" in
  ok) echo ok; exit 0 ;;
  already) echo "Package is already installed" >&2; exit 1 ;;
  apps) printf 'com.example.app\tExample App\n'; exit 0 ;;
  logs) for i in $(seq 1 3); do echo "line$i"; done; sleep 30 ;;
  logs_die) for i in $(seq 1 3); do echo "line$i"; done; echo "boom: device disconnected" >&2; exit 1 ;;
esac
echo ok; exit 0
`

func xmlSteps(n int, xml string) []faketoolkit.Step {
	steps := make([]faketoolkit.Step, n)
	for i := range steps {
		steps[i] = faketoolkit.XMLStep(xml)
	}
	return steps
}

// Serialization (invariant 13): concurrent SendKeys on one target never
// interleave — the fake toolkit's call log must show complete
// press→read windows: every getPageSource is preceded by exactly one
// injectInputKeyEvent since the previous read.
func TestSendKeysSerializesPerTarget(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("injectInputKeyEvent", xmlSteps(32, "")...)
	h.tk.Script("getPageSource", xmlSteps(32, appXMLFocusedResume)...)
	h.tk.Script("getWindowRect", xmlSteps(2, "")...)

	const goroutines = 4
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.client.SendKeys(context.Background(), &vdbv1.SendKeysRequest{
				Udid:       serial,
				Keys:       []*vdbv1.KeySpec{{Name: "DOWN"}, {Name: "DOWN"}},
				FocusAfter: true,
			})
			if err != nil {
				t.Errorf("SendKeys: %v", err)
			}
		}()
	}
	wg.Wait()

	// Serialization model: one SendKeys call = its presses (here 2) then
	// ONE focus read, all under the target's input lock. Interleaved
	// callers would show more presses between two reads than a single call
	// issues (2+2=4 from two racing calls).
	calls := h.tk.Calls()
	const pressesPerCall = 2
	injectsSinceRead := 0
	sawRead := false
	for _, c := range calls {
		switch c.Method {
		case "injectInputKeyEvent":
			injectsSinceRead++
			if injectsSinceRead > pressesPerCall {
				t.Fatalf("more presses between reads than one call issues — presses interleaved: %s", callSeq(calls))
			}
		case "getPageSource":
			sawRead = true
			injectsSinceRead = 0
		}
	}
	if !sawRead {
		t.Fatalf("no focus reads observed: %s", callSeq(calls))
	}
}

func callSeq(calls []faketoolkit.Call) string {
	var parts []string
	for _, c := range calls {
		parts = append(parts, c.Method)
	}
	return strings.Join(parts, " → ")
}

// Forward death mid-session: the next RPC transparently re-forwards and
// succeeds (caller policy, invariant 4).
func TestForwardDeathReestablishes(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("getPageSource", xmlSteps(16, appXMLFocusedResume)...)

	if _, err := h.client.DescribeUI(context.Background(), &vdbv1.DescribeUIRequest{Udid: serial}); err != nil {
		t.Fatalf("baseline DescribeUI: %v", err)
	}

	// Kill the forward's owning child.
	killForwardOwner(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.client.DescribeUI(ctx, &vdbv1.DescribeUIRequest{Udid: serial}); err != nil {
		t.Fatalf("post-death DescribeUI must re-establish the forward: %v", err)
	}
}

func killForwardOwner(t *testing.T) {
	t.Helper()
	dir := os.Getenv("FAKEVDA_STATE_DIR")
	entries, err := filepath.Glob(filepath.Join(dir, "forward-*.pid"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no forward pid files in %s: %v", dir, err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(e)
		if err != nil {
			t.Fatal(err)
		}
		pid := 0
		for _, c := range strings.TrimSpace(string(data)) {
			if c < '0' || c > '9' {
				break
			}
			pid = pid*10 + int(c-'0')
		}
		if pid == 0 {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
		_ = os.Remove(e)
	}
}

// A ~12 MB capture transits the API byte-intact (invariant 14).
func TestLargeScreenshotTransitsIntact(t *testing.T) {
	h := newHarness(t)
	big := make([]byte, 12<<20)
	copy(big, faketoolkit.ValidPNG)
	big[33] ^= 0xFF // distinct bytes within the payload
	h.tk.Script("takeScreenshot", faketoolkit.PNGStep(big))

	resp, err := h.client.Screenshot(context.Background(), &vdbv1.ScreenshotRequest{Udid: serial})
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(resp.GetPng()) != len(big) {
		t.Fatalf("png bytes = %d, want %d (truncation!)", len(resp.GetPng()), len(big))
	}
	for i := range big {
		if resp.GetPng()[i] != big[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

// Oversized captures error RESOURCE_EXHAUSTED — never silent truncation.
func TestOversizedScreenshotErrorsNotTruncates(t *testing.T) {
	h := newHarnessCfg(t, Config{MaxScreenshotBytes: 32})
	h.tk.Script("takeScreenshot", faketoolkit.PNGStep(faketoolkit.ValidPNG))

	_, err := h.client.Screenshot(context.Background(), &vdbv1.ScreenshotRequest{Udid: serial})
	if err == nil {
		t.Fatal("oversized screenshot must error")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("code = %s, want ResourceExhausted (%v)", status.Code(err), err)
	}
	if !strings.Contains(err.Error(), "no truncation") {
		t.Errorf("error should state the no-truncation policy: %v", err)
	}
}

// SendKeys responses carry post-action focus (invariant 20).
func TestSendKeysReturnsFocusState(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("injectInputKeyEvent", xmlSteps(4, "")...)
	h.tk.Script("getPageSource", xmlSteps(8, appXMLFocusedEpisodes)...)

	resp, err := h.client.SendKeys(context.Background(), &vdbv1.SendKeysRequest{
		Udid:       serial,
		Keys:       []*vdbv1.KeySpec{{Name: "DOWN"}},
		FocusAfter: true,
	})
	if err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if resp.GetFocus().GetFocusedId() != "test_id:episodes" || resp.GetFocus().GetTreeDigest() == "" {
		t.Errorf("focus = %+v", resp.GetFocus())
	}
}

// Navigate streams steps then the final focus (invariant 20 + closed loop).
func TestNavigateStreamsStepsAndFocus(t *testing.T) {
	h := newHarness(t)
	// resume is focused; DOWN moves focus to episodes.
	h.tk.Script("injectInputKeyEvent", xmlSteps(4, "")...)
	script := append(xmlSteps(1, appXMLFocusedResume), xmlSteps(8, appXMLFocusedEpisodes)...)
	h.tk.Script("getPageSource", script...)

	stream, err := h.client.Navigate(context.Background(), &vdbv1.NavigateRequest{Udid: serial, Selector: "test_id=episodes"})
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	var steps []string
	var reached *vdbv1.FocusState
	var failure *vdbv1.NavigateFailure
	for {
		resp, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch u := resp.GetUpdate().(type) {
		case *vdbv1.NavigateResponse_Step:
			steps = append(steps, u.Step)
		case *vdbv1.NavigateResponse_Reached:
			reached = u.Reached
		case *vdbv1.NavigateResponse_Failure:
			failure = u.Failure
		}
		if reached != nil || failure != nil {
			break
		}
	}
	if failure != nil {
		t.Fatalf("navigate failed: reason=%s detail=%s", failure.GetReason(), failure.GetDetail())
	}
	if strings.Join(steps, ",") != "DOWN" || reached.GetFocusedId() != "test_id:episodes" {
		t.Errorf("steps=%v reached=%+v", steps, reached)
	}
}

// WaitIdle on a churning tree returns unstable within its deadline.
func TestWaitIdleNeverStabilizing(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("getPageSource", faketoolkit.ChurnSteps(200)...)

	start := time.Now()
	resp, err := h.client.WaitIdle(context.Background(), &vdbv1.WaitIdleRequest{
		Udid: serial, StableMs: 400, PollMs: 50, TimeoutMs: 600,
	})
	if err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if resp.GetStable() {
		t.Error("churning tree must not report stable")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("WaitIdle overshot its deadline: %v", time.Since(start))
	}
}

// WaitIdle on a settled tree reports stable with the digest.
func TestWaitIdleStabilizes(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("getPageSource", xmlSteps(64, appXMLFocusedResume)...)

	resp, err := h.client.WaitIdle(context.Background(), &vdbv1.WaitIdleRequest{
		Udid: serial, StableMs: 100, PollMs: 40, TimeoutMs: 5000,
	})
	if err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if !resp.GetStable() || resp.GetTreeDigest() == "" {
		t.Errorf("stable=%v digest=%q", resp.GetStable(), resp.GetTreeDigest())
	}
}

// DescribeUI filters to the foreground app via getScreenContext.
func TestDescribeUIForegroundFilter(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("getPageSource", xmlSteps(4, multiAppXML)...)
	ctxSteps := make([]faketoolkit.Step, 4)
	for i := range ctxSteps {
		ctxSteps[i] = faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"})
	}
	h.tk.Script("getScreenContext", ctxSteps...)

	resp, err := h.client.DescribeUI(context.Background(), &vdbv1.DescribeUIRequest{Udid: serial})
	if err != nil {
		t.Fatalf("DescribeUI: %v", err)
	}
	if strings.Contains(resp.GetTreeJson(), "launcher-tiles") {
		t.Error("foreground filter must exclude the launcher tree")
	}
	if !strings.Contains(resp.GetTreeJson(), "test_id\":\"episodes") && !strings.Contains(resp.GetTreeJson(), "episodes") {
		t.Error("app nodes missing from filtered tree")
	}
	if resp.GetTreeDigest() == "" || resp.GetSummary() == "" {
		t.Error("digest and summary required")
	}
}

// Log stream: subprocess death mid-stream ends the stream with
// Unavailable while the companion keeps serving other RPCs.
func TestLogStreamDeathKeepsCompanionAlive(t *testing.T) {
	h := newHarness(t)
	t.Setenv("VEGA_MODE", "logs_die")
	h.tk.Script("getPageSource", xmlSteps(4, appXMLFocusedResume)...)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := h.client.Log(ctx, &vdbv1.LogRequest{Udid: serial})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	got := 0
	for {
		_, err := stream.Recv()
		if err != nil {
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("stream end code = %s (%v), want Unavailable", status.Code(err), err)
			}
			break
		}
		got++
		if got > 10 {
			t.Fatal("endless lines without stream end")
		}
	}
	// The companion must still serve.
	if _, err := h.client.DescribeUI(ctx, &vdbv1.DescribeUIRequest{Udid: serial}); err != nil {
		t.Fatalf("companion must keep serving after log-stream death: %v", err)
	}
}

// Install: partial upload failure leaves no temp file behind.
func TestInstallPartialFailureCleansTemp(t *testing.T) {
	h := newHarness(t)
	// Client vanishes mid-upload: cancel the RPC context after the first
	// chunk — the server must clean its temp file on the error path.
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.Install(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Meta{
		Meta: &vdbv1.InstallMeta{Udid: serial, FileName: "app.vpkg"},
	}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Chunk{Chunk: []byte("half a pkg")}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel() // client aborts before EOF

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(h.tmpDir)
		if len(entries) == 0 {
			return // cleaned on the failure path — the invariant
		}
		time.Sleep(50 * time.Millisecond)
	}
	entries, _ := os.ReadDir(h.tmpDir)
	t.Errorf("temp files left behind after aborted install: %d", len(entries))
}

// Daemon-level lifecycle: registry close releases forwards (no stray
// forward children remain).
func TestRegistryCloseReleasesForwards(t *testing.T) {
	h := newHarness(t)
	h.tk.Script("getPageSource", xmlSteps(4, appXMLFocusedResume)...)
	if _, err := h.client.DescribeUI(context.Background(), &vdbv1.DescribeUIRequest{Udid: serial}); err != nil {
		t.Fatalf("DescribeUI: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.daemon.Registry.Close(ctx)

	entries, _ := filepath.Glob(filepath.Join(os.Getenv("FAKEVDA_STATE_DIR"), "forward-*.pid"))
	if len(entries) != 0 {
		t.Errorf("forward pid files left after registry close: %v", entries)
	}
}
