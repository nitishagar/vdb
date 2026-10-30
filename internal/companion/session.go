// Package companion is the vdb daemon: a target registry owning one session
// per device (cached port-forward + toolkit client), a gRPC service
// implementing vdb.v1.Companion, and lifecycle glue. Conventions:
//   - per-target input serialization (invariant 13): session.inputMu wraps
//     every injection-and-verify loop;
//   - forward re-establishment (invariant 4/12): a dead forward surfaces as
//     a transport error, is reset, re-forwarded once, and the call retried
//     once — a second failure returns to the caller;
//   - every exit path releases the forward (defer + Close).
package companion

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/nitishagar/vdb/internal/toolkit"
	"github.com/nitishagar/vdb/internal/vda"
	"github.com/nitishagar/vdb/internal/vegacli"
)

// EnvVarToolkitPort overrides the on-device automation-toolkit port
// (default 8383, the Amazon-observed value).
const EnvVarToolkitPort = "VDB_TOOLKIT_PORT"

// ToolkitPort resolves the configured toolkit device port.
func ToolkitPort() int {
	if v := os.Getenv(EnvVarToolkitPort); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return 8383
}

// Session owns every resource bound to one target: the cached VDA
// port-forward, the toolkit client riding it, and the input lock.
type Session struct {
	Udid    string
	VDA     vda.VDA
	Vega    vegacli.VegaCLI
	Toolkit int // device port of the automation toolkit

	// inputMu serializes key injection per target (invariant 13): at most
	// one in-flight press loop; held across a Navigate's press-and-verify
	// cycle; always released via defer.
	inputMu sync.Mutex

	// fwdMu guards forward (re-)establishment.
	fwdMu  sync.Mutex
	fwd    vda.ForwardHandle
	client *toolkit.Client

	// shotMu guards lastShotDigest (read/written by concurrent Screenshot
	// RPCs; a plain field would be a data race).
	shotMu         sync.Mutex
	lastShotDigest string
}

// SetLastShot records the last capture digest (thread-safe).
func (s *Session) SetLastShot(d string) {
	s.shotMu.Lock()
	s.lastShotDigest = d
	s.shotMu.Unlock()
}

// LastShot returns the last capture digest (thread-safe).
func (s *Session) LastShot() string {
	s.shotMu.Lock()
	defer s.shotMu.Unlock()
	return s.lastShotDigest
}

// maxSessions bounds the per-host session map: an unbounded map keyed by
// caller-supplied udids would grow without limit.
const maxSessions = 64

// SessionFor fetches-or-creates the session for a target. An empty udid is
// rejected; attachment is verified lazily by the first device call.
func (r *Registry) SessionFor(udid string) (*Session, error) {
	if udid == "" {
		return nil, fmt.Errorf("companion: empty target udid")
	}
	r.mu.RLock()
	s, ok := r.sessions[udid]
	r.mu.RUnlock()
	if ok {
		return s, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[udid]; ok {
		return s, nil
	}
	if len(r.sessions) >= maxSessions {
		return nil, fmt.Errorf("companion: session limit reached (%d targets)", maxSessions)
	}
	s = &Session{Udid: udid, VDA: r.vda, Vega: r.vega, Toolkit: ToolkitPort()}
	r.sessions[udid] = s
	return s, nil
}

// toolkitClient lazily establishes the port-forward and client.
func (s *Session) toolkitClient(ctx context.Context) (*toolkit.Client, error) {
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	fwd, err := s.VDA.Forward(ctx, s.Udid, s.Toolkit)
	if err != nil {
		return nil, fmt.Errorf("companion: forward %s: %w", s.Udid, err)
	}
	s.fwd = fwd
	s.client = toolkit.NewClient(fmt.Sprintf("http://127.0.0.1:%d", fwd.LocalPort()))
	return s.client, nil
}

// resetForward drops the cached forward (called on transport errors).
func (s *Session) resetForward(ctx context.Context) {
	s.fwdMu.Lock()
	defer s.fwdMu.Unlock()
	if s.fwd != nil {
		_ = s.fwd.Release(ctx)
	}
	s.fwd = nil
	s.client = nil
}

// withToolkit runs fn against the toolkit client, re-establishing the
// forward exactly once if fn fails at the transport layer.
func (s *Session) withToolkit(ctx context.Context, fn func(*toolkit.Client) error) error {
	c, err := s.toolkitClient(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if !toolkit.IsTransport(err) {
		return err
	}
	// Forward death: reset, re-forward once, retry the call once.
	s.resetForward(ctx)
	c2, err2 := s.toolkitClient(ctx)
	if err2 != nil {
		return err2
	}
	return fn(c2)
}

// Close releases the session's forward (idempotent).
func (s *Session) Close(ctx context.Context) {
	s.resetForward(ctx)
}

// Registry tracks sessions per target udid.
type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	vda      vda.VDA
	vega     vegacli.VegaCLI
}

// NewRegistry binds a registry to the adapters.
func NewRegistry(v vda.VDA, g vegacli.VegaCLI) *Registry {
	return &Registry{sessions: map[string]*Session{}, vda: v, vega: g}
}

// ListTargets reports live device state from the VDA.
func (r *Registry) ListTargets(ctx context.Context) ([]vda.Device, error) {
	return r.vda.Devices(ctx)
}

// Close releases every session.
func (r *Registry) Close(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for udid, s := range r.sessions {
		s.Close(ctx)
		delete(r.sessions, udid)
	}
}
