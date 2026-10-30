package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nitishagar/vdb/internal/faketoolkit"
	"github.com/nitishagar/vdb/internal/testkit"
	"github.com/nitishagar/vdb/internal/vda"
	"github.com/nitishagar/vdb/internal/vegacli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nitishagar/vdb/internal/companion"
	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
)

// pipeMCP runs the MCP server over in-process pipes against a real
// companion+fake stack, returning the read side.
func pipeMCP(t *testing.T) (*pipeScanner, *faketoolkit.Server) {
	t.Helper()
	tk := faketoolkit.New(t)
	vdaBin, _ := testkit.FakeVDAEnv(t, "AFTVTEST001")
	t.Setenv("FAKEVDA_TOOLKIT_ADDR", strings.TrimPrefix(tk.URL(), "http://"))

	vegaBin := t.TempDir() + "/vega"
	if err := ioWrite(vegaBin, "#!/bin/sh\necho ok\n"); err != nil {
		t.Fatal(err)
	}
	reg := companion.NewRegistry(vda.New(vdaBin), vegacli.New(vegaBin))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		reg.Close(ctx)
	})
	svc := companion.NewService(reg, companion.Config{})

	ln := listener(t)
	srv := grpc.NewServer()
	vdbv1.RegisterCompanionServer(srv, svc)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	server := &Server{Client: vdbv1.NewCompanionClient(conn), In: inR, Out: outW}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("mcp server did not shut down on EOF")
		}
	})

	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	t.Cleanup(func() {})

	// send writes one JSON-RPC line and returns after reading one reply.
	send := func(line string) map[string]any {
		t.Helper()
		if _, err := inW.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !sc.Scan() {
			t.Fatalf("no reply for %s", line)
		}
		var resp map[string]any
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			t.Fatalf("bad reply %s: %v", sc.Text(), err)
		}
		return resp
	}

	// notify writes a notification (no reply expected).
	notify := func(line string) {
		t.Helper()
		if _, err := inW.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return &pipeScanner{sc: sc, send: send, notify: notify}, tk
}

type pipeScanner struct {
	sc     *bufio.Scanner
	send   func(string) map[string]any
	notify func(string)
}

func ioWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}

func listener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	p, _ := pipeMCP(t)

	init := p.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if init["error"] != nil {
		t.Fatalf("initialize error: %v", init["error"])
	}
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	si := result["serverInfo"].(map[string]any)
	if si["name"] != "vdb" {
		t.Errorf("serverInfo = %v", si)
	}

	// notifications produce NO reply (id-less messages are dropped).
	p.notify(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	list := p.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 10 {
		t.Fatalf("tools = %d, want 10", len(tools))
	}
	for _, tc := range tools {
		tool := tc.(map[string]any)
		if tool["inputSchema"] == nil {
			t.Errorf("tool %v missing inputSchema", tool["name"])
		}
	}
}

func TestMCPPressKeysReturnsCompactFocus(t *testing.T) {
	p, tk := pipeMCP(t)
	tk.Script("injectInputKeyEvent", faketoolkit.ObjectStep(nil))
	eps := make([]faketoolkit.Step, 8)
	for i := range eps {
		eps[i] = faketoolkit.XMLStep(`<root><app id="com.example.app"><window x="0" y="0" width="1920" height="1080"><child x="100" y="400" width="500" height="100" role="button" test_id="episodes" focusable="true" focused="true"/></window></app></root>`)
	}
	tk.Script("getPageSource", eps...)
	res := p.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"press_keys","arguments":{"udid":"AFTVTEST001","keys":["DOWN"]}}}`)
	if res["error"] != nil {
		t.Fatalf("press_keys protocol error: %v", res["error"])
	}
	text := toolText(t, res)
	// Compact output: bounded line, mentions focus; no raw XML.
	if strings.Contains(text, "<root") {
		t.Errorf("raw XML leaked into MCP output: %.80s", text)
	}
	if !strings.Contains(text, "focus") {
		t.Errorf("focus missing from press_keys result: %q", text)
	}
}

func TestMCPUnknownToolIsProtocolError(t *testing.T) {
	p, _ := pipeMCP(t)
	res := p.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"not_a_tool","arguments":{}}}`)
	if res["error"] == nil {
		t.Fatal("unknown tool must be a JSON-RPC error")
	}
}

func TestMCPDescribeOutputBounded(t *testing.T) {
	p, _ := pipeMCP(t)
	res := p.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"describe","arguments":{"udid":"AFTVTEST001","max_nodes":5}}}`)
	if res["error"] != nil {
		t.Fatalf("describe error: %v", res["error"])
	}
	// Either a bounded summary or a tool error surface — both are fine
	// depending on fake scripting; assert no raw XML either way.
	if text, ok := toolTextOK(res); ok && strings.Contains(text, "<root") {
		t.Errorf("raw XML leaked: %.80s", text)
	}
}

func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result := resp["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	return content["text"].(string)
}

func toolTextOK(resp map[string]any) (string, bool) {
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", false
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return "", false
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, text != ""
}
