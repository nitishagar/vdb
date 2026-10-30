// Package apiparity enforces invariant 16 as an executable contract: every
// CLI capability verb maps to a vdb.v1 RPC, every MCP tool maps to an RPC,
// and every RPC in the generated CompanionClient interface is reachable
// from a verb — no capability escapes the API, no RPC is orphaned.
package apiparity

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nitishagar/vdb/internal/cli"
	vdbv1 "github.com/nitishagar/vdb/internal/gen/vdb/v1"
	"github.com/nitishagar/vdb/internal/mcp"
)

// interfaceMethods reflects the generated client's RPC method names.
func interfaceMethods(t *testing.T) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf((*vdbv1.CompanionClient)(nil)).Elem()
	out := map[string]bool{}
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		// Generated clients also carry Close() (grpc.ClientConnInterface);
		// not an RPC.
		if name == "Close" {
			continue
		}
		out[name] = true
	}
	if len(out) == 0 {
		t.Fatal("reflected zero RPC methods — parity oracle is broken")
	}
	return out
}

func TestEveryVerbMapsToAnRPC(t *testing.T) {
	rpcs := interfaceMethods(t)
	for _, v := range cli.Verbs {
		if v.Server {
			if v.RPC != "" {
				t.Errorf("server verb %s must not claim an RPC", v.Name)
			}
			continue
		}
		if v.RPC == "" {
			t.Errorf("verb %s has no RPC mapping", v.Name)
			continue
		}
		if !rpcs[v.RPC] {
			t.Errorf("verb %s maps to %q which is not a CompanionClient method", v.Name, v.RPC)
		}
	}
}

func TestEveryRPCIsReachableFromAVerb(t *testing.T) {
	rpcs := interfaceMethods(t)
	for _, v := range cli.Verbs {
		if !v.Server {
			delete(rpcs, v.RPC)
		}
	}
	for orphan := range rpcs {
		t.Errorf("RPC %s is not reachable from any CLI verb", orphan)
	}
}

// mcpToolRPCs maps every MCP tool to the RPC(s) it drives. Kept here (not
// in mcp) so this test is the enforcement point.
var mcpToolRPCs = map[string][]string{
	"list_devices": {"ListTargets"},
	"install":      {"Install"},
	"launch_app":   {"Launch"},
	"press_keys":   {"SendKeys"},
	"navigate_to":  {"Navigate"},
	"describe":     {"DescribeUI"},
	"focused":      {"Focused"},
	"screenshot":   {"Screenshot"},
	"wait_idle":    {"WaitIdle"},
	"logs_tail":    {"Log"},
}

func TestEveryMCPToolMapsToRPCs(t *testing.T) {
	rpcs := interfaceMethods(t)
	for tool, wants := range mcpToolRPCs {
		for _, w := range wants {
			if !rpcs[w] {
				t.Errorf("MCP tool %s references non-existent RPC %s", tool, w)
			}
		}
	}
}

func TestMCPToolsListMatchesTable(t *testing.T) {
	got := mcp.ToolNames()
	if len(got) != len(mcpToolRPCs) {
		t.Fatalf("tool count = %d (%s), parity table has %d — update mcpToolRPCs",
			len(got), strings.Join(got, ","), len(mcpToolRPCs))
	}
	for _, name := range got {
		if _, ok := mcpToolRPCs[name]; !ok {
			t.Errorf("MCP tool %q advertised but missing from the parity table", name)
		}
	}
}
