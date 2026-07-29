# Agent Controller

> **Declarative runtime for AI agents.** Define an agent in YAML (ADL — Agent Definition Language) and run the same spec on four different harnesses by changing one field.

Agent Controller separates **agent intent** — model, persona, tools, skills, MCP servers, guardrails, observability — from **execution substrate**. `agentctl` compiles a spec and dispatches it to a runtime adapter over a versioned stdio NDJSON protocol.

## Adapters

`runtime.type` selects the harness. Everything else in the spec stays the same.

| `runtime.type` | Harness | Package | Also needs |
|---|---|---|---|
| `local-pi` (or `local`) | Pi | `@agent-controller/runtime` | — |
| `local-opencode` | opencode | `@agent-controller/runtime-opencode` | `opencode` CLI on `PATH` |
| `local-codex` | Codex | `@agent-controller/runtime-codex` | `codex` CLI on `PATH`; `model.provider: openai` |
| `local-claude` | Claude Agent SDK | `@agent-controller/runtime-claude` | `model.provider: anthropic` |

Not every feature exists on every harness — Pi extensions are Pi-only, subagents are unsupported on Codex, and so on. The [capability matrix](docs/architecture/harness-matrix.md) is the authoritative per-field table.

## Quick start

```bash
# 1. agentctl — from the latest release, or:
go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@v0.7.0

# 2. the adapter you want (see the table above)
npm install -g @agent-controller/runtime

# 3. a self-contained spec — no registry refs, so it runs from anywhere
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

# 4. run it
export ANTHROPIC_API_KEY=sk-ant-...
AGENT_CONTROLLER_RUNTIME="$(npm root -g)/@agent-controller/runtime/dist/index.js" \
  agentctl run /tmp/hello.yaml
```

From a source clone, build the adapter you need and run from the repo root — the
adapter path and the `tools/` `extensions/` `skills/` `agents/` registry resolve
relative to cwd:

```bash
(cd runtime && npm install --ignore-scripts && npm run build)   # or runtime-opencode / -codex / -claude
(cd cli && go build -o bin/agentctl ./cmd/agentctl)
./cli/bin/agentctl run examples/hello.yaml
```

## Architecture

<p align="center">
  <img src="docs/architecture/architecture.svg" alt="Agent Controller architecture — agentctl dispatches a compiled spec over a stdio NDJSON wire protocol to the Pi, opencode, codex, or claude runtime adapter, which loads the local registry and calls the LLM provider" width="780">
</p>

The adapter loads the local registry — tools, extensions, skills, agents, MCP servers — and drives the session against the model provider. Backends beyond Local (Kubernetes skeleton, AgentCore) are reserved in the schema but not fully wired. See [`docs/architecture/overview.md`](docs/architecture/overview.md) for the layer breakdown and wire-protocol reference.

## Commands

```bash
agentctl validate spec.yaml                         # check ADL against JSON Schema
agentctl compile  spec.yaml                         # print the resolved CompiledSpec
agentctl run      spec.yaml                         # run once, stream NDJSON events
agentctl chat     spec.yaml                         # interactive REPL, sessions persist
agentctl serve    spec.yaml --port 8080             # long-lived HTTP/SSE server
agentctl sessions list                              # list persisted sessions
agentctl install  npm:<pkg>                         # install a Pi package/extension

# run flags
--task "override"          # override spec.task
--resume <session-id>      # continue a prior session
--binding binding.yaml     # resolve against a RuntimeBinding
--input k=v  --input k=@f  # ${inputs.k} interpolation; @ reads from a file
--output-file out.json     # capture the reply, validated by spec.outputSchema
--workspace ./run-42       # durable memory shared across steps
```

## What ADL can declare

| Field | Since | What it is |
|---|---|---|
| `model` | v0.0.1 | Provider + model name (Anthropic / OpenAI / Google) and optional temperature |
| `persona` | v0.0.1 | `role` + `instructions` — prepended to the system prompt |
| `task` | v0.0.1 | Initial prompt driving the session; supports `${inputs.<key>}` |
| `outputSchema` | v0.7 | JSON Schema — with `--output-file`, the reply is parsed, validated, written |
| `tools` | v0.0.1 | Tool allowlist — registry names or built-ins (`bash`, `read`, `edit`, `write`) |
| `extensions` | v0.0.1 | Pi extension allowlist — registry, or `source: npm:<pkg>` to auto-install |
| `skills` | v0.1.2 | Markdown skill files, inlined into the system prompt at session start |
| `subagents` | v0.1.3 | Child agents the parent can delegate to |
| `mcpServers` | v0.1.5 | MCP servers — `stdio` / `streamable-http` / `sse` |
| `guardrails` | v0.1.8 | Hallucination detector mode — `block` / `warn` / `correct` |
| `observability.tracing` | v0.5 | Emit OTel spans to an OTLP endpoint |
| `runtime` | v0.0.1 | `type` (see [Adapters](#adapters)) + optional `requirements` for capability matching |

Full schema: [`schemas/adl.v1alpha1.json`](schemas/adl.v1alpha1.json). Unknown fields are rejected at compile time.

## Feature guides

### Skills

Bodies are inlined into the system prompt at session start — no lazy loading. Declare by name; the runtime reads `skills/<name>/SKILL.md`. Vendoring an external skill is a file copy.

```yaml
spec:
  skills:
    - name: example-time-skill
  tools:
    - name: bash          # only if the skill prescribes shell commands
```

### MCP servers

Registered tools surface to the model as `mcp_<server>_<tool>`.

```yaml
spec:
  mcpServers:
    - name: time-server
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-time"]
      lifecycle: eager    # connect at session start; default is lazy
```

### Guardrails

Three layers defend against the model writing fake tool-call XML: an honesty preamble, skill-body framing, and a detector that scans every assistant message.

```yaml
spec:
  guardrails:
    hallucinationDetector: warn   # block (default) | warn | correct
```

`block` errors and exits non-zero · `warn` scrubs the XML and continues · `correct` also re-prompts once.

### Tracing

One `agentctl.run` root span per run (plus a `chat.turn` span per REPL turn), with adapter, LLM, and tool spans nested underneath. Set `observability.tracing: true` and point the exporter anywhere OTLP-compatible:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 agentctl run examples/tracing-demo.yaml
```

### Sessions

`agentctl chat` persists across process restarts via SQLite at `$XDG_DATA_HOME/agent-controller/sessions.db`. A resumed session keeps full turn history. Retire idle ones with `agentctl sessions sweep --ttl 168h`.

### RuntimeBinding

Maps a spec to a concrete execution target. The CLI matches `runtime.requirements` against the target's advertised capabilities — unmet requirements warn and proceed, unless the binding sets `target.strict: true`.

```yaml
apiVersion: agent-controller.dev/v1alpha1
kind: RuntimeBinding
metadata: { name: local-default }
spec:
  selector:
    runtimeType: local-pi
    capabilities: { streaming: true, sandbox: false }
  target:
    type: local
```

```bash
agentctl run spec.yaml --binding examples/bindings/local-default.yaml
```

### Scheduler tasks

Since v0.7, `agentctl run` is a **per-step task primitive**: an external scheduler (Maestro, Airflow, Temporal) owns the DAG and agent-controller runs one agent per step. No workflow YAML, no in-process engine — just parameterize (`--input`), capture (`--output-file`), and share memory (`--workspace`). Copy-paste configs live in [`examples/schedulers/`](examples/schedulers/).

## Serving over HTTP

`agentctl serve` wraps any spec in a long-lived HTTP/SSE server so schedulers, UIs, and tests can open sessions and exchange turns without a process per request.

```bash
agentctl serve examples/hello.yaml --port 8080
```

Flags, endpoints, and a runnable client: [`docs/serve.md`](docs/serve.md).

## Examples

Runnable specs live in [`examples/`](examples/) — one `hello-*.yaml` per adapter, plus MCP, skills, subagents, guardrail modes, tracing, RuntimeBindings, and scheduler integrations. See [`examples/README.md`](examples/README.md) for the annotated index.

## Project

- Repository layout and contributor conventions: [`AGENTS.md`](AGENTS.md)
- Release history: [`CHANGELOG.md`](CHANGELOG.md) · direction: [`ROADMAP.md`](ROADMAP.md)
- Dev setup and workflow: [`CONTRIBUTING.md`](CONTRIBUTING.md) · security: [`SECURITY.md`](SECURITY.md)

Developer preview — the ADL surface is stable enough to build on, but `v1alpha1` still means breaking changes are possible.

## License

MIT.
