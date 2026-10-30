// Package cli is the vdb command-line surface: a thin client of the vdb.v1
// gRPC API. The verb table is exported so the API-parity test can enforce
// that every capability verb maps to an RPC (invariant 16) — no CLI-only
// capability escapes the API.
package cli

// Verb is one CLI capability verb.
type Verb struct {
	Name   string
	RPC    string // generated CompanionClient method name
	Usage  string
	Args   string // usage hint for arguments
	Server bool   // server-mode verb (no RPC by design, e.g. `mcp`)
}

// Verbs is the complete verb table. The parity test asserts:
//   - every non-server verb maps to a method of vdbv1.CompanionClient;
//   - every CompanionClient method is reachable through some verb.
var Verbs = []Verb{
	{Name: "list-targets", RPC: "ListTargets", Usage: "list attached targets"},
	{Name: "describe-target", RPC: "DescribeTarget", Usage: "describe one target (versions, toolkit reachability)", Args: "<udid>"},
	{Name: "install", RPC: "Install", Usage: "install a .vpkg", Args: "<path>"},
	{Name: "uninstall", RPC: "Uninstall", Usage: "uninstall an app", Args: "<component-id>"},
	{Name: "launch", RPC: "Launch", Usage: "launch an app", Args: "<component-id>"},
	{Name: "terminate", RPC: "Terminate", Usage: "terminate an app", Args: "<component-id>"},
	{Name: "list-apps", RPC: "ListApps", Usage: "list installed apps"},
	{Name: "keys", RPC: "SendKeys", Usage: "press keys (comma-separated)", Args: "<UP,DOWN,ENTER,...>"},
	{Name: "describe", RPC: "DescribeUI", Usage: "print the UI tree summary", Args: "[--app <id>] [--max-nodes n]"},
	{Name: "focused", RPC: "Focused", Usage: "print the focused element"},
	{Name: "navigate", RPC: "Navigate", Usage: "move focus to a selector", Args: "<test_id=...|id=...|role=...|text=...>"},
	{Name: "screenshot", RPC: "Screenshot", Usage: "capture a screenshot", Args: "[out.png] [--fresh]"},
	{Name: "logs", RPC: "Log", Usage: "stream device logs (Ctrl-C to stop)"},
	{Name: "shell", RPC: "Shell", Usage: "run a shell command on the device", Args: "<command>"},
	{Name: "push", RPC: "Push", Usage: "push a file to the device", Args: "<local> <remote>"},
	{Name: "pull", RPC: "Pull", Usage: "pull a file from the device", Args: "<remote> <local>"},
	{Name: "wait-idle", RPC: "WaitIdle", Usage: "block until the UI stabilizes", Args: "[--stable-ms n] [--timeout-ms n]"},
	{Name: "mcp", RPC: "", Server: true, Usage: "serve MCP (Model Context Protocol) on stdio"},
}

// Find returns the verb with the given name.
func Find(name string) *Verb {
	for i := range Verbs {
		if Verbs[i].Name == name {
			return &Verbs[i]
		}
	}
	return nil
}
