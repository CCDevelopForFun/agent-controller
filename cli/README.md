# `agentctl` — Agent Controller CLI

The Go binary that compiles ADL specs and dispatches them to a runtime adapter. Single static binary, no Node/Python runtime required for the CLI itself.

## Building

```bash
go build -o bin/agentctl ./cmd/agentctl
```

Requires Go ≥ 1.25 (matches `cli/go.mod`; the floor was bumped in slice 5.1 for OTel SDK v1.44+). The Go module is rooted at `cli/`, not the repo root — run all `go` commands from this directory (or use `go -C cli ...`).

## Testing

```bash
go test ./...
```

Tests cover: ADL compilation (`internal/adl`), registry resolution (`internal/registry`), backend dispatch (`internal/backend`), wire-event decoding (`internal/wire`), and the `cmd/agentctl` command surface.

## Commands

```bash
agentctl validate <spec.yaml>            # JSON-Schema validate against schemas/adl.v1alpha1.json
agentctl compile  <spec.yaml>            # print CompiledSpec JSON
agentctl run      <spec.yaml>            # spawn the runtime, stream events
agentctl run      <spec.yaml> --task "override"          # override spec.task
agentctl run      <spec.yaml> --resume <session-id>      # continue a prior session (Pi only)
agentctl run      <spec.yaml> --raw-out events.ndjson    # also write raw NDJSON
agentctl sessions list                                   # list persisted sessions (Pi only)
agentctl install  extension <name>                       # delegate to `pi install npm:<name>`
agentctl install  --from <spec.yaml>                     # install each entry in the legacy spec.installs[] list
                                                         #   (NOT spec.extensions[]; that field auto-installs at session start)
```

## Architecture role

`agentctl` is the single user-facing entry point. It owns:

- YAML parsing + JSON Schema validation
- Manifest registry resolution (`tools/`, `extensions/`, `skills/`, `agents/`)
- ADL → CompiledSpec compilation
- Subprocess management (spawning the right adapter based on `spec.runtime.type`)
- Signal handling, exit codes, pretty-printing
- Streaming the NDJSON wire-event protocol from the runtime back to the user

It does **not** own:

- LLM calls (the runtime adapter does that, via Pi or opencode)
- Tool execution (the runtime adapter does that)
- Anything Pi-specific or opencode-specific (only the dispatch logic in `cmd/agentctl/main.go::resolveRuntimeCommand`)

## Adapter dispatch

`spec.runtime.type` selects which adapter binary the CLI spawns:

| `runtime.type` | Adapter | Default runtime path |
|---|---|---|
| `local` (legacy) | Pi adapter | `<cwd>/runtime/dist/index.js` |
| `local-pi` (explicit alias) | Pi adapter | `<cwd>/runtime/dist/index.js` |
| `local-opencode` | opencode adapter | `<cwd>/runtime-opencode/dist/index.js` |

Paths are resolved from the process's current working directory (`os.Getwd()`), **not** relative to the CLI binary. **Run `agentctl` from the repo root.** Both the runtime-adapter lookup AND the manifest registry scan (`tools/`, `extensions/`, `skills/`, `agents/`) are cwd-relative.

Setting `AGENT_CONTROLLER_RUNTIME=<absolute-path-to-dist/index.js>` overrides the runtime-adapter path, but it does **not** override the registry-scan root. From `cli/`, even with `AGENT_CONTROLLER_RUNTIME` set, `agentctl run ../examples/hello.yaml` still fails with `tool "get_time" not found` because the registry scanner looks for `cli/tools/`, `cli/extensions/`, etc. There is no equivalent env var for the registry root today (tracked as a v0.3 follow-up).

Example failure mode: `cd cli && bin/agentctl run ../examples/hello.yaml` fails. Workarounds:
- `cd` to the repo root and run `cli/bin/agentctl run examples/hello.yaml`
- Or use an absolute spec path AND make sure your cwd contains the registry directories

## Package layout

| Package | Purpose |
|---|---|
| `cmd/agentctl` | Cobra commands, entry point, signal handling |
| `internal/adl` | YAML parser + JSON Schema validator + compiler (ADL → CompiledSpec) |
| `internal/backend` | Backend interface + LocalBackend (subprocess spawn, NDJSON parse, stderr capture) |
| `internal/registry` | Tool / extension / skill / subagent manifest discovery + resolution |
| `internal/wire` | Wire-event envelope types + JSON decoder |

See [`../docs/architecture/overview.md`](../docs/architecture/overview.md) for the multi-adapter architecture diagram.
