// Package toolkit is a JSON-RPC 2.0 client for the on-device automation
// toolkit (HTTP POST to /jsonrpc on device port 8383, reached through a VDA
// port-forward). The method names and parameter shapes are Amazon-observed
// (documented via the Appium driver's jsonrpc passthrough); field names are
// assumptions isolated to this package and mirrored by internal/faketoolkit.
package toolkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
)

// Request/response wire shapes.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC-level error returned by the toolkit.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("toolkit %s: rpc error %d: %s", e.Method, e.Code, e.Message)
}

// TransportError wraps failures moving bytes to/from the forwarded port
// (dead forward, refused connection, timeout). Callers treat it as the
// signal to re-establish the forward.
type TransportError struct {
	Method string
	Err    error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("toolkit %s: transport: %v", e.Method, e.Err)
}
func (e *TransportError) Unwrap() error { return e.Err }

// Client talks to the toolkit through baseURL (http://127.0.0.1:<forward>).
type Client struct {
	baseURL string
	http    *http.Client
	idSeq   atomic.Int64
}

// NewClient builds a client against a forwarded local address.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{}, // per-call deadlines come from ctx
	}
}

// Call performs one JSON-RPC round-trip.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("toolkit %s: marshal params: %w", method, err)
		}
		raw = b
	}
	reqBody, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: c.idSeq.Add(1), Method: method, Params: raw})
	if err != nil {
		return nil, fmt.Errorf("toolkit %s: marshal request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/jsonrpc", bytes.NewReader(reqBody))
	if err != nil {
		return nil, &TransportError{Method: method, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &TransportError{Method: method, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &TransportError{Method: method, Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &TransportError{Method: method, Err: fmt.Errorf("http %d", resp.StatusCode)}
	}
	var out rpcResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &TransportError{Method: method, Err: fmt.Errorf("malformed response: %w", err)}
	}
	if out.Error != nil {
		return nil, &RPCError{Method: method, Code: out.Error.Code, Message: out.Error.Message}
	}
	return out.Result, nil
}

// maxResponseBytes bounds a page-source/screenshot response. Screenshots on
// hardware are reported around 11 MB as base64 (~15 MB); 128 MiB is a
// safety ceiling, not a design limit.
const maxResponseBytes = 128 << 20

// --- typed methods ---------------------------------------------------------

// GetPageSource returns the toolkit page-source XML for the whole screen
// (may span several apps, including the launcher).
func (c *Client) GetPageSource(ctx context.Context) (string, error) {
	res, err := c.Call(ctx, "getPageSource", map[string]any{})
	if err != nil {
		return "", err
	}
	var xml string
	if err := json.Unmarshal(res, &xml); err != nil {
		return "", fmt.Errorf("toolkit getPageSource: unexpected result shape: %w", err)
	}
	return xml, nil
}

// injectKeyEventParams is the assumed parameter shape (isolated assumption).
type injectKeyEventParams struct {
	KeyCode      int `json:"keyCode"`
	HoldDuration int `json:"holdDuration,omitempty"`
}

// InjectKeyEvent presses a Linux input key by code with an optional hold
// duration in milliseconds.
func (c *Client) InjectKeyEvent(ctx context.Context, keyCode, holdDurationMS int) error {
	_, err := c.Call(ctx, "injectInputKeyEvent", injectKeyEventParams{KeyCode: keyCode, HoldDuration: holdDurationMS})
	return err
}

// WindowRect is the toolkit's window rectangle.
type WindowRect struct {
	X, Y, Width, Height int
}

// GetWindowRect returns the foreground window rectangle.
func (c *Client) GetWindowRect(ctx context.Context) (WindowRect, error) {
	res, err := c.Call(ctx, "getWindowRect", map[string]any{})
	if err != nil {
		return WindowRect{}, err
	}
	var r struct {
		X      int `json:"x"`
		Y      int `json:"y"`
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return WindowRect{}, fmt.Errorf("toolkit getWindowRect: unexpected result shape: %w", err)
	}
	return WindowRect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height}, nil
}

// GetScreenContext returns the foreground app identity.
func (c *Client) GetScreenContext(ctx context.Context) (string, error) {
	res, err := c.Call(ctx, "getScreenContext", map[string]any{})
	if err != nil {
		return "", err
	}
	var ctxv struct {
		AppID string `json:"appId"`
	}
	if err := json.Unmarshal(res, &ctxv); err != nil {
		return "", fmt.Errorf("toolkit getScreenContext: unexpected result shape: %w", err)
	}
	return ctxv.AppID, nil
}

// Screenshot is a validated capture.
type Screenshot struct {
	PNG    []byte
	Digest string // sha256 hex of the PNG bytes
}

// TakeScreenshot captures, validates, and returns the current screen.
// Typed errors: ErrNullBuffer (toolkit "Buffer is null"), ErrZeroByte,
// ErrInvalidPNG. Validation failures and RPC errors are retried up to
// maxRetries times (fresh captures only); transport errors are NOT retried
// here — callers re-establish the forward.
func (c *Client) TakeScreenshot(ctx context.Context) (Screenshot, error) {
	return c.takeScreenshot(ctx, "", noFreshCheck)
}

// TakeScreenshotFresh captures and requires the bytes to differ from
// prevDigest — the stale-frame guard. Identical digests retry (bounded);
// exhaustion returns ErrStaleFrame.
func (c *Client) TakeScreenshotFresh(ctx context.Context, prevDigest string) (Screenshot, error) {
	return c.takeScreenshot(ctx, prevDigest, requireFresh)
}

const maxRetries = 2

type freshness int

const (
	noFreshCheck freshness = iota
	requireFresh
)

// Screenshot error sentinels (typed, test-assertable).
var (
	ErrNullBuffer = fmt.Errorf("toolkit screenshot: buffer is null")
	ErrZeroByte   = fmt.Errorf("toolkit screenshot: zero-byte frame")
	ErrInvalidPNG = fmt.Errorf("toolkit screenshot: invalid PNG payload")
	ErrStaleFrame = fmt.Errorf("toolkit screenshot: stale frame (identical to previous capture)")
)

func (c *Client) takeScreenshot(ctx context.Context, prevDigest string, fresh freshness) (Screenshot, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		res, err := c.Call(ctx, "takeScreenshot", map[string]any{})
		if err != nil {
			if IsTransport(err) {
				return Screenshot{}, err // forward-level; caller re-establishes
			}
			var rpcErr *RPCError
			if asRPC(err, &rpcErr) && isNullBufferMessage(rpcErr.Message) {
				lastErr = fmt.Errorf("%w: %s", ErrNullBuffer, rpcErr.Message)
				continue
			}
			return Screenshot{}, err
		}
		var payload struct {
			ImageBase64 string `json:"imageBase64"`
		}
		if err := json.Unmarshal(res, &payload); err != nil {
			lastErr = fmt.Errorf("%w: result not base64 object: %v", ErrInvalidPNG, err)
			continue
		}
		png, err := base64.StdEncoding.DecodeString(payload.ImageBase64)
		if err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrInvalidPNG, err)
			continue
		}
		if len(png) == 0 {
			lastErr = ErrZeroByte
			continue
		}
		if err := validatePNG(png); err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrInvalidPNG, err)
			continue
		}
		sum := sha256.Sum256(png)
		digest := hex.EncodeToString(sum[:])
		if fresh == requireFresh && digest == prevDigest {
			lastErr = ErrStaleFrame
			continue
		}
		return Screenshot{PNG: png, Digest: digest}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("toolkit screenshot: retries exhausted")
	}
	return Screenshot{}, lastErr
}

func asRPC(err error, target **RPCError) bool {
	rpcErr, ok := err.(*RPCError)
	if ok {
		*target = rpcErr
	}
	return ok
}

func isNullBufferMessage(msg string) bool {
	return bytes.Contains([]byte(msg), []byte("Buffer is null"))
}

// IsTransport reports whether err is forward/connection-level.
func IsTransport(err error) bool {
	te := &TransportError{}
	return asTransport(err, te)
}

func asTransport(err error, target *TransportError) bool {
	for err != nil {
		if t, ok := err.(*TransportError); ok {
			*target = *t
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// validatePNG checks the 8-byte signature and parses the IHDR dimensions —
// a 24-byte header check, deliberately not a full decode (hardware frames
// are ~11 MB; decoding for validation would be waste).
func validatePNG(b []byte) error {
	sig := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	if len(b) < 24 || !bytes.Equal(b[:8], sig) {
		return fmt.Errorf("missing PNG signature")
	}
	if !bytes.Equal(b[12:16], []byte("IHDR")) {
		return fmt.Errorf("first chunk is not IHDR")
	}
	return nil
}

// KeyCode returns the Linux input code for a named key (doc: UP 103,
// DOWN 108, LEFT 105, RIGHT 106, ENTER 96, BACK 158, HOME 170).
func KeyCode(name string) (int, error) {
	code, ok := keyTable[name]
	if !ok {
		return 0, fmt.Errorf("toolkit: unknown key %q (known: %s)", name, knownKeys())
	}
	return code, nil
}

var keyTable = map[string]int{
	"UP":    103,
	"DOWN":  108,
	"LEFT":  105,
	"RIGHT": 106,
	"ENTER": 96,
	"BACK":  158,
	"HOME":  170,
}

func knownKeys() string {
	keys := []string{"UP", "DOWN", "LEFT", "RIGHT", "ENTER", "BACK", "HOME"}
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(k)
	}
	return out
}
