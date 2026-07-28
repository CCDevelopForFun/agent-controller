# Changelog

All notable changes to Agent Controller are tracked here. Format loosely follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project pre-dates this changelog so the v0.1.x history is a retrospective summary — see git tags `v0.1.0` … `v0.1.10` for per-slice detail.

This project adheres to [Semantic Versioning](https://semver.org/) for the umbrella release version; individual artifacts (CLI, runtime packages, schemas, wire protocol, runtime images) version independently. See [`docs/versioning.md`](docs/versioning.md) for the multi-dimensional policy.

## [Unreleased]

### Added — claude adapter (`runtime.type: local-claude`)

Fourth runtime adapter for Agent Controller. Drives sessions through `@anthropic-ai/claude-agent-sdk` (pinned to `0.3.220`), making it the only adapter that needs no external CLI on `PATH`.

**Key facts:**

- **New `runtime.type: local-claude`** selector. Dispatched by the existing `resolveRuntimeCommand` path in `cli/cmd/agentctl/main.go`.
- **New package `@agent-controller/runtime-claude`** (`runtime-claude/`). Calls the SDK's `query()` in-process; the SDK spawns its own bundled executable. Each `SDKMessage` is translated to the NDJSON wire-event stream via `event-translator.ts`.
- **No external CLI required.** Unlike opencode (needs the `opencode` CLI) and codex (needs the `codex` CLI), this adapter only needs `ANTHROPIC_API_KEY` in the environment. `Options.env` is never set, so `ANTHROPIC_API_KEY` / `ANTHROPIC_BASE_URL` pass through from the process env.
- **Anthropic-only.** `model.provider` must be `anthropic`; `openai` and `google` are rejected at compile time by `checkClaudeIncompatibilities` and again at adapter startup with a field-naming error. Bedrock / Vertex routing is deferred.
- **Isolation: `settingSources: []`** is set unconditionally, so the SDK never loads `~/.claude/settings.json`, project `.claude/` config, or `CLAUDE.md` files. A spec's behavior does not depend on the operator's machine.
- **Tools** (`spec.tools[]`). Pi built-ins map onto SDK tool names (`bash`→`Bash`, `read`→`Read`, `edit`→`Edit`, `write`→`Write`) and are set as `Options.tools`, which is the SDK's *restriction* list — a `tools: []` spec runs with zero built-in tools, matching Pi and opencode semantics. The same names go into `Options.allowedTools` so a non-interactive run does not stall on a permission prompt. An ADL built-in with no SDK equivalent throws instead of being dropped.
- **Subagents** (`spec.subagents[]`). Parsed from `agents/<slug>.md` and registered natively via the SDK `agents` option. Declaring subagents also grants the `Agent` delegation tool, without which the registration would be unreachable. Supported here and on Pi/opencode; rejected by codex.
- **Session resume** (`--resume`, `agentctl chat`, `agentctl serve`). agentctl session ids (`s_<hex>`) are not SDK session ids — `Options.sessionId` requires a UUID. The adapter derives a UUIDv5 from the agentctl id over a fixed namespace (implemented locally with `node:crypto`; no new dependency), sets it via `Options.sessionId` on the first turn, and passes the SDK session id captured from the `system`/`init` message to `Options.resume` on later turns. The captured id is persisted at `$XDG_DATA_HOME/agent-controller/claude-sessions/<derived-uuid>/sdk-session-id` because the adapter is a fresh process per turn.
- **MCP servers** (`spec.mcpServers[]`). All three ADL transports map onto the SDK's config union: stdio → `{type:"stdio"}`, streamable-http → `{type:"http"}`, sse → `{type:"sse"}`. Each declared server is auto-approved with an `mcp__<server>` allow rule; servers not in the spec are never granted. `lifecycle: eager | lazy` is accepted by the schema and **ignored** — the SDK has no eager/lazy control.
- **Skills** (`spec.skills[]`). SKILL.md bodies are read at startup, YAML frontmatter stripped, and inlined into the system prompt via `wrapSkillBody()`. Same "active by default" semantic as Pi, opencode, and codex. Native plugin-backed SDK skills are deferred.
- **Guardrails** (`spec.guardrails`). `block` and `warn` behave identically to the other adapters. `correct` **degrades to `warn`** — a corrective re-prompt would need a second turn, which is out of scope for this single-shot adapter (same treatment codex gives it).
- **No `model.request` / `model.response` events.** The SDK does not surface per-model-request events and synthesizing them would lose fidelity — the same gap opencode and codex have. Pi remains the only adapter emitting them.
- **`model.temperature` is ignored.** The SDK has no temperature option; the field is accepted by the ADL schema and the adapter writes a note to stderr rather than failing.
- **Rejected at adapter startup:** `spec.extensions[]`, `spec.installs[]`, custom Pi-extension tools (entrypoint set, `builtin` unset), non-`anthropic` providers.
- **New example:** `examples/hello-claude.yaml` — minimal `runtime.type: local-claude`, `model.provider: anthropic`, persona + task, `tools: []`.
- **`schemas/runtimebinding.v1alpha1.json`** `selector.runtimeType` gains `local-claude` (and `local-codex`, which had been missing).
- **Harness matrix updated** (`docs/architecture/harness-matrix.md`) with a full claude column across all feature tables.

**Prerequisites:** `ANTHROPIC_API_KEY` environment variable set. No separate CLI install.

### Added — codex adapter (`runtime.type: local-codex`)

Third runtime adapter for Agent Controller. Drives sessions via the OpenAI `codex` CLI (`codex exec`), making it the only adapter in the project with a native sandbox.

**Key facts:**

- **New `runtime.type: local-codex`** selector. Dispatched by the existing `resolveRuntimeCommand` path in `cli/cmd/agentctl/main.go`.
- **New package `@agent-controller/runtime-codex`** (`runtime-codex/`). Spawns `codex exec -C <cwd> -m <model> -s workspace-write` with an isolated `CODEX_HOME`. Reads JSONL stdout line-by-line; translates to the NDJSON wire-event stream via `event-translator.ts`.
- **OpenAI-only.** `model.provider` must be `openai`; `anthropic` and `google` are rejected at adapter startup with a field-naming error. Requires `OPENAI_API_KEY` in the environment; the adapter seeds it into the fresh `CODEX_HOME` via `codex login --with-api-key`.
- **Native sandbox (`-s workspace-write`).** `codex exec` is always spawned with the `workspace-write` sandbox flag, restricting file-system writes to the current working directory. This is the only adapter in the project with a native sandbox; Pi and opencode have no equivalent enforcement layer.
- **Session resume** (`--resume`). `resolveCodexHome` keys a stable `CODEX_HOME` off the agentctl session id. The `thread_id` captured during a run is persisted to `$CODEX_HOME/.agentctl-thread-id`; the next `codex exec` invocation reads it via `--thread-id` to resume the conversation. Ephemeral homes (no `--resume`) are wiped after each run.
- **MCP servers** (`spec.mcpServers[]`). stdio and streamable-http transports are written as `[mcp_servers.<name>]` blocks in `$CODEX_HOME/config.toml`. SSE transport is rejected at adapter startup (not in the codex TOML schema).
- **Skills** (`spec.skills[]`). SKILL.md bodies are read at startup, YAML frontmatter stripped, and inlined into the `--instructions` prompt via `wrapSkillBody()`. Same "active by default" semantic as Pi and opencode.
- **Guardrails** (`spec.guardrails`). `block` and `warn` behave identically to the other adapters. `correct` degrades to `warn` in v1 — a corrective re-prompt would require a new `codex exec resume` invocation which is out of scope for this single-shot adapter.
- **Output schema** (`spec.outputSchema`). Supported via the shared CLI `--output-file` + `spec.outputSchema` path (same as Pi and opencode).
- **Rejected at adapter startup:** `spec.extensions[]`, `spec.installs[]`, `spec.subagents[]`, custom Pi-extension tools (entrypoint set, `builtin` unset), non-`openai` providers.
- **New example:** `examples/hello-codex.yaml` — minimal `runtime.type: local-codex`, `model.provider: openai`, persona + task, `tools: []`.
- **Harness matrix updated** (`docs/architecture/harness-matrix.md`) with a full codex column across all feature tables.

**Prerequisites:** `codex` CLI on PATH (install via `npm install -g @openai/codex` or the official release); `OPENAI_API_KEY` environment variable set.

## [0.8.0] — 2026-07-02

**`agentctl serve` — expose ADL agents over HTTP/SSE.** v0.8 turns the Agent Controller into a long-lived network service. Same Agent definitions, same SessionStore from v0.6, now accessible via HTTP so external clients (schedulers, UIs, integration tests) can open sessions and stream turns without spawning a new process per request.

The headline surface, end to end:
- **`agentctl serve <spec.yaml>`** — starts a single-process HTTP/SSE server on `--port` (default 8080). Accepts `--in-memory` for tests, `--max-concurrent-turns`, `--max-sessions`, `--session-ttl`, and `--shutdown-grace` for production tuning.
- **Session REST API** — `POST /v1/sessions` (create; optional `{"inputs":{...}}` body), `GET /v1/sessions` (list), `GET /v1/sessions/{id}` (get), `DELETE /v1/sessions/{id}` (delete).
- **Turn streaming** — `POST /v1/sessions/{id}/turns` with `{"input":"..."}` returns a `text/event-stream` SSE response; each frame is `event: <wire-type>\ndata: <json>\n\n`; stream ends on `session.ended`.
- **Error codes** — 409 (session busy), 429 (turn cap or session cap), 503 (draining).
- **Health probes** — `GET /healthz` (liveness, always 200), `GET /readyz` (readiness, 503 during drain).
- **Graceful shutdown** — SIGTERM triggers drain; in-flight turns complete up to `--shutdown-grace`; `/readyz` returns 503 immediately so load-balancers stop routing new traffic.

Coordinated artifact bumps:
- `agentctl` CLI → v0.8.0 (version embedded from the release tag at build time).
- `@agent-controller/runtime` and `@agent-controller/runtime-opencode` unchanged this cycle (no adapter changes required for the serve surface).
- ADL schema unchanged (`agent-controller.dev/v1alpha1`).

**Deferred to v0.8.x:** metrics endpoint (OTel meter registry), auth middleware (OIDC / API-key hooks). TLS is expected to be terminated at a proxy.

### Added

- **`agentctl serve <spec.yaml>`** command (`cli/cmd/agentctl/serve.go`) — HTTP/SSE server wrapping any ADL agent spec.
- **Session manager** (`cli/internal/serve/`) — `Manager` type owns the session lifecycle, turn dispatch, concurrency limiter, and graceful-drain logic.
- **SSE writer** (`cli/internal/serve/sse.go`) — `WriteSSE` formats wire events as SSE frames.
- **REST handlers** (`cli/internal/serve/handlers.go`) — multiplexer over the session REST API and health probes.
- **`examples/serve-client.sh`** — copy-paste curl example: create session → POST turn → stream SSE (no jq required).
- **`e2e/serve.sh`** — hermetic E2E test (no API key): builds CLI, starts server with `--in-memory`, asserts `/healthz`, session create, and session get; live tier (gated on `AGENT_CONTROLLER_RUN_LIVE=1`) also POSTs a turn and asserts the SSE stream.

## [0.7.0] — 2026-06-25

**Make `agentctl run` a first-class scheduler task (Option-B pivot).** v0.7 ships NO new workflow YAML and NO in-process orchestrator; instead it turns `agentctl run` into a clean, composable per-step task primitive that external schedulers (Maestro / Airflow / Temporal) drive, and gives multi-step agent DAGs the three things they need: parameterized input, captured output, and durable shared memory.

The headline surface, end to end:
- **Parameterize** a run — `--input k=v` + `${inputs.foo}` interpolation (7.1), with `--input KEY=@<path>` file values and `--input-file <json>` (7.4).
- **Capture** a result — `--output-file <path>` + optional `spec.outputSchema` JSON validation (7.2); `--skip-if-output-exists` for scheduler idempotency (7.4).
- **Remember** across steps — `--workspace <dir>` injects a harness-agnostic MCP memory server (`workspace_remember` / `recall` / `note_append` / `list_outputs`), working on both the Pi and opencode adapters (7.5).
- **Copy-paste scheduler examples** for Maestro (OSS `kubernetes` step), Airflow, and Temporal (7.3).
- **Exit-code contract** and shell/expansion-injection safety documented throughout.

Coordinated artifact bumps:
- `agentctl` CLI → v0.7.0 (version embedded from the release tag at build time).
- `@agent-controller/runtime` 0.6.0 → 0.7.0 (Pi adapter: atomic `.pi/mcp.json` write for the workspace MCP server).
- `@agent-controller/runtime-opencode` 0.6.0 → 0.7.0 (no code change this cycle; bumped to satisfy the release's lockstep version-validation).
- ADL schema unchanged (`agent-controller.dev/v1alpha1`) — all v0.7 fields (`outputSchema`) were additive.
- Runtime image **not** bumped (released on its own `runtime-image/v*` tag; the K8s `outputSchema` round-trip + a v0.7-bundled image remain a tracked follow-up — v0.7's surface is host-side CLI, and the K8s backend is opportunistic per the roadmap).

Known follow-ups (Pi adapter, to make `--workspace` first-class on Pi; opencode already handles all cases): preserve declared built-in tools alongside MCP tools, and per-run MCP config isolation so one cwd can serve many workspaces.

### Added (slice 7.5 — `--workspace` durable agent memory, harness-agnostic via MCP)

Fifth slice of v0.7 (Option-B pivot). Completes the DAG story: 7.2/7.4 pass a step's *output* to the next step as input; 7.5 adds durable, structured **memory** an agent can read and write across runs — `"save memory somewhere and have the next agent reuse it"`.

- **`agentctl run --workspace <dir>`** — points the run at a shared workspace directory and auto-injects an MCP server exposing four memory tools to the agent:
  - `workspace_remember(key, value)` / `workspace_recall(key?)` — a SQLite key/value store (recall with no key lists everything).
  - `workspace_note_append(note)` — appends a timestamped line to `notes.md` in the workspace dir (a file, so it's also a normal handoff artifact: it shows in `list_outputs` and a downstream step can read it with `--input note=@<dir>/notes.md`).
  - `workspace_list_outputs()` — lists the regular files in the workspace dir (the `--output-file` artifacts from prior steps + `notes.md`); the internal SQLite files are excluded.
  - Runs that point `--workspace` at the same dir share the same memory.
- **Harness-agnostic by design**: the memory tools are exposed over **MCP**, not as harness-specific native tools. `agentctl` itself is the MCP server — `--workspace` injects an `mcpServers` entry that runs `agentctl __workspace-mcp --workspace <absdir>` over stdio (command = the absolute path to the running executable, so it resolves regardless of the adapter subprocess's PATH). Both the Pi adapter and the opencode adapter consume `spec.mcpServers`, so the **same workspace works on both** with zero adapter-specific code. (Per the pi.dev/packages check: existing Pi memory extensions — `pi-hermes-memory`, `gentle-engram` — are Pi-only and would break harness-agnosticism, so a native MCP server keyed to a `--workspace` dir was the right fit.)
- **SQLite store** (`cli/internal/workspace`) mirrors the slice-6.2 session-store patterns: `modernc.org/sqlite` (pure Go, no CGO → still cross-compiles), WAL + `busy_timeout` for safe concurrent DAG steps, forced WAL/SHM creation + `0600` perms because remembered values can be secrets, and rejection of `file:`/`?`-DSN paths that would mis-target the chmod hardening.
- **Kubernetes target rejected**: `--workspace` errors with a clear message when the binding targets `kubernetes` — the workspace SQLite store is host-local and a Pod can't reach it. Cross-step state under K8s uses a shared volume + the file-handoff flags instead.
- **New dependency**: `github.com/modelcontextprotocol/go-sdk` (the official MCP Go SDK) for the stdio server. Chosen over hand-rolling the JSON-RPC/MCP protocol so the server is guaranteed spec-compliant against both adapters' MCP clients; chosen over `mark3labs/mcp-go` for being the official, org-maintained implementation.
- **`--workspace` on Pi: per-cwd config constraint** (codex passes 2-3 of slice 7.5): the Pi adapter keys its MCP config off a single `<cwd>/.pi/mcp.json`. `--workspace` injects a memory server whose args carry the workspace path, so reusing the **same** workspace from a cwd is idempotent (identical content → no-op), but a **different** workspace from the same cwd would conflict on that one file. We keep the adapter's existing safe behavior — it **fails loudly** (`Cannot write MCP config: … already exists with different contents`) rather than silently overwriting, because overwriting a shared per-cwd file would let two overlapping runs race and cross-load each other's MCP servers (cross-contaminating workspaces). Guidance: run each scheduler step from its **own working directory** (containers/task dirs already do this), or use `runtime.type: local-opencode`, whose per-run XDG config dir has no such constraint and handles `--workspace` cleanly in all cases. Per-run config isolation on Pi (so one cwd can serve many workspaces) is a tracked follow-up; it relocates all adapter cwd-relative state and warrants its own slice.
- **`--workspace` value validated before absolutizing** (codex pass 3 P3): a `--workspace file:/…` value would otherwise have `filepath.Abs` prefix the cwd before `workspace.Open`'s `file:`/`?` guard ran, creating a nested `<cwd>/file:/…` dir instead of failing. The raw flag value is now rejected up front.
- **`list_outputs` hides in-flight output temp files** (codex pass 4 P2): when `--output-file` targets a path inside the workspace, slice 7.2's atomic writer leaves a `.agentctl-output-*.tmp` file until the final rename. `list_outputs` now treats that prefix as internal (alongside the SQLite DB files) so a concurrent run or a crash can't surface a partial/temp file as a durable step artifact.
- **Atomic Pi MCP config creation** (codex pass 5 P2): `writeMcpJson` now writes `.pi/mcp.json` with the exclusive-create flag (`wx`) and reconciles on `EEXIST`, instead of a check-then-write. This closes the first-creation TOCTOU where two concurrent runs from the same cwd (neither finding an existing file) could both write and clobber each other — so the "fails loudly, never silently cross-loads" guarantee now holds even when the file doesn't exist yet, not only when it already does.
- **Reject explicit empty `--workspace ""`** (codex pass 5 P2): a wrapper expanding an unset path (`--workspace "$WS"` with `WS` unset) marks the flag provided but empty; the old `!= ""` guard treated it as omitted and ran with no durable memory. `run` now uses `cmd.Flags().Changed("workspace")` and errors on an empty value (same fix as slice 7.4's `--input-file`).
- **`note_append` re-asserts `0600` on an existing journal** (codex pass 6 P2): `OpenFile`'s mode only applies when creating the file, so a pre-existing `notes.md` with looser perms kept them. `AppendNote` now `chmod`s the journal to `0600` before appending — notes can be sensitive, matching the DB/output hardening.
- **Honest message on concurrent Pi config creation** (codex pass 6 P2): the exclusive-create loser reads `.pi/mcp.json` to reconcile, but the winner may be mid-write (the window between `O_CREAT` and `write`), so an empty read would otherwise misreport "different contents". An empty existing file is now surfaced as a concurrent-same-cwd error pointing at the per-run-dir / opencode guidance. (Full concurrent-same-cwd correctness on Pi remains the per-run config-isolation follow-up.)
- **Known limitation — Pi built-in tools** (codex pass 1 of slice 7.5): the Pi adapter switches to `noTools: "builtin"` whenever ANY MCP server is present (it can't keep declared built-ins AND allow unknown MCP tool names at once — Pi's `noTools` is `"all" | "builtin"`, not a per-tool list). So adding `--workspace` to a **Pi** spec that declares built-in tools (`bash`/`read`/`edit`/`write`) would suppress them. `agentctl run` now prints a clear warning naming the affected tools when `--workspace` is combined with a Pi runtime + declared built-ins, and points at `runtime.type: local-opencode` (whose per-tool permission model is unaffected — built-ins and the workspace tools coexist) or the file-handoff flags. The proper fix (preserve declared built-ins alongside MCP tools on Pi) is an adapter change tracked as a follow-up; it affects every `mcpServers` + built-ins spec, not just `--workspace`.
- **Tests**: 11 store cases (remember/recall round-trip, missing key, upsert, empty-value-vs-not-found, empty-key rejection, sorted recall-all, note journal, persist-across-reopen, list-outputs excludes the DB + sorts, idempotent close, path rejection, 0600 perms — the perms case skipped on Windows like the session store) + 11 command/server cases (in-memory client↔server: lists 4 tools, remember→recall, recall-all, missing-key-not-an-error, note+list-outputs, empty-note tool error; injection appends/collision-rejects/preserves-existing; the declared-builtins detector; `--workspace` + kubernetes target rejected end-to-end). Full module suite green; cross-compiles for `GOOS=windows`.

### Added (slice 7.4 — file-handoff input primitives + run idempotency)

Fourth slice of v0.7 under the **Option-B pivot**: makes `agentctl run` compose in a multi-step DAG where one step's output becomes the next step's input, and lets a scheduler safely retry a partially-failed DAG.

- **`agentctl run --input KEY=@<path>`** — the value form `@<path>` reads the input value from a file (capped at 1 MiB) instead of the literal text. This is the file-handoff path: a previous step writes its result with `--output-file`, the next step consumes it with `--input text=@/shared/prev-result.json`. The file's exact bytes become the value (no trimming) so an `--output-file` → `--input` round-trip is lossless. There is deliberately no escape for a literal leading `@` (consistent with slice 7.1's no-escapes-yet stance) — pass such values through `--input-file`, whose JSON values are always literal.
- **`agentctl run --input-file <path>`** — merges a JSON object of inputs (multi-input handoff: one upstream step emits a `{key: value}` object the downstream step consumes in one shot). Scalar values only — strings pass through, numbers keep their exact textual form (`json.Number`, so large ids / decimals survive losslessly), bools become `"true"`/`"false"`; arrays, objects, and null are rejected (they can't interpolate into the text of `spec.task`). Keys must match the same `[A-Za-z_][A-Za-z0-9_]*` shape as `--input`. A key supplied via BOTH `--input` and `--input-file` is a hard error (cross-channel duplicate), mirroring slice 7.1's repeated-key rule. Trailing content after the JSON object is rejected (same defense slice 7.2 applies to captured output).
- **`agentctl run --skip-if-output-exists`** — if `--output-file` already points at an existing regular file, skip the run and exit 0. Because slice 7.2's output write is atomic (tmp+rename) and only happens on a clean run with a non-empty message, a present file means a prior run succeeded — so a scheduler retrying a DAG re-runs only the steps that didn't finish, without re-spending tokens on the ones that did. Requires `--output-file`; an existing directory at the path is a configuration error. The check runs after cheap/deterministic config validation but before reading input files, interpolating, or initializing tracing/backends, so the skip path stays cheap and side-effect-free.
- **1 MiB cap** on both file-reading paths (`maxInputFileBytes`), enforced via an `io.LimitReader` of cap+1 so an oversized file is detected without reading it fully into memory. A handoff larger than this is a smell — pass a reference (path/URI) rather than inlining the payload into the prompt.
- **Tests**: 27 cases — `@<path>` read / exact-byte preservation / empty-path / missing-file / over-cap / at-cap-boundary / leading-`@`-only; `--input-file` basic / scalar coercion / large-int precision / cross-channel duplicate / **intra-file duplicate** / invalid key / non-object / null / nested-non-scalar / trailing-content / over-cap / empty-object no-op; the interpolation-intent gate; an empty-`--input-file`-still-validates flow; an end-to-end file+JSON→interpolation flow; and `outputAlreadyExists` exists/missing/directory/**FIFO**.

**Codex pass 1 (slice 7.4) hardened three edge cases:**
- **`--input-file` is interpolation intent** (P2): the original gate `len(inputs) > 0` meant an explicit `--input-file {}` (empty object) skipped interpolation entirely, so a `${inputs.foo}` reference reached the model literally instead of failing for the missing input. The gate now keys on caller intent via a `shouldInterpolateInputs(inputFlags, inputFile)` helper (a supplied flag, not a non-empty map) — which still keeps the in-Pod K8s child (no flags) off the interpolation path.
- **Intra-file duplicate keys rejected** (P2): decoding straight into a map silently kept the last value for `{"topic":"a","topic":"b"}`, reintroducing the last-wins ambiguity the slice rejects everywhere else. A new `decodeInputObject` walks the object with a JSON token stream and errors on a repeated key (and still rejects trailing content).
- **Non-regular output paths rejected** (P3): `--skip-if-output-exists` pointed at a FIFO/socket/device passed the old `!IsDir()` check and skipped the run, though such a path can't be the regular file a prior run wrote. `outputAlreadyExists` now requires `Mode().IsRegular()` and surfaces a configuration error otherwise.

**Codex pass 2 caught two special-file hazards on the same surfaces:**
- **Input FIFO could hang the task** (P2): `--input KEY=@<fifo>` / `--input-file <fifo>` opened the path with `os.Open`, which blocks indefinitely on a read-side FIFO open (waiting for a writer) — the `LimitReader` never gets a chance. `readCappedFile` now `os.Stat`s before opening and rejects non-regular files, so a special-file handoff fails fast instead of wedging the scheduler. (A symlink to a regular file is still accepted — only the resolved type matters for reading.)
- **Symlinked output path falsely skipped** (P2): `outputAlreadyExists` used `os.Stat`, which follows symlinks, so an `--output-file` symlink to any regular file counted as a completed prior run — but `writeOutputFile` replaces the symlink itself, so it is not evidence this step succeeded. Switched to `os.Lstat` so a symlink at the output path is a configuration error.

**Codex pass 3** (P2): the FIFO tests used `syscall.Mkfifo`, which is undefined on Windows, so the whole test package failed to COMPILE under `GOOS=windows` before `t.Skipf` could run (the release builds cross-platform binaries). Moved the FIFO tests into a `//go:build unix` file; the Windows test binary compiles again and Unix still exercises them.

**Codex pass 4 caught two more `--input-file` edge cases:**
- **Error message leaked file contents** (P2): when `--input-file` pointed at a valid JSON scalar (e.g. a string holding a prior step's secret output) instead of an object, the error printed the decoded value in full — up to the 1 MiB cap — and scheduler stderr is commonly logged. `decodeInputObject` now reports the token KIND via a `describeJSONToken` helper, never the value.
- **Explicitly-empty `--input-file ""` silently ignored** (P2): a wrapper expanding an unset path (`--input-file "$PARAMS_JSON"` with the var unset) passes an empty string; the `if inputFile != ""` guard then skipped both the file read and the interpolation-intent path, so a `${inputs.foo}` task reached the model literally. RunE now distinguishes flag presence with `cmd.Flags().Changed("input-file")` and errors on an empty value.

**Codex pass 5** (P2): the remaining content-leak path — a `--input-file` that starts with a valid object but has trailing bytes (an object followed by a prior step's secret) — still echoed up to 80 bytes of that trailing content in the error. The trailing-content error now reports only the byte count, never the bytes, consistent with the pass-4 non-object fix.

**Deferred from the original 7.4 plan**: store-aware `--resume` for `run` (active-session refusal via the SQLite session store). `run --resume` already works at the adapter level (it sets `spec.SessionID` and the Pi adapter continues that session); adding session-store lookup + expired/active/cross-agent guards is a session-*lifecycle* concern, not file-handoff, and fits more naturally alongside the memory/workspace work in slice 7.5. Keeping 7.4 scoped to the file-handoff I/O primitives keeps it cohesive.

### Added (slice 7.3 — scheduler integration examples)

Third slice of v0.7 under the **Option-B pivot**: working examples showing operators how to call `agentctl run` as a per-step task primitive from external schedulers, exercising the `--input` / `${inputs.foo}` / `--output-file` / `outputSchema` surface from slices 7.1 and 7.2.

- `examples/schedulers/text-classifier.yaml` — shared agent spec used by all three scheduler examples. Single-purpose: classify a snippet of text's sentiment with a confidence score. Demonstrates `${inputs.text}` interpolation in `spec.task` and a typed `outputSchema` for `{label, confidence}`.
- `examples/schedulers/maestro/workflow.yaml` — [Maestro](https://github.com/Netflix/maestro) workflow that runs `agentctl` as a `kubernetes` step (OSS Maestro's containerized step type) using the `agent-runtime-base` image, with a downstream step that reads the typed JSON result. Passes `--input` via an `args` STRING_ARRAY so there is no shell layer between Maestro and agentctl (injection-safe by construction), and keys the shared-volume result path on a workflow-level `params.nextUniqueId()` value referenced from both steps.
- `examples/schedulers/airflow/classify_dag.py` — Apache Airflow DAG using `BashOperator` to invoke `agentctl run` and `PythonOperator` to read the result file and push to XCom. Passes BOTH scheduler-provided values — the input text and the `{{ dag_run.run_id }}`-keyed result path — via the operator's `env=` dict (read inside the script as `"$TEXT_INPUT"` / `"$RESULT_PATH"`) rather than splicing them into `bash_command`; env vars are opaque to bash's parser, so neither command-substitution nor a stray quote in a custom run id can reach the shell. The DAG run id keys the shared-volume result path so concurrent runs don't clobber each other.
- `examples/schedulers/temporal/classifier_workflow.go` — Temporal Workflow (deterministic) + Activity (side-effect) pair. The Activity shells out to `agentctl run` with retry policy + timeouts; the Workflow stays deterministic so Temporal can replay it cleanly. Tagged `//go:build ignore` so the file documents the pattern without being picked up by any Go toolchain.

All three examples lean on `spec.outputSchema` — the scheduler-side reader can `json.Unmarshal` directly without re-validating because agentctl already enforced the shape before writing the file.

**Codex passes 1-9 hardened the examples**. Each finding caught a different production-failure mode. The recurring lesson: never trust a scheduler-provided value (input text OR run identifiers) as-is — keep it out of any shell AND out of any other expansion layer (bash, Airflow Jinja, kubelet `$(VAR)`), derive a safe bounded basename when it becomes a filesystem path, and make cross-step file handoffs work under non-root / root-squashed volumes.

- **Airflow path rendering** (pass 1 P2): module-level Jinja-templated strings are NOT rendered by Airflow's Jinja layer — only operator template fields are. Resolution: pass the templated path via the operator's `templates_dict=` parameter, which IS rendered, and read the substituted value from the callable's `templates_dict` kwarg.
- **Airflow shell-safe input** (pass 1 P2): scheduler-provided text reaches bash via the operator's `env=` dict and is read inside the script with `"$TEXT_INPUT"`. Env vars are opaque to bash's parser, so values containing `$(...)` or backticks reach agentctl as literal strings instead of being command-substituted.
- **Maestro shell-safe input** (pass 2 P1, pass 3 P2 — *superseded by pass 5*): early drafts used a SHELL step and worried about Maestro's `${...}` template substitution happening before bash sees the script. Passes 2-3 hardened that SHELL approach (env-var mapping, unbraced reads). Pass 5 then found OSS Maestro has no SHELL step at all, so the whole concern was designed out — see the pass-5 finding below.
- **Multi-worker file handoff** (pass 2 P2): result files don't survive multi-worker schedulers (Airflow Celery/Kubernetes, multi-node Maestro). Resolution: both examples use a documented shared-volume path with prominent comments naming the operator's responsibility to mount that volume (PVC / NFS / EFS) on every worker pool / step pod. Airflow keys the path on `{{ dag_run.run_id }}`; Maestro keys it on a workflow-level `params.nextUniqueId()` value.
- **Airflow worker-env preservation** (pass 2 P2): setting `env=` on `BashOperator` without `append_env=True` REPLACES the worker environment, dropping `PATH` / `ANTHROPIC_API_KEY` / etc. Resolution: `append_env=True` so the custom env merges on top of the inherited worker env.
- **Temporal per-attempt output path** (pass 3 P2): the original used `WorkflowExecution.RunID` alone to key the result file, which collides across retries and parallel `RunAgentctl` activities in the same workflow. Resolution: include `ActivityID` and `Attempt` in the path so each attempt writes its own file.
- **Output contract must live in the prompt, not just `outputSchema`** (pass 4 P2): the runtime sends only `spec.task` to the model — `spec.outputSchema` is enforced AFTER generation, never shown to the model. The original `text-classifier.yaml` task said "match the outputSchema below", but the model never sees that schema, so it could emit different keys and fail every scheduler example at validation time. Resolution: spell the JSON contract out inside `spec.task` (exact field names, allowed values, a structural example) so the model is told the shape; `outputSchema` then enforces it server-side. The takeaway for `outputSchema` users generally: treat the schema as a validation gate, and still describe the desired shape in the task prompt.
- **Maestro example targeted a non-existent DSL** (pass 5): the original Maestro example used `type: SHELL` steps and a `${maestro.instance_uuid}` parameter — neither exists in OSS Maestro. Verified against the OSS repo + wiki: Maestro's step types are `NoOp` / `Foreach` / `Subworkflow` / `kubernetes`; there is no inline-shell step (containerized work runs as a `kubernetes` step), and `${workflow_instance_id}` is explicitly NOT globally unique (it restarts from 1 per workflow). Resolution: rewrote the example as a real `kubernetes` step that runs the `agent-runtime-base` image (which already bundles agentctl), passing `--input` through an `args` STRING_ARRAY — this removes the shell layer entirely, so the pass 2-3 SHELL-injection concern is designed out rather than mitigated. The unique result key now comes from a workflow-level `params.nextUniqueId()` SEL expression referenced from both steps (computing it per-step would yield different values and break the write→read handoff). Comments call out the operator-supplied pieces the OSS k8s-step sample doesn't cover (shared volume, `ANTHROPIC_API_KEY` secret, spec ConfigMap, and an image bundling agentctl >= v0.7.2).
- **Maestro image tag conflated version dimensions** (pass 6 P2): the rewritten Maestro example pinned `agent-runtime-base:0.7.0`, but runtime-image tags version INDEPENDENTLY of the agentctl CLI / ADL schema (per the versioning policy in ROADMAP.md — the current published image line is `:0.1.x`). A `:0.7.0` tag both doesn't exist and falsely implies the image tracks the CLI version. Resolution: the image value is now an explicit, deliberately un-pullable placeholder (`:<tag-with-agentctl-0.7.2-or-newer>`) so an unedited copy fails loudly, plus a header note that the tag must be chosen by the agentctl version the image bundles, not by matching `v0.7.x`.
- **Temporal SIGKILL orphaned the agent on cancellation** (pass 6 P2): `exec.CommandContext`'s default cancel behavior is `Process.Kill()` (SIGKILL), which bypasses agentctl's SIGTERM/SIGINT cleanup. On Temporal Activity cancellation / timeout (and the subsequent retry), the agentctl process and the runtime subprocess + in-flight model request it owns could keep running, orphaned. Resolution: set `cmd.Cancel` to send SIGTERM and `cmd.WaitDelay` to escalate to SIGKILL after a grace period, and added a heartbeat goroutine (with `HeartbeatTimeout` on the Activity options) so Temporal actually delivers the cancellation that triggers the graceful path — otherwise ctx isn't canceled until `StartToCloseTimeout`.
- **Airflow run-id in the shell path** (pass 7 P2): the example carefully routed `params.text` through `env=` but still rendered `{{ dag_run.run_id }}` directly into a single-quoted `--output-file` argument in `bash_command`. Airflow deployments can permit custom DagRun IDs, and a run id containing a single quote would break out of the shell word — reintroducing the very injection class the example avoids for the text. Resolution: the result path now also travels via `env=` (`RESULT_PATH`, Jinja-rendered) and is read as `"$RESULT_PATH"`; only operator-controlled constants (`SHARED_DIR`, `AGENT_SPEC`) remain spliced into the command body.
- **Airflow run-id as a raw filename** (pass 8 P2): even out of the shell, using the raw `dag_run.run_id` as a filesystem path component is unsafe — a custom run id containing `/` or `..` can traverse out of `SHARED_DIR`, and a long id plus the suffix can exceed the 255-byte filename limit and fail the write before the reader runs. Resolution: a `_safe_key` helper hashes the run id to 16 hex chars (registered as a `user_defined_macros` Jinja callable, so the writer and reader tasks render the same key) and that hash — not the raw id — keys the result file.
- **Maestro kubelet `$(VAR)` expansion** (pass 9 P1): the rewritten Maestro example claimed an `args` STRING_ARRAY was "injection-safe by construction" because no shell is involved. True for bash, but kubelet ALSO expands `$(VAR)` references in container `command`/`args` (and dependent expansion in env values) against the pod's env before exec. With `ANTHROPIC_API_KEY` mounted into the pod, an attacker-controlled `text` of `$(ANTHROPIC_API_KEY)` would be rewritten to the secret before agentctl ran — secret exfiltration with no shell. Resolution: corrected the example's security narrative to describe BOTH expansion layers, and documented the safe options (pass untrusted input as a mounted file via the planned `--input KEY=@<path>` form; escape `$`→`$$`; don't co-locate secrets with untrusted-arg expansion). The inline `text=${text}` is annotated as safe only for trusted/static input.
- **Maestro cross-pod file permissions** (pass 9 P2): the downstream reader used `busybox` (root or an arbitrary uid), but agentctl writes the result `0600` / its parent `0700` as the non-root uid baked into agent-runtime-base, so on non-root-enforced or root-squashed clusters the reader couldn't read the handoff file. Resolution: the reader now uses the SAME agent-runtime-base image (same uid as the writer), plus a FILE OWNERSHIP note recommending a shared `securityContext` (matching `runAsUser` + an `fsGroup` that owns the volume) on both step pods.

### Added (slice 7.2 — `agentctl run --output-file` + optional `outputSchema` extraction)

Second slice of v0.7 under the **Option-B pivot**: gives schedulers a clean way to capture an agent's result so it can flow into the next workflow step.

- **`agentctl run --output-file <path>`** — writes the agent's last assistant message to `<path>` on successful exit. Failed or cancelled runs leave the path untouched, so a scheduler reading the file sees the result of the last successful run (or nothing). Atomic write (write tmp + rename); parent dirs are created `mkdir -p`-style (schedulers often point at a fresh-workspace path). File perms are `0600` since the output may contain sensitive task results.
- **`spec.outputSchema`** (new optional field) — a JSON Schema document. When set AND `--output-file` is passed, the CLI extracts JSON from the agent's last assistant message (stripping a single ` ```json ` fenced block if present), parses it, validates against the schema, and writes the **re-marshaled** validated JSON to `<path>`. Failure modes — non-JSON output, schema compilation failure, validation failure — all fail the run with a clear stderr error and exit 1. Without `--output-file` the field has no runtime effect; validation is gated on a consumer being present.
- **Last-message capture**: the CLI accumulates the LAST `EventMessage{role:assistant}` (not concatenate). Single-turn `agentctl run` produces exactly one; rare multi-turn runs surface the final reply (chat uses `agentctl chat`, not `run`).
- **Empty-final hard error**: if `--output-file` was requested but no assistant message arrived, the run fails. Silent zero-byte files become diagnosis puzzles later.
- **K8s round-trip intentionally NOT included** (codex pass 5 P2): the default in-Pod image (`agent-runtime-base:0.1.5`) embeds an older ADL schema that pre-dates `outputSchema`. Writing the field into the in-Pod YAML would make every default K8s run that uses `outputSchema` fail in-Pod validation BEFORE the agent runs. The host-side capture path is unaffected — the host CLI keeps the schema in its own CompiledSpec and validates from the wire stream the Pod streams back. The K8s round-trip will return in a future slice that bumps the default runtime image and pins image-version → schema-field compatibility.
- **Empty schema preserved** (codex pass 1 P2): `outputSchema: {}` is valid JSON Schema (accepts any JSON value) and is the natural way to require JSON syntax with no further constraints. The compiler and K8s marshaller both branch on key-presence / non-nil — not `len > 0` — so an empty-schema run still flips the CLI into JSON-parse mode and rejects non-JSON output instead of silently downgrading to raw-text capture.
- **Numeric precision preserved** (codex pass 1 P2): the JSON validator now uses `Decoder.UseNumber()` so integers above 2^53 (Snowflake-style IDs, etc.) and high-precision decimals survive the parse → validate → re-marshal round trip losslessly. Without this, `json.Unmarshal(_, any)` rounds every number to `float64` and the re-marshal writes the rounded value, silently corrupting scheduler payloads.
- **Trailing-content rejection** (codex pass 1 P2 + pass 2 P2): the JSON parse path now rejects anything after the decoded value. Pass 1 used `dec.More()`, but pass 2 caught that `More()` is array/object-iteration-only and returns false on closing delimiters like `]` or `}` — so `{"a":1}]` would slip through. The check now inspects bytes past `dec.InputOffset()` and only allows whitespace, catching stray delimiters, additional values, and prose tails.
- **`OutputSchema` is `*map[string]any`** (codex pass 3 P2): `omitempty` on a bare `map[string]any` would drop a zero-length map at JSON marshal time. That meant `agentctl compile` (which marshals CompiledSpec to JSON) silently lost the `outputSchema: {}` distinction the rest of the slice carefully preserves. Pointer-to-map + `omitempty` drops only nil pointers, so empty-map values survive end-to-end.
- **Pre-run schema compilation** (codex pass 3 P2): a new `prepareOutputCapture` helper compiles the JSON Schema BEFORE `be.Submit`. Without this, a malformed `spec.outputSchema` would only surface AFTER the agent ran — wasting tokens and possibly executing tools before failing on a deterministic config error. `finalizeOutput` now takes the pre-compiled `*jsonschema.Schema` directly.
- **Boolean JSON Schema shorthand intentionally not supported** (codex pass 4 P2 — deferred): JSON Schema draft 2020-12 allows `outputSchema: true` (accept any) and `outputSchema: false` (reject all). 7.2 restricts `outputSchema` to object form because (a) `true` is already expressible as `outputSchema: {}`, (b) `false` rejects all output and has no real use, (c) supporting both would widen the field's type from `*map[string]any` to `any` across the compiler, K8s marshaller, and pre-compile path for negligible practical value. The ADL schema rejects boolean shorthand at validate time with a clear error; a regression test pins this so a future widening is an explicit, observable change.

### Added (slice 7.1 — `agentctl run --input k=v` + `${inputs.foo}` interpolation)

First slice of v0.7 under the **Option-B pivot** (see ROADMAP.md): agentctl is the agent runtime; external schedulers (Maestro, Airflow, Temporal) are the orchestrators. v0.7 ships ZERO new workflow YAML format and ZERO in-process orchestrator; instead it makes `agentctl run` a clean scheduler-callable task primitive.

- **`agentctl run --input KEY=VALUE`** — repeatable flag. The scheduler calls agentctl once per task, passing parameters via this CLI surface. Keys must match `[A-Za-z_][A-Za-z0-9_]*` (so they're addressable from the interpolation syntax). Duplicate `--input KEY=` flags are rejected (last-wins would be silently buggy from a shell wrapper). Empty values are accepted (`KEY=` is a real input).
- **`${inputs.<key>}` interpolation** in `spec.task`. Pre-7.1 the task was a literal string; 7.1 lets operators write ONE Agent YAML with `task: "Research \"${inputs.topic}\""` and dispatch with different `--input topic=X` per scheduler task instance. Interpolation runs AFTER `--task` override so the override itself can reference `${inputs.X}`.
- **Error semantics**: a referenced input key that has no `--input` is a HARD error (interpolation fails the run with all missing keys listed in one message; iteration-style "fix one at a time" is the wrong UX). An `--input` key not referenced anywhere is a stderr warning (operator might intend it as reserved for a future field).
- **No escaping yet**: no syntax for a literal `${inputs.X}` in the task. If an operator needs that, they break the string up. Adding escape rules now would lock us into a syntax we may regret.
- **Malformed placeholder rejection**: any text matching `${inputs.` that ISN'T a well-formed `${inputs.<valid-key>}` (e.g. `${inputs.topic-name}` with a dash, `${inputs.topic` without closing brace, `${inputs.}` empty key) is a HARD error in the TEMPLATE. Codex pass 1 P2 caught the silent-pass-through bug; codex pass 2 P2 caught the follow-up: the check originally ran on the rendered OUTPUT, which incorrectly rejected legitimate input VALUES that happened to contain literal `${inputs.foo}` text (e.g. a code snippet a scheduler passes via `--input snippet='${inputs.foo}'`). Input values are opaque single-pass payloads; the malformed check now runs on the template only.
- **Post-interpolation empty-task check**: codex pass 2 P2 caught that a template like `"${inputs.prompt}"` with `--input prompt=` would render to empty and reach the backend, bypassing the schema's existing `spec.task` `minLength: 1` invariant (which runs before interpolation). The CLI now re-enforces non-empty (after `TrimSpace`) after interpolation and points the operator at the offending empty input.
- **Interpolation gate is caller-intent, not content sniff**: codex pass 3 P2 caught the Kubernetes recursion bug. The K8s backend marshals the host-resolved spec (task already interpolated) into a Secret and runs `agentctl run` in-Pod without forwarding `--input` flags. If the host operator passed an opaque value like `--input snippet='${inputs.foo}'`, the in-Pod child would see literal `${inputs.foo}` text in `spec.task` and — under the old `strings.Contains(spec.Task, "${inputs.")` gate — re-trigger interpolation, then fail with `unknown inputs: foo`. The gate is now `len(inputs) > 0` only. Cost: a host operator who writes `${inputs.X}` in YAML and forgets `--input X=` no longer gets a pre-flight error; the literal text reaches the model and the operator learns from the confused response. Acceptable trade — the K8s correctness bug is worse than losing the soft pre-check.
- **Only `spec.task` is interpolated.** Other CompiledSpec fields (persona, tools, model) change agent identity and should live in distinct Agent YAMLs.
- **Tests**: 15 cases covering flag parsing (basic, empty value, equals-in-value, missing-equals, empty key, invalid key, duplicate key) + interpolation (basic, repeated reference, missing key as error, all-missing-keys-reported-once, unused-keys returned, no-reference no-op, no-collision with `$HOME`/`${OTHER_VAR}` syntax).

### Roadmap

- **v0.7 direction pivot** recorded in ROADMAP.md. Previous v0.7 plan (in-process `AgentWorkflow` engine with parallel/conditional/foreach step shapes) is dropped. Reasoning: Maestro already does orchestration well; a custom workflow engine would duplicate it with no upside, and the durability problem (workflows outliving a single process) is exactly what Maestro/Temporal solve. New v0.7 plan: enhance `agentctl run` as a scheduler-callable task primitive.

## [0.6.0] — 2026-06-15

The v0.6.0 release rolls slices 6.1 through 6.6 — the full **long-running agents** track. `agentctl chat <spec.yaml>` is now a first-class interactive surface; sessions persist across process restarts via SQLite by default; the operator can retire idle sessions with `agentctl sessions sweep`; and the slice-5.x OTel tracing chain extends naturally to a chat-root → per-turn → adapter-session span tree.

Coordinated artifact bumps:
- `agentctl` v0.6.0 (CLI)
- `@agent-controller/runtime` 0.6.0 (Pi adapter)
- `@agent-controller/runtime-opencode` 0.6.0 (opencode adapter)
- `ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5` (runtime image)

### Added (slice 6.6 — v0.6.0 acceptance + release)

End-to-end acceptance test covering the v0.6 chat surface:

- `TestV06AcceptanceMultiTurnChatRoundtrip` — opens a real chat-root span, runs three turns against a fake backend through the SQLite store, asserts that all `chat.turn` spans share the chat-root trace id and parent under the chat-root span id, that the session's `LastActiveAt` advances per turn, and that the exit transition to `StatusPaused` lands in the store.
- `TestV06AcceptanceResumeAcrossReopen` — closes the SQLite store, reopens against the same file, resumes the session by id, asserts the resume snapshot carries the prior `paused` status and the resumed session is `active` again.
- `TestV06AcceptanceSweepThenResumeRejected` — seeds a stale Active session, runs `MarkExpired`, asserts the session is now `expired` AND that `openOrResumeChatSession` returns `errSessionExpired` with the snapshot's `Status=expired` so `newChatCmd` emits the right wire event.

No new CLI surface in 6.6 itself; it's the release-readiness gate for slices 6.1–6.5.

### Added (slice 6.5 — per-turn OTel child spans in chat)

Closes the v0.6 tracing story. Slice 6.3 wired a single OTel root span around the WHOLE `agentctl chat` REPL — every turn's adapter spans shared the same chat-root parent. Slice 6.5 inserts a `chat.turn` span per turn so the trace tree gets the natural three-level shape:

```
agentctl.run                    (chat root, whole REPL)
├── chat.turn 1
│   └── agent.session           (adapter, parented via slice 5.2 TRACEPARENT)
│       ├── gen_ai.chat         (slice 5.3 — adapter-emitted LLM span)
│       └── gen_ai.tool.call
├── chat.turn 2
│   └── agent.session
│       └── ...
└── chat.turn N
```

- **`chat.turn` span** opened in `runChatTurn` as a child of the caller's ctx, via the shared `observability.Tracer()` (instrumentation scope `github.com/CCDevelopForFun/agent-controller/cli` — same scope as `agentctl.run` and adapter spans, so saved dashboard queries that filter by scope see ALL agentctl spans uniformly). Codex pass 1 P2 caught the original separate-scope choice that would have silently dropped turn spans from existing operator queries. The adapter dispatch (`be.Resolve` / `be.Submit`) uses the turn span's ctx, so the slice-5.2 TRACEPARENT env injection parents adapter spans under THIS turn — not the chat-root.
- **Attributes**: `chat.turn.index` (1-based monotonic counter per chat), `chat.turn.prompt.bytes` (size of the user input), `agent_controller.session.id` (the durable session id). Observability tools can navigate "turn 1 → turn 2 → …" without timestamp arithmetic.
- **Status**: named-return `turnErr` lets a deferred `span.End()` observe the error and set `Status=Error` + `RecordError` when the turn ends with `error` / `cancelled`. Matches the chat-root pattern from slice 6.3 codex pass 4.
- **No-op when tracing is off**: the OTel SDK's no-op tracer returns a no-op span; no allocation, no propagation, no perf cost.

Tests:
- `TestRunChatTurnEmitsChatTurnSpanWithIndex` — uses `tracetest.InMemoryExporter` to capture spans and asserts the attribute contract.
- `TestRunChatTurnTurnSpanMarkedErrorOnTurnFailure` — verifies `Status=Error` when the adapter emits `session.ended { reason: "error" }`.

What slice 6.5 does NOT do:
- No turn-counter persistence across `--resume`. Each chat invocation starts at `turn.index = 1` because the chat-root span is also fresh. Resuming gets a NEW chat-root (with its own trace id); the resumed turns still nest correctly within their own chat-root. This is the simpler / correct semantic for "one trace per chat invocation".
- No adapter-side awareness of turn boundaries. Pi/opencode just see the adapter session continue across turns; the turn-boundary structure lives at the host trace tree.

### Added (slice 6.4 — session lifecycle wire events)

Closes the v0.6 chat lifecycle. The slice 6.3 REPL already tracked session state; slice 6.4 makes the transitions observable on the wire and adds a sweep operation for retiring idle sessions.

- **Three new wire-event types** (`cli/internal/wire/events.go` + TS mirrors in both adapter packages):
  - `session.resumed` — emitted by `agentctl chat --resume <id>` immediately after a stored session is rehydrated. Payload carries `sessionId`, `agentName`, `createdAt`, `previousLastActiveAt`, and `previousStatus` so observability dashboards can compute time-since-last-touch and distinguish a resume-of-ended from a continuation-of-paused.
  - `session.paused` — emitted at chat exit when the user steps away without an explicit `/exit` (EOF, SIGTERM, idle SIGINT). The session record's `Status` becomes `paused`.
  - `session.expired` — emitted by `agentctl chat --resume <id>` when the targeted session was already swept to `StatusExpired`. The chat then bails with a clear error.
- **`Store.MarkExpired(ctx, cutoff)`** — new bulk operation on the Store interface. Transitions every `StatusActive` session whose `LastActiveAt` is strictly before `cutoff` to `StatusExpired`. Returns the IDs (sorted, for determinism). Only `Active` sessions are touched — already-`Paused` / `-Ended` sessions are left intact so the lifecycle history is preserved. Both `MemoryStore` and `SQLiteStore` implement it; SQLite uses a transactional SELECT + UPDATE so concurrent `Update` calls can't race it into inconsistency. Idempotent.
- **`agentctl sessions sweep --ttl <duration>`** — operator command for retiring idle sessions. Safe to run from cron / systemd timers. `--ttl 0s` is rejected (would expire everything). Defaults to 24h. Prints the list of expired ids to stdout; nothing to expire prints `no sessions to expire (cutoff <RFC3339>)`.
- **Chat exit-reason tracking**: `agentctl chat` now distinguishes the four exit paths and stores the right terminal status:
  - `/exit` → `StatusEnded` + `session.ended` wire event
  - EOF (Ctrl-D), SIGTERM, idle Ctrl-C → `StatusPaused` + `session.paused` wire event
  - Pre-slice-6.4 all exits set `ended`. Resume-fitness logic in slice 6.6 will prefer paused sessions when suggesting one to continue.
- **Store-enforced terminal-Expired invariant**: `Store.Update` now returns the new `sessions.ErrSessionExpired` sentinel when the existing row's `Status` is `StatusExpired` AND the incoming update would transition it elsewhere. Enforced atomically — Memory via mutex-held read-then-write, SQLite via a `WHERE id=? AND (status != 'expired' OR new = 'expired')` clause. Codex pass 1 P2 (the original Get-then-Update guard) was still racy: sweep could fire between the Get and the Update commit. Codex pass 2 P2 closed the race architecturally — the guard lives at the data layer. Chat's per-turn LastActiveAt bump and exit-status write both call `Update`; on `ErrSessionExpired` the chat emits `session.expired` on the wire, prints to stderr, and ends cleanly.
- **Pre-turn expiration check**: codex pass 3 P2 caught that the post-turn `Update` only catches expiration AFTER the model dispatch — letting an expired session run one extra prompt-and-tokens. Added a pre-turn touch via `Update`; on `ErrSessionExpired` the chat bails BEFORE submitting to the adapter. Best-effort (still need the post-turn check for the in-flight case) but closes the common "user idled past TTL then sent one more prompt" path.
- **Sweep-race during resume touch**: codex pass 3 P2 caught that `openOrResumeChatSession`'s Get-then-Update could race with sweep — the Update would return `ErrSessionExpired` but the caller wrapped it as a generic touch error, losing the `session.expired` wire signal. Now the resume path detects `ErrSessionExpired` and propagates via the same `errSessionExpired` sentinel `newChatCmd` already handles — so the wire event lands either way.
- **`MarkExpired` race-free via `UPDATE ... RETURNING`**: codex pass 4 P2 caught that the SELECT + UPDATE inside a deferred transaction couldn't promote a stale read snapshot under concurrent chat writes — `agentctl sessions sweep` could fail with SQLITE_BUSY. Rewrote as a single `UPDATE ... RETURNING id` (SQLite 3.35+, included in modernc.org/sqlite v1.52). No transaction needed; atomic at the row level.
- **Expiry check before cross-agent check on resume**: codex pass 4 P3 caught that a swept session with a drifted YAML would return the cross-agent error first, never surfacing `errSessionExpired` to `newChatCmd` → no `session.expired` wire event. The expired session isn't resumable under either spec; the TTL lifecycle event takes precedence.
- **`--ttl` help text** clarified: Go's `time.ParseDuration` doesn't support the `d` (day) unit — multi-day TTLs must be expressed in hours (`168h` not `7d`). Codex pass 1 P3.
- **Tests**: shared Store-contract harness gains a `MarkExpired` subtest (both Memory + SQLite pin identical semantics); chat tests updated for the new `openOrResumeChatSession` 3-return signature; `sessions_sweep_test.go` covers happy path, no-matches message, zero-TTL rejection, and the sweep-overwrite-by-chat-exit race guard.

What slice 6.4 does NOT do:
- No automatic background sweep — operators schedule `sessions sweep` themselves.
- Sweep doesn't emit `session.expired` on the wire (no active wire-stream consumer to emit on). The wire event is emitted by `chat --resume` when it hits a swept session.
- `agentctl sessions show <id>` / `sessions rm <id>` still pending — likely slice 6.6.

### Added (slice 6.3 — `agentctl chat` REPL)

First user-facing surface for v0.6. The slice 6.1 `Store` interface + slice 6.2 SQLite impl get a CLI driver: `agentctl chat <spec.yaml>` opens an interactive multi-turn chat that keeps a single session alive across many prompts. Pi's own session loader maintains the conversation context across turns; our `SessionStore` records the higher-level metadata (id, agent, lifecycle status, last-active timestamp, original spec snapshot, opaque adapter state).

- **`newChatCmd`** in `cli/cmd/agentctl/chat.go`. Behavior:
  - Parse + compile the spec, open the `SessionStore` (SQLite default; `--in-memory` for ephemeral chats), create or `--resume <id>` an existing `Session`. Cross-agent reuse — resuming a session whose stored agent name differs from the current spec — is **rejected** with a clear error: the model has been seeing the prior agent's persona and swapping mid-stream produces confused state.
  - One OTel root span wraps the whole REPL (slice 6.5 will refactor to per-turn child spans); host TRACEPARENT propagation from slice 5.2 + Pi adapter spans from slice 5.3 still apply per turn.
  - REPL loop reads stdin one line at a time, scanner buffer raised to 1 MiB to match the host wire stdout cap (long pasted prompts don't truncate mid-line).
  - `/exit` and EOF (Ctrl-D) end the chat cleanly; Ctrl-C (SIGINT) cancels the **current turn only** via `backend.Stop` and leaves the REPL alive. **SIGTERM** is handled at the REPL level — a process manager's shutdown actually terminates `agentctl chat` rather than getting absorbed as a turn-cancel. Codex pass 1 P2 caught the original handler that listened to both; codex pass 2 P2 caught that the SIGTERM-driven cancel went unnoticed while the main goroutine was blocked inside `scanner.Scan()` — fixed by reading stdin via a goroutine + selecting on the channel + the REPL context so an idle prompt also unwinds on SIGTERM.
  - Raw prompt text is dispatched unmodified (intentional leading whitespace — code blocks, indented snippets — survives). Trimming applies only to the empty-line + `/exit` dispatch decision. Codex pass 1 P3.
  - Turn failure detection: a terminal `session.ended { reason: "error" }` OR `{ reason: "cancelled" }` produces a turn error even when no separate `error` wire event is emitted. Codex pass 1 P2 caught the original gap.
  - Session lifecycle on early errors: any setup failure between session creation and the REPL's normal exit (OTel init, runtime resolution, staleness check, stdin scanner error) now marks the session record `StatusFailed` via a deferred cleanup that observes the named-return `runErr`. Without this, the session would stay `active` forever and `sessions ls` would show a ghost record for a chat that never actually ran. Codex pass 3 P2 caught the gap.
  - **Ctrl-C at idle prompt** — a top-level SIGINT handler catches the signal so a Ctrl-C while waiting at the prompt cancels the REPL cleanly (deferred session cleanup runs) instead of taking Go's default signal path that skips defers. An atomic `inTurn` flag tells the handler to ignore signals delivered DURING a turn — those are the per-turn handler's responsibility (cancel the turn, keep the REPL alive). Codex pass 4 P2.
  - **Scanner error vs EOF race**: the stdin-reader goroutine no longer closes `inputCh` when `scanner.Err()` is non-nil. Closing both `inputCh` AND `scanErrCh` would let the select pick the closed channel and treat a real scanner failure (e.g. a line over the 1 MiB cap) as a clean EOF nondeterministically. Now the two terminal signals are mutually exclusive. Codex pass 4 P2.
  - **Chat root span marked Error on failure**: deferred `span.End()` now observes `runErr` and calls `SetStatus(Error, ...)` + `RecordError` before ending — matches the `run` command's pattern (slice 5.1 codex pass 5). Without this, traced chat sessions that fail during setup would show UNSET status in BrainTrust / MLflow / Langfuse. Codex pass 4 P2.
  - **Stored-spec wins on `--resume`**: the freshly-parsed YAML is still validated against the schema but the stored `Session.Spec` drives the rest of the chat. The on-disk YAML is just a lookup credential. Any drift (model / persona / tools / runtime change) prints a `[resume] spec on disk differs from the session's stored snapshot` warning to stderr but does NOT silently apply mid-conversation — that would have changed tools/persona under the model without it knowing, the same "confused state" failure mode the cross-agent check guards against. Codex pass 5 P2.
  - **Single-channel signal dispatch** (codex passes 5 / 6 / 7 of slice 6.3 — three iterations on the same fan-out hazard):
    - Pass 5: a Ctrl-C delivered to BOTH the per-turn handler AND the idle channel's buffer would fire as an idle interrupt after the turn returned and end the REPL.
    - Pass 6: the pass-5 fix (`signal.Stop` + re-`Notify` around each turn) opened a window with NO SIGINT handler — a Ctrl-C in that window fell through to Go's default and killed the process.
    - Pass 7: the pass-6 fix (always-armed idle channel + `inTurn` flag check) had an inherent race — the dispatcher goroutine could take a signal, be descheduled BEFORE the flag check, and by the time it resumed the turn had ended → flag=false → REPL ends, even though the user pressed Ctrl-C mid-turn.
    - Final fix: SINGLE signal channel, no fan-out. One dispatcher goroutine routes every SIGINT/SIGTERM. SIGTERM always ends the REPL; SIGINT is forwarded to `turnCancelCh` if a turn is active (the REPL loop reads `turnActive` written by it as the only writer), or ends the REPL otherwise. `runChatTurn` listens on `turnCancelCh` to cancel its own ctx + call `be.Stop(h)`. Each signal is delivered exactly once to exactly one consumer — no race possible.
  - **Runtime-type rejection moved post-resume**: `local-opencode` is rejected based on the EFFECTIVE spec (the stored one after `--resume`, the freshly-parsed one otherwise). Without this, a user with a working Pi chat would fail to resume just because their YAML drifted to opencode. Codex pass 6 P2.
  - **Cleanup-defer registered before runtime check**: codex pass 8 P2 caught that the runtime-type rejection returned BEFORE the deferred `StatusFailed` cleanup was installed, so an opencode-rejection left the session record stuck `active`. Re-ordered to install the cleanup defer first.
  - **SIGINT grace window**: codex pass 8 P2 caught that even the single-channel design (pass 7) had an unresolvable race — a signal queued during a turn could be dispatched AFTER the REPL clears `turnActive`, getting routed as REPL-end. The fix is fundamental-limit-aware: a 50ms grace window after each turn end where SIGINT still routes to the (just-ended) turn channel rather than ending the REPL. Kernel signal delivery is asynchronous; no pure flag-based design can perfectly synchronize with user-space state. The bias is intentional — a stray cancel routed to a finished turn is a benign no-op; an unintended REPL-end is catastrophic to user intent. 50ms is ~50x typical signal latency, well below the time a user spends reading the prompt before deciding to send a deliberate idle-Ctrl-C.
  - Each turn: clone the spec, set `Task = userInput` and `SessionID = &sess.ID` on the LOCAL copy (the caller's spec stays untouched), dispatch through `LocalBackend.Submit`, drain events, update `Session.LastActiveAt` in the store.
  - On exit: mark `Session.Status = ended` in the store (failure here is informational, not fatal — the chat already happened).
- **opencode rejected** (`runtime.type: local-opencode`) with a clear message — opencode's `--resume` story isn't there yet (slice 3.4 noted this for `run`; chat inherits the same gap). Pi adapter only for MVP.
- **Tests** in `chat_test.go` (11 cases): `--in-memory` vs default SQLite store selection (the SQLite path tests persistence across reopen against a per-test `$XDG_DATA_HOME`); session create/resume; resume refresh of `Status` + `LastActiveAt` while preserving `CreatedAt`; cross-agent reuse rejection; missing-resume-id error; per-turn `Task` + `SessionID` injection via a fake backend; the per-turn LOCAL-copy invariant (caller's spec.Task / SessionID don't drift across turns); error-event-as-turn-error semantics; clean-completion semantics; Submit-failure propagation. The TTY-driven REPL loop itself is exercised end-to-end in slice 6.6's acceptance test against a real adapter.

What slice 6.3 does NOT do:
- K8s `--binding` chat (each turn spawning a fresh Pod is wasteful and slow) — deferred to slice 6.6.
- Lifecycle wire events `session.resumed` / `session.paused` / `session.expired` — slice 6.4.
- Per-turn OTel child spans under a chat-root span — slice 6.5.
- `agentctl sessions show <id>` / `agentctl sessions rm <id>` — likely 6.4 with the lifecycle event work.

### Added (slice 6.2 — SQLite session store)

File-backed `Store` impl. The default session store for v0.6; in-memory (slice 6.1) is now the fallback for tests and ephemeral REPLs.

- **`SQLiteStore`** in `cli/internal/sessions/sqlite.go`. Wraps a single SQLite connection (writes are SQLite-serialized anyway) with WAL mode + `synchronous=NORMAL` + a 5-second busy timeout. Schema lives inline as `schemaSQL` + `pragmaSQL`; both are idempotent so `NewSQLiteStore` is safe to call repeatedly on the same file. `schema_version` column reserved for future migrations.
- **`modernc.org/sqlite`** picked over `mattn/go-sqlite3`: pure Go (transpiled from C), no CGO, keeps cross-platform binary distribution simple. Negligible perf cost for our workload (single-digit writes per chat session).
- **`DefaultSQLiteStorePath()`** resolves the conventional location: `$XDG_DATA_HOME/agent-controller/sessions.db` when XDG is set, otherwise `$HOME/.local/share/agent-controller/sessions.db`. `NewSQLiteStore` mkdir's the parent at mode `0700` if missing — session metadata is per-user.
- **Shared contract test harness** in `contract_test.go`. Both `MemoryStore` and `SQLiteStore` run through the same 11 subtests so both impls are pinned to identical behavior. New impls (Postgres / Redis in v0.8) just plug in via a `storeFactory` closure.
- **SQLite-specific tests**: in-memory (`:memory:`) AND file-backed runs of the full contract, persists-across-reopen, creates-parent-directory, rejects-empty-path, idempotent Close, concurrent Close + ops race coverage (codex pass 1), XDG / HOME path resolution.
- **Concurrent-Close safety**: `sync.Once` around `db.Close()` + the `*sql.DB` handle is NOT nil'd, so a racing `Close + Create/Get/...` from another goroutine fails cleanly with `sql.ErrConnDone` instead of panicking on a nil-pointer dereference. Codex pass 1 P2 caught the original unsynchronized read/write of `s.db`.
- **Tight file permissions**: `NewSQLiteStore` chmods the DB / `-wal` / `-shm` files to `0o600` after they're created (every open re-enforces). Without this, a typical `0o022` umask leaves the files `0o644` — readable by other local users — and `spec_json` can hold MCP env vars / headers that are real secrets (API keys, OAuth tokens). Codex pass 2 P2.
- **Narrow-ownership directory chmod**: parent dir is chmodded to `0o700` ONLY when `NewSQLiteStore` itself created it (detected via a pre-MkdirAll Stat). Pre-existing parent dirs — including the CWD when a caller passes a bare filename like `./sessions.db` — are left alone. Codex pass 3 P2 caught the unconditional chmod that would have silently stripped group/other perms from caller-owned directories.
- **`busy_timeout` applied before WAL setup**: split out as the first pragma Exec, BEFORE `journal_mode=WAL`. The WAL switch briefly takes an EXCLUSIVE lock; without an installed busy handler, a concurrent SQLITE_BUSY at that instant fails open immediately even though the 5s retry was intended to cover it. Codex pass 4 P2.
- **SQLite `file:` URIs explicitly rejected**: a previous doc-comment claimed they were accepted, but `filepath.Dir` mangles them during the parent-dir prep + permission hardening (`file:/tmp/sessions.db?cache=shared` → bogus `file:/tmp` directory creation, and the real DB file misses the chmod). Now `NewSQLiteStore` returns a clear error for any `file:`-prefixed path. Codex pass 4 P2.
- **Forced WAL/SHM creation at open**: a `BEGIN IMMEDIATE; COMMIT;` runs after the schema Exec so the WAL + SHM files always exist by the time the chmod loop runs. Without this, reopening an existing DB (where the idempotent schema is a no-op) leaves WAL/SHM uncreated at chmod time; the next `Create`/`Update` then creates them with the process umask (`0o644`), leaking `spec_json` secrets. SQLite preserves file perms through subsequent WAL checkpoints, so a once-at-open chmod sticks. Codex pass 5 P2.
- **DSN query strings also rejected**: paths containing `?` (e.g. `sessions.db?_pragma=busy_timeout(5000)`) are modernc DSN query strings — the driver strips them at open, but our chmod uses the literal path and hardens nonexistent files while the REAL DB/WAL/SHM stay at the process umask. Same class of bug as the `file:` URI rejection; same fix. Codex pass 6 P2.

What slice 6.2 does NOT do:
- No `agentctl chat` CLI surface yet — slice 6.3 wires the REPL into either store via a config knob.
- No expiry / TTL sweep — slice 6.4.
- No `sessions ls/show/rm` subcommands — likely slice 6.3 alongside chat, or 6.6 if scope gets tight.

### Added (slice 6.1 — SessionStore interface + in-memory impl)

First slice of v0.6 (long-running agents). Lays the durable-session foundation that slice 6.3 (REPL), 6.4 (lifecycle events), and 6.5 (cross-turn tracing) all build on. No user-facing surface yet.

- **New package `cli/internal/sessions`** with the `Store` interface, the `Session` value type, the `SessionStatus` enum (`active` / `ended` / `paused` / `expired` / `failed`), and `ListFilter`. Five statuses defined upfront so slice 6.4 (paused/expired) doesn't have to widen an in-use enum.
- **Session field mutability split** pinned by the doc-comment and enforced by every Store impl:
  - Immutable after `Create`: `ID`, `AgentName`, `RuntimeType`, `CreatedAt`, `Spec`, `TraceContext` — session identity can't drift through Update.
  - Mutable on `Update`: `Status`, `LastActiveAt`, `AdapterState` (an opaque map the adapter uses to find its on-disk state).
- **`MemoryStore`** — ephemeral in-memory implementation, sync.RWMutex-guarded. Suitable for tests + REPL fallback when SQLite (slice 6.2) is unavailable. Returns deep copies (not interior pointers) so callers can't mutate stored sessions through returned values — `CompiledSpec`'s nested slices and maps (`Tools[].Config`, `MCPServers[].Env`, etc.) are JSON-roundtripped to preserve isolation; the spec is already designed to be JSON-serializable as the wire-format payload to adapters, so the roundtrip is lossless and consistent with existing usage. Codex pass 1 caught the shallow-copy bug. List sorts by `LastActiveAt` desc with `ID` tiebreak for determinism. Close is idempotent.
- **13 tests** covering: create-then-get roundtrip, duplicate-ID rejection, empty-ID rejection, NotFound semantics on Get/Update, the immutable-on-Update contract, idempotent Delete, List filter + order + limit, Close drops data + errors after, shallow copy-isolation on Create and Get, deep copy-isolation through nested spec slices/maps (codex pass 1), deep copy-isolation through nested AdapterState maps/slices (codex pass 2), context cancellation.

What slice 6.1 does NOT do:
- No SQLite impl yet — slice 6.2.
- No `agentctl chat` / `sessions ls` / etc. CLI surface — slice 6.3.
- No lifecycle wire events (`session.resumed`, `session.paused`, `session.expired`) — slice 6.4.
- No span linkage across resumed turns — slice 6.5.

## [0.5.1] — 2026-06-11

The v0.5.1 release rolls slices 5.2 through 5.6 — every piece of the v0.5 tracing track beyond the host-side OTel root span that shipped in 0.5.0 (slice 5.1). Together these slices deliver a fully stitched OTel trace from the host `agentctl.run` span down through Kubernetes Pod env, the runtime adapter (Pi or opencode), and per-LLM / per-tool spans inside the adapter — emit-able to any OTLP-HTTP collector.

Coordinated artifact bumps:
- `agentctl` v0.5.1 (CLI)
- `@agent-controller/runtime` 0.5.1 (Pi adapter)
- `@agent-controller/runtime-opencode` 0.5.1 (opencode adapter)
- `ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.4` (runtime image carrying the full chain)

### Added (slice 5.6 — OTLP roundtrip acceptance)

The v0.5 tracing track's acceptance gate. Stands up an `httptest.Server` that emulates an OTLP-HTTP collector (BrainTrust, MLflow, Langfuse, Jaeger, otel-collector — all implement the same wire surface), runs the host's `InitTracerProvider` + `StartRootSpan` path against it, decodes the received protobuf payload, and asserts:

- **Wire format**: requests hit `/v1/traces` with `Content-Type: application/x-protobuf` (transparently handles gzip-compressed bodies in case the SDK enables compression in a future release)
- **Span attributes**: `gen_ai.system`, `gen_ai.request.model`, `gen_ai.operation.name`, `agent_controller.agent.name`, `agent_controller.runtime.type` all present and match the values passed via `RunAttributes`
- **Resource attributes**: `service.name = "agentctl"` and `service.version` match the build version (operators filter dashboards by `service.name`; drift here breaks saved views silently)

This is the test the v0.5 tracing track was waiting on — it proves that what we emit is what any generic OTLP collector will ingest, without needing a real one in CI. New file: `cli/internal/observability/otlp_roundtrip_test.go`. Promotes `go.opentelemetry.io/proto/otlp` and `google.golang.org/protobuf` from indirect to direct deps (needed for the protobuf decode in the fake collector handler).

### Added (slice 5.5 — K8s trace-chain verification)

Pure test addition. Slice 5.2 already shipped both halves of the K8s end-to-end propagation (host-side `KubernetesBackend.Submit` injecting `TRACEPARENT` into the Pod container env; in-Pod `agentctl` extracting via `ExtractTraceContextFromEnv` after `InitTracerProvider`); slice 5.5 closes the verification gap with three integration-level tests in `cli/internal/backend/kubernetes_test.go`:

- `TestKubernetesBackendSubmitPropagatesActiveTraceparentToPodEnv` — Submits with an active OTel span in ctx, then inspects the resulting Pod's container envVars and asserts `TRACEPARENT` is present in W3C format AND its trace-id segment matches the host span's trace id. Also confirms `Value`-not-`ValueFrom` (so a `secretKeys` collision can't accidentally hijack the trace header).
- `TestKubernetesBackendSubmitOmitsTraceparentWithoutActiveSpan` — pins the no-op contract: when the host has no active OTel span, the Pod's env carries neither `TRACEPARENT` nor `TRACESTATE`. Prevents a regression that would deliver a stale/empty parent to the in-Pod agentctl and leave it producing a detached root span.
- `TestKubernetesBackendSubmitTraceEnvCoexistsWithSecretRefEnv` — Verifies that `secretRef`-injected env vars (`ANTHROPIC_API_KEY` via `ValueFrom: SecretKeyRef`) and trace-injected env vars (`TRACEPARENT` via plain `Value`) coexist on the same container without one clobbering the other. Defense against a future refactor that might re-route trace injection through a SecretKeyRef.

Together with the existing `trace_propagation_test.go` (helper-level) and `env_propagation_test.go` (extraction-level) suites, these tests pin every link of the K8s trace chain at the unit-test layer without needing a real cluster.

### Added (slice 5.4 — opencode adapter tracing)

Mirror of slice 5.3 for the opencode adapter. Smaller delta than the Pi adapter because opencode owns its own LLM/tool span emission via the Vercel AI SDK's `experimental_telemetry`; the adapter only needs to wrap the run in an `agent.session` parent span and flip opencode's config flag.

- **`spec.observability.tracing: true`** flips `experimental.openTelemetry: true` in the opencode config the SDK passes to the spawned `opencode` child. The child's AI-SDK telemetry then emits its own LLM + tool spans via whatever OTel SDK opencode initializes from env.
- **`runtime-opencode/src/observability.ts`** (new): same two-condition gate as the Pi adapter (`spec.observability.tracing` AND `OTEL_EXPORTER_OTLP_ENDPOINT`/`_TRACES_ENDPOINT` set). When live, opens an `agent.session` span with `gen_ai.*` attributes nested under the host TRACEPARENT (slice 5.2 env injection).
- **`process.env.TRACEPARENT` rewrite**: after the adapter opens its `agent.session` span, it overwrites `process.env.TRACEPARENT` with the new span's id so the opencode child — which the SDK spawns via `cross-spawn` with `{ ...process.env }` — picks up the deeper parent. Without this rewrite, opencode's spans would skip the `agent.session` level and parent off `agentctl.run` directly. Stale `TRACESTATE` from the host is also cleared on the same path (mirrors the cleanup the Go injectTraceparent does, slice 5.2 codex pass 2).
- **Wire-envelope stamping**: when tracing is active, every wire event picks up `apiVersion: v1alpha1` + the active `traceparent`. Implemented via a module-level `activeTraceparent` holder rather than a function param rewrite — opencode adapter has one session per process so the holder is unambiguous.
- **Shutdown** uses the same 5s three-layered timeout pattern as the Pi adapter (OTLP exporter `timeoutMillis`, BSP `exportTimeoutMillis`, outer `Promise.race` watchdog). Setup-failure and cancellation paths both flush correctly:
  - **Setup throws** (e.g. `createOpencode` fails, missing `opencode` CLI): `initAdapterTracing` lives in `main()` rather than `runOpencode`, so the outer catch in `main()` runs `tracing.end("error", realExceptionMessage)` with the actual error — the span records the real cause, not a placeholder. Terminal wire events emitted from main's catch still carry `apiVersion` + `traceparent` because `activeTraceparent` isn't cleared until after the emit. Codex pass 1 P2.
  - **SIGINT / SIGTERM**: the signal handler now defers `process.exit(130)` through an async IIFE so the OTel BSP gets its full 5s shutdown budget before the process terminates. A re-entry guard prevents a second signal during the flush from re-running the handler body. The cancelled-session wire event goes through `emit()` rather than a raw `stdout.write`, picking up the new envelope fields. Codex pass 1 P2.
- **Race-free terminal-event ownership**: codex pass 2 caught two race conditions exposed by the new async tracing flushes — (a) on cancellation, the SSE main loop saw the abort-induced stream close and was about to append a duplicate `error` + `session.ended(error)` after the handler's `cancelled` terminator; (b) on success/error exit, the post-emit `tracing.end()` 5s flush left the signal listeners installed, so Ctrl-C during that flush would emit a second `cancelled` terminator and flip the exit code to 130 on an already-completed run. Fixed by (a) a `cancelledByUser` flag the main loop checks before its terminal-emit block, and (b) removing signal listeners *before* awaiting the post-emit `tracing.end()`.
- **Reliable stdout drain on cancellation**: codex pass 3 caught that the `writableNeedDrain` check from pass 1 leaves a hole — for small writes (which the cancelled terminator usually is) the flag stays false even though bytes are still queued, so `process.exit(130)` could fire before the line actually shipped. Restored the write-callback pattern (empty `process.stdout.write("", cb)` resolved only when the OS has consumed everything queued ahead of it) so the cancelled wire event is guaranteed to land before exit.
- **Cancellation-fetch-failure suppression**: codex pass 4 caught a third cancellation signature — SDK requests like `session.create()` or `promptAsync()` whose underlying connection is severed by `abortController.abort()` reject with `TypeError: fetch failed`, NOT `AbortError`. The old string-only check (`err.name === "AbortError" || err.message.includes("aborted")`) missed those, so main's catch was about to emit duplicate `error` + `session.ended(error)` after the handler's `cancelled` terminator. Fixed by hoisting `cancelledByUserGlobal` to module scope (set synchronously by the signal handler) and checking it first in main's catch — string matches remain as defense-in-depth.
- **Server-close ordering across all three exit paths**: codex pass 5 caught that the new async `tracing.end()` 5s flush left the opencode child running during the wait. On cancellation that meant the child kept doing tool work for up to 5s after Ctrl-C (visible to the user as ignored cancellation); on success/error with handlers already removed, an unhandled SIGINT during the flush would take Node's default-exit path, skip `finally`, and orphan the child. Fixed by calling `server?.close()` BEFORE `tracing.end()` in all three exit paths. A `sessionTerminated` flag now also drops any wire event that arrives via emit() after a terminator was already written — closes the residual race where an in-flight SSE event lost to the cancelled terminator by microseconds.

### Known limitation
- **Adapter `agent.session` → opencode child link is best-effort**: the adapter writes `process.env.TRACEPARENT` so the opencode child inherits the deeper parent context, but opencode's AI-SDK telemetry only nests under it if opencode itself extracts `TRACEPARENT` during its OTel init. We have no way to enforce that from outside the binary. Until verified against a real opencode build (slice 5.6 acceptance), opencode-emitted LLM/tool spans may surface as detached roots in your collector — the adapter's `agent.session` span will still be present and correctly nested under `agentctl.run`. Codex pass 4 flagged this; the env injection stays for future-compat and zero-cost upside.

What slice 5.4 does NOT do:
- The captureContent path captures `spec.task` on the adapter's `agent.session` span only (matching the Pi pattern). Per-LLM-call prompt/completion capture inside opencode is opencode's responsibility via its own AI-SDK telemetry semconv attrs — we don't reach into the child to control that.
- No end-to-end BrainTrust / MLflow / Langfuse verification yet — slice 5.6.

### Added (slice 5.3 — Pi adapter OTel emission)

Pi-adapter side of the v0.5 tracing story. The host emits a root span (slice 5.1) and threads TRACEPARENT into the adapter env (slice 5.2); slice 5.3 makes the Pi adapter consume that parent and emit its own child spans for sessions, LLM calls, and tool calls.

- **`runtime/src/observability.ts`** (new): self-contained OTel SDK init. Returns a no-op tracer unless BOTH `spec.observability.tracing: true` AND `OTEL_EXPORTER_OTLP_ENDPOINT` is set — matches the host-side two-condition gate. When live, the module:
  - Sets up `NodeTracerProvider` + `BatchSpanProcessor` + `OTLPTraceExporter` (HTTP, JSON).
  - Extracts the host TRACEPARENT via the W3C propagator and opens an `agent.session` span under it.
  - Exposes a callback surface (`AdapterTracing`) the adapter calls from its Pi event subscriber.
- **`runtime/src/adapter.ts`** wires the Pi event stream into the tracer:
  - `turn_start` / `turn_end` → `gen_ai.chat` span. (Pi's `before_provider_request` / `after_provider_response` are extension-runner hooks and never reach a session subscriber — codex pass 1 of slice 5.3 caught the original mis-wiring against the wrong event channel.) Token usage + `stopReason` get pulled from `turn_end.message.usage` / `stopReason`; an error/aborted stopReason marks the LLM span as ERROR.
  - `tool_execution_start` / `tool_execution_end` → `gen_ai.tool.call <name>` span nested under the active LLM span (so tools parent off the LLM that requested them, matching the OTel GenAI semconv guidance).
  - Defensive cleanup if Pi ever leaves a span open (LLM superseded without an end event, tools open at session.dispose) — set to ERROR and closed at session shutdown.
  - `session.dispose` is followed by an awaited `tracing.end()` so the BatchSpanProcessor drains before the adapter subprocess exits. 5s timeout mirrors the host flush budget. The watchdog timer is `clearTimeout`'d after the shutdown wins the race so it cannot keep the Node event loop alive for the full 5 seconds on normal exits (codex pass 1 P2).
- **`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`** support: the env-resolver honors the trace-specific variable in addition to the generic `OTEL_EXPORTER_OTLP_ENDPOINT`, with the trace-specific one winning when both are set — matches the standard OTel SDK precedence and the host-side `isEnabled()` check in `cli/internal/observability/otel.go`. Codex pass 1 P2 caught the silent no-op when only the trace-specific var was configured.
- **Setup-failure flush path**: `runSession` now wraps its post-tracing-init body in an outer try/catch that emits an `error` + `session.ended` terminator and calls `tracing.end("error", ...)` on any escape (e.g. `createAgentSession` or `bindExtensions` throwing). Without this the early setup throws left the `agent.session` span open and dropped the failure from telemetry. The catch only re-throws when the terminator emit itself failed — otherwise returns normally, so `index.ts` doesn't append a duplicate error after the terminal `session.ended`. Codex pass 2 P2 + pass 3 P2.
- **Prompt capture**: when `captureContent` is on, `spec.task` is stamped as `gen_ai.prompt` on the `agent.session` span at init time, so specs opting into content capture get the full prompt → completion → tool args → tool result chain that the schema docs promise. Codex pass 2 P2.
- **`@opentelemetry/sdk-trace-base`** is now an explicit runtime dep (`runtime/package.json`). Previously the `BatchSpanProcessor` import resolved only via hoisting in flat `node_modules`; under pnpm / Yarn PnP / a future npm layout that doesn't hoist, the import would have failed at adapter startup. Codex pass 2 P2.
- **Three-layered shutdown timeout**: the OTLP exporter's `timeoutMillis`, the `BatchSpanProcessor`'s `exportTimeoutMillis`, AND the outer `Promise.race` watchdog are all set to a shared 5s budget. Without the SDK-level timeouts, a slow or blackholed collector at process exit could keep the adapter alive 30s past intent (the BSP's default export-timeout) — `Promise.race` stops awaiting but does not cancel pending I/O. Codex pass 4 P2.
- **Content-cap correctness**: `truncate()` now reserves space for the `…[truncated]` marker (13 UTF-8 bytes) before slicing, so the returned attribute's byte length is always `<= MAX_CONTENT_BYTES` even on the truncation path. Previously the marker was appended after slicing to the full cap, overflowing it. Codex pass 4 P2.
- **Session id consistency on `--resume`**: the adapter's `agent.session` span (and the resource `agent_controller.session.id` attribute) prefer `spec.sessionId` over the ephemeral wire-id when set, so resumed runs index host + adapter spans under the same session id. The wire-event `sessionId` stays ephemeral (wire semantics predate `--resume` and changing them would break existing consumers). Codex pass 5 P2.
- **Model attrs on every `gen_ai.*` span**: `gen_ai.system` and `gen_ai.request.model` are stamped directly on every `gen_ai.chat` span (and `gen_ai.system` on every `gen_ai.tool.call` span). OTel attributes don't inherit through the span tree, so backends that filter LLM/tool spans by provider or model would have seen blanks without this. Codex pass 5 P2.
- **`spec.observability.captureContent`** (new ADL field): when true, the adapter attaches `gen_ai.completion`, `gen_ai.tool.call.arguments`, and `gen_ai.tool.call.result` as span attributes. Off by default for privacy. Content payloads are JSON-stringified and capped at 64 KiB per attribute (matches the host stdout-scanner buffer cap in `cli/internal/backend/local.go`) — large tool outputs get a `…[truncated]` marker rather than blowing up the OTLP batch.
- **Wire-envelope stamping**: when tracing is active on the adapter side, `apiVersion: "agent-controller.dev/events/v1alpha1"` (the slice 5.2 reservation) and the active `traceparent` get stamped onto every outgoing wire event via a thin `wrapEmitWithTracing` wrapper. When tracing is off the wrapper is the identity — no per-event overhead and legacy v0.x consumers keep seeing the legacy envelope shape.
- **Mirrors**: `cli/internal/adl/types.go::Observability`, `cli/internal/adl/compiler.go` extract path, both copies of `schemas/adl.v1alpha1.json`, and `runtime/src/types.ts` + `runtime-opencode/src/types.ts` all gain the `captureContent` field.

What slice 5.3 does NOT do yet:
- The opencode adapter still ignores tracing — slice 5.4 flips its `experimental.openTelemetry: true` flag and threads TRACEPARENT the same way.
- BrainTrust / Databricks MLflow / Langfuse end-to-end acceptance is slice 5.6.
- The `audit.event` / `artifact.created` event types reserved in slice 5.2 still have no emitters.

### Added (slice 5.2 — wire-event protocol v1alpha1)

Prerequisite for adapter-side payload capture in slices 5.3 / 5.4.

- **Wire-event envelope** now carries two optional fields:
  - `apiVersion`: `agent-controller.dev/events/v1alpha1` once an adapter starts emitting the new shape (5.3+). Absent on legacy v0.4.x and earlier — consumers should treat absence as the implicit v0 namespace.
  - `traceparent`: W3C TraceContext header value, so adapter-side spans can be stitched as children of the host `agentctl.run` span emitted in slice 5.1.
- **Reserved event types** (constants only — emission lands in slices 5.3 / 5.4 / 5.5):
  - `tool.started`, `tool.completed`, `tool.failed` — splits the legacy `tool.call`/`tool.result` pair into three discrete events with richer span boundaries (maps onto OTel `gen_ai.tool.call.*` semconv)
  - `artifact.created` — emitted when an agent run produces a durable artifact
  - `audit.event` — structured audit log entries for the v0.5+ governance track
- **Host-side TRACEPARENT injection.** `LocalBackend.Submit` adds `TRACEPARENT` (and `TRACESTATE` when present) to the runtime adapter subprocess env via the OTel propagator. `KubernetesBackend.Submit` does the same into the Pod's agent-container env. Tracing-off is a no-op (the SDK's default propagator writes nothing) so this is unconditional — `injectTraceparent` in `cli/internal/backend/trace_propagation.go`. The helper strips any pre-existing `TRACEPARENT`/`TRACESTATE` from the caller env before appending the freshly-extracted pair, so a child process can never receive a fresh trace id paired with a stale vendor tracestate inherited from a wrapper script (codex pass 2 of slice 5.2 caught the pairing bug).
- **Entrypoint TRACEPARENT extraction.** Symmetric to the injection: `agentctl` reads `TRACEPARENT` (+ `TRACESTATE`) from `os.Environ` immediately after `InitTracerProvider`, so when it runs inside a Pod launched by an outer agentctl, its root span nests under the host `agentctl.run` span instead of starting a detached trace. `observability.ExtractTraceContextFromEnv` is a no-op when env is unset or malformed — codex pass 1 of slice 5.2 caught the missing extraction half.
- **TS mirrors.** Both `runtime/src/types.ts` and `runtime-opencode/src/types.ts` add `apiVersion` + `traceparent` to `WireEvent`, the new event-type literals, and export `EventsAPIVersionV1alpha1` so adapter code in 5.3 / 5.4 can stamp the namespace consistently.

What slice 5.2 does NOT change yet:
- Existing `tool.call` / `tool.result` events keep their current shape and are still emitted by both adapters — no break.
- No adapter actually reads `TRACEPARENT` yet. That wiring lives in slices 5.3 (Pi via `@raindrop-ai/pi-agent`) and 5.4 (opencode via `experimental.openTelemetry`).

### Added (slice 5.1 — first chunk of v0.5.0)

Coordinated bumps for the v0.5.0 release: CLI version `v0.5.0`, npm adapters `@agent-controller/runtime@0.5.0` and `@agent-controller/runtime-opencode@0.5.0` (version-bump only — adapters themselves unchanged this release), runtime image `runtime-image/v0.1.3` (carries the new agentctl + schema that recognizes `spec.observability`). Go floor bumped to **1.25** for the OTel SDK v1.44+ requirement; runtime-image Dockerfile builder bumped to `golang:1.25-bookworm` in lockstep.

- **OpenTelemetry root span around `agentctl run`.** New `internal/observability/` package wires the OTel Go SDK + OTLP/HTTP exporter. Each `agentctl run` opens an `agentctl.run` span tagged with OTel GenAI semconv attributes (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.operation.name`) plus agent-controller-specific attributes (`agent_controller.agent.name`, `agent_controller.runtime.type`, `agent_controller.backend.type`, `agent_controller.binding.name`, `agent_controller.session.id`). The `backend.type` attribute separates Kubernetes-backend submissions from local ones (where `runtime.type` would otherwise be misleading — it names the in-Pod adapter, not the orchestration plane). Span status flips to Error for any non-zero exit (including reason=cancelled). The span opens immediately after spec parse, so preflight failures (`--resume`/`local-opencode` mismatch, invalid `--binding`, K8s client setup, staleness check) all get traced. RunE uses a named return so the defer captures the actual returned error even when error paths shadow the outer `err` variable. Adapter-side and per-tool spans land in slices 5.3 / 5.4.
- **`spec.observability.tracing`** ADL field (boolean, default false). The spec must opt in AND the operator must point `OTEL_EXPORTER_OTLP_ENDPOINT` at a collector — either missing → no-op provider (zero cost). Lets a sensitive spec disable tracing even when the host has the env exporter configured. The compiler passes the field through into CompiledSpec; the KubernetesBackend round-trip preserves it into the in-Pod Agent YAML.
- **`main.version`** string variable wired into the ldflags-injection that release.yml already does, surfaced as `service.version` on every span so traces correlate to a specific CLI build.
- **[`examples/tracing-demo.yaml`](examples/tracing-demo.yaml)** with the env-var commands for a local OTel collector and for BrainTrust.

**Roadmap pivot 2026-06-10** — see [`ROADMAP.md`](ROADMAP.md) for the full rationale. After shipping the v0.4.0 KubernetesBackend skeleton (slice 4.3), the next four minor releases focus on observability + agent shapes rather than further K8s backend polish:

- **v0.5.0 — Tracing (OpenTelemetry).** GenAI semconv, OTLP exporter, three-layer trace propagation (CLI → adapter → tool/MCP), one observability-platform smoke (MLflow or BrainTrust). Includes the event-protocol v1alpha1 bump (`tool.{started,completed,failed}`, `artifact.created`, `audit.event`).
- **v0.6.0 — Long-running agents (REPL + durable sessions).** `agentctl chat`; pluggable session store (SQLite default, in-memory for tests); foundation for the v0.8 server.
- **v0.7.0 — Declarative orchestration.** New `AgentWorkflow` ADL kind for multi-agent pipelines. `agentctl workflow run`. Composes with v0.5 tracing and v0.6 sessions.
- **v0.8.0 — Long-running agents, part 2.** `agentctl serve` with HTTP/SSE endpoints; multi-tenant primitives; workflow server endpoint.

Deferred to opportunistic background (no committed release): slice 4.4 (advanced K8s binding fields), slice 4.5 (sandboxing enforcement), slice 4.7 (Docker local-container backend). They ship when there's a clear user-signal pull.

### Added (pre-v0.4.0)

Slice 4.3 lands the K8s skeleton. Coordinated bumps for the v0.4.0 release: CLI version `v0.4.0`, npm adapters `@agent-controller/runtime@0.4.0` and `@agent-controller/runtime-opencode@0.4.0` (version-bump only — no functional change in the adapters themselves; the release workflow gates require matching versions), runtime image `runtime-image/v0.1.2` (carries the new agentctl with `--ndjson-stdout`).

- **`runtime-image/`** — multi-stage Dockerfile + GHCR publish workflow for `agent-runtime-base`. Image bundles `agentctl` (built from `cli/` in-tree), `@agent-controller/runtime`, `@agent-controller/runtime-opencode`, and the `opencode-ai` CLI; non-root user; tini PID-1; `linux/amd64` + `linux/arm64`. Versions independently of the umbrella tag — `runtime-image/v0.1.0` (first publish), `v0.1.1` (opencode-postinstall fix + adapter 0.3.3), `v0.1.2` (agentctl with `--ndjson-stdout` + adapters at 0.4.0; required by `KubernetesBackend`).
- **`KubernetesBackend`** (slice 4.3): new `Backend` implementation that submits an agent run as a Pod from `agent-runtime-base`. Adds `RuntimeBinding.spec.target.type: kubernetes` plus `target.kubernetes.{namespace, image, secretRef.{name, keys}}` (the schema's conditional `required` rule rejects `kubernetes` targets that omit `secretRef` at `agentctl validate` time, not at runtime). The CLI dispatches to this backend automatically when the active Binding's target.type is `kubernetes`. Submit creates a **Secret** holding the rendered Agent YAML (Secret rather than ConfigMap because `spec.mcpServers[].env` and `headers` can carry API tokens), creates a Pod that mounts it at `/workspace/spec.yaml` via `subPath` (so `/workspace` stays writable), reads adapter credentials from the user-supplied Secret named in `secretRef.name`, passes `--ndjson-stdout` to the in-Pod agentctl so its stdout is parseable wire NDJSON, sets `AGENT_CONTROLLER_RUNTIME` to the opencode adapter when `spec.runtime.type` is `local-opencode`, and streams the in-Pod agentctl's NDJSON stdout as wire events (always specifying `Container: "agent"` so sidecar-injected Pods still stream). Stop deletes the Pod and spec Secret; cleanup also fires automatically when the log stream ends. User Ctrl-C surfaces as `session.ended reason=cancelled` (matches LocalBackend's exit-130 convention) rather than `reason=error`. `NewKubernetesBackend` tries `rest.InClusterConfig()` before kubeconfig discovery when no explicit path/context override is set, so `agentctl` runs from inside a Pod with only a ServiceAccount token. Default image is `ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.2` (the version that carries the matching agentctl).

  Out of scope for 4.3 and deferred to later 4.x slices: SecurityContext / NetworkPolicy enforcement (4.5), explicit kubeconfig path / context / serviceAccount fields (4.4), Job vs Pod toggle, registry-backed specs in the Pod, custom Pi-extension tools / extensions / skills / subagents / installs / `--resume` (all rejected by `validateCompiledSpecForK8s`). See [`examples/bindings/kubernetes-kind.yaml`](examples/bindings/kubernetes-kind.yaml) for a working example against a local `kind` cluster.
- **`agentctl run --ndjson-stdout`** flag: swaps stdout from human-formatted `[type] {...}` lines to raw NDJSON (one wire event per line). The KubernetesBackend passes this flag to the in-Pod agentctl so the host can decode `kubectl logs` directly with `wire.Decode`.
- **Shared matcher** (`cli/internal/backend/matcher.go`): selector + capability-matching logic factored out of `local.go` so `LocalBackend.Resolve` and `KubernetesBackend.Resolve` apply identical policy. Behavior unchanged vs v0.3.3.

## [0.3.3] — 2026-06-08

Adapter fix so `ANTHROPIC_BASE_URL` works with both adapters using the same value (e.g. an internal Anthropic-compatible AI gateway).

### Fixed

- **opencode adapter URL composition.** v0.3.2 honored `ANTHROPIC_BASE_URL` only when it already contained `/v1` (Vercel `@ai-sdk/anthropic` appends `/messages`). Pi-style URLs (no `/v1`) made opencode request `${base}/messages` and gateways returned 404. The adapter now normalizes the URL at `runOpencode()` startup: if the value doesn't end in `/vN`, `/v1` is appended in-place before any opencode subprocess inherits the env. Standard `api.anthropic.com/v1` is preserved as-is.
- **`runtime-image/v0.1.1`** — Dockerfile now explicitly runs `opencode-ai`'s `postinstall.mjs` after `npm install -g --ignore-scripts`. Without this the image shipped a stub `/usr/local/bin/opencode` that exited with "postinstall script was not run" on first invocation.

### Changed

- **New export `normalizeAnthropicBaseUrlForOpencode`** in `runtime-opencode/src/index.ts` — exposed for unit tests that pin the normalization rules (7 cases: undefined, empty, Pi-style with/without trailing slash, already-`/v1`, hypothetical `/v2`, sub-path).
- **`@agent-controller/runtime` v0.3.3** — version bump only (release workflow's version-match gate; no functional change in the Pi adapter).

## [0.3.2] — 2026-06-05

Bug-fix patch: npm-installed `@agent-controller/runtime` now supports `spec.subagents` (previously broken in v0.3.1).

### Fixed

- **Vendored subagent extension was missing from the npm tarball.** v0.3.1's `runtime/src/adapter.ts` resolved the subagent extension via `../../extensions/subagent/entrypoint.ts` relative to `runtime/dist/adapter.js`. The path was valid only in the source-tree layout (where `extensions/` is a sibling of `runtime/`); the npm package shipped just `dist/` so the file didn't exist after `npm install`. ADL specs that declared `spec.subagents` would fail at Pi load with "extension not found." Caught by codex pass 3 of slice 4.2.

### Changed

- **`runtime/scripts/copy-vendored-extensions.mjs`** — new build step that copies `extensions/subagent/` into `runtime/dist/extensions/subagent/`. Runs as part of `npm run build`, so the file is part of the published tarball.
- **`runtime/src/adapter.ts`** — subagent-extension path resolution now probes two candidate locations:
  1. `<adapter.js>/extensions/subagent/entrypoint.ts` (in-package; used after npm install)
  2. `<adapter.js>/../../extensions/subagent/entrypoint.ts` (source tree; used when running from a clone)

  Falls back to the in-package path if neither file exists (Pi then surfaces a clear file-not-found at session start, matching pre-v0.3.2 behavior).
- **`@agent-controller/runtime-opencode`** version bumped to 0.3.2 to match the umbrella tag's version-gate (no functional change in the opencode adapter itself).

## [0.3.1] — 2026-06-05

Self-contained install. Closes the "downloaded binary works standalone" gap called out as a v0.3.0 known limitation.

### Added

- **npm-published runtime adapters** — [`@agent-controller/runtime`](https://www.npmjs.com/package/@agent-controller/runtime) (Pi) and [`@agent-controller/runtime-opencode`](https://www.npmjs.com/package/@agent-controller/runtime-opencode) (opencode). Each ships its built `dist/`, README, and LICENSE — no source, no test scaffolding (except `runtime/dist/testing/fake-provider.*` which is part of the public API for hermetic downstream tests). Tarball sizes 27.7 kB / 36.9 kB.
- **`publish-npm` workflow job** (`.github/workflows/release.yml`) — runs in parallel with the GitHub Release job, builds each adapter, asserts `package.json` version matches the release tag (catches forgot-to-bump cases), and publishes with `npm publish --access public`. Gated on `NPM_TOKEN` repo secret — skipped gracefully when the secret is unset (forks, binary-only releases). Pre-release tags (with a hyphen) skip the publish for now.
- **New install path documented**: `npm install -g @agent-controller/runtime` + `AGENT_CONTROLLER_RUNTIME="$(npm root -g)/@agent-controller/runtime/dist/index.js" agentctl run ...`. Surfaced in `README.md` Quick Start and in the release-body template.

### Changed

- **`runtime/package.json`** + **`runtime-opencode/package.json`** — dropped `private: true`; added `publishConfig.access: public`, `files: ["dist", "README.md", "LICENSE"]`, `repository.{type, url, directory}`, `homepage`, `bugs`, `license: MIT`, `description`, `keywords`, `engines.node: ">=22"`; bumped `version` to `0.3.1`.
- **`runtime/README.md`** + **`runtime-opencode/README.md`** — rewritten as npm landing pages: install/use sections lead, all repo links converted to absolute GitHub URLs (relative `../docs/` links don't resolve on npmjs.com), cross-package references use npmjs.com URLs.
- **LICENSE** — copied into each package directory (npm includes it in the tarball; npm's convention is per-package).
- **Release-body template** in `.github/workflows/release.yml` — removed the "What's NOT in this release: adapters not bundled" section; added an npm-install section pointing at the new packages. Source-clone + build is now documented as a fallback path (forks, offline use) rather than the recommended one.

### Codex review history

(Filled in at slice commit time.)

## [0.3.0] — 2026-06-04

The Runtime Abstraction release. ADL now declares not just what the agent needs (`spec.runtime.type` + `spec.runtime.requirements`) but the new `RuntimeBinding` resource kind separately declares what each execution target provides. The `Backend.Resolve()` step matches the two before running. This is the K8s-style separation between PodSpec intent and runtime/scheduler enforcement that the architecture memo committed to — see [`ROADMAP.md`](ROADMAP.md).

Mostly non-breaking for v0.2.x specs: Bindings, requirements, and the matcher are all opt-in — specs that do not use them continue to compile and run identically. Two new compile-time rejections do break the v0.2 status quo for specs that relied on them:

- Pi built-ins with `spec.tools[].config` (silent no-op pre-v0.1.11 — now a compile error pointing at [`@gotgenes/pi-permission-system`](https://www.npmjs.com/package/@gotgenes/pi-permission-system)).
- `runtime.type: local-opencode` combined with `spec.extensions[]`, `spec.installs[]`, or custom Pi-extension tools (formerly failed at adapter startup — now failed at compile).

Upgrade path for both: see the "Added — compile-time adapter compatibility checks" and "Added — `spec.tools[].config` rejection" sections below.

### Added — `RuntimeBinding` resource kind

- **`schemas/runtimebinding.v1alpha1.json`** — new top-level kind `RuntimeBinding`. `metadata.name` + `spec.selector.{runtimeType, capabilities}` + `spec.target.{type, runtimeCommand, strict}`. Lives in the same `agent-controller.dev/v1alpha1` namespace as Agents.
- **`agentctl validate`** dispatches by `kind:` — both `Agent` and `RuntimeBinding` documents validate against the right schema. Unknown kinds produce a clear error naming the supported set.
- **`agentctl run --binding <path>`** — loads + validates a RuntimeBinding YAML and passes it to the resolver. Without `--binding`, runs flow through `Backend.Resolve(spec, nil)` — exact v0.2.x default behavior.
- **`spec.target.runtimeCommand`** — overrides the cwd-relative runtime-adapter lookup per-Binding (equivalent to `AGENT_CONTROLLER_RUNTIME` but scoped). When set, the cwd-relative staleness check is skipped.
- **Example bindings**: `examples/bindings/local-default.yaml` (warn-but-proceed) and `examples/bindings/local-strict.yaml` (target.strict).

### Added — capability matcher

- **`spec.runtime.requirements`** — additive free-form boolean map on Agent specs. Reserved well-known keys: `streaming`, `sandbox`, `gpu`, `restrictedNetwork`, `ephemeralFilesystem`. Arbitrary keys accepted so capability bundles can advertise their own (e.g. `spark`, `notebookContext`).
- **`Backend.Resolve()` + `ResolvedRunSpec`** — new two-phase Backend interface. Resolve compares `spec.runtime.requirements` against the Binding's `selector.capabilities`; Submit takes the resolved shape.
- **Selector check** — `binding.spec.selector.runtimeType` must equal `spec.runtime.type`. Mismatch is a hard error regardless of strict mode (wiring bug, not capability gap).
- **Warn-but-proceed default policy** — unmet `requirement: true` entries emit one wire `warning` event each (`kind: unmet_runtime_requirement`, sorted alphabetically for stable output). The run proceeds. Default per [ROADMAP "Recorded design decisions"](ROADMAP.md).
- **`spec.target.strict: true`** — opt-in per-Binding: promotes unmet requirements from warn-but-proceed to a hard error before any `session.started` event.

### Added — compile-time adapter compatibility checks

- The compiler now rejects three opencode-incompatible spec shapes at `agentctl compile` time (previously they failed at adapter startup):
  - non-empty `spec.extensions[]`
  - non-empty `spec.installs[]`
  - custom Pi-extension tools in `spec.tools[]` (with `entrypoint` set, `builtin` false)
- All three problems are surfaced together in one error. The runtime adapter retains the same checks as defense-in-depth.
- `agentctl run` also rejects `--resume <id>` + `runtime.type: local-opencode` upfront (the session id is applied to the spec after `parseValidateCompile`, so the check lives in the CLI rather than the compiler).
- Closes Phase 2 follow-up #6 from the harness matrix.

### Added — `spec.tools[].config` rejection for built-ins (v0.1.11 carryover)

- The Pi adapter previously silently accepted `config` on `bash`/`read`/`edit`/`write` and dropped it at runtime (built-ins don't read `AGENT_CONTROLLER_EXT_CONFIG`). The compiler now rejects this at compile time with a clear pointer at the supported allowlist path: [`@gotgenes/pi-permission-system`](https://www.npmjs.com/package/@gotgenes/pi-permission-system) as a `spec.extensions[].source: npm:` entry. See `examples/bash-allowlist.yaml`.

### Added — release hygiene

- **`docs/features.md`** — verify-each-feature-locally walkthrough.
- **`go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@v0.3.0`** works via the `cli/v0.3.0` Go-submodule tag (mirroring the umbrella tag).
- **CHANGELOG.md** — this file.
- **GitHub repo metadata** — description, homepage, topics (`agent`, `ai-agents`, `adl`, `llm`, `anthropic`, `opencode`, `pi`, `control-plane`, `declarative`, `agentic`).

### Added — recorded design decisions

Durable entries in [`ROADMAP.md`](ROADMAP.md) for two upcoming features so future contributors don't re-litigate:

- **Tracing track** (v0.5+): three-layer OpenTelemetry design — CLI-side wire-event converter (adapter-agnostic), Pi-side via [`@raindrop-ai/pi-agent`](https://www.npmjs.com/package/@raindrop-ai/pi-agent), opencode-side via `experimental.openTelemetry: true`. Shared trace ID via OTel propagation.
- **Sandboxing track**: enforcement lands on the Kubernetes target in v0.4 (Pods provide the primitives — SecurityContext, NetworkPolicy, emptyDir; honest about vanilla NetworkPolicy's lack of FQDN filtering); `LocalBackend` fallback via `sandbox-exec`/`unshare`+seccomp ships in v0.5+.

### Changed

- **ADL schema** (`schemas/adl.v1alpha1.json`): `spec.runtime.requirements` added (additive). Field descriptions updated to describe v0.3.3 matcher behavior honestly.
- **Backend interface** (`cli/internal/backend/`): `Submit(ctx, spec)` → `Resolve(ctx, spec, binding) + Submit(ctx, run)`. Internal-only change (no external implementers existed).
- **`agentctl validate`** now dispatches by `kind:`. Documents with `kind: RuntimeBinding` validate against the new schema.

### Known limitations / Deferred to v0.4+

- `agentctl validate` (schema-only) does NOT run the compile-time adapter-compatibility checks — only `agentctl compile` does. CI gates that stop at `validate` will still pass specs with incompatible fields.
- `requirements` enforcement is best-effort today: the matcher emits warnings or errors based on what the Binding advertises, but no Backend actually enforces capabilities like `sandbox: true` or `restrictedNetwork: true` end-to-end. v0.4 Kubernetes target ships real enforcement via Pod primitives.
- Opencode adapter feature gaps remain. Where they're caught is now precise:
  - `spec.extensions[]`, `spec.installs[]`, and custom Pi-extension tools combined with `runtime.type: local-opencode` — rejected by `agentctl compile` (v0.3.4).
  - `--resume <id>` with `runtime.type: local-opencode` — rejected by `agentctl run` (the resume id is applied after compile, so this check lives in the CLI).
  - Built-in tools with `spec.tools[].config` — rejected globally by `agentctl compile` for both adapters (v0.1.11 carryover, not opencode-specific). Pi previously accepted these and dropped them silently at runtime; opencode never supported them.
- GitHub Release artifacts still ship `agentctl` only; runtime adapters require source clone + `npm build`. npm-publishing the adapters is on the v0.4 roadmap.

## [0.2.0] — 2026-06-03

First multi-adapter release. ADL specs now run on either the legacy Pi adapter or the new opencode adapter (built on `@opencode-ai/sdk`) by setting `spec.runtime.type`. The NDJSON wire protocol is the unified contract.

### Added

- **opencode adapter** (`runtime-opencode/`) — full session dispatch via `createOpencode()` + `session.promptAsync` + SSE event translation. Skills inlined into the system prompt, subagents via opencode's native `cfg.agent` map with `mode: "subagent"`, MCP servers via opencode's native `cfg.mcp`. Opencode's own native agents (`plan`, `build`, `general`, `explore`, `scout`) are explicitly disabled so the `task` tool can't bypass the ADL allowlist.
- **ADL `runtime.type`** selector: `local`, `local-pi`, `local-opencode` (schema enum). The legacy `local` value remains a backwards-compatible alias for `local-pi`.
- **Harness capability matrix** ([`docs/architecture/harness-matrix.md`](docs/architecture/harness-matrix.md)) — every ADL surface mapped to ✅/⚠/❌ per adapter. Phase 2 acceptance gate: zero `🚧` cells in the opencode column.
- **Dual-adapter E2E harness**: `ADAPTER=pi|opencode ./e2e/run.sh`.
- **Release-hardening docs**: `ROADMAP.md`, `SECURITY.md`, `CONTRIBUTING.md`, `docs/versioning.md`, per-subdirectory READMEs (`cli/`, `runtime/`, `runtime-opencode/`, `schemas/`). `docs/features.md` (verify-each-feature-locally walkthrough) was added post-tag — see Unreleased.
- **Automated cross-platform release workflow** (`.github/workflows/release.yml`) — builds `agentctl` binaries for `{darwin,linux}-{amd64,arm64}` + `windows-amd64`, runs the full Go + Pi + opencode test suite, publishes a GitHub Release with `schemas.zip` and `checksums.txt`. Prerelease tags (containing `-`) are marked as such; stable tags get `make_latest: true`.
- (Post-tag) `go install github.com/CCDevelopForFun/agent-controller/cli/cmd/agentctl@v0.2.0` resolves via the `cli/v0.2.0` Go-submodule tag (mirrors the umbrella `v0.2.0` tag). See Unreleased.

### Changed

- **Pi adapter** (`runtime/`): cumulative bug fixes and the addition of the slice 1.1 hermetic E2E harness (`runtime/src/e2e/runsession-fake.test.ts`). No breaking changes to existing v0.1.x specs.
- **ADL schema** (`schemas/adl.v1alpha1.json`): `spec.runtime.type` enum extended with `local-pi` and `local-opencode` (additive — `local` still accepted).
- **CLI dispatch** (`cli/cmd/agentctl/main.go::resolveRuntimeCommand`): routes to `runtime-opencode/dist/index.js` when `runtime.type: local-opencode`.

### Known limitations / Deferred

Tracked in [`docs/architecture/harness-matrix.md`](docs/architecture/harness-matrix.md) as v0.3+ follow-ups:

- GitHub Release binaries ship `agentctl` only — the runtime adapters (`runtime/`, `runtime-opencode/`) still require a source clone + `npm install` + `npm run build`. npm-publishing the adapters is on the v0.3 roadmap.
- Opencode hermetic E2E pending (needs an Anthropic-API-compatible mock server reachable via `ANTHROPIC_BASE_URL`).
- Opencode `--resume <id>` is rejected at startup; resume support deferred.
- Opencode-unsupported-field rejections happen at adapter startup, not at `agentctl compile`/`validate` time.
- Pi adapter still uses plain `{}` objects for `spec.mcpServers[].name`-keyed maps (no prototype-pollution hardening); the opencode adapter has it.
- Opencode adapter + custom-provider configurations (e.g. a corporate LLM proxy with provider-prefixed models) — our `XDG_CONFIG_HOME` isolation hides the user's provider blocks, so specs that need a non-default provider currently can't reach the right model. Design fix is part of v0.3 RuntimeBinding work.

### Codex review history

This release went through approximately **70 codex review passes** across slices 1.1 → 2.7. Pass details live in the per-slice commit messages.

## [0.1.10] — 2026-06-01 (retrospective)

The last single-adapter release. Hardened the Pi adapter with the three-mode hallucination guardrail (`block` / `warn` / `correct`), the dist-staleness check, and the connection-flow documentation.

## [0.1.0] — [0.1.9] — retrospective

The v0.1.x cumulative `v0.1` tag bundles the original MVP plus session resume (v0.1.1), Skills (v0.1.2 / v0.1.7), Subagents (v0.1.3), MCP servers (v0.1.5), auto-install (v0.1.6), and Pi built-in tools (v0.1.9). See `git log v0.1.0..v0.1.10` for details.

---

[Unreleased]: https://github.com/CCDevelopForFun/agent-controller/compare/v0.3.3...HEAD
[0.3.3]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.3.3
[0.3.2]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.3.2
[0.3.1]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.3.1
[0.3.0]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.3.0
[0.2.0]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.2.0
[0.1.10]: https://github.com/CCDevelopForFun/agent-controller/releases/tag/v0.1.10
