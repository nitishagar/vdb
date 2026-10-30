// Package faketoolkit is an in-process fake of the on-device automation
// toolkit: a JSON-RPC 2.0 server on an ephemeral port with scripted,
// sequenced per-method responses — including the observed quirks (null
// buffer, zero-byte and stale screenshots, multi-app trees).
//
// Test integrity rule (research advisor finding #7): the fake's focus
// behavior is SCRIPTED from fixtures. It must never implement a spatial
// heuristic mirroring the navigator's, or navigator tests would pass by
// construction.
package faketoolkit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"testing"
)

// Step is one scripted response for a method call. Exactly one field set.
type Step struct {
	// XML returns the string as the JSON-RPC result (getPageSource).
	XML string
	// Base64PNG returns {"imageBase64": …} (takeScreenshot).
	Base64PNG string
	// RPCError returns a JSON-RPC error object.
	RPCError *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	// Object returns an arbitrary JSON object result.
	Object map[string]any
	// String returns a bare JSON string result.
	String *string
}

// Call is a recorded invocation.
type Call struct {
	Method string
	Params json.RawMessage
}

// Server is the fake toolkit.
type Server struct {
	t        testing.TB
	mu       sync.Mutex
	script   map[string][]Step
	pos      map[string]int
	calls    []Call
	listener net.Listener
	srv      *http.Server
	url      string
}

// New starts a fake toolkit on an ephemeral port. It fails the test on
// startup error and auto-shuts-down via t.Cleanup.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		t:      t,
		script: map[string][]Step{},
		pos:    map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/jsonrpc", s.handle)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("faketoolkit: listen: %v", err)
	}
	s.listener = ln
	s.url = fmt.Sprintf("http://%s", ln.Addr().String())
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(s.Close)
	return s
}

// URL is the base URL to point a toolkit.Client at.
func (s *Server) URL() string { return s.url }

// Script installs a response sequence for a method. When the sequence is
// exhausted, the last step repeats — a deliberate foot-gun avoided: the
// server instead returns an explicit "script exhausted" error so tests
// notice undersized scripts.
func (s *Server) Script(method string, steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[method] = steps
	s.pos[method] = 0
}

// Calls returns the recorded call log (method + raw params), in order.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}

// CallsFor returns the invocation count of a method.
func (s *Server) CallsFor(method string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Close stops the server immediately (also wired via t.Cleanup).
func (s *Server) Close() {
	shutdownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.srv.Shutdown(shutdownCtx)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: req.Method, Params: req.Params})
	pos := s.pos[req.Method]
	steps := s.script[req.Method]
	if pos >= len(steps) {
		s.mu.Unlock()
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32000, "message": fmt.Sprintf("faketoolkit: script for %q exhausted at call %d", req.Method, pos+1)},
		})
		return
	}
	step := steps[pos]
	s.pos[req.Method] = pos + 1
	s.mu.Unlock()

	switch {
	case step.RPCError != nil:
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": step.RPCError.Code, "message": step.RPCError.Message},
		})
	case step.XML != "":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": step.XML})
	case step.Base64PNG != "":
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"imageBase64": step.Base64PNG}})
	case step.String != nil:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": *step.String})
	case step.Object != nil:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": step.Object})
	default:
		writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- step constructors ------------------------------------------------------

// XMLStep scripts a string result (page source).
func XMLStep(xml string) Step { return Step{XML: xml} }

// StringStep scripts a bare string result.
func StringStep(v string) Step { return Step{String: &v} }

// PNGStep scripts a screenshot result from raw PNG bytes.
func PNGStep(png []byte) Step { return Step{Base64PNG: base64.StdEncoding.EncodeToString(png)} }

// ErrorStep scripts a JSON-RPC error.
func ErrorStep(code int, msg string) Step {
	return Step{RPCError: &struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: msg}}
}

// NullBufferStep scripts the observed "Buffer is null" toolkit fault.
func NullBufferStep() Step { return ErrorStep(-32000, "takeScreenshot: Buffer is null") }

// ObjectStep scripts an object result.
func ObjectStep(m map[string]any) Step { return Step{Object: m} }

// ValidPNG is a minimal 1×1 valid PNG (signature + IHDR + IEND).
var ValidPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', // signature
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R', // IHDR length + type
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, // 1x1
	0x08, 0x06, 0x00, 0x00, 0x00, // bit depth, color type, ...
	0x1f, 0x15, 0xc4, 0x89, // CRC (not validated by our header check)
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// PNGWithByte returns a copy of ValidPNG with byte index i of the IDAT-ish
// body varied, producing a different digest while remaining structurally a
// PNG for header validation. For stale-vs-fresh scripting.
func PNGWithByte(i, v byte) []byte {
	b := make([]byte, len(ValidPNG))
	copy(b, ValidPNG)
	idx := 33 + int(i)%(len(ValidPNG)-34) // vary inside IEND-adjacent payload
	b[idx] = v
	return b
}

// KnownMethods lists methods that have scripts installed (diagnostics).
func (s *Server) KnownMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for m := range s.script {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
