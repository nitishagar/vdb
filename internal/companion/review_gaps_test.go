package companion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/testkit"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
)

// Concurrent SendKeys AND Navigate on one target serialize (spec concurrency
// edge + plan S5 criterion): the fake toolkit's call log must never show
// more presses between two reads than a single caller issues.
func TestSendKeysAndNavigateSerialize(t *testing.T) {
	h := newHarness(t)
	injects := make([]faketoolkit.Step, 32)
	for i := range injects {
		injects[i] = faketoolkit.ObjectStep(nil)
	}
	h.tk.Script("injectInputKeyEvent", injects...)
	pages := make([]faketoolkit.Step, 64)
	for i := range pages {
		pages[i] = faketoolkit.XMLStep(appXMLFocusedEpisodes)
	}
	h.tk.Script("getPageSource", pages...)

	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.client.SendKeys(context.Background(), &vdbv1.SendKeysRequest{
				Udid: serial, Keys: []*vdbv1.KeySpec{{Name: "DOWN"}, {Name: "DOWN"}}, FocusAfter: true,
			})
			if err != nil {
				t.Errorf("SendKeys: %v", err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		stream, err := h.client.Navigate(context.Background(), &vdbv1.NavigateRequest{Udid: serial, Selector: "test_id=episodes"})
		if err != nil {
			t.Errorf("Navigate: %v", err)
			return
		}
		for {
			resp, err := stream.Recv()
			if err != nil {
				t.Errorf("navigate stream: %v", err)
				return
			}
			if resp.GetReached() != nil || resp.GetFailure() != nil {
				return
			}
		}
	}()
	wg.Wait()

	// Serialization model: SendKeys issues 2 presses then 1 read; Navigate
	// issues 1 press then 1 read per step — all under the target input
	// lock. Interleaving would produce >2 presses between two reads.
	calls := h.tk.Calls()
	injectsSinceRead := 0
	for _, c := range calls {
		switch c.Method {
		case "injectInputKeyEvent":
			injectsSinceRead++
			if injectsSinceRead > 2 {
				t.Fatalf("presses interleaved across callers: %s", callSeq(calls))
			}
		case "getPageSource":
			injectsSinceRead = 0
		}
	}
}

// Client cancellation of a live Log stream: the RPC ends Canceled and the
// stream's subprocess is reaped (spec concurrency edge #5, first half).
func TestLogStreamClientCancel(t *testing.T) {
	h := newHarness(t)
	t.Setenv("VEGA_MODE", "logs") // endless lines
	h.tk.Script("getPageSource", xmlSteps(4, appXMLFocusedResume)...)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.client.Log(ctx, &vdbv1.LogRequest{Udid: serial})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	got := 0
	for {
		_, err := stream.Recv()
		if err != nil {
			if status.Code(err) != codes.Canceled {
				t.Fatalf("cancel code = %s (%v), want Canceled", status.Code(err), err)
			}
			break
		}
		got++
		if got >= 3 {
			cancel()
		}
		if got > 100 {
			t.Fatal("stream did not end after cancel")
		}
	}
	cancel()
}

// Daemon lifecycle: Serve → RPC → cancel → Serve returns (graceful path
// driven, no leaks) (spec concurrency edge #5, second half).
func TestDaemonServeShutdownJoins(t *testing.T) {
	// Leak check registered FIRST so it runs LAST (after the fake toolkit's
	// own cleanup closes its server goroutine).
	t.Cleanup(func() {
		time.Sleep(100 * time.Millisecond)
		goleak.VerifyNone(t,
			goleak.IgnoreTopFunction("google.golang.org/grpc/internal/resolver/passthrough.(*passthroughBuilder).Build.func1"),
			goleak.IgnoreTopFunction("google.golang.org/grpc/internal/transport.(*http2Client).keepalive"),
		)
	})

	tk := faketoolkit.New(t)
	vdaBin, _ := testkit.FakeVDAEnv(t, serial)
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", strings.TrimPrefix(tk.URL(), "http://"))
	vegaBin := filepath.Join(t.TempDir(), "vega")
	if err := os.WriteFile(vegaBin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VDB_VDA_PATH", vdaBin)
	t.Setenv("VDB_VEGA_PATH", vegaBin)

	d, err := NewDaemon("127.0.0.1:0", Config{})
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- d.Serve(ctx) }()

	conn, err := grpc.NewClient(d.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	client := vdbv1.NewCompanionClient(conn)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx2, c2 := context.WithTimeout(context.Background(), time.Second)
		_, err = client.ListTargets(ctx2, &vdbv1.ListTargetsRequest{})
		c2()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("daemon never served: %v", err)
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	// Belt-and-braces: ensure the listener fd is closed even if grpc's
	// stop path raced, then give the runtime a beat before the leak check.
	_ = d.listener.Close()
	time.Sleep(200 * time.Millisecond)
	// No forward children survive shutdown.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pids, _ := filepath.Glob(filepath.Join(os.Getenv("FAKEVDA_STATE_DIR"), "forward-*.pid"))
		if len(pids) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("forward children left after daemon shutdown")
}

// Zero-payload install (meta then EOF, no chunks) is InvalidArgument.
func TestInstallZeroPayloadRejected(t *testing.T) {
	h := newHarness(t)
	stream, err := h.client.Install(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Meta{
		Meta: &vdbv1.InstallMeta{Udid: serial, FileName: "empty.vpkg"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// Empty udid is rejected before any session work.
func TestSessionForEmptyUdid(t *testing.T) {
	h := newHarness(t)
	_, err := sessionFor(h.daemon.Registry, "")
	if !statusExists(err, codes.InvalidArgument) {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// Non-loopback bind is refused without explicit opt-in (trust boundary).
func TestNonLoopbackBindRefused(t *testing.T) {
	_, err := NewDaemon("0.0.0.0:0", Config{})
	if err == nil || !strings.Contains(err.Error(), "VDB_ALLOW_REMOTE") {
		t.Fatalf("want refusal naming VDB_ALLOW_REMOTE, got %v", err)
	}
	t.Setenv("VDB_ALLOW_REMOTE", "1")
	defer t.Setenv("VDB_ALLOW_REMOTE", "")
	d, err := NewDaemon("0.0.0.0:0", Config{})
	if err == nil {
		// Bind succeeded under the opt-in; close it.
		d.Server.Stop()
		return
	}
	if strings.Contains(err.Error(), "refusing") {
		t.Fatalf("opt-in should allow the bind, got %v", err)
	}
	// Adapter resolution failures are fine in this environment only if the
	// bind check passed — accept missing-SDK errors.
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Session map is bounded.
func TestSessionMapBounded(t *testing.T) {
	h := newHarness(t)
	reg := h.daemon.Registry
	for i := 0; i < maxSessions; i++ {
		if _, err := reg.SessionFor(fakeUdid(i)); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}
	if _, err := reg.SessionFor(fakeUdid(maxSessions)); err == nil {
		t.Fatal("session limit must be enforced")
	}
}

func fakeUdid(i int) string {
	return "AFTV" + strings.Repeat("X", 4) + string(rune('0'+i%10)) + string(rune('a'+i/10))
}

func statusExists(err error, code codes.Code) bool {
	return err != nil && status.Code(err) == code
}
