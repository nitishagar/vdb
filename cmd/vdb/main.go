// Command vdb is the developer-facing CLI: a thin client of the vdb
// companion's gRPC API (vdb.v1). Run `vdb-companion` first, then point vdb
// at it with --bind or VDB_BIND.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nitishagar/vdb/internal/cli"
	companion "github.com/nitishagar/vdb/internal/companion"
	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
	"github.com/nitishagar/vdb/internal/input"
	"github.com/nitishagar/vdb/internal/mcp"
)

const maxMsg = companion.DefaultMaxMessageBytes

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	if len(argv) == 0 || argv[0] == "help" || argv[0] == "--help" || argv[0] == "-h" {
		usage(os.Stdout)
		return 0
	}
	name := argv[0]
	verb := cli.Find(name)
	if verb == nil {
		fmt.Fprintf(os.Stderr, "vdb: unknown command %q\n\n", name)
		usage(os.Stderr)
		return 2
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	bind := fs.String("bind", companion.BindAddr(), "companion address (env VDB_BIND)")
	target := fs.String("target", os.Getenv("VDB_TARGET"), "target serial (env VDB_TARGET)")
	app := fs.String("app", "", "describe: filter the tree to one app id")
	maxNodes := fs.Int("max-nodes", 0, "describe: summary node cap")
	fresh := fs.Bool("fresh", false, "screenshot: require bytes to differ from the previous capture")
	stableMS := fs.Int("stable-ms", 0, "wait-idle: stability window in ms")
	timeoutMS := fs.Int("timeout-ms", 0, "wait-idle: deadline in ms")
	if err := fs.Parse(argv[1:]); err != nil {
		return 2
	}
	opts := flags{
		app: *app, maxNodes: *maxNodes, fresh: *fresh,
		stableMS: *stableMS, timeoutMS: *timeoutMS,
	}

	if verb.Server {
		return runServer(fs, *bind)
	}

	conn, err := grpc.NewClient(*bind,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxMsg), grpc.MaxCallRecvMsgSize(maxMsg)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vdb: connect %s: %v\n", *bind, err)
		return 1
	}
	defer conn.Close()
	client := vdbv1.NewCompanionClient(conn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return dispatch(ctx, client, verb, *target, fs.Args(), opts)
}

func usage(w io.Writer) {
	fmt.Fprint(w, "vdb — a development bridge for Vega OS devices (unofficial)\n\nUsage:\n  vdb <command> [args]\n\nCommands:\n")
	for _, v := range cli.Verbs {
		fmt.Fprintf(w, "  %-16s %s\n", v.Name, v.Usage)
	}
	fmt.Fprint(w, "\nGlobal flags go directly after the command, before its arguments:\n  vdb <command> --bind ADDR --target SERIAL [args…]\n  (env: VDB_BIND, VDB_TARGET)\n")
}

// runServer handles server-mode verbs (currently `mcp`).
func runServer(fs *flag.FlagSet, bind string) int {
	conn, err := grpc.NewClient(bind,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxMsg), grpc.MaxCallRecvMsgSize(maxMsg)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vdb: connect %s: %v\n", bind, err)
		return 1
	}
	defer conn.Close()
	srv := &mcp.Server{Client: vdbv1.NewCompanionClient(conn), In: os.Stdin, Out: os.Stdout}
	if err := srv.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "vdb-mcp: %v\n", err)
		return 1
	}
	return 0
}

// flags carries verb-specific options parsed once in run().
type flags struct {
	app       string
	maxNodes  int
	fresh     bool
	stableMS  int
	timeoutMS int
}

func dispatch(ctx context.Context, c vdbv1.CompanionClient, verb *cli.Verb, target string, args []string, fl flags) int {
	var err error
	switch verb.Name {
	case "list-targets":
		err = cmdListTargets(ctx, c)
	case "describe-target":
		err = needTarget(target, func() error { return cmdDescribeTarget(ctx, c, target) })
	case "install":
		err = withPath(args, func(p string) error { return cmdInstall(ctx, c, target, p) })
	case "uninstall":
		err = withArg(args, func(id string) error {
			_, e := c.Uninstall(ctx, &vdbv1.UninstallRequest{Udid: target, ComponentId: id})
			return e
		})
	case "launch":
		err = withArg(args, func(id string) error {
			_, e := c.Launch(ctx, &vdbv1.LaunchRequest{Udid: target, ComponentId: id})
			return e
		})
	case "terminate":
		err = withArg(args, func(id string) error {
			resp, e := c.Terminate(ctx, &vdbv1.TerminateRequest{Udid: target, ComponentId: id})
			if e == nil {
				fmt.Println(resp.GetResult())
			}
			return e
		})
	case "list-apps":
		err = cmdListApps(ctx, c, target)
	case "keys":
		err = withArg(args, func(spec string) error { return cmdKeys(ctx, c, target, spec) })
	case "describe":
		err = cmdDescribe(ctx, c, target, fl)
	case "focused":
		err = needTarget(target, func() error { return cmdFocused(ctx, c, target) })
	case "navigate":
		err = withArg(args, func(sel string) error { return cmdNavigate(ctx, c, target, sel) })
	case "screenshot":
		err = cmdScreenshot(ctx, c, target, args, fl)
	case "logs":
		err = cmdLogs(ctx, c, target)
	case "shell":
		err = withArg(args, func(cmd string) error {
			resp, e := c.Shell(ctx, &vdbv1.ShellRequest{Udid: target, Command: cmd})
			if e != nil {
				return e
			}
			fmt.Print(string(resp.GetStdout()))
			fmt.Fprint(os.Stderr, string(resp.GetStderr()))
			if code := resp.GetExitCode(); code != 0 {
				return fmt.Errorf("exit %d", code)
			}
			return nil
		})
	case "push":
		err = withPaths(args, func(local, remote string) error {
			_, e := c.Push(ctx, &vdbv1.PushRequest{Udid: target, LocalPath: local, RemotePath: remote})
			return e
		})
	case "pull":
		err = withPaths(args, func(remote, local string) error {
			_, e := c.Pull(ctx, &vdbv1.PullRequest{Udid: target, RemotePath: remote, LocalPath: local})
			return e
		})
	case "wait-idle":
		err = cmdWaitIdle(ctx, c, target, fl)
	default:
		err = fmt.Errorf("vdb: unimplemented verb %q", verb.Name)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vdb %s: %v\n", verb.Name, err)
		return 1
	}
	return 0
}

func cmdListTargets(ctx context.Context, c vdbv1.CompanionClient) error {
	resp, err := c.ListTargets(ctx, &vdbv1.ListTargetsRequest{})
	if err != nil {
		return err
	}
	if len(resp.GetTargets()) == 0 {
		fmt.Println("no targets attached")
		return nil
	}
	for _, t := range resp.GetTargets() {
		fmt.Printf("%s  %-7s %s\n", t.GetUdid(), t.GetKind(), t.GetState())
	}
	return nil
}

func cmdDescribeTarget(ctx context.Context, c vdbv1.CompanionClient, target string) error {
	resp, err := c.DescribeTarget(ctx, &vdbv1.DescribeTargetRequest{Udid: target})
	if err != nil {
		return err
	}
	fmt.Printf("udid:      %s\nvda:       %s\ntoolkit:   %v\n",
		resp.GetTarget().GetUdid(), resp.GetVdaVersion(), resp.GetToolkitReachable())
	return nil
}

func cmdInstall(ctx context.Context, c vdbv1.CompanionClient, target, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	stream, err := c.Install(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Meta{
		Meta: &vdbv1.InstallMeta{Udid: target, FileName: filepath.Base(path)},
	}}); err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if err := stream.Send(&vdbv1.InstallRequest{Part: &vdbv1.InstallRequest_Chunk{Chunk: chunk}}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	fmt.Printf("%s (%d bytes)\n", resp.GetResult(), resp.GetBytesReceived())
	return nil
}

func cmdListApps(ctx context.Context, c vdbv1.CompanionClient, target string) error {
	resp, err := c.ListApps(ctx, &vdbv1.ListAppsRequest{Udid: target})
	if err != nil {
		return err
	}
	for _, a := range resp.GetApps() {
		fmt.Printf("%s\t%s\n", a.GetComponentId(), a.GetName())
	}
	return nil
}

func cmdKeys(ctx context.Context, c vdbv1.CompanionClient, target, spec string) error {
	seq, err := input.ParseSequence(spec)
	if err != nil {
		return err
	}
	specs := make([]*vdbv1.KeySpec, len(seq))
	for i, p := range seq {
		specs[i] = &vdbv1.KeySpec{Name: p.Key, HoldMs: int32(p.HoldMS)}
	}
	resp, err := c.SendKeys(ctx, &vdbv1.SendKeysRequest{Udid: target, Keys: specs, FocusAfter: true})
	if err != nil {
		return err
	}
	if resp.GetFocus() != nil && resp.GetFocus().GetFocusedId() != "" {
		fmt.Printf("focus: %s\n", resp.GetFocus().GetFocusedId())
	}
	return nil
}

func cmdDescribe(ctx context.Context, c vdbv1.CompanionClient, target string, fl flags) error {
	resp, err := c.DescribeUI(ctx, &vdbv1.DescribeUIRequest{Udid: target, AppId: fl.app, MaxNodes: int32(fl.maxNodes)})
	if err != nil {
		return err
	}
	fmt.Print(formatSummary(resp.GetSummary()))
	for _, w := range resp.GetWarnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return nil
}

// formatSummary renders the compact outline JSON the API returns.
func formatSummary(summaryJSON string) string {
	var s struct {
		MaxNodes  int    `json:"max_nodes"`
		Truncated bool   `json:"truncated"`
		Nodes     []node `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(summaryJSON), &s); err != nil {
		return summaryJSON
	}
	var b strings.Builder
	for _, n := range s.Nodes {
		b.WriteString(strings.Repeat("  ", n.Depth))
		if n.Focused {
			b.WriteString("[*] ")
		} else if n.Focusable {
			b.WriteString("[ ] ")
		} else {
			b.WriteString(" ·  ")
		}
		if n.Role != "" {
			b.WriteString(n.Role + " ")
		}
		if n.ID != "" {
			b.WriteString("#" + n.ID + " ")
		}
		if n.Text != "" {
			b.WriteString(n.Text + " ")
		}
		fmt.Fprintf(&b, "(%d,%d %dx%d)\n", n.Rect.X, n.Rect.Y, n.Rect.W, n.Rect.H)
	}
	if s.Truncated {
		fmt.Fprintf(&b, "… truncated at %d nodes\n", s.MaxNodes)
	}
	return b.String()
}

type node struct {
	Depth     int    `json:"d"`
	Role      string `json:"role"`
	ID        string `json:"id"`
	Text      string `json:"text"`
	Rect      rect   `json:"rect"`
	Focused   bool   `json:"focused"`
	Focusable bool   `json:"focusable"`
}

type rect struct {
	X, Y, W, H int32
}

func cmdFocused(ctx context.Context, c vdbv1.CompanionClient, target string) error {
	resp, err := c.Focused(ctx, &vdbv1.FocusedRequest{Udid: target})
	if err != nil {
		return err
	}
	fmt.Printf("focused: %s\ntree:    %s\n", resp.GetFocus().GetFocusedId(), resp.GetFocus().GetTreeDigest())
	return nil
}

func cmdNavigate(ctx context.Context, c vdbv1.CompanionClient, target, sel string) error {
	stream, err := c.Navigate(ctx, &vdbv1.NavigateRequest{Udid: target, Selector: sel})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		switch u := resp.GetUpdate().(type) {
		case *vdbv1.NavigateResponse_Step:
			fmt.Printf("step %s\n", u.Step)
		case *vdbv1.NavigateResponse_Reached:
			fmt.Printf("reached %s\n", u.Reached.GetFocusedId())
			return nil
		case *vdbv1.NavigateResponse_Failure:
			return fmt.Errorf("%s: %s (last focus: %s)", u.Failure.GetReason(), u.Failure.GetDetail(), u.Failure.GetFocus().GetFocusedId())
		}
	}
}

func cmdScreenshot(ctx context.Context, c vdbv1.CompanionClient, target string, args []string, fl flags) error {
	out := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = a
		}
	}
	resp, err := c.Screenshot(ctx, &vdbv1.ScreenshotRequest{Udid: target, Fresh: fl.fresh})
	if err != nil {
		return err
	}
	if out == "" {
		fmt.Printf("%d bytes, digest %s\n", len(resp.GetPng()), resp.GetDigest())
		return nil
	}
	if err := os.WriteFile(out, resp.GetPng(), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s (%d bytes, digest %s)\n", out, len(resp.GetPng()), resp.GetDigest())
	return nil
}

func cmdLogs(ctx context.Context, c vdbv1.CompanionClient, target string) error {
	stream, err := c.Log(ctx, &vdbv1.LogRequest{Udid: target})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		fmt.Println(string(resp.GetLine()))
	}
}

func cmdWaitIdle(ctx context.Context, c vdbv1.CompanionClient, target string, fl flags) error {
	stableMS, timeoutMS := fl.stableMS, fl.timeoutMS
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS+5000)*time.Millisecond)
	defer cancel()
	resp, err := c.WaitIdle(ctx, &vdbv1.WaitIdleRequest{Udid: target, StableMs: int32(stableMS), TimeoutMs: int32(timeoutMS)})
	if err != nil {
		return err
	}
	fmt.Printf("stable=%v tree=%s\n", resp.GetStable(), resp.GetTreeDigest())
	return nil
}

// --- small arg helpers -------------------------------------------------------

func needTarget(target string, fn func() error) error {
	if target == "" {
		return fmt.Errorf("no target: pass --target or set VDB_TARGET")
	}
	return fn()
}

func withArg(args []string, fn func(string) error) error {
	if len(args) < 1 {
		return fmt.Errorf("missing argument")
	}
	return fn(args[0])
}

func withPath(args []string, fn func(string) error) error {
	return withArg(args, fn)
}

func withPaths(args []string, fn func(a, b string) error) error {
	if len(args) < 2 {
		return fmt.Errorf("expected two arguments")
	}
	return fn(args[0], args[1])
}
