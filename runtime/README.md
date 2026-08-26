# `@agent-controller/runtime` — Pi runtime adapter

Node/TypeScript adapter that runs an [agent-controller](https://github.com/CCDevelopForFun/agent-controller) `CompiledSpec` against a Pi session and emits the NDJSON wire-event stream that `agentctl` consumes. This is the original / production adapter; the opencode adapter ([`@agent-controller/runtime-opencode`](https://www.npmjs.com/package/@agent-controller/runtime-opencode)) is the v0.2 sibling.

Requires **Node 22.19.0+** — `@earendil-works/pi-ai` and `@earendil-works/pi-coding-agent` declare `engines.node: ">=22.19.0"` (the nested `undici` they ship needs that floor). Older Node 22.x will install but is outside the supported range.

## Install

```bash
npm install -g @agent-controller/runtime
# or per-project
npm install --save-dev @agent-controller/runtime
```

## Use with `agentctl`

`agentctl` (the Go CLI) spawns this package as a subprocess. The example specs under [`examples/`](https://github.com/CCDevelopForFun/agent-controller/tree/main/examples) reference cwd-relative registry entries (`tools/`, `extensions/`, `skills/`, `agents/`) that ship with the source repo but not with this npm package. For an npm-only install, use a self-contained spec:

```bash
# 1) Write a self-contained spec (no tools[]/extensions[]/skills[]/subagents[])
cat > /tmp/hello.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: hello }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  persona: { role: Helpful demo, instructions: Answer concisely. }
  task: Say hello.
  tools: []
  runtime: { type: local }
EOF

# 2) Point agentctl at the adapter and run
export ANTHROPIC_API_KEY=sk-ant-...

# global install
AGENT_CONTROLLER_RUNTIME="$(npm root -g)/@agent-controller/runtime/dist/index.js" \
  agentctl run /tmp/hello.yaml

# or per-project (from a directory with the package in node_modules)
AGENT_CONTROLLER_RUNTIME="./node_modules/@agent-controller/runtime/dist/index.js" \
  agentctl run /tmp/hello.yaml
```

Without `AGENT_CONTROLLER_RUNTIME`, `agentctl run` looks under `<cwd>/runtime/dist/index.js` — the source-clone-and-build install path.

`agentctl` (matching version) is available from the [GitHub Releases page](https://github.com/CCDevelopForFun/agent-controller/releases) as cross-platform binaries.

## Architecture role

This package is the **subprocess** spawned by `agentctl run` when `spec.runtime.type` is `local` or `local-pi`. It:

1. Reads a `CompiledSpec` JSON document from stdin
2. Constructs Pi's `DefaultResourceLoader` from the spec (skills paths, agent files, MCP config)
3. Materializes `<cwd>/.pi/mcp.json` and `<cwd>/.pi/agents/*.md` for Pi's loaders
4. Auto-installs `spec.extensions[].source` packages on demand
5. Builds the system prompt: honesty preamble + persona + role + skill bodies
6. Calls `createAgentSession` + `session.prompt`
7. Translates Pi's internal events into the wire-protocol NDJSON envelope
8. Runs the runtime-side hallucination detector on assistant messages

## Environment variables

| Variable | Effect |
|---|---|
| `AGENT_CONTROLLER_EXT_CONFIG` | Per-extension config blob (set by adapter from `spec.extensions[].config`) |
| `AGENT_CONTROLLER_USE_FAKE_PROVIDER` | If `1`, the adapter loads the test fake-provider before starting the session (Layer-1 E2E only) |
| `AGENT_CONTROLLER_NO_AUTO_INSTALL` | If `1`, auto-installation of `spec.extensions[].source` is disabled; missing packages fail fast |
| `ANTHROPIC_BASE_URL` | Forwarded to Pi for model gateway override |
| `AC_PI_BIN` | Path to the `pi` CLI for subagent process spawning (set by the adapter when subagents exist) |
| `PI_CODING_AGENT_DIR` | Pi's project-local config dir; isolated under a temp dir in tests |

## What this adapter does NOT cover

- opencode-style native subagents — see [`@agent-controller/runtime-opencode`](https://www.npmjs.com/package/@agent-controller/runtime-opencode)
- Kubernetes / remote execution — planned v0.4 via the `Backend` interface in the CLI

## Building from source

```bash
git clone https://github.com/CCDevelopForFun/agent-controller.git
cd agent-controller/runtime
npm install --ignore-scripts
npm run build           # tsc → dist/
npm test                # vitest
```

`--ignore-scripts` is important: Pi's nested install hook will otherwise try to set up the user's `~/.pi` directory.

## Cross-references

- [agent-controller README](https://github.com/CCDevelopForFun/agent-controller#readme) — project overview + quickstart
- [Dual-adapter architecture overview](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/overview.md)
- [Harness capability matrix](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/harness-matrix.md) — per-feature support table

## License

MIT — see [LICENSE](https://github.com/CCDevelopForFun/agent-controller/blob/main/LICENSE).
