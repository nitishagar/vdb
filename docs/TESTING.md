# Testing — invariant → gate matrix

Every claim below is enforced by an automated gate (`make check`; CI runs
the same). Tests run against fakes only (`internal/fakevda`,
`internal/faketoolkit`): no device, SDK, or network is required.

| Invariant (spec) | Gate |
|---|---|
| No Amazon binaries bundled; SDK optional for build/test | repo-lint (scripts/repo-lint.sh) via internal/repolint |
| Interfaces + fakes everywhere | every adapter test constructs its fake (internal/vda, internal/toolkit, internal/vegacli) |
| Fixture-driven parsers | internal/vda/testdata, internal/vegacli/testdata goldens; malformed output errors |
| Port-forward release on all paths | forward_integration_test.go (release, idempotent release, no pid litter) |
| Subprocess hygiene (no orphans) | execrun_test.go (pid-file /proc check), vegacli flood test, e2e process sweep |
| Idempotent lifecycle semantics | TestLifecyclePerVerbAlreadySo (per-verb; cross-verb hard failures stay errors) |
| Tree normalization fidelity | internal/tree golden JSON round-trips, geometry parse warnings, zero-size preservation |
| Navigator terminates | budget tests (all press kinds bound), trap, no-progress, target-not-found |
| Key codes exact (7 documented) | toolkit key-table test + input wire test (HOME→170) |
| Screenshot validation + stale path | zero-byte/null-buffer/invalid typed errors; TakeScreenshotFresh stale-then-fresh |
| wait_idle = tree-hash stability | companion churn→unstable, settle→stable |
| Streams respect cancellation, no leaks | goleak per companion test; log-death → Unavailable + companion alive |
| Per-target input serialization | TestSendKeysSerializesPerTarget (call-log windows) |
| Large payloads intact / no truncation | 12 MB byte-equal transit; oversized → ResourceExhausted |
| Config/discovery explicit | resolution tests listing searched paths; env override wins |
| API parity (no CLI-only capability) | internal/apiparity (verbs ↔ RPCs ↔ MCP tools, reflection-driven) |
| MCP bounded output | internal/mcp tests (compact focus, no raw XML) |
| Post-action focus on input calls | SendKeys/Navigate response-shape tests |

## Oracles worth knowing

- **Navigator fixtures are hand-computed**: expected key sequences are
  derived from the fixture geometry on paper (see comments in
  internal/navigator/navigator_test.go), never from planner output — the
  fake's focus behavior is scripted, not a mirror of the planner's
  heuristic.
- **Golden files** regenerate deliberately: `UPDATE_GOLDEN=1 go test ./internal/tree/`.
- **e2e** (build tag `e2e`, in `make check`): builds the real
  `vdb-companion` + `vdb` binaries, drives the full agent loop against the
  fakes, and asserts every spawned process exits.
