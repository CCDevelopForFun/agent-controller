# AGENTS.md

Guidance for AI agents (and human contributors) working in this repository.
This is the canonical, tool-agnostic guide; [`CLAUDE.md`](CLAUDE.md) imports it.

## What this is

Agent Controller is a **declarative runtime for AI agents**: define an agent in
YAML (ADL — Agent Definition Language) and run the same spec on the **Pi**,
**opencode**, or **Codex** runtime through a consistent backend interface. The
`agentctl` Go binary compiles a spec and dispatches it over a versioned stdio
NDJSON wire protocol to a Node runtime adapter, which loads the local registry
(tools, extensions, skills, agents, MCP servers) and drives the session against
the model provider.

## Repository layout

| Path | What |
|---|---|
| `cli/` | `agentctl` — Go binary (`validate` / `compile` / `run` / `chat` / `serve`). |
| `runtime/` | Pi runtime adapter (Node/TS). `runtime.type: local`. |
| `runtime-opencode/` | opencode adapter (Node/TS). `runtime.type: local-opencode`. |
| `runtime-codex/` | Codex adapter (Node/TS). `runtime.type: local-codex` (requires `model.provider: openai`). |
| `schemas/` | ADL + manifest JSON Schemas — the source of truth. |
| `examples/` | Example ADL specs + scheduler integrations (Maestro / Airflow / Temporal). |
| `skills/`, `tools/`, `extensions/`, `agents/` | Local registry the adapters load. |
| `e2e/` | End-to-end harness (`ADAPTER=pi\|opencode\|codex`). |
| `docs/architecture/` | Overview, wire protocol, per-adapter capability matrix. |

## Setup, build, test

Requires **Go ≥ 1.25** and **Node ≥ 22.19.0 + npm** (the strictest adapter
`engines` floor; `runtime-opencode` / `runtime-codex` only need ≥ 22).

```bash
# Build
(cd runtime          && npm install --ignore-scripts && npm run build)
(cd runtime-opencode && npm install --ignore-scripts && npm run build)
(cd runtime-codex    && npm install --ignore-scripts && npm run build)
(cd cli && go build -o bin/agentctl ./cmd/agentctl)

# Test
(cd runtime && npm test)            # vitest
(cd runtime-opencode && npm test)   # builds, then vitest
(cd runtime-codex && npm test)      # builds, then vitest
(cd cli && go test ./...)           # includes the schema-sync test
```

End-to-end against a live model (a no-op unless `AGENT_CONTROLLER_RUN_LIVE=1`, so
it's safe to call without credentials):

```bash
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ./e2e/run.sh                    # Pi
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ADAPTER=opencode ./e2e/run.sh   # opencode
```

## Running an agent

```bash
cli/bin/agentctl validate examples/hello.yaml   # schema + semantic checks
cli/bin/agentctl compile  examples/hello.yaml   # emit the compiled spec
cli/bin/agentctl run      examples/hello.yaml   # one-shot run (Pi adapter)
cli/bin/agentctl chat     examples/hello.yaml   # interactive REPL with session persistence
```

The opencode/codex adapters need their respective CLI on `PATH` (`opencode`,
`codex`); the codex adapter also needs `OPENAI_API_KEY` set.

## Conventions an agent MUST follow

- **Run `codex review --uncommitted` before declaring non-trivial work done.**
  Ship only with no actionable findings; re-run after each non-trivial fix.
  "Non-trivial" = anything beyond a one-liner / typo / pure-comment edit.
- **No `Co-Authored-By` trailers in commits** — the project owner's preference.
- **Small, bounded slices.** One coherent change per commit, typically 1–5
  files. Conventional-commit style: `<area>: <summary>` (`feat(pi)`,
  `feat(opencode)`, `feat(codex)`, `docs`, `fix`, `deps`, `test`).
- **ADL / schema changes touch every layer in the same commit:** edit
  `schemas/adl.v1alpha1.json`, copy it to the embedded path
  (`cp schemas/adl.v1alpha1.json cli/internal/adl/schemas/adl.v1alpha1.json`),
  update `cli/internal/adl/compiler.go`, all three adapters' type files
  (`runtime/src/types.ts`, `runtime-opencode/src/types.ts`,
  `runtime-codex/src/types.ts`), and the relevant row in
  `docs/architecture/harness-matrix.md`. `go -C cli test ./...`
  fails if the embedded copy drifts. Same protocol for `manifest.v1.json`.
- **Catalog-first for Pi primitives** (MCP, subagents, memory, skills, tools):
  check https://pi.dev/packages before building your own; record the
  vendor/wrap/build decision in the commit. (Does not apply to opencode/codex
  wiring — those have native primitives.)
- **Keep docs in sync** in the same commit when behavior changes:
  `docs/architecture/harness-matrix.md` and `ROADMAP.md`.
- **Preserve cross-adapter portability** — the core invariant. A spec that
  sticks to the cross-adapter feature set must keep running on Pi, opencode, or
  codex by changing one field (`runtime.type`). Some features are intentionally
  narrower: custom Pi-extension tools and `spec.extensions[]` are Pi-only
  (opencode and codex reject them at startup), and `spec.subagents[]` works on
  Pi and opencode but is rejected by Codex. These rejections are by design —
  don't "fix" them by weakening the checks. See the capability matrix for the
  authoritative per-adapter support.

## Where to look

- [`CONTRIBUTING.md`](CONTRIBUTING.md) — full dev workflow, release process, ADL-change checklist.
- [`README.md`](README.md) — quick start + capability summary.
- [`ROADMAP.md`](ROADMAP.md) — committed direction and open questions.
- [`docs/architecture/overview.md`](docs/architecture/overview.md) — layers + wire-protocol reference.
- [`docs/architecture/harness-matrix.md`](docs/architecture/harness-matrix.md) — per-adapter capability matrix and known gaps.
