# vdb — a development bridge for Vega OS devices

`vdb` is an unofficial, open-source host-side companion for developing and
automating apps on **Amazon Fire TV devices running Vega OS 2.0** — inspired
by Facebook's [`idb`](https://github.com/facebook/idb) for iOS, designed
D-pad-first for TV.

It unifies the pieces Amazon already ships — the `vda` device bridge, the
`vega` CLI, and the on-device automation toolkit (JSON-RPC on port 8383) —
behind one gRPC API with a CLI and an MCP server, so humans and AI agents
can drive a TV the way `idb` drives an iPhone.

> **Unofficial.** vdb is not affiliated with, endorsed by, or sponsored by
> Amazon.com, Inc. "Amazon", "Fire TV" and "Vega" are trademarks of
> Amazon.com, Inc. vdb never bundles or redistributes any Amazon SDK
> materials — it orchestrates the *user's own* installed Vega SDK tools
> (`vda`, `vega`) as subprocesses and talks only to Amazon-designated
> interfaces (the automation toolkit's documented JSON-RPC surface).

## What you get

- **`vdb-companion`** — a per-host gRPC daemon (`vdb.v1`, default
  `127.0.0.1:10882`) that discovers targets, manages port-forwards, and
  drives the automation toolkit.
- **`vdb`** — a CLI: list/install/launch/terminate apps, press keys,
  describe the UI tree, navigate by selector, screenshots, logs, shell,
  push/pull, wait-idle.
- **`vdb mcp`** — a [Model Context Protocol](https://modelcontextprotocol.io)
  server (stdio) exposing ~10 coarse tools so AI agents can drive a device:
  `list_devices`, `install`, `launch_app`, `press_keys`, `navigate_to`,
  `describe`, `focused`, `screenshot`, `wait_idle`, `logs_tail`.

D-pad navigation is closed-loop: press one key, re-read the tree, re-plan —
with a hard step budget, trap detection, and virtualized-list scrolling.
Every input call returns the post-action focus so agents close the loop in
one round-trip.

## Prerequisites

- Linux or macOS host (the Vega SDK supports Ubuntu 20.04+ and macOS 10.15+).
- The **Vega SDK 2.0** installed by you (free download; vdb will not ship
  it): `vda` and `vega` on your `PATH`, or pointed at via env.
- A Fire TV device with Developer Mode enabled (or a Vega Virtual Device).
- Go 1.26+ to build; `buf` + protoc plugins only if you change the proto.

## Install

```sh
go install github.com/nitishagar/vdb/cmd/vdb@latest
go install github.com/nitishagar/vdb/cmd/vdb-companion@latest
```

Or from a checkout: `make build`.

## Quickstart

```sh
# Terminal 1: start the companion (uses your vda/vega)
vdb-companion

# Terminal 2: drive a device
vdb list-targets
vdb install app.vpkg --target AFTVTEST001
vdb launch com.example.app --target AFTVTEST001
vdb describe --target AFTVTEST001          # bounded UI tree summary
vdb navigate test_id=episodes --target AFTVTEST001
vdb keys DOWN,ENTER --target AFTVTEST001   # prints post-action focus
vdb screenshot out.png --target AFTVTEST001
vdb logs --target AFTVTEST001

# AI agent mode
vdb mcp | your-mcp-client
```

## Configuration

| Env | Default | Meaning |
|---|---|---|
| `VDB_BIND` | `127.0.0.1:10882` | companion listen address |
| `VDB_TARGET` | — | default target serial for the CLI |
| `VDB_VDA_PATH` | PATH → `~/vega/sdk/*/bin/tools/vda` | `vda` binary override |
| `VDB_VEGA_PATH` | PATH → `~/vega/bin/vega` | `vega` binary override |
| `VDB_TOOLKIT_PORT` | `8383` | on-device automation-toolkit port |
| `VDB_SCREENSHOT_DIR` | working dir | MCP screenshot output root (writes outside it are rejected) |
| `VDB_ALLOW_REMOTE` | unset | set `1` to allow a non-loopback companion bind (see security note) |

CLI global flags (`--bind`, `--target`) go directly after the command.

## Development

```sh
make check      # gofmt + vet + build + tests (-race) + codegen drift + repo-lint + e2e
make test       # unit/integration suites (fakes only — no device needed)
make e2e        # builds the real binaries and drives the full loop vs fakes
make generate   # buf codegen (requires buf + protoc-gen-go[-grpc])
```

Everything is tested against fakes (`internal/fakevda`,
`internal/faketoolkit`) that encode the observed toolkit quirks (null-buffer
screenshots, stale frames, multi-app trees, the focus-alignment defect).
See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and
[docs/TESTING.md](docs/TESTING.md).

## Security note

The companion is **unauthenticated by design** (like adb/idb on a dev
machine) and binds loopback only: any local process can drive the device
and use Push/Pull. Run it on single-user machines. A non-loopback bind is
refused unless `VDB_ALLOW_REMOTE=1` is set explicitly.

## Scope and limits

- VVD (emulator) gRPC video streaming, inputd-cli, and HDMI capture are not
  implemented; the input backend interface is ready for them.
- Screenshot capture on hardware inherits the toolkit's reliability
  characteristics (typed errors + bounded retries; the API reports digests
  so callers can detect stale frames).
- Undocumented interfaces (toolkit method schemas, VDA output text) are
  isolated behind adapters with golden fixtures; expect to adjust them on
  first contact with a new SDK release.

## License

Apache-2.0 — see [LICENSE](LICENSE).
