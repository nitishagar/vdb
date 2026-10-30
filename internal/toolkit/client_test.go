package toolkit

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
)

func TestGetPageSource(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("getPageSource", faketoolkit.XMLStep("<root/>"))
	c := NewClient(srv.URL())
	xml, err := c.GetPageSource(context.Background())
	if err != nil {
		t.Fatalf("GetPageSource: %v", err)
	}
	if xml != "<root/>" {
		t.Errorf("xml = %q", xml)
	}
}

func TestKeyCodesExact(t *testing.T) {
	cases := map[string]int{
		"UP": 103, "DOWN": 108, "LEFT": 105, "RIGHT": 106,
		"ENTER": 96, "BACK": 158, "HOME": 170,
	}
	for name, code := range cases {
		got, err := KeyCode(name)
		if err != nil {
			t.Fatalf("KeyCode(%s): %v", name, err)
		}
		if got != code {
			t.Errorf("KeyCode(%s) = %d, want %d", name, got, code)
		}
	}
	if _, err := KeyCode("NOT_A_KEY"); err == nil {
		t.Error("unknown key must error, never code 0")
	}
}

func TestInjectKeyEventSendsParams(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("injectInputKeyEvent", faketoolkit.ObjectStep(nil))
	c := NewClient(srv.URL())
	if err := c.InjectKeyEvent(context.Background(), 103, 500); err != nil {
		t.Fatalf("InjectKeyEvent: %v", err)
	}
	calls := srv.Calls()
	if len(calls) != 1 || calls[0].Method != "injectInputKeyEvent" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(string(calls[0].Params), `"keyCode":103`) ||
		!strings.Contains(string(calls[0].Params), `"holdDuration":500`) {
		t.Errorf("params = %s", calls[0].Params)
	}
}

func TestGetWindowRectAndScreenContext(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("getWindowRect", faketoolkit.ObjectStep(map[string]any{"x": 0, "y": 0, "width": 1920, "height": 1080}))
	srv.Script("getScreenContext", faketoolkit.ObjectStep(map[string]any{"appId": "com.example.app"}))
	c := NewClient(srv.URL())
	rect, err := c.GetWindowRect(context.Background())
	if err != nil || rect.Width != 1920 || rect.Height != 1080 {
		t.Fatalf("rect = %+v err = %v", rect, err)
	}
	app, err := c.GetScreenContext(context.Background())
	if err != nil || app != "com.example.app" {
		t.Fatalf("app = %q err = %v", app, err)
	}
}

// IHDR with zero dimensions is invalid (final-review nit coverage).
func TestScreenshotZeroDimsInvalid(t *testing.T) {
	zero := append([]byte(nil), faketoolkit.ValidPNG...)
	for i := 16; i < 24; i++ {
		zero[i] = 0
	}
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot",
		faketoolkit.PNGStep(zero),
		faketoolkit.PNGStep(faketoolkit.ValidPNG),
	)
	c := NewClient(srv.URL())
	if _, err := c.TakeScreenshot(context.Background()); err != nil {
		t.Fatalf("zero-dims must retry to success: %v", err)
	}
	if n := srv.CallsFor("takeScreenshot"); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestRPCErrorTyped(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("getPageSource", faketoolkit.ErrorStep(-32601, "method not found"))
	c := NewClient(srv.URL())
	_, err := c.GetPageSource(context.Background())
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("want RPCError, got %T %v", err, err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("code = %d", rpcErr.Code)
	}
}

// Transport death mid-call: typed TransportError returned promptly
// (invariant: forward-death surfaces as transport, and the S1 riskiest-edge
// check — a dead endpoint errors in < 5 s, never hangs).
func TestTransportDeathFast(t *testing.T) {
	// Reserve a port then close its listener: connections refuse instantly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	_ = ln.Close()
	c := NewClient(url)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = c.GetPageSource(ctx)
	if err == nil {
		t.Fatal("want error")
	}
	if !IsTransport(err) {
		t.Fatalf("want TransportError, got %T %v", err, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("transport death took %v", time.Since(start))
	}
}

func TestScreenshotValid(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot", faketoolkit.PNGStep(faketoolkit.ValidPNG))
	c := NewClient(srv.URL())
	shot, err := c.TakeScreenshot(context.Background())
	if err != nil {
		t.Fatalf("TakeScreenshot: %v", err)
	}
	if len(shot.PNG) == 0 || shot.Digest == "" {
		t.Errorf("shot = %+v", shot)
	}
}

func TestScreenshotZeroByteTypedRetried(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot",
		faketoolkit.PNGStep(nil), // zero-byte
		faketoolkit.PNGStep(faketoolkit.ValidPNG),
	)
	c := NewClient(srv.URL())
	shot, err := c.TakeScreenshot(context.Background())
	if err != nil {
		t.Fatalf("zero-byte must retry to success: %v", err)
	}
	if len(shot.PNG) == 0 {
		t.Error("retried capture is empty")
	}
	if n := srv.CallsFor("takeScreenshot"); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestScreenshotZeroByteExhaustsTyped(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot", faketoolkit.PNGStep(nil), faketoolkit.PNGStep(nil), faketoolkit.PNGStep(nil))
	c := NewClient(srv.URL())
	_, err := c.TakeScreenshot(context.Background())
	if !errors.Is(err, ErrZeroByte) {
		t.Fatalf("want ErrZeroByte, got %v", err)
	}
	if n := srv.CallsFor("takeScreenshot"); n != maxRetries+1 {
		t.Errorf("attempts = %d, want %d", n, maxRetries+1)
	}
}

func TestScreenshotNullBufferTyped(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot",
		faketoolkit.NullBufferStep(),
		faketoolkit.PNGStep(faketoolkit.ValidPNG),
	)
	c := NewClient(srv.URL())
	if _, err := c.TakeScreenshot(context.Background()); err != nil {
		t.Fatalf("null-buffer must retry to success: %v", err)
	}
	if n := srv.CallsFor("takeScreenshot"); n != 2 {
		t.Errorf("attempts = %d", n)
	}
}

func TestScreenshotInvalidPNGTyped(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot",
		faketoolkit.PNGStep([]byte("not a png at all")),
		faketoolkit.PNGStep(faketoolkit.ValidPNG),
	)
	c := NewClient(srv.URL())
	if _, err := c.TakeScreenshot(context.Background()); err != nil {
		t.Fatalf("invalid PNG must retry to success: %v", err)
	}
}

// Stale-then-fresh: TakeScreenshotFresh errors ErrStaleFrame on identical
// bytes and retries until bytes differ (bounded ≤ maxRetries).
func TestScreenshotStaleThenFresh(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot",
		faketoolkit.PNGStep(faketoolkit.ValidPNG), // stale: same as prev digest
		faketoolkit.PNGStep(faketoolkit.ValidPNG), // still stale
		faketoolkit.PNGStep(faketoolkit.PNGWithByte(1, 0xAB)),
	)
	c := NewClient(srv.URL())
	prev := digest(t, c)
	shot, err := c.TakeScreenshotFresh(context.Background(), prev)
	if err != nil {
		t.Fatalf("stale frames must retry to fresh: %v", err)
	}
	if shot.Digest == prev {
		t.Error("returned digest equals previous — stale frame returned as fresh")
	}
	// 1 baseline + 2 fresh attempts (1 stale + 1 fresh).
	if n := srv.CallsFor("takeScreenshot"); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

func TestScreenshotStaleExhaustsTyped(t *testing.T) {
	srv := faketoolkit.New(t)
	srv.Script("takeScreenshot", faketoolkit.PNGStep(faketoolkit.ValidPNG), faketoolkit.PNGStep(faketoolkit.ValidPNG), faketoolkit.PNGStep(faketoolkit.ValidPNG), faketoolkit.PNGStep(faketoolkit.ValidPNG))
	c := NewClient(srv.URL())
	prev := digest(t, c)
	_, err := c.TakeScreenshotFresh(context.Background(), prev)
	if !errors.Is(err, ErrStaleFrame) {
		t.Fatalf("want ErrStaleFrame, got %v", err)
	}
	// 1 baseline + 3 exhausted attempts (maxRetries=2 → 3 total).
	if n := srv.CallsFor("takeScreenshot"); n != 1+maxRetries+1 {
		t.Errorf("attempts = %d, want %d", n, maxRetries+2)
	}
}

func digest(t *testing.T, c *Client) string {
	t.Helper()
	srvDigest, err := c.TakeScreenshot(context.Background())
	if err != nil {
		t.Fatalf("baseline capture: %v", err)
	}
	return srvDigest.Digest
}
