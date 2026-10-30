# vdb architecture

```
vdb CLI | MCP server (stdio)          ← thin clients (cmd/vdb, internal/mcp)
        │ gRPC vdb.v1 (proto/vdb/v1/vdb.proto)
        ▼
vdb-companion (internal/companion)    ← one process per host, N targets
  ├─ Registry: sessions per udid (cached port-forward + toolkit client)
  ├─ per-target input lock            ← serializes all key injection
  ├─ forward re-establishment         ← transport error → re-forward once → retry once
  ├─ vda adapter (internal/vda)       ← subprocess: devices -l / forward / shell / push / pull
  ├─ vega adapter (internal/vegacli)  ← subprocess: install/launch/terminate/apps/log-stream
  ├─ toolkit client (internal/toolkit)← JSON-RPC 2.0 → device:8383 via the forward
  ├─ tree (internal/tree)             ← XML → typed model, multi-app filter, hash, summaries
  ├─ navigator (internal/navigator)   ← spatial planner + closed-loop executor
  └─ input (internal/input)           ← InputBackend interface (toolkit impl today)
```

## Design rules

1. **Adapters over everything.** Every device-facing interface is a Go
   interface with a subprocess/HTTP implementation and a fake. Undocumented
   formats (VDA output, toolkit schemas) live in exactly one package each,
   parsed against golden fixtures.
2. **The gRPC API is the contract.** CLI and MCP are thin clients; an
   executable parity test enforces no capability escapes the API.
3. **Closed-loop navigation.** One tree read per press (2k+1 device calls
   for k presses); budget-bound across all press kinds; trap/no-progress
   detection; failures carry last-known focus.
4. **Explicit limits.** 64 MiB message ceiling (screenshots are ~11 MB on
   hardware); oversized payloads error, never truncate.
5. **No Amazon binaries.** The user's SDK is resolved (env → PATH → default
   install location); a missing binary errors with every searched location.

## Lifecycle notes

- Companion shutdown: stop accepting → GracefulStop (10 s hard-stop bound)
  → release every session forward.
- Subprocess hygiene: vdb owns process groups; children of children die
  with the command (a lone child kill leaves grandchildren holding pipes).
- Log streams pump directly from the subprocess pipe into the gRPC stream:
  blocking sends give natural flow control; subprocess death ends the
  stream with Unavailable while the companion keeps serving.
