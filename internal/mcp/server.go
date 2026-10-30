// Package mcp implements a Model Context Protocol server (JSON-RPC 2.0 over
// stdio, hand-rolled per the plan's smallest-dependency decision) exposing
// ~10 coarse tools over the vdb gRPC API. Tree outputs are compact, bounded
// summaries — never raw XML (invariant 17).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
)

// ProtocolVersion is the MCP protocol revision this server speaks.
const ProtocolVersion = "2025-06-18"

// CompanionClient is the surface the tools drive (the generated client
// satisfies it).
type CompanionClient = vdbv1.CompanionClient

// Server serves MCP on the given streams.
type Server struct {
	Client CompanionClient
	In     io.Reader
	Out    io.Writer
}

// toolDef is one tool advertisement.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// jsonrpc wire shapes.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads newline-delimited JSON-RPC until EOF. It returns nil on a
// clean shutdown (input closed) or the first malformed-line error.
func (s *Server) Serve(ctx context.Context) error {
	sc := bufio.NewScanner(s.In)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			return fmt.Errorf("mcp: malformed request line: %w", err)
		}
		if len(msg.ID) == 0 {
			continue // notification (e.g. notifications/initialized)
		}
		result, err := s.dispatch(ctx, msg)
		resp := &rpcResponse{JSONRPC: "2.0", ID: msg.ID}
		if err != nil {
			resp.Error = &rpcError{Code: -32603, Message: err.Error()}
		} else {
			resp.Result = result
		}
		out, _ := json.Marshal(resp)
		if _, err := fmt.Fprintln(s.Out, string(out)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, msg rpcMsg) (any, error) {
	switch msg.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vdb", "version": "0.1.0"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, fmt.Errorf("mcp: bad tools/call params: %w", err)
		}
		return s.callTool(ctx, params.Name, params.Arguments)
	default:
		return nil, fmt.Errorf("mcp: unknown method %q", msg.Method)
	}
}

// ToolNames lists the advertised tool names (parity-test surface).
func ToolNames() []string {
	defs := tools()
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

func tools() []toolDef {
	schema := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		}
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	num := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	return []toolDef{
		{Name: "list_devices", Description: "List attached Vega targets (devices and virtual devices).", InputSchema: schema(nil)},
		{Name: "install", Description: "Install a .vpkg package on a target.", InputSchema: schema(map[string]any{"udid": str("target serial"), "path": str("host path to the .vpkg")}, "path")},
		{Name: "launch_app", Description: "Launch an app by component id.", InputSchema: schema(map[string]any{"udid": str("target serial"), "component_id": str("app component id")}, "component_id")},
		{Name: "press_keys", Description: "Press D-pad keys (UP/DOWN/LEFT/RIGHT/ENTER/BACK/HOME) and return the post-action focus.", InputSchema: schema(map[string]any{"udid": str("target serial"), "keys": map[string]any{"type": "array", "items": str("key name"), "description": "keys in order"}, "repeat": num("repeat each key N times")}, "keys")},
		{Name: "navigate_to", Description: "Move focus to an element by selector (test_id=…, id=…, role=…, text=…) with bounded D-pad navigation.", InputSchema: schema(map[string]any{"udid": str("target serial"), "selector": str("element selector")}, "selector")},
		{Name: "describe", Description: "Describe the current UI as a bounded tree summary (optionally filtered to one app).", InputSchema: schema(map[string]any{"udid": str("target serial"), "app_id": str("filter to app"), "max_nodes": num("summary cap (default 200)")})},
		{Name: "focused", Description: "Return the currently focused element and tree digest.", InputSchema: schema(map[string]any{"udid": str("target serial")})},
		{Name: "screenshot", Description: "Capture a screenshot; returns digest and size (optionally saves to a host path).", InputSchema: schema(map[string]any{"udid": str("target serial"), "out": str("host path to save the PNG"), "fresh": map[string]any{"type": "boolean", "description": "require bytes to differ from the previous capture"}})},
		{Name: "wait_idle", Description: "Block until the UI tree is stable for a stability window (or timeout).", InputSchema: schema(map[string]any{"udid": str("target serial"), "stable_ms": num("stability window (default 500)"), "timeout_ms": num("deadline (default 10000)")})},
		{Name: "logs_tail", Description: "Read the next N device log lines.", InputSchema: schema(map[string]any{"udid": str("target serial"), "lines": num("how many lines (default 20)")})},
	}
}

// maxLogLines bounds a logs_tail request (allocation guard).
const maxLogLines = 1000

// textResult builds a successful tool result.
func textResult(text string) any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

// errorResult marks a tool-level failure (distinct from protocol errors).
func errorResult(text string) any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}

// args is a thin accessor over the raw arguments object.
type args map[string]any

func (a args) str(key string) string {
	if v, ok := a[key].(string); ok {
		return v
	}
	return ""
}

func (a args) num(key string) int {
	if v, ok := a[key].(float64); ok {
		return int(v)
	}
	return 0
}

func (a args) boolean(key string) bool {
	if v, ok := a[key].(bool); ok {
		return v
	}
	return false
}

func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var a args
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &a)
	}
	c := s.Client

	switch name {
	case "list_devices":
		resp, err := c.ListTargets(ctx, &vdbv1.ListTargetsRequest{})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		var b strings.Builder
		for _, t := range resp.GetTargets() {
			fmt.Fprintf(&b, "%s  %s  %s\n", t.GetUdid(), t.GetKind(), t.GetState())
		}
		if b.Len() == 0 {
			b.WriteString("no targets\n")
		}
		return textResult(b.String()), nil

	case "install":
		path := a.str("path")
		if path == "" {
			return errorResult("install: 'path' is required"), nil
		}
		res, err := s.install(ctx, a.str("udid"), path)
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult(res), nil

	case "launch_app":
		if _, err := c.Launch(ctx, &vdbv1.LaunchRequest{Udid: a.str("udid"), ComponentId: a.str("component_id")}); err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult("launched " + a.str("component_id")), nil

	case "press_keys":
		keys, err := keySpecs(a)
		if err != nil {
			return errorResult(err.Error()), nil
		}
		resp, err := c.SendKeys(ctx, &vdbv1.SendKeysRequest{Udid: a.str("udid"), Keys: keys, FocusAfter: true})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult(fmt.Sprintf("pressed %d key(s); focus: %s (tree %s…)",
			len(keys), resp.GetFocus().GetFocusedId(), shortDigest(resp.GetFocus().GetTreeDigest()))), nil

	case "navigate_to":
		resp, err := s.navigate(ctx, a.str("udid"), a.str("selector"))
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult(resp), nil

	case "describe":
		resp, err := c.DescribeUI(ctx, &vdbv1.DescribeUIRequest{Udid: a.str("udid"), AppId: a.str("app_id"), MaxNodes: int32(a.num("max_nodes"))})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		out := resp.GetSummary()
		if len(resp.GetWarnings()) > 0 {
			out += "\nwarnings: " + strings.Join(resp.GetWarnings(), "; ")
		}
		return textResult(out), nil

	case "focused":
		resp, err := c.Focused(ctx, &vdbv1.FocusedRequest{Udid: a.str("udid")})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult(fmt.Sprintf("focused: %s\ntree: %s", resp.GetFocus().GetFocusedId(), resp.GetFocus().GetTreeDigest())), nil

	case "screenshot":
		resp, err := c.Screenshot(ctx, &vdbv1.ScreenshotRequest{Udid: a.str("udid"), Fresh: a.boolean("fresh")})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		out := a.str("out")
		saved := ""
		if out != "" {
			if err := writeAll(os.Getenv(EnvVarScreenshotDir), out, resp.GetPng()); err != nil {
				return errorResult(err.Error()), nil
			}
			saved = fmt.Sprintf(" saved to %s", out)
		}
		return textResult(fmt.Sprintf("screenshot: %d bytes%s\ndigest: %s", len(resp.GetPng()), saved, resp.GetDigest())), nil

	case "wait_idle":
		resp, err := c.WaitIdle(ctx, &vdbv1.WaitIdleRequest{Udid: a.str("udid"), StableMs: int32(a.num("stable_ms")), TimeoutMs: int32(a.num("timeout_ms"))})
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		state := "unstable (timeout)"
		if resp.GetStable() {
			state = "stable"
		}
		return textResult(fmt.Sprintf("ui %s; tree %s", state, shortDigest(resp.GetTreeDigest()))), nil

	case "logs_tail":
		// Clamp before allocating: a caller-supplied count must never
		// drive a huge make() (crash guard).
		n := a.num("lines")
		if n <= 0 || n > maxLogLines {
			n = 20
		}
		lines, err := s.logsTail(ctx, a.str("udid"), n)
		if err != nil {
			return errorResult(rpcErrText(err)), nil
		}
		return textResult(strings.Join(lines, "\n")), nil

	default:
		return nil, fmt.Errorf("mcp: unknown tool %q", name)
	}
}

func keySpecs(a args) ([]*vdbv1.KeySpec, error) {
	raw, ok := a["keys"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("press_keys: 'keys' must be a non-empty array")
	}
	repeat := a.num("repeat")
	specs := make([]*vdbv1.KeySpec, 0, len(raw))
	for _, item := range raw {
		name, _ := item.(string)
		specs = append(specs, &vdbv1.KeySpec{Name: name, Repeat: int32(repeat)})
	}
	return specs, nil
}

func (s *Server) navigate(ctx context.Context, udid, selector string) (string, error) {
	stream, err := s.Client.Navigate(ctx, &vdbv1.NavigateRequest{Udid: udid, Selector: selector})
	if err != nil {
		return "", err
	}
	var steps []string
	for {
		resp, err := stream.Recv()
		if err != nil {
			return "", err
		}
		switch u := resp.GetUpdate().(type) {
		case *vdbv1.NavigateResponse_Step:
			steps = append(steps, u.Step)
		case *vdbv1.NavigateResponse_Reached:
			return fmt.Sprintf("reached %s in %d step(s): %s",
				u.Reached.GetFocusedId(), len(steps), strings.Join(steps, "→")), nil
		case *vdbv1.NavigateResponse_Failure:
			return fmt.Sprintf("navigation failed (%s): %s — last focus: %s",
				u.Failure.GetReason(), u.Failure.GetDetail(), u.Failure.GetFocus().GetFocusedId()), nil
		}
	}
}

func (s *Server) logsTail(ctx context.Context, udid string, n int) ([]string, error) {
	stream, err := s.Client.Log(ctx, &vdbv1.LogRequest{Udid: udid})
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, n)
	for len(lines) < n {
		resp, err := stream.Recv()
		if err != nil {
			if len(lines) > 0 {
				return lines, nil // stream ended; return what we have
			}
			return nil, err
		}
		lines = append(lines, string(resp.GetLine()))
	}
	return lines, nil
}

func (s *Server) install(ctx context.Context, udid, path string) (string, error) {
	pkg, meta, err := openPkg(path)
	if err != nil {
		return "", err
	}
	defer pkg.Close()
	stream, err := s.Client.Install(ctx)
	if err != nil {
		return "", err
	}
	if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Meta{Meta: meta}}); err != nil {
		return "", err
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := pkg.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Chunk{Chunk: chunk}}); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("install %s: %s (%d bytes)", meta.GetFileName(), resp.GetResult(), resp.GetBytesReceived()), nil
}

// rpcErrText renders a gRPC error compactly with its code.
func rpcErrText(err error) string {
	if code := status.Code(err); code != codes.Unknown {
		return fmt.Sprintf("%s: %v", code, err)
	}
	return err.Error()
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
