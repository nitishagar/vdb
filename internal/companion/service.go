package companion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
	"github.com/nitishagar/vdb/internal/input"
	"github.com/nitishagar/vdb/internal/navigator"
	"github.com/nitishagar/vdb/internal/toolkit"
	"github.com/nitishagar/vdb/internal/tree"
	"github.com/nitishagar/vdb/internal/vda"
)

// Config tunes the service; zero values get production defaults.
type Config struct {
	// MaxScreenshotBytes bounds a capture payload (default 64 MiB). A larger
	// capture errors RESOURCE_EXHAUSTED — never silent truncation
	// (invariant 14).
	MaxScreenshotBytes int64
	// InstallTmpDir hosts streamed .vpkg chunks (default os.TempDir()).
	InstallTmpDir string
	// WaitIdle defaults.
	WaitIdleStableMS int
	WaitIdlePollMS   int
}

func (c *Config) maxScreenshot() int64 {
	if c.MaxScreenshotBytes > 0 {
		return c.MaxScreenshotBytes
	}
	return 64 << 20
}

func (c *Config) waitIdleDefaults() (stable, poll int) {
	stable, poll = c.WaitIdleStableMS, c.WaitIdlePollMS
	if stable <= 0 {
		stable = 500
	}
	if poll <= 0 {
		poll = 200
	}
	return
}

// Service implements vdb.v1.Companion.
type Service struct {
	vdbv1.UnimplementedCompanionServer
	Reg *Registry
	Cfg Config
}

// NewService builds the service.
func NewService(reg *Registry, cfg Config) *Service {
	return &Service{Reg: reg, Cfg: cfg}
}

// --- error mapping ------------------------------------------------------------

// toStatus maps typed adapter errors onto gRPC codes with layer-prefixed
// messages so a miss localizes to its layer.
func toStatus(layer string, err error) error {
	if err == nil {
		return nil
	}
	if isStatusErr(err) {
		return err // already a status error — pass through untouched
	}
	var resolveErr *vda.ResolveError
	switch {
	case errors.As(err, &resolveErr):
		return status.Errorf(codes.FailedPrecondition, "%s: %v", layer, err)
	case toolkit.IsTransport(err):
		return status.Errorf(codes.Unavailable, "%s: toolkit transport: %v", layer, err)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%s: %v", layer, err)
	case errors.Is(err, context.Canceled):
		return status.Errorf(codes.Canceled, "%s: %v", layer, err)
	default:
		return status.Errorf(codes.Internal, "%s: %v", layer, err)
	}
}

func isStatusErr(err error) bool {
	_, ok := status.FromError(err)
	return ok && status.Code(err) != codes.Unknown
}

func sessionFor(reg *Registry, udid string) (*Session, error) {
	s, err := reg.SessionFor(udid)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "companion: %v", err)
	}
	return s, nil
}

// --- targets --------------------------------------------------------------------

func (svc *Service) ListTargets(ctx context.Context, _ *vdbv1.ListTargetsRequest) (*vdbv1.ListTargetsResponse, error) {
	devs, err := svc.Reg.ListTargets(ctx)
	if err != nil {
		return nil, toStatus("vda", err)
	}
	resp := &vdbv1.ListTargetsResponse{}
	for _, d := range devs {
		state := "offline"
		if d.Attached {
			state = "attached"
		}
		kind := "device"
		if strings.HasPrefix(d.Serial, "emulator-") {
			kind = "vvd"
		}
		resp.Targets = append(resp.Targets, &vdbv1.Target{Udid: d.Serial, State: state, Kind: kind})
	}
	return resp, nil
}

func (svc *Service) DescribeTarget(ctx context.Context, req *vdbv1.DescribeTargetRequest) (*vdbv1.DescribeTargetResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	version, err := s.VDA.Version(ctx)
	if err != nil {
		return nil, toStatus("vda", err)
	}
	toolkitOK := true
	if err := s.withToolkit(ctx, func(c *toolkit.Client) error {
		_, err := c.GetWindowRect(ctx)
		return err
	}); err != nil {
		toolkitOK = false
	}
	return &vdbv1.DescribeTargetResponse{
		Target:           &vdbv1.Target{Udid: s.Udid, State: "attached", Kind: "device"},
		VdaVersion:       version,
		ToolkitReachable: toolkitOK,
	}, nil
}

// --- lifecycle --------------------------------------------------------------------

func (svc *Service) Install(stream vdbv1.Companion_InstallServer) error {
	ctx := stream.Context()
	meta, err := stream.Recv()
	if err != nil {
		return toStatus("companion", err)
	}
	m := meta.GetMeta()
	if m == nil {
		return status.Error(codes.InvalidArgument, "companion: install: first message must carry meta")
	}
	s, err := sessionFor(svc.Reg, m.GetUdid())
	if err != nil {
		return err
	}
	name := m.GetFileName()
	if name == "" {
		name = "upload.vpkg"
	}
	tmp, err := os.CreateTemp(svc.Cfg.InstallTmpDir, "vdb-install-*.vpkg")
	if err != nil {
		return toStatus("companion", err)
	}
	tmpName := tmp.Name()
	// The temp file is removed on EVERY exit path (invariant: no leak).
	defer func() { os.Remove(tmpName) }()

	var total int64
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return toStatus("companion", err)
		}
		chunk := part.GetChunk()
		if chunk == nil {
			continue
		}
		n, werr := tmp.Write(chunk)
		total += int64(n)
		if werr != nil {
			return toStatus("companion", werr)
		}
	}
	if err := tmp.Close(); err != nil {
		return toStatus("companion", err)
	}
	if total == 0 {
		return status.Error(codes.InvalidArgument, "companion: install: no payload chunks")
	}

	res, err := s.Vega.Install(ctx, s.Udid, tmpName)
	if err != nil {
		return toStatus("vega", err)
	}
	return stream.SendAndClose(&vdbv1.InstallResponse{Result: string(res), BytesReceived: total})
}

func (svc *Service) Uninstall(ctx context.Context, req *vdbv1.UninstallRequest) (*vdbv1.LifecycleResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	res, err := s.Vega.Uninstall(ctx, s.Udid, req.GetComponentId())
	if err != nil {
		return nil, toStatus("vega", err)
	}
	return &vdbv1.LifecycleResponse{Result: string(res)}, nil
}

func (svc *Service) Launch(ctx context.Context, req *vdbv1.LaunchRequest) (*vdbv1.LaunchResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	if err := s.Vega.Launch(ctx, s.Udid, req.GetComponentId()); err != nil {
		return nil, toStatus("vega", err)
	}
	return &vdbv1.LaunchResponse{}, nil
}

func (svc *Service) Terminate(ctx context.Context, req *vdbv1.TerminateRequest) (*vdbv1.LifecycleResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	res, err := s.Vega.Terminate(ctx, s.Udid, req.GetComponentId())
	if err != nil {
		return nil, toStatus("vega", err)
	}
	return &vdbv1.LifecycleResponse{Result: string(res)}, nil
}

func (svc *Service) ListApps(ctx context.Context, req *vdbv1.ListAppsRequest) (*vdbv1.ListAppsResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	apps, err := s.Vega.ListApps(ctx, s.Udid)
	if err != nil {
		return nil, toStatus("vega", err)
	}
	resp := &vdbv1.ListAppsResponse{}
	for _, a := range apps {
		resp.Apps = append(resp.Apps, &vdbv1.AppInfo{ComponentId: a.ComponentID, Name: a.Name})
	}
	return resp, nil
}

// --- input --------------------------------------------------------------------

func (svc *Service) SendKeys(ctx context.Context, req *vdbv1.SendKeysRequest) (*vdbv1.SendKeysResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	if len(req.GetKeys()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "companion: send-keys: empty key sequence")
	}
	// Validate every key name BEFORE pressing anything (unknown name →
	// error, never code 0; no partial presses).
	for _, k := range req.GetKeys() {
		if _, err := toolkit.KeyCode(k.GetName()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "companion: %v", err)
		}
	}

	// Invariant 13: one in-flight injection loop per target.
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	for _, k := range req.GetKeys() {
		repeat := int(k.GetRepeat())
		if repeat <= 0 {
			repeat = 1
		}
		for i := 0; i < repeat; i++ {
			if err := s.withToolkit(ctx, func(c *toolkit.Client) error {
				return c.InjectKeyEvent(ctx, mustCode(k.GetName()), int(k.GetHoldMs()))
			}); err != nil {
				return nil, toStatus("toolkit", err)
			}
		}
	}

	if req.GetFocusAfter() {
		focus, err := svc.focusOf(ctx, s)
		if err != nil {
			return nil, err
		}
		return &vdbv1.SendKeysResponse{Focus: focus}, nil
	}
	return &vdbv1.SendKeysResponse{}, nil
}

func mustCode(name string) int {
	code, _ := toolkit.KeyCode(name)
	return code
}

// focusOf reads the tree and returns the focus snapshot (empty focused_id
// when nothing claims focus).
func (svc *Service) focusOf(ctx context.Context, s *Session) (*vdbv1.FocusState, error) {
	var focus *vdbv1.FocusState
	err := s.withToolkit(ctx, func(c *toolkit.Client) error {
		xml, err := c.GetPageSource(ctx)
		if err != nil {
			return err
		}
		tr, err := tree.Parse(xml)
		if err != nil {
			return err
		}
		focus = &vdbv1.FocusState{TreeDigest: tr.Hash()}
		if f := tr.Focused(); f != nil {
			focus.FocusedId = describeElement(f)
		}
		return nil
	})
	if err != nil {
		return nil, toStatus("toolkit", err)
	}
	return focus, nil
}

func describeElement(e *tree.Element) string {
	switch {
	case e.TestID != "":
		return "test_id:" + e.TestID
	case e.ID != "":
		return "id:" + e.ID
	default:
		return fmt.Sprintf("%s@%d,%d", e.Role, e.Rect.X, e.Rect.Y)
	}
}

// --- UI --------------------------------------------------------------------

func (svc *Service) DescribeUI(ctx context.Context, req *vdbv1.DescribeUIRequest) (*vdbv1.DescribeUIResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	var resp *vdbv1.DescribeUIResponse
	err = s.withToolkit(ctx, func(c *toolkit.Client) error {
		xml, err := c.GetPageSource(ctx)
		if err != nil {
			return err
		}
		tr, err := tree.Parse(xml)
		if err != nil {
			return err
		}
		appID := req.GetAppId()
		if appID == "" {
			// Foreground resolution: the toolkit's screen context names the
			// front app; fall back to the unfiltered tree on failure.
			if fg, err := c.GetScreenContext(ctx); err == nil && fg != "" {
				appID = fg
			}
		}
		filtered := tr
		if appID != "" {
			filtered = tr.Filter(appID)
		}
		maxNodes := int(req.GetMaxNodes())
		if maxNodes <= 0 {
			maxNodes = 200
		}
		treeJSON, err := filtered.MarshalJSONString()
		if err != nil {
			return err
		}
		sumJSON, err := filtered.Summary(maxNodes).MarshalJSONString()
		if err != nil {
			return err
		}
		resp = &vdbv1.DescribeUIResponse{
			TreeJson:   treeJSON,
			TreeDigest: filtered.Hash(),
			Summary:    sumJSON,
			Warnings:   filtered.Warnings,
		}
		return nil
	})
	if err != nil {
		return nil, toStatus("toolkit", err)
	}
	return resp, nil
}

func (svc *Service) Focused(ctx context.Context, req *vdbv1.FocusedRequest) (*vdbv1.FocusedResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	focus, err := svc.focusOf(ctx, s)
	if err != nil {
		return nil, err
	}
	return &vdbv1.FocusedResponse{Focus: focus}, nil
}

func (svc *Service) Navigate(req *vdbv1.NavigateRequest, stream vdbv1.Companion_NavigateServer) error {
	ctx := stream.Context()
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return err
	}
	sel, err := navigator.ParseSelector(req.GetSelector())
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "companion: %v", err)
	}
	budget := int(req.GetBudget())
	if budget <= 0 {
		budget = 16
	}
	if budget > 64 {
		budget = 64
	}

	// Invariant 13: the whole press-and-verify loop is serialized per
	// target; the lock releases via defer on every exit path.
	s.inputMu.Lock()
	defer s.inputMu.Unlock()

	exec := &navigator.Executor{
		Source: &sessionTreeSource{session: s},
		Keys:   &sessionInjector{session: s},
		Serial: s.Udid,
		Budget: budget,
	}

	res, navErr := exec.Navigate(ctx, sel)
	for _, step := range res.Steps {
		if err := stream.Send(&vdbv1.NavigateResponse{Update: &vdbv1.NavigateResponse_Step{Step: step}}); err != nil {
			return toStatus("companion", err)
		}
	}
	if navErr != nil {
		fail := &vdbv1.NavigateFailure{Reason: navigateReason(navErr), Detail: navErr.Error()}
		if state, ok := navigator.FocusDiagnostics(navErr); ok {
			fail.Focus = &vdbv1.FocusState{FocusedId: state.FocusedID, TreeDigest: state.TreeDigest}
		}
		if err := stream.Send(&vdbv1.NavigateResponse{Update: &vdbv1.NavigateResponse_Failure{Failure: fail}}); err != nil {
			return toStatus("companion", err)
		}
		return nil
	}
	return stream.Send(&vdbv1.NavigateResponse{Update: &vdbv1.NavigateResponse_Reached{
		Reached: &vdbv1.FocusState{FocusedId: res.Focus.FocusedID, TreeDigest: res.Focus.TreeDigest},
	}})
}

// navigateReason maps navigator termination errors onto the wire enum.
func navigateReason(err error) vdbv1.NavigateFailure_Reason {
	switch {
	case errors.Is(err, navigator.ErrBudgetExceeded):
		return vdbv1.NavigateFailure_BUDGET_EXCEEDED
	case errors.Is(err, navigator.ErrNoProgress):
		return vdbv1.NavigateFailure_NO_PROGRESS
	case errors.Is(err, navigator.ErrTrap):
		return vdbv1.NavigateFailure_TRAP
	case errors.Is(err, navigator.ErrTargetNotFound):
		return vdbv1.NavigateFailure_TARGET_NOT_FOUND
	default:
		return vdbv1.NavigateFailure_INTERNAL
	}
}

// sessionTreeSource reads trees through the session's forward.
type sessionTreeSource struct{ session *Session }

func (src *sessionTreeSource) ReadTree(ctx context.Context) (*tree.Tree, error) {
	var tr *tree.Tree
	err := src.session.withToolkit(ctx, func(c *toolkit.Client) error {
		xml, err := c.GetPageSource(ctx)
		if err != nil {
			return err
		}
		tr, err = tree.Parse(xml)
		return err
	})
	return tr, err
}

// sessionInjector injects keys through the session's forward.
type sessionInjector struct{ session *Session }

func (inj *sessionInjector) InjectKey(ctx context.Context, serial, key string, holdMS int) error {
	return inj.session.withToolkit(ctx, func(c *toolkit.Client) error {
		return c.InjectKeyEvent(ctx, mustCode(key), holdMS)
	})
}

// --- screenshot --------------------------------------------------------------------

func (svc *Service) Screenshot(ctx context.Context, req *vdbv1.ScreenshotRequest) (*vdbv1.ScreenshotResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	var shot toolkit.Screenshot
	prevDigest := req.GetPrevDigest()
	if prevDigest == "" {
		prevDigest = s.lastShotDigest // companion's last capture for this target
	}
	capture := func(c *toolkit.Client) error {
		var err error
		if req.GetFresh() && prevDigest != "" {
			shot, err = c.TakeScreenshotFresh(ctx, prevDigest)
		} else {
			shot, err = c.TakeScreenshot(ctx)
		}
		return err
	}
	if err := s.withToolkit(ctx, capture); err != nil {
		return nil, toStatus("toolkit", err)
	}
	if int64(len(shot.PNG)) > svc.Cfg.maxScreenshot() {
		return nil, status.Errorf(codes.ResourceExhausted,
			"toolkit: screenshot %d bytes exceeds the %d-byte limit (no truncation)",
			len(shot.PNG), svc.Cfg.maxScreenshot())
	}
	s.lastShotDigest = shot.Digest
	return &vdbv1.ScreenshotResponse{Png: shot.PNG, Digest: shot.Digest}, nil
}

// --- logs --------------------------------------------------------------------

func (svc *Service) Log(req *vdbv1.LogRequest, stream vdbv1.Companion_LogServer) error {
	ctx := stream.Context()
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return err
	}
	ls, err := s.Vega.StartLogStream(ctx, s.Udid)
	if err != nil {
		return toStatus("vega", err)
	}
	defer ls.Stop()
	for {
		select {
		case <-ctx.Done():
			return toStatus("companion", ctx.Err())
		case line, ok := <-ls.Lines():
			if !ok {
				// Subprocess ended (death or EOF): surface as Unavailable;
				// the companion keeps serving other RPCs.
				return status.Errorf(codes.Unavailable, "vega: log stream ended: %s", ls.Stderr())
			}
			if err := stream.Send(&vdbv1.LogResponse{Line: []byte(line)}); err != nil {
				return toStatus("companion", err)
			}
		}
	}
}

// --- files & shell --------------------------------------------------------------------

func (svc *Service) Push(ctx context.Context, req *vdbv1.PushRequest) (*vdbv1.PushResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	if err := s.VDA.Push(ctx, s.Udid, req.GetLocalPath(), req.GetRemotePath()); err != nil {
		return nil, toStatus("vda", err)
	}
	return &vdbv1.PushResponse{}, nil
}

func (svc *Service) Pull(ctx context.Context, req *vdbv1.PullRequest) (*vdbv1.PullResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	if err := s.VDA.Pull(ctx, s.Udid, req.GetRemotePath(), req.GetLocalPath()); err != nil {
		return nil, toStatus("vda", err)
	}
	return &vdbv1.PullResponse{}, nil
}

func (svc *Service) Shell(ctx context.Context, req *vdbv1.ShellRequest) (*vdbv1.ShellResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	out, err := s.VDA.Shell(ctx, s.Udid, req.GetCommand())
	if err != nil {
		return nil, toStatus("vda", err)
	}
	return &vdbv1.ShellResponse{Stdout: []byte(out)}, nil
}

// --- wait-idle --------------------------------------------------------------------

func (svc *Service) WaitIdle(ctx context.Context, req *vdbv1.WaitIdleRequest) (*vdbv1.WaitIdleResponse, error) {
	s, err := sessionFor(svc.Reg, req.GetUdid())
	if err != nil {
		return nil, err
	}
	defStable, defPoll := svc.Cfg.waitIdleDefaults()
	stableMS := int(req.GetStableMs())
	if stableMS <= 0 {
		stableMS = defStable
	}
	pollMS := int(req.GetPollMs())
	if pollMS <= 0 {
		pollMS = defPoll
	}
	timeoutMS := int(req.GetTimeoutMs())
	if timeoutMS <= 0 {
		timeoutMS = 10000
	}
	deadline := time.Now().Add(time.Duration(timeoutMS) * time.Millisecond)
	stableSince := time.Time{}
	var lastDigest string
	first := true

	for {
		if err := ctx.Err(); err != nil {
			return nil, toStatus("companion", err)
		}
		digest, err := svc.digestOf(ctx, s)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		if !first && digest == lastDigest {
			if stableSince.IsZero() {
				stableSince = now
			}
			if now.Sub(stableSince) >= time.Duration(stableMS)*time.Millisecond {
				return &vdbv1.WaitIdleResponse{Stable: true, TreeDigest: digest}, nil
			}
		} else {
			stableSince = time.Time{}
		}
		first = false
		lastDigest = digest

		if now.Add(time.Duration(pollMS) * time.Millisecond).After(deadline) {
			return &vdbv1.WaitIdleResponse{Stable: false, TreeDigest: lastDigest}, nil
		}
		select {
		case <-ctx.Done():
			return nil, toStatus("companion", ctx.Err())
		case <-time.After(time.Duration(pollMS) * time.Millisecond):
		}
	}
}

func (svc *Service) digestOf(ctx context.Context, s *Session) (string, error) {
	var digest string
	err := s.withToolkit(ctx, func(c *toolkit.Client) error {
		xml, err := c.GetPageSource(ctx)
		if err != nil {
			return err
		}
		tr, err := tree.Parse(xml)
		if err != nil {
			return err
		}
		digest = tr.Hash()
		return nil
	})
	if err != nil {
		return "", toStatus("toolkit", err)
	}
	return digest, nil
}

var _ input.Backend = (*sessionInjector)(nil)
