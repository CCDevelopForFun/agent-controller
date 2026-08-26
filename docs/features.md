# Features — and how to verify each locally

This document enumerates Agent Controller features and gives a concrete local-verification command for each. Use it as a smoke-test checklist after building from source.

> **Coverage note:** Sections 1–6 document the v0.1–v0.3 surface by feature area; Sections 7–10 cover the later version tracks — v0.4 (Kubernetes backend + `RuntimeBinding`), v0.5 (OpenTelemetry tracing), v0.6 (`chat` REPL + durable sessions), and v0.7 (scheduler task surface). Section 11 lists known gaps. Each command is tagged **[hermetic]** (no API key or infra needed), **[LIVE]** (needs `ANTHROPIC_API_KEY`), or **[infra]** (needs a cluster / OTLP collector).

> **Build prerequisites:** Go ≥ 1.25 (bumped in slice 5.1 for OTel SDK), Node.js ≥ 22, npm, `jq` (used in several verification commands below). Optional: `opencode` CLI on PATH (for opencode-adapter checks), `ANTHROPIC_API_KEY` (for live-model checks).

## Section 0 — Set up your local repo

```bash
git clone https://github.com/CCDevelopForFun/agent-controller.git
cd agent-controller

# Build both runtime adapters + the CLI
(cd runtime && npm install --ignore-scripts && npm run build)
(cd runtime-opencode && npm install --ignore-scripts && npm run build)
(cd cli && go build -o bin/agentctl ./cmd/agentctl)

# Quick sanity check — CLI shows its help
./cli/bin/agentctl --help
```

Everything else in this document assumes you ran the above and are sitting at the repo root.

### Alternative: `go install` + npm adapters

```bash
go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@v0.7.0

# On the very first install of a freshly-pushed tag, proxy.golang.org
# may return 404 until it indexes the tag (~minutes). Workaround:
GOPROXY=direct GOSUMDB=off go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@v0.7.0
```

This puts `agentctl` in `$GOBIN` if set, otherwise `$(go env GOPATH)/bin` (default `~/go/bin`). Make sure that directory is on your `PATH`.

For `run`, also install the adapter(s) and point `agentctl` at them:

```bash
npm install -g @agent-controller/runtime              # for runtime.type: local / local-pi
npm install -g @agent-controller/runtime-opencode     # for runtime.type: local-opencode

AGENT_CONTROLLER_RUNTIME="$(npm root -g)/@agent-controller/runtime/dist/index.js" \
  agentctl run my-self-contained-spec.yaml
```

**What works from anywhere** (standalone `go install`, no source tree, no npm adapter):

- `agentctl validate <spec.yaml>` — schema check, no filesystem dependencies
- `agentctl compile <spec.yaml>` — works for **self-contained specs** (no `tools`/`extensions`/`skills`/`subagents` blocks that reference local registry entries). `registry.Scan` silently skips missing directories, so a spec with `tools: []` and no extensions/skills/subagents compiles fine from `/tmp`.

**What needs the npm adapter** (anywhere; no source tree):

- `agentctl run <spec.yaml>` for self-contained specs — set `AGENT_CONTROLLER_RUNTIME` as above.

**What needs the source tree (cwd-relative scan):**

- `agentctl compile <spec.yaml>` where the spec references registry entries — fails with `tool "X" not found in registry` if `<cwd>/tools/X/` doesn't exist. Example: `examples/hello.yaml` declares `tools: [get_time]` and `extensions: [audit-log]`, both of which need `<cwd>/tools/get_time/` and `<cwd>/extensions/audit-log/` to be present.
- `agentctl run <spec.yaml>` for registry-backed specs — same registry requirement as `compile`.

---

## Section 1 — CLI commands (Go binary)

All commands are hermetic except `run` (spawns a runtime adapter; may call a model) and `install` (shells out to `pi install`, which fetches packages and mutates the user's Pi package store under `~/.pi/agent/npm/`).

### 1.1 `agentctl validate` — schema-check an ADL YAML file

```bash
./cli/bin/agentctl validate examples/hello.yaml
# expected: prints "ok"

./cli/bin/agentctl validate examples/hello-opencode.yaml
# expected: prints "ok"

# Negative test: validate a non-existent spec
./cli/bin/agentctl validate /tmp/does-not-exist.yaml
# expected: non-zero exit + clear "no such file" error
```

### 1.2 `agentctl compile` — print the normalized CompiledSpec JSON

```bash
./cli/bin/agentctl compile examples/hello.yaml | head -30
# expected: JSON document with `v: 1`, `metadata`, `model`, `task`, `tools`, etc.
```

### 1.3 `agentctl run` — execute a spec (LIVE MODEL CALL — needs API key)

```bash
export ANTHROPIC_API_KEY=sk-ant-...

# Pi adapter — full feature set
./cli/bin/agentctl run examples/hello.yaml
# expected: streams "session.started", "tool.call" (get_time), "tool.result",
#           "message" (assistant), "session.ended" (reason=completed)

# opencode adapter — same wire-event shape, different runtime substrate
./cli/bin/agentctl run examples/hello-opencode.yaml
# expected: streams "session.started", "message" (assistant), "session.ended"
#           (opencode-compatible spec has tools: [], so no tool events)
```

Useful flags:

| Flag | What it does | Verify |
|---|---|---|
| `--task "override"` | Override `spec.task` from the CLI | Append `--task "Just say hello, don't call any tools"` and observe the spec's original task isn't used |
| `--raw-out events.ndjson` | Also write the raw NDJSON wire-event stream to a file | Run with `--raw-out /tmp/run.ndjson`; `cat /tmp/run.ndjson \| jq -r '.type'` lists every event type |
| `--resume <session-id>` | Continue a previous session (**Pi only**) | Sessions persist under `~/.pi/agent/sessions/agentctl/<id>`; re-running with `--resume <id>` continues an existing session. Trying the same flag with `runtime.type: local-opencode` fails with a clear "not yet supported" error |
| `--no-staleness-check` | Skip the dist/ vs src/ freshness check | Useful for hot-reload workflows; otherwise the CLI refuses to launch if `runtime-opencode/dist/` is older than `runtime-opencode/src/` |

### 1.4 `agentctl sessions list` — list persisted Pi sessions

```bash
./cli/bin/agentctl sessions list
# expected if sessions exist: a table with columns ID / LAST MODIFIED /
#   SIZE, one row per directory under ~/.pi/agent/sessions/agentctl/
# expected on a fresh install: the literal line "no sessions yet"
#   (printed before the header row is even produced)
```

(Opencode session listing is NOT yet supported — see Section 5.)

### 1.5 `agentctl install` — delegate to Pi's package system

```bash
# Install a single Pi-format npm package (Pi must be installed locally)
./cli/bin/agentctl install extension pi-mcp-extension          # alias for: pi install npm:pi-mcp-extension
./cli/bin/agentctl install npm:any-pi-pkg                      # direct npm: prefix
./cli/bin/agentctl install --from examples/with-installs.yaml  # iterate the legacy spec.installs[] list
```

Note: `--from <spec.yaml>` reads `spec.installs[]` (the legacy field), **not** `spec.extensions[]`. The latter auto-installs at session start via `spec.extensions[].source: npm:<pkg>`.

---

## Section 2 — ADL surface (per `spec` field)

This section walks through every field you can put in a spec, with a one-liner verify command for each.

### 2.1 `spec.model` — provider, name, temperature

Supported providers: `anthropic`, `openai`, `google`. Other providers are rejected by the schema enum.

```bash
# Schema rejects unknown providers
echo 'apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: {name: hello}
spec:
  model: {provider: badprovider, name: x}
  task: hi
  tools: []
  runtime: {type: local}' > /tmp/bad.yaml
./cli/bin/agentctl validate /tmp/bad.yaml
# expected: validation error citing the `provider` enum
```

### 2.2 `spec.persona` — role + instructions

Both fields are folded into the system prompt. Verify by inspecting the compiled spec:

```bash
./cli/bin/agentctl compile examples/hello.yaml | jq '.persona'
# expected: {"role": "Helpful demo assistant", "instructions": "Greet the user..."}
```

### 2.3 `spec.task` — the initial prompt

Verify the override semantic:

```bash
./cli/bin/agentctl compile examples/hello.yaml | jq -r '.task'
# expected: "Tell me the current UTC time.\n"
```

### 2.4 `spec.tools[]` — tool allowlist (built-ins + custom Pi extensions)

Built-ins (`bash`, `read`, `edit`, `write`) are recognized by name; custom tools must live under `tools/<name>/` with a manifest.

**v0.1.11 change:** `spec.tools[].config` on a built-in is now **rejected by `agentctl compile`** (the field was a silent governance no-op pre-v0.1.11). NOTE: `agentctl validate` (JSON Schema only) does NOT catch this today — CI gates that stop at `validate` will still pass the silently-broken spec. Always run `compile` in CI to catch the case. For bash command allowlisting, use the [`@gotgenes/pi-permission-system`](https://www.npmjs.com/package/@gotgenes/pi-permission-system) extension — see [`examples/bash-allowlist.yaml`](../examples/bash-allowlist.yaml).

```bash
# Verify the rejection works
cat > /tmp/bad-bash.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: bad }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  task: hi
  tools:
    - name: bash
      config: { allowedCommands: [ls, cat] }
  runtime: { type: local }
EOF
./cli/bin/agentctl compile /tmp/bad-bash.yaml; echo "exit=$?"
# expected: exit=1, stderr names "bash" and points at @gotgenes/pi-permission-system
```

```bash
# Verify get_time is discovered from tools/get_time/manifest.yaml
./cli/bin/agentctl compile examples/hello.yaml | jq '.tools'
# expected: [{"name": "get_time", "entrypoint": "<abs path>/tools/get_time/entrypoint.ts", ...}]

# opencode + custom Pi-extension tool is rejected at `agentctl compile` time
# (v0.3.4 moved this from adapter startup to the compiler). Construct a spec
# that mixes the opencode adapter with a Pi-only field and confirm.
cat > /tmp/bad-opencode.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: bad-opencode }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  task: hi
  tools:
    - name: get_time         # custom Pi-extension tool — opencode rejects
  runtime: { type: local-opencode }
EOF
./cli/bin/agentctl compile /tmp/bad-opencode.yaml 2>&1; echo "exit=$?"
# expected: exit=1, stderr mentions runtime "local-opencode" + custom
#   Pi-extension tool incompatibility (the compiler aggregates all three
#   opencode-incompatible spec shapes into one error)
```

### 2.5 `spec.extensions[]` — Pi extension allowlist

Supported on Pi adapter only. Two forms:

```yaml
spec:
  extensions:
    # Form A: registry-resolved (extensions/<name>/ must exist locally)
    - name: audit-log
      config: { path: ./audit.log }

    # Form B: source-bound auto-install (v0.1.6+)
    - name: pi-mcp-extension
      source: npm:pi-mcp-extension
```

Verify:

```bash
./cli/bin/agentctl compile examples/hello.yaml | jq '.extensions'
# expected: [{"name": "audit-log", "entrypoint": "...", "config": {"path": "./audit.log"}}]

./cli/bin/agentctl compile examples/self-contained-mcp.yaml | jq '.extensions'
# expected: [{"name": "pi-mcp-extension", "source": "npm:pi-mcp-extension"}]
# (no `entrypoint` field — source-bound extensions resolve at session start;
#  the field is omitted from JSON via Go's `omitempty` tag)
```

opencode + non-empty `spec.extensions[]` is rejected at `agentctl compile` time (v0.3.4).

```bash
cat > /tmp/bad-opencode-ext.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: bad-opencode-ext }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  task: hi
  tools: []
  extensions:
    - name: audit-log
      config: { path: ./audit.log }
  runtime: { type: local-opencode }
EOF
./cli/bin/agentctl compile /tmp/bad-opencode-ext.yaml 2>&1; echo "exit=$?"
# expected: exit=1, stderr names spec.extensions[] as the incompatible field
```

### 2.6 `spec.skills[]` — markdown skill bodies inlined into the system prompt

Active-by-default since v0.1.7 (Pi); v0.2 slice 2.5 brought parity to opencode.

```bash
ls skills/
# expected: example-time-skill  using-superpowers

./cli/bin/agentctl compile examples/claude-skills-demo.yaml | jq '.skills'
# expected: array with entrypoint paths to SKILL.md files

# Confirm the SKILL.md content the adapter will inline
head -20 skills/example-time-skill/SKILL.md
# expected: YAML frontmatter (name: example-time-skill) followed by the
#           "Time Formatting" rules body that the adapter prepends to
#           the system prompt
```

The system prompt itself is NOT emitted on the wire — to actually observe a skill influencing model output, run the spec against a live API key and look at the assistant `message` events for behavior consistent with the skill's rules (e.g. with `example-time-skill`, every timestamp the model produces should be ISO-8601 UTC).

### 2.7 `spec.subagents[]` — hierarchical agent delegation

```bash
ls agents/
# expected: sql-explorer.md

./cli/bin/agentctl compile examples/subagent-demo.yaml | jq '.subagents'
# expected: [{"name": "sql-explorer", "entrypoint": "<abs>/agents/sql-explorer.md"}]
```

Pi adapter: spawns child `pi` processes via the vendored subagent extension. Opencode adapter: registers each subagent as `cfg.agent[name]` with `mode: "subagent"`.

### 2.8 `spec.mcpServers[]` — MCP server registration

Three transports: `stdio`, `streamable-http`, `sse`.

```bash
./cli/bin/agentctl compile examples/mcp-time.yaml | jq '.mcpServers'
# expected: array with name, transport, and command/args (stdio) or url (http/sse)
```

Pi adapter writes `<cwd>/.pi/mcp.json` from this list and loads `pi-mcp-extension`. Opencode adapter writes `cfg.mcp[name]` directly to opencode's native MCP config.

### 2.9 `spec.guardrails.hallucinationDetector` — block / warn / correct

```bash
# Three demo specs, one per mode
./cli/bin/agentctl validate examples/guardrails-block.yaml      # mode: block (v0.1.10 default)
./cli/bin/agentctl validate examples/guardrails-warn.yaml       # mode: warn
./cli/bin/agentctl validate examples/guardrails-correct.yaml    # mode: correct
```

To see modes in action, run a spec under one of the guardrail-demo files with a live API key. The relevant event types are `warning` (warn / correct), `error` (block), and `session.ended.reason=error` (block).

### 2.10 `spec.runtime.type` — adapter selector

```bash
# Three accepted values
grep "type:" examples/hello.yaml examples/hello-opencode.yaml
# expected:
#   examples/hello.yaml:     type: local             ← legacy v0.1.x alias for local-pi
#   examples/hello-opencode.yaml: type: local-opencode

# Validate that the schema accepts each
./cli/bin/agentctl validate examples/hello.yaml             # local
./cli/bin/agentctl validate examples/hello-opencode.yaml    # local-opencode
```

The CLI dispatches to the appropriate adapter via `resolveRuntimeCommand` in `cli/cmd/agentctl/main.go`.

### 2.11 `spec.installs[]` — deprecated install list

The `spec.installs[]` field is parsed by the compiler and carried through to the runtime, but neither adapter installs from it at session start. Verify the parse path without running a model:

```bash
./cli/bin/agentctl compile examples/with-installs.yaml | jq '.installs'
# expected: an array of package names (e.g. ["pi-mcp-extension"])
```

The Pi adapter logs a deprecation warning to its own stderr when `installs[]` is non-empty, but `LocalBackend` in the CLI buffers child stderr and only surfaces it on startup crashes, so it won't appear during normal `agentctl run` output. To observe the warning directly:

```bash
# Pipe a CompiledSpec straight into the Pi adapter binary, bypassing the CLI's
# stderr capture. The warning shows up on the adapter's stderr.
./cli/bin/agentctl compile examples/with-installs.yaml \
  | node runtime/dist/index.js 2>&1 >/dev/null \
  | grep -i "spec.installs\|deprecated" || true
# expected: a line mentioning spec.installs being deprecated
# (the adapter may then attempt to start a session; ctrl-C is fine)
```

The opencode adapter rejects non-empty `spec.installs[]` at `agentctl compile` time (v0.3.4 moved this from adapter startup to the compiler) with a clear error.

### 2.12 `spec.runtime.requirements` — capability declarations (v0.3.1+)

Free-form boolean map declaring what capabilities the runtime must provide. Verify the parse path:

```bash
cat > /tmp/agent-with-reqs.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: needs-stuff }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  task: hi
  tools: []
  runtime:
    type: local
    requirements:
      streaming: true
      sandbox: true
      gpu: false
EOF
./cli/bin/agentctl compile /tmp/agent-with-reqs.yaml | jq '.runtime.requirements'
# expected: {"gpu":false,"sandbox":true,"streaming":true}
# (keys alphabetical — Go's encoding/json sorts map keys)
```

Reserved well-known keys: `streaming`, `sandbox`, `gpu`, `restrictedNetwork`, `ephemeralFilesystem`. Arbitrary additional keys accepted (e.g. `spark`, `notebookContext`). Enforcement runs in `Backend.Resolve()` against the active `RuntimeBinding` — see section 2.13.

### 2.13 `RuntimeBinding` resource kind (v0.3.2+)

A separate top-level resource (`kind: RuntimeBinding`) that advertises what capabilities a target provides. Used by the matcher to decide whether to warn or fail when an Agent declares requirements the target can't satisfy.

```bash
# Validate a Binding (uses the new kind-dispatching validator)
./cli/bin/agentctl validate examples/bindings/local-default.yaml
# expected: ok

./cli/bin/agentctl validate examples/bindings/local-strict.yaml
# expected: ok

# Trying to compile/run a Binding (it's not an Agent) fails with a clear error
./cli/bin/agentctl compile examples/bindings/local-default.yaml 2>&1 | head -1
# expected: error mentioning "kind: Agent" requirement
```

### 2.14 `agentctl run --binding <path>` + capability matcher (v0.3.3+)

The matcher is activated by passing `--binding <path>` to `agentctl run`. The Binding's `selector.runtimeType` must equal the Agent's `spec.runtime.type` — the shipped `examples/bindings/local-default.yaml` selects `local-pi`, so Agents using it need `runtime.type: local-pi` (NOT the legacy alias `local`).

A spec/binding pair that matches:

```bash
# Make sure the spec uses local-pi (not the legacy `local` alias) so the
# Binding's selector matches:
sed 's/type: local$/type: local-pi/' examples/hello.yaml > /tmp/hello-local-pi.yaml

ANTHROPIC_API_KEY=... ./cli/bin/agentctl run /tmp/hello-local-pi.yaml \
    --binding examples/bindings/local-default.yaml
```

(Passing `examples/hello.yaml` directly with the shipped binding gives a hard error "binding ... targets runtime.type 'local-pi' but the spec declares 'local'" — that's the selector check working as designed.)

Behavior matrix:

- **No `--binding`** → resolver is a no-op pass-through (v0.2.x semantics preserved)
- **`--binding` + all requirements met** → no warnings, run proceeds
- **`--binding` + some `requirement: true` unmet, default mode** → one wire `warning` event per unmet capability (kind `unmet_runtime_requirement`), run proceeds
- **`--binding` + some `requirement: true` unmet, `target.strict: true`** → hard error, `agentctl run` exits 1 before any session.started
- **`--binding` selector runtime-type mismatch** → hard error regardless of strict mode (wiring bug, not a capability gap)

Quick verify the warn-but-proceed path without a real API key:

```bash
cat > /tmp/agent-needs-stuff.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: needs-stuff }
spec:
  model: { provider: anthropic, name: claude-sonnet-5 }
  task: hi
  tools: []
  runtime:
    type: local-pi
    requirements: { gpu: true, sandbox: true }
EOF

# The CLI dispatches the runtime via `node <runtime-command>`, so the fake
# must be valid JS (not a shell binary like /bin/true). Use a stub that
# emits a single session.ended event so the CLI doesn't synthesize an error
# event for "session never started." The matcher's warnings are written to
# --raw-out BEFORE the adapter spawns, so /tmp/r.ndjson captures them.
cat > /tmp/fake-runtime.js <<'EOF'
const ev = { v: 1, type: "session.ended", ts: new Date().toISOString(),
             sessionId: "fake", data: { reason: "ok" } };
process.stdout.write(JSON.stringify(ev) + "\n");
process.exit(0);
EOF
AGENT_CONTROLLER_RUNTIME=/tmp/fake-runtime.js ./cli/bin/agentctl run /tmp/agent-needs-stuff.yaml \
    --binding examples/bindings/local-default.yaml --raw-out /tmp/r.ndjson >/dev/null 2>&1
grep -c "unmet_runtime_requirement" /tmp/r.ndjson
# expected: 2 (gpu + sandbox unmet, sorted alphabetically)
```

Strict mode (`local-strict.yaml`) refuses the same spec instead — the matcher
errors out before the adapter ever spawns, so the fake runtime is unused:

```bash
AGENT_CONTROLLER_RUNTIME=/tmp/fake-runtime.js ./cli/bin/agentctl run /tmp/agent-needs-stuff.yaml \
    --binding examples/bindings/local-strict.yaml >/dev/null 2>&1
echo "exit=$?"
# expected: exit=1 (hard error before any subprocess starts)
```

---

## Section 3 — Adapter capabilities side-by-side

The full per-feature table lives in [`docs/architecture/harness-matrix.md`](architecture/harness-matrix.md). Quick summary of where the adapters differ:

| Feature | Pi adapter (`local` / `local-pi`) | opencode adapter (`local-opencode`) |
|---|---|---|
| Custom Pi-extension tools | ✅ | ❌ rejected at `agentctl compile` |
| `spec.extensions[]` (any kind) | ✅ | ❌ rejected at `agentctl compile` |
| Built-in tools with `config` (e.g. bash allowlist) | ❌ rejected at `agentctl compile` — use [`@gotgenes/pi-permission-system`](https://www.npmjs.com/package/@gotgenes/pi-permission-system) as a `spec.extensions[]` entry | ❌ rejected at `agentctl compile` |
| Session resume (`--resume`) | ✅ | ❌ rejected by `agentctl run` |
| `audit-log` extension | ✅ | ❌ not implemented |
| Skills inlined into prompt | ✅ | ✅ |
| Subagent delegation | ✅ via vendored ext | ✅ via opencode native |
| MCP servers | ✅ via `pi-mcp-extension` | ✅ via opencode native `cfg.mcp` |
| Hallucination guardrails | ✅ | ✅ |
| `session.ended { reason: "cancelled" }` on SIGINT | ❌ surfaces as `error` | ✅ |

Verify the opencode column has zero 🚧 cells (Phase 2 acceptance gate):

```bash
# Each adapter row in the matrix uses the layout "| Feature | Pi | opencode | hermes-agent |"
# Column $4 (split on '|') is the opencode cell. Count rows where that cell is 🚧.
awk -F'|' '/🚧/ { if ($4 ~ /🚧/) print }' docs/architecture/harness-matrix.md | wc -l
# expected: 0
```

A plain `grep -c "🚧" docs/architecture/harness-matrix.md` will return a non-zero number because the hermes-agent column (which is intentionally deferred — see [`ROADMAP.md`](../ROADMAP.md)) still contains 🚧 cells.

---

## Section 4 — Test suites (hermetic, no API key needed)

### 4.1 Go test suite — CLI internals

```bash
(cd cli && go test ./...)
# expected: 5 packages OK
#   - github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl
#   - github.com/CCDevelopForFun/agent-controller/cli/internal/adl
#   - github.com/CCDevelopForFun/agent-controller/cli/internal/backend
#   - github.com/CCDevelopForFun/agent-controller/cli/internal/registry
#   - github.com/CCDevelopForFun/agent-controller/cli/internal/wire
```

Covers: ADL compilation, JSON Schema validation, embedded-schema drift detection (slice 1.2), manifest registry scanning, backend subprocess dispatch, wire-event NDJSON decoding.

### 4.2 Pi adapter tests

```bash
(cd runtime && npm test)
# expected: 4 test files, 92 tests passing
#   - src/adapter.test.ts            (Pi session loop)
#   - src/e2e/runsession-fake.test.ts (full E2E against pi-ai's faux provider)
#   - src/testing/fake-provider.test.ts
#   - src/wire.test.ts
```

`runsession-fake.test.ts` is the hermetic Layer-1 E2E added in slice 1.1 — it exercises `runSession` end-to-end without an API key by intercepting at the `pi-ai` api-registry layer.

### 4.3 opencode adapter tests

```bash
(cd runtime-opencode && npm test)
# expected: 5 test files, 75 tests passing
#   - src/event-translator.test.ts   (opencode SSE → wire-event translation)
#   - src/index.test.ts              (subprocess dispatch + spec validation + unsupported-field rejection)
#   - src/opencode-config.test.ts    (ADL → opencode config mapper, allowlist preservation)
#   - src/sdk-smoke.test.ts          (@opencode-ai/sdk shape smoke test)
#   - src/wire.test.ts
```

Covers every code path through `buildOpencodeConfig`, the event translator, and the adapter plumbing. **Does NOT** exercise the opencode subprocess actually calling a model — that requires an Anthropic-API mock server (v0.3 follow-up).

### 4.4 All-suite check (matches CI)

```bash
(cd cli && go test ./...) && \
  (cd runtime && npm run build && npm test) && \
  (cd runtime-opencode && npm run build && npm test)
# expected: all green
```

---

## Section 5 — End-to-end against a live model

This requires `ANTHROPIC_API_KEY`. Without it, `e2e/run.sh` is a no-op.

```bash
# Pi adapter live E2E
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ./e2e/run.sh
# expected: assertions on session.started, tool.call (get_time), tool.result, session.ended

# opencode adapter live E2E (also requires `opencode` CLI on PATH)
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ADAPTER=opencode ./e2e/run.sh
# expected: assertions on session.started + session.ended (no tool events; spec has tools: [])
```

---

## Section 6 — Wire protocol

The CLI ↔ runtime contract is **NDJSON-on-stdout** with this envelope (current `v: 1`):

```json
{
  "v": 1,
  "type": "tool.call",
  "ts": "2026-06-03T16:34:50.123Z",
  "sessionId": "s_abc123",
  "data": { ... }
}
```

Event types emitted by both adapters:
`session.started`, `model.request` (Pi only), `model.response` (Pi only), `tool.call`, `tool.result`, `message`, `warning`, `error`, `session.ended`.

Verify the envelope shape:

```bash
ANTHROPIC_API_KEY=sk-ant-... ./cli/bin/agentctl run examples/hello.yaml --raw-out /tmp/run.ndjson > /dev/null
jq -c '.' /tmp/run.ndjson | head -3
# expected: every line has v:1, type, ts, sessionId, data
```

Wire-protocol versioning is documented in [`versioning.md`](versioning.md). The planned migration to `apiVersion: agent-controller.dev/events/v1alpha1` is a v0.5+ track.

---

## Section 7 — Kubernetes backend + RuntimeBinding (v0.4)

v0.4 generalized the local-only model into a `Backend` interface and shipped the first remote backend (Kubernetes, skeleton). A **`RuntimeBinding`** is a separate resource mapping an Agent's abstract `runtime.requirements` to a concrete deployment target; `agentctl run --binding <file>` activates the capability matcher. (Per the 2026-06-10 roadmap pivot, the K8s backend is opportunistic — enough to prove the `Backend` interface against a real cluster.)

### 7.1 RuntimeBindings validate like Agents **[hermetic]**

```bash
./cli/bin/agentctl validate examples/bindings/local-default.yaml      # expected: ok
./cli/bin/agentctl validate examples/bindings/local-strict.yaml       # expected: ok
./cli/bin/agentctl validate examples/bindings/kubernetes-kind.yaml    # expected: ok
```

`agentctl validate` dispatches on `kind:` (added v0.3.2) — it accepts both `Agent` and `RuntimeBinding`.

### 7.2 The capability matcher **[hermetic]**

Two distinct checks run at `Resolve`, before any backend/cluster work:

**Selector match — always fatal.** A binding's `selector.runtimeType` must equal the Agent's `spec.runtime.type`; a mismatch is a hard error *regardless* of `target.strict`:

```bash
# local-strict selects runtimeType "local-pi"; hello.yaml declares runtime.type "local"
./cli/bin/agentctl run examples/hello.yaml --binding examples/bindings/local-strict.yaml --no-staleness-check
# expected: exit≠0,
#   Error: binding "local-strict" targets runtime.type "local-pi" but the spec
#   declares "local" — the selector does not match. …
# (local-default.yaml fails identically — a selector mismatch ignores strict.)
```

**Capability match — strict-gated.** When the runtimeType *does* match but the binding's advertised `capabilities` don't satisfy the Agent's `spec.runtime.requirements`, `target.strict: true` makes it a hard error; the default (`strict` false, e.g. `local-default.yaml`) emits a warn-but-proceed stderr warning and runs anyway.

### 7.3 Submit to a Kubernetes cluster **[infra — needs a `kind` cluster]**

`examples/bindings/kubernetes-kind.yaml` runs the agent in a Pod built from the `agent-runtime-base` image. It needs a cluster + a `Secret` carrying `ANTHROPIC_API_KEY` (see the prerequisites in that file's comments). The `KubernetesBackend` submits a Pod + spec-`Secret`, streams the in-Pod agentctl's NDJSON wire events back to your terminal, and tears the Pod down on Ctrl-C.

```bash
# one-time: kind create cluster … ; kind load docker-image … ; kubectl create secret … (see the binding file)
./cli/bin/agentctl run <self-contained-spec>.yaml --binding examples/bindings/kubernetes-kind.yaml
# expected: the same wire-event stream as a local run, sourced from the Pod
```

---

## Section 8 — OpenTelemetry tracing (v0.5)

v0.5 added end-to-end OTel tracing: a host `agentctl.run` root span, OTLP/HTTP export, W3C `TRACEPARENT` propagation into the runtime adapters (and K8s Pods), and per-LLM / per-tool / per-session child spans using GenAI semantic-convention attributes (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.operation.name`). Two-condition opt-in: `spec.observability.tracing: true` **and** `OTEL_EXPORTER_OTLP_ENDPOINT` set.

### 8.1 The `observability` field validates **[hermetic]**

```bash
cat > /tmp/traced.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: traced }
spec:
  model: { provider: anthropic, name: claude-sonnet-4-6 }
  task: hi
  tools: []
  runtime: { type: local }
  observability: { tracing: true }
EOF
./cli/bin/agentctl validate /tmp/traced.yaml      # expected: ok
```

### 8.2 No exporter → no-op (safe to leave tracing on) **[hermetic]**

With `tracing: true` but NO OTLP endpoint configured — i.e. **neither** `OTEL_EXPORTER_OTLP_ENDPOINT` **nor** the traces-specific `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set — the trace provider initializes as a **no-op**: the run behaves identically to tracing-off, with no error. (Either variable being set is treated as opt-in and turns on export.) So a spec can ship `tracing: true` without forcing every operator to run a collector (the field description in the schema documents this).

### 8.3 Real spans → an OTLP collector **[infra — needs an OTLP/HTTP collector]**

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318    # your collector
export ANTHROPIC_API_KEY=sk-ant-...
./cli/bin/agentctl run /tmp/traced.yaml --task "say hi"
# expected (in your collector): an `agentctl.run` root span with child spans
#   for the LLM call / tools, carrying gen_ai.* attributes
```

A `TRACEPARENT` from a parent process/scheduler is honored: set `TRACEPARENT=<w3c-header>` and the run's root span parents under it (this is how a scheduler stitches each step's trace together).

---

## Section 9 — Long-running agents: `chat` + durable sessions (v0.6)

v0.6 added an interactive multi-turn REPL (`agentctl chat`) backed by a durable SQLite session store, plus lifecycle management (Active → Paused/Expired). The store lives at `$XDG_DATA_HOME/agent-controller/sessions.db` (or `~/.local/share/agent-controller/sessions.db`).

> **Two different "session" stores:** `agentctl sessions list` (Section 1.4) lists the **Pi on-disk session directories** under `~/.pi/agent/sessions/agentctl/` — a *different* store from the v0.6 **SQLite chat store** that `agentctl chat` and `agentctl sessions sweep` operate on.

### 9.1 `agentctl chat` — interactive REPL **[LIVE — needs `ANTHROPIC_API_KEY` + a TTY]**

```bash
./cli/bin/agentctl chat examples/hello.yaml
# Type a message → reply; type another — each turn shares one chat-root trace
# and bumps the session's LastActiveAt.
#
# Ctrl-C behavior depends on timing (slice 6.3 single-channel signal dispatch):
#   - DURING a turn  → cancels just that turn; the REPL stays open, session
#                      remains Active.
#   - at the idle prompt (or Ctrl-D / EOF) → exits the REPL and marks the
#                      session Paused in the store.
```

`--in-memory` uses an ephemeral store (no persistence across runs); `--resume <id>` continues a prior chat. **The whole `chat` surface is Pi-only for now** — a spec with `runtime.type: local-opencode` is rejected before any turn (`Error: chat is not yet supported on runtime.type: local-opencode … Use runtime.type: local (Pi adapter)`); see Section 11.

### 9.2 `agentctl sessions sweep` — expire idle sessions **[hermetic]**

```bash
./cli/bin/agentctl sessions sweep --in-memory --ttl 1h
# expected (empty store): "no sessions to expire (cutoff <timestamp>)"
```

`sweep` marks SQLite sessions that have been `Active` longer than `--ttl` (default 24h; Go `time.ParseDuration` syntax — express days/weeks in hours) as `Expired`; an `Expired` session can't be resumed. `--in-memory` targets the ephemeral store (mostly for tests).

---

## Section 10 — Scheduler task surface (v0.7)

v0.7 makes `agentctl run` a per-step task primitive for external schedulers (Maestro / Airflow / Temporal). The flags below need no API key to verify their *plumbing* — they fail or short-circuit before the model runs — so each check is hermetic unless noted. Use a self-contained spec; the snippets assume `agentctl` on PATH and this minimal spec:

```bash
cat > /tmp/classify.yaml <<'EOF'
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata: { name: classify }
spec:
  model: { provider: anthropic, name: claude-sonnet-4-6 }
  task: |
    Classify the sentiment of: ${inputs.text}
    Reply with one JSON object: {"label": "...", "confidence": 0.0}
  outputSchema:
    type: object
    additionalProperties: false
    required: [label, confidence]
    properties:
      label: { type: string, enum: [positive, negative, neutral] }
      confidence: { type: number, minimum: 0, maximum: 1 }
  tools: []
  runtime: { type: local }
EOF
```

### 10.1 `--input KEY=VALUE` + `${inputs.<key>}` interpolation

```bash
# Provide an input, but NOT the one spec.task references → hard error, before the
# model runs (hermetic). Interpolation runs whenever ANY --input/--input-file is
# given, then reports every unreferenced key at once.
agentctl run /tmp/classify.yaml --input other=x
# expected: exit≠0, "Error: spec.task references unknown inputs: text (provide via --input KEY=VALUE)"

# Provide it (LIVE — needs ANTHROPIC_API_KEY)
agentctl run /tmp/classify.yaml --input text="I love this"
# expected: ${inputs.text} is substituted into spec.task before the run
```

Notes: with NO `--input`/`--input-file` flags at all, interpolation is intentionally **skipped** — `${inputs.x}` is left literal (this preserves opaque values when the Kubernetes backend re-runs an already-rendered spec in-Pod, slice 7.1). Duplicate `--input KEY=` is rejected; an `--input` key not referenced anywhere is a stderr warning.

### 10.2 `--input KEY=@<path>` (file value) and `--input-file <json>`

```bash
# LIVE (needs ANTHROPIC_API_KEY): these PROVIDE text, so interpolation succeeds and
# the run proceeds to the model — they exercise the value plumbing end to end.
echo "this product is great" > /tmp/snippet.txt
agentctl run /tmp/classify.yaml --input text=@/tmp/snippet.txt   # value = file contents (≤1 MiB, exact bytes)

echo '{"text":"works perfectly"}' > /tmp/params.json
agentctl run /tmp/classify.yaml --input-file /tmp/params.json    # merge a JSON object of scalar inputs
```

Hermetic negative checks (no API key — they fail before the model):

```bash
# @<missing-file> fails fast
agentctl run /tmp/classify.yaml --input text=@/tmp/does-not-exist
# expected: exit≠0, "read input file" error

# Same key via BOTH channels is rejected
echo '{"text":"dup"}' > /tmp/params.json
agentctl run /tmp/classify.yaml --input text=a --input-file /tmp/params.json
# expected: exit≠0, "provided via both --input and --input-file"

# --input-file that isn't a JSON object is rejected
echo '[1,2,3]' > /tmp/bad.json
agentctl run /tmp/classify.yaml --input-file /tmp/bad.json
# expected: exit≠0, "must be a JSON object"
```

Also rejected (hermetic): duplicate keys within the JSON, an oversized (>1 MiB) file, and `--input KEY=@<fifo>` (special files fail fast rather than block).

### 10.3 `--output-file` + `spec.outputSchema`

```bash
# LIVE: writes the result only on a clean run; with outputSchema the reply is parsed
# as JSON, validated, and the re-marshaled JSON is written (else exit 1).
agentctl run /tmp/classify.yaml --input text="great" --output-file /tmp/result.json
cat /tmp/result.json
# expected: a {label,confidence} JSON object; file perms 0600
```

### 10.4 `--skip-if-output-exists` (idempotency)

```bash
echo '{"label":"positive","confidence":1}' > /tmp/result.json
agentctl run /tmp/classify.yaml --input text="x" --output-file /tmp/result.json --skip-if-output-exists
# expected (hermetic): "[skip] --output-file ... already exists; skipping run", exit 0, no model call
```

### 10.5 `--workspace <dir>` durable memory

```bash
# LIVE: injects an MCP server exposing workspace_remember/recall/note_append/list_outputs.
agentctl run /tmp/classify.yaml --input text="hi" --workspace /tmp/ws-demo
ls /tmp/ws-demo            # .agentctl-workspace.db (+ notes.md if the agent journaled)

# Inspect the SQLite memory directly (hermetic):
sqlite3 /tmp/ws-demo/.agentctl-workspace.db 'SELECT key, value FROM kv;'
```

The memory tools ride on `spec.mcpServers`, so the same workspace works on Pi and opencode. Hermetic negative checks: `--workspace` with a `kubernetes` binding errors (host-local DB); `--workspace ""` errors; on Pi, combining `--workspace` with declared built-in tools prints a warning (Pi suppresses built-ins when any MCP server is present).

### 10.6 Scheduler examples

```bash
# Validate the shared example spec used by all three scheduler configs:
agentctl validate examples/schedulers/text-classifier.yaml   # expected: ok
```

See [`examples/schedulers/`](../examples/schedulers/) for the Maestro (OSS `kubernetes` step), Airflow, and Temporal configs, each with prominent comments on the injection-safe input-passing + shared-volume file handoff patterns.

---

## Section 11 — What is NOT working yet (honest gaps)

Tracked in [`docs/architecture/harness-matrix.md`](architecture/harness-matrix.md) and [`ROADMAP.md`](../ROADMAP.md); deferred to v0.4+:

1. **Opencode hermetic E2E** needs an Anthropic-API mock server reachable via `ANTHROPIC_BASE_URL`. Today the opencode subprocess can only be tested against a live model.
2. **`--resume <id>` for opencode** is rejected upfront by `agentctl run`; resume support is deferred.
3. **`audit-log` extension** runs on Pi only — needs an opencode-native equivalent or a wire-event consumer hook.
4. **`cancelled` reason on SIGINT for the Pi adapter** — opencode emits it, Pi still surfaces SIGINT as `error`.
5. **`agentctl validate` (schema-only) does not run compile-time adapter-compatibility checks** — only `agentctl compile` does. CI gates that stop at `validate` won't catch opencode-unsupported fields early; the compile step (or `agentctl run`) is required.
6. **Pi adapter prototype-pollution hardening** — `runtime/src/adapter.ts` still uses plain `{}` objects for `spec.mcpServers[].name`-keyed maps. The opencode adapter has null-prototype maps (slice 2.5); Pi does not.
7. **`requirements` enforcement is best-effort today** — the matcher emits warnings or errors based on what the Binding advertises, but no Backend actually enforces capabilities like `sandbox: true` or `restrictedNetwork: true`. Real enforcement was originally planned for v0.4.5 (SecurityContext + NetworkPolicy + emptyDir on the Kubernetes target) but is now opportunistic background per the 2026-06-10 roadmap pivot. Treat sandbox/network requirements as documentation rather than policy until a future enforcement slice ships.

---

## Cross-references

- [`README.md`](../README.md) — project overview + quickstart
- [`ROADMAP.md`](../ROADMAP.md) — committed direction for v0.4 / v0.5+
- [`docs/architecture/overview.md`](architecture/overview.md) — architecture + multi-adapter diagram + connection-flow
- [`docs/architecture/harness-matrix.md`](architecture/harness-matrix.md) — every-field per-adapter support table
- [`docs/versioning.md`](versioning.md) — multi-dimensional version policy
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) — slice-by-slice workflow + ADL change checklist
- [`SECURITY.md`](../SECURITY.md) — threat model + vulnerability reporting
