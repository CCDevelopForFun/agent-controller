# `@agent-controller/runtime-opencode` — opencode runtime adapter

Node/TypeScript adapter that runs an [agent-controller](https://github.com/CCDevelopForFun/agent-controller) `CompiledSpec` against the [sst/opencode](https://opencode.ai) CLI via the official `@opencode-ai/sdk`, and emits the NDJSON wire-event stream that `agentctl` consumes. Sibling to the Pi adapter ([`@agent-controller/runtime`](https://www.npmjs.com/package/@agent-controller/runtime)); both consume the same `CompiledSpec` and emit the same wire-event format.

Requires **Node 22+**.

## Install

```bash
npm install -g @agent-controller/runtime-opencode
# or per-project
npm install --save-dev @agent-controller/runtime-opencode
```

The `opencode` CLI must also be on `PATH`. Install with `npm install -g opencode-ai` or follow https://opencode.ai/docs/.

## Use with `agentctl`

`agentctl` (the Go CLI) spawns this package as a subprocess when `spec.runtime.type` is `local-opencode`. The example specs under [`examples/`](https://github.com/CCDevelopForFun/agent-controller/tree/main/examples) reference cwd-relative registry entries that ship with the source repo but not with this npm package. For an npm-only install, use a self-contained spec:

```bash
# 1) Write a self-contained spec (no tools[]/extensions[]/skills[]/subagents[])
cat > /tmp/hello-opencode.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: hello-opencode }
spec:
  model: { provider: anthropic, name: claude-sonnet-4-20250514 }
  persona: { role: Helpful demo, instructions: Answer concisely. }
  task: Say hello.
  tools: []
  runtime: { type: local-opencode }
EOF

# 2) Point agentctl at the adapter and run
export ANTHROPIC_API_KEY=sk-ant-...

# global install
AGENT_CONTROLLER_RUNTIME="$(npm root -g)/@agent-controller/runtime-opencode/dist/index.js" \
  agentctl run /tmp/hello-opencode.yaml

# or per-project
AGENT_CONTROLLER_RUNTIME="./node_modules/@agent-controller/runtime-opencode/dist/index.js" \
  agentctl run /tmp/hello-opencode.yaml
```

Credentials via environment: `ANTHROPIC_API_KEY` (and / or `ANTHROPIC_BASE_URL`). Auth from `~/.opencode/auth.json` is intentionally NOT used — the adapter isolates `HOME` to prevent ambient config leakage.

`agentctl` (matching version) is available from the [GitHub Releases page](https://github.com/CCDevelopForFun/agent-controller/releases) as cross-platform binaries.

## Architecture role

This package is the **subprocess** spawned by `agentctl run` when `spec.runtime.type` is `local-opencode`. It:

1. Reads a `CompiledSpec` JSON document from stdin
2. Rejects unsupported ADL surface (`spec.sessionId`) at startup with a clear error (other opencode-incompatible shapes — `spec.extensions[]`, `spec.installs[]`, custom Pi-extension tools, built-ins with `config` — are rejected at `agentctl compile` since v0.3.0)
3. Builds an opencode-native config (`cfg.agent[primary]`, `cfg.agent[subagent_N]`, `cfg.mcp[server_N]`) from the spec via `buildOpencodeConfig`
4. Spawns opencode via `@opencode-ai/sdk`'s `createOpencode()` with an isolated `HOME` / `XDG_*` / cwd so ambient user config can't leak undeclared capabilities
5. Submits the task via `session.promptAsync()` (non-blocking)
6. Consumes the SSE event stream via a producer-consumer queue
7. Translates opencode events into wire-protocol events (tool.call/result, message, session.ended/error/warning, hallucination warnings)
8. Disables opencode's native agents (`plan`, `build`, `general`, `explore`, `scout`) so the `task` tool can't bypass the ADL allowlist

## ADL allowlist preservation

Several layers of defense keep ADL's "only declared capabilities are reachable" contract intact:

1. Opencode permissions start from a deny baseline (`"*": "deny"` + every known opencode tool explicitly denied), then declared tools become `allow`
2. Native opencode agents are disabled so the `task` tool can't delegate to undeclared agents
3. MCP server names are validated against `[A-Za-z0-9._-]+` (no glob metacharacters), against opencode built-in prefixes (`repo`, `doom`, `external`), and for sanitization collisions
4. Opencode-incompatible spec shapes are rejected at `agentctl compile` (v0.3.0)

## Capability differences vs the Pi adapter

See [`docs/architecture/harness-matrix.md`](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/harness-matrix.md) for the per-feature table. Highlights:

| Feature | Pi adapter | opencode adapter |
|---|---|---|
| Custom Pi-format tools / extensions | ✅ | ❌ rejected at `agentctl compile` |
| Session resume (`--resume`) | ✅ | ❌ rejected by `agentctl run` |
| `task` tool / native subagent delegation | via vendored ext | ✅ native |
| `cancelled` reason on SIGINT | ❌ (surfaces as `error`) | ✅ |
| `bash` allowlist via tool `config` | ❌ rejected at `agentctl compile` (use [`@gotgenes/pi-permission-system`](https://www.npmjs.com/package/@gotgenes/pi-permission-system)) | ❌ rejected at `agentctl compile` |

## Building from source

```bash
git clone https://github.com/CCDevelopForFun/agent-controller.git
cd agent-controller/runtime-opencode
npm install --ignore-scripts
npm run build           # tsc → dist/
npm test                # vitest
```

## Cross-references

- [agent-controller README](https://github.com/CCDevelopForFun/agent-controller#readme) — project overview + quickstart
- [Dual-adapter architecture overview](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/overview.md)
- [Harness capability matrix](https://github.com/CCDevelopForFun/agent-controller/blob/main/docs/architecture/harness-matrix.md)

## License

MIT — see [LICENSE](https://github.com/CCDevelopForFun/agent-controller/blob/main/LICENSE).
