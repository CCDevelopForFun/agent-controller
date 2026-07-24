# Agent Controller — Roadmap

> **Core thesis:** Agent Controller is a control plane for AI agents. ADL describes the agent's intent. RuntimeBinding maps that intent to infrastructure. Runtime images provide execution substrate. Backends launch the substrate. **From v0.5 onward** the project's investment shifts from "more execution backends" to the three things that turn a working agent into a *production-fleet* primitive: end-to-end observability (OTel), long-running agent shapes beyond one-off jobs, and cross-agent orchestration.

This document captures committed direction. **Roadmap pivot 2026-06-10**: v0.4 K8s work shipped to a working skeleton (slice 4.3); remaining K8s slices (4.4 advanced binding fields, 4.5 sandboxing enforcement, 4.7 Docker peer) move to opportunistic/background. The next four minor releases — v0.5 (tracing), v0.6 (REPL + durable sessions), v0.7 (declarative orchestration), v0.8 (HTTP/SSE server) — focus on what turns a working agent into a production-fleet primitive. See [the recorded design decision](#v05-pivot-tracing-long-running-and-orchestration-over-more-backends) below for the rationale.

---

## Versioning policy

Separate version dimensions, separate tags. **`agentctl vX.Y.Z` does NOT imply `ADL vX.Y.Z`.**

- Agent Controller release: `v0.2.0`, `v0.3.0`, … (umbrella)
- `agentctl` CLI: own version (Go module)
- `@agent-controller/runtime` + `@agent-controller/runtime-opencode`: own npm versions
- ADL schema: `agent-controller.dev/v1alpha1` (own bump for breaking changes)
- Manifest schema: `manifest.v1.json` (file-format version; `apiVersion` value shares the ADL namespace `agent-controller.dev/v1alpha1`)
- Wire protocol (NDJSON events): `v: 1` today; will migrate to `apiVersion: "agent-controller.dev/events/v1alpha1"` in v0.5+ when the event-protocol expansion lands
- Runtime image: `agent-runtime-base:0.1.0` (independent, v0.4+)

See [`docs/versioning.md`](docs/versioning.md) for the full policy and compatibility windows.

---

## v0.2.0 — multi-adapter, release-ready (current)

**Shipped:**

- Pi adapter (legacy from v0.1.x, production)
- opencode adapter (`runtime-opencode/`) — full session dispatch via `@opencode-ai/sdk`, SSE event translation, MCP + subagents + skills wiring
- **codex adapter (`runtime-codex/`)** — third adapter, shipped post-v0.2.0 on the `codex-adapter` branch. `runtime.type: local-codex`; OpenAI-only; native `workspace-write` sandbox (the only adapter with a built-in sandbox); session resume via stable `CODEX_HOME` + persisted `thread_id`; MCP (stdio + streamable-http), skills, guardrails, output-schema. Requires `codex` CLI on PATH + `OPENAI_API_KEY`. See [`runtime-codex/`](runtime-codex/) and [`CHANGELOG.md`](CHANGELOG.md) for the full entry.
- ADL `runtime.type: local | local-pi | local-opencode | local-codex` selector
- Harness capability matrix ([`docs/architecture/harness-matrix.md`](docs/architecture/harness-matrix.md)) — every ADL feature mapped to ✅/⚠/❌ per adapter (Pi, opencode, codex columns complete; hermes-agent deferred)
- Dual-adapter E2E (`ADAPTER=pi|opencode ./e2e/run.sh`)
- This document + `SECURITY.md` + `CONTRIBUTING.md`
- GitHub Actions release workflow: cross-platform `agentctl` binaries + checksums on `v*` tag

**The headline:** ADL is harness-agnostic. The same spec runs on Pi, opencode, or codex by changing one field.

---

## v0.3.0 — Runtime Abstraction ✅ SHIPPED 2026-06-04

Generalized the local-only execution model so v0.4+ can plug in remote backends without ADL changes. All five sub-goals delivered:

- ✅ `runtime.requirements` field on ADL — additive, non-breaking
- ✅ `RuntimeBinding` schema v1alpha1 — separate resource mapping abstract requirements to deployment targets
- ✅ `Backend.Resolve()` step + `ResolvedRunSpec` type — two-phase backend interface
- ✅ Capability matcher with warn-but-proceed default + `target.strict: true` opt-in
- ✅ Opencode adapter-startup rejections moved into `agentctl compile`

See [CHANGELOG.md](CHANGELOG.md) for the full v0.3.0 entry and [PRs #5–#10](https://github.com/CCDevelopForFun/agent-controller/pulls?q=is%3Apr+is%3Aclosed) for the implementation slices.

Deferred to v0.4: deprecation decision on `runtime.type` (kept for now; revisit once `RuntimeBinding` has real adoption).

---

## v0.3.1 — Self-contained install ✅ SHIPPED 2026-06-05

Closes the "downloaded binary works standalone" gap from the v0.3.0 known-limitations list. Bridge release between the v0.3 abstractions and the v0.4 Kubernetes backend.

- ✅ `runtime/` published as [`@agent-controller/runtime`](https://www.npmjs.com/package/@agent-controller/runtime)
- ✅ `runtime-opencode/` published as [`@agent-controller/runtime-opencode`](https://www.npmjs.com/package/@agent-controller/runtime-opencode)
- ✅ Release workflow extended with `publish-npm` job (gated on `NPM_TOKEN`; skips gracefully when absent)
- ✅ Tag-version assertion catches forgot-to-bump cases
- ✅ Install instructions in `README.md` + release-body template rewritten around npm

See [CHANGELOG.md](CHANGELOG.md) for the full v0.3.1 entry.

---

## v0.4.0 — First Remote Backend (Kubernetes) ✅ SKELETON SHIPPED 2026-06-09

The OSS proof point that ADL is harness-agnostic at the *infrastructure* layer, not just at the adapter layer. v0.4 is intentionally a skeleton: enough to demonstrate the Backend interface works against a real cluster, while leaving the polish (sandboxing enforcement, advanced binding fields, Docker peer) as opportunistic background per the 2026-06-10 roadmap pivot.

- ✅ **Slice 4.2 — `agent-runtime-base` image** (`ghcr.io/ccdevelopforfun/agent-runtime-base`). Multi-stage Dockerfile + GHCR publish workflow. Bundles agentctl + Pi adapter + opencode adapter + opencode CLI. Multi-arch. Non-root + tini PID-1.
- ✅ **Slice 4.3 — `KubernetesBackend` skeleton** (released as v0.4.0): client-go integration, kubeconfig loading + in-cluster fallback, `RuntimeBinding.spec.target.kubernetes.{namespace, image, secretRef}` schema, Pod + spec-Secret submission, NDJSON-log → wire-event streaming (new `--ndjson-stdout` flag on agentctl), Stop-tears-down with sync.Once-guarded cleanup. CLI dispatches by `target.type`. Shared matcher (`backend/matcher.go`) extracted so K8s + Local apply identical selector/capability policy. End-to-end verified via local `kind` cluster.

### Deferred to background (opportunistic, no committed release)

Per the 2026-06-10 pivot, these slices ship when there's a clear user-signal pull rather than on a fixed cadence:

- **Slice 4.4 — RuntimeBinding K8s extensions**: explicit kubeconfig path/context fields, `target.kubernetes.serviceAccount`, `imagePullSecrets`, registry-backed Pods (mount tools/extensions/skills/agents as additional ConfigMaps), `agentctl run-compiled` subcommand so the Pod receives CompiledSpec JSON directly.
- **Slice 4.5 — Sandboxing enforcement**: wire `requirements.sandbox` / `restrictedNetwork` / `ephemeralFilesystem` to Pod SecurityContext + NetworkPolicy + emptyDir. K8s target's matcher default flips warn→fail-closed. Vanilla NetworkPolicy can't filter by FQDN; v0.4.5 would ship a `target.networkProfile` field that's honest about the gap, leaving FQDN filtering to operator-side concerns (Cilium, egress proxy sidecar).
- **Slice 4.7 — Local-container backend (Docker)** — same code path as K8s, one-machine.

---

## v0.5.0 — Tracing (OpenTelemetry) 🎯 NEXT

**Prerequisite for v0.6 / v0.7.** You can't reason about long-running agents or multi-agent orchestration without distributed traces. v0.5 wires OTel GenAI semconv through all three layers (CLI, adapter, tool) with one trace per `agentctl run`, then ships an OTLP exporter that drops spans into any compatible backend (Databricks MLflow, BrainTrust, Langfuse, Helicone, Honeycomb, Tempo, …).

**Design pillars:**

- **OTel GenAI semantic conventions.** The [OTel GenAI semconv](https://github.com/open-telemetry/semantic-conventions/tree/main/docs/gen-ai) is stabilizing in 2026; use it directly so MLflow / BrainTrust / Langfuse "just work" without per-platform exporters. Trade-off: the spec is still moving, we'll ride churn through the v0.5.x patches.
- **Trace propagation through three layers.** Root span starts in `agentctl run` (or in the K8s controller for `target.type: kubernetes`). The trace ID is propagated to:
  - **(A) Wire-event annotations.** Each NDJSON event gets a `traceparent` field per [W3C TraceContext](https://www.w3.org/TR/trace-context/). Already-shipped events stay backward-compatible (extra field is ignored by old consumers).
  - **(B) Adapter subprocesses.** Pi and opencode adapters receive `TRACEPARENT` via env when spawned. The Pi adapter's session id (already on every event) becomes a span attribute. opencode's `experimental.openTelemetry: true` opt-in is enabled when the spec opts in.
  - **(C) Tools and MCP servers.** Tool entrypoints + MCP servers receive `TRACEPARENT` via env. The Pi extension API already supports per-tool hooks where we inject the propagator.
- **`spec.observability.tracing`** ADL field — opt-in per spec. Defaults to off so existing specs don't suddenly start exporting to nowhere.
- **OTLP exporter** (gRPC + HTTP). Endpoint configured via standard OTel env vars (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, etc.) so users don't learn a new config surface. One platform smoke-tested end-to-end before tagging — likely **MLflow** (broad reach) or **BrainTrust** (agent-native, clean UX). Pick during slice 5.1.
- **Event protocol v1alpha1 lands here too.** Adding `tool.{started,completed,failed}` events is the same work as adding rich tool spans; do them together. This is the wire-protocol bump previously parked in "v0.5+ parallel tracks."

### Proposed slice breakdown

| Slice | Goal |
|---|---|
| 5.1 | OTel SDK wired into agentctl; root span per `run`; OTLP exporter; choose one downstream platform; verify it shows up |
| 5.2 | Wire-event `traceparent` field + event-protocol v1alpha1 (split `tool.{started,completed,failed}`, add `artifact.created`, `audit.event`) |
| 5.3 | Pi adapter span emission (model call, each tool, each MCP roundtrip); `TRACEPARENT` propagation to tool subprocesses |
| 5.4 | opencode adapter span emission via `experimental.openTelemetry: true` |
| 5.5 | K8s backend: trace context propagated into Pod env; spans cross the agentctl→Pod boundary |
| 5.6 | `spec.observability.tracing` ADL field + schema; one full E2E platform integration test |

---

## v0.6.0 — Long-running agents (interactive REPL + persistent sessions)

Today every `agentctl run` is a one-shot subprocess. v0.6 ships **interactive chat** — the simplest long-running shape — plus the durable-session foundation that v0.8 (HTTP/SSE server) and v0.7 (workflow orchestration) both need.

- **`agentctl chat <spec.yaml>`** — interactive REPL mode that keeps a single session alive across many user prompts. Reuses Pi and opencode's existing TTY UX where it exists; we wrap rather than re-invent. Ctrl-D / `/exit` ends the session cleanly. Ctrl-C interrupts the current turn without killing the session.
- **Durable session store.** Pre-v0.6 `--resume <session-id>` is file-based and lives under `$HOME/.pi/agent/sessions/agentctl/<id>/` (managed by Pi's session loader; `agentctl sessions ls` mirrors the same directory). v0.6 adds a pluggable session-store interface with two impls shipped:
  - **SQLite** (default) — file-backed, single-host, zero config
  - **In-memory** — for tests and ephemeral REPLs
  - Postgres/Redis hooks land in v0.8 with HTTP/SSE
- **Session lifecycle events.** New wire events: `session.resumed`, `session.paused`, `session.expired`. Plays naturally with v0.5 tracing — each resumed turn is a child span under the session's root.
- **Multi-turn context windows** are the adapter's job, not ours. We just hand the persisted session id to Pi/opencode; they decide what to keep in context.

### Out of scope for v0.6 (deferred to v0.8)

- Multi-user / multi-tenant servers
- HTTP/SSE/WebSocket endpoints
- Auth (OIDC, API keys, etc.)
- Horizontal scaling / session sharding

---

## v0.7.0 — Make `agentctl run` a first-class scheduler task (Option-B pivot) ✅ SHIPPED 2026-06-25

Delivered across slices 7.1–7.5 (PRs #30–#34): `--input k=v` + `${inputs.foo}` interpolation, `--input KEY=@<path>` / `--input-file`, `--output-file` + `spec.outputSchema`, `--skip-if-output-exists`, Maestro/Airflow/Temporal examples, and `--workspace` durable agent memory via a harness-agnostic MCP server (works on Pi and opencode). Coordinated bumps: `agentctl` v0.7.0, `@agent-controller/runtime` + `@agent-controller/runtime-opencode` 0.7.0; ADL schema unchanged (additive `outputSchema`). See [CHANGELOG.md](CHANGELOG.md) for the full per-slice entry. Deferred (tracked follow-ups): the K8s `outputSchema` round-trip + a v0.7-bundled runtime image, and two Pi-adapter improvements to make `--workspace` first-class on Pi (built-in-tool coexistence; per-run MCP config isolation).

**Direction change recorded 2026-06-15.** The pre-v0.7 plan described agentctl shipping its own in-process workflow engine (`agentctl workflow run`) with declarative `AgentWorkflow` YAML, parallel/conditional/foreach step shapes, etc. After thinking through how this would compose with Maestro / Airflow / Temporal — the orchestrators that actually run production multi-step workloads — we picked **Option B**: agentctl is the AGENT RUNTIME, the external scheduler is the ORCHESTRATOR.

Concretely:
- v0.7 ships NO new workflow YAML format.
- v0.7 ships NO in-process multi-step engine.
- v0.7 ships enhancements to `agentctl run` so it composes well with Maestro / Airflow / Temporal tasks.

Why we pivoted:
- **A custom workflow engine duplicates what Maestro already does** (DAG resolution, retries, parallel-fan-out, conditional, foreach). Maestro is a mature, battle-tested orchestrator. Inventing a parallel YAML format adds maintenance burden with no upside.
- **Durability is the hard problem.** Any in-process engine eventually needs to outlive a single process (host reboot, deploy). Reinventing what Maestro/Temporal already solve isn't where v0.7 effort earns the most leverage.
- **The agent-relationship graph is naturally a Maestro DAG.** A multi-agent pipeline maps cleanly onto Maestro step semantics (parameters, outputs, parallel, foreach). Translation by hand is straightforward.
- **Tracing already stitches.** v0.5's TRACEPARENT propagation means each `agentctl run` invocation joins the parent scheduler's trace context if the scheduler injects it via env. No bespoke workflow-tracing layer needed.

### What v0.7 ships

- **`agentctl run --input k=v`** (repeatable) — parameterize an Agent's `spec.task` at runtime via `${inputs.<key>}` interpolation. Operators write ONE Agent YAML with `task: "Research \"${inputs.topic}\""` and call agentctl with different `--input topic=X` per task instance from their scheduler.
- **`agentctl run --output-file <path>`** — write the captured final assistant message to a file. Scheduler picks it up and passes to downstream tasks via its own input mechanism. Optional `spec.outputSchema` (JSON Schema) for structured extraction.
- **`examples/`** entries showing how to call agentctl from Maestro / Airflow / Temporal — concrete configs operators can copy.
- **Exit-code contract** documented unambiguously: 0 = success, 1 = error, 130 = cancelled, 2 = usage.

### What v0.7 explicitly does NOT ship

- No `AgentWorkflow` YAML kind. No `agentctl workflow` subcommand. No in-process orchestrator.
- No declarative parallel / conditional / foreach step shapes from agentctl. Those live in your scheduler's DSL.
- No federated agent-to-agent (MCP/A2A) calls. Still parked, opportunistic; would build on top of the per-step primitive if it lands later.

### Could the old plan come back?

Possibly — but only if a real need surfaces for a portable, scheduler-agnostic agent-workflow format AND someone is willing to maintain N translators (`compile --to maestro|airflow|temporal`). For now, leaning into Maestro / Airflow / Temporal as the orchestrators avoids inventing primitives those systems already provide.

---

## v0.8.0 — Long-running agents, part 2 (HTTP/SSE server) [DELIVERED]

Built on v0.6 (durable sessions) and the v0.7 Option-B per-step task primitive to expose agents over the network. v0.8 is the "agentctl as a network service" track — same Agent definitions, same SessionStore, exposed as HTTP.

- **`agentctl serve <spec.yaml> --port 8080`** — single-process server that accepts prompts via HTTP+SSE. Each connection gets a session; multiple concurrent sessions share the server. SSE streams the wire-event NDJSON in the same `event: <type>\ndata: <json>` framing as the wire protocol.
- **Session management endpoints.** `POST /v1/sessions` (create), `GET /v1/sessions` (list), `GET /v1/sessions/{id}` (get), `DELETE /v1/sessions/{id}` (delete), `POST /v1/sessions/{id}/turns` (SSE-stream a turn).
- **Per-request concurrency limits.** `--max-concurrent-turns` (429 when exceeded), `--max-sessions` (429 on create), `--session-ttl` background sweep, `--shutdown-grace` drain window.
- **Health + readiness** endpoints (`/healthz` liveness, `/readyz` readiness with 503 during drain) suitable for K8s liveness probes.
- **Deferred to v0.8.x:** metrics endpoint (OTel meter registry), auth middleware (OIDC / API-key hooks), TLS (terminate at a proxy).

---

## Parallel / opportunistic tracks

These move independently of the v0.5/v0.6/v0.7/v0.8 cadence. Order of arrival depends on user signal.

| Track | Description |
|---|---|
| **K8s polish (slices 4.4 / 4.5 / 4.7)** | See "Deferred to background" under v0.4.0 above. |
| **Sandboxing enforcement (both targets)** | `requirements.sandbox` / `restrictedNetwork` / `ephemeralFilesystem` are still **advisory** as of v0.4.0 — the matcher emits warnings but no Backend enforces them yet. Enforcement lands when *either* the K8s slice 4.5 (SecurityContext + NetworkPolicy + emptyDir on Pods) *or* the LocalBackend fallback (macOS `sandbox-exec` / Linux `unshare`+seccomp) actually ships. Whichever lands first flips its target to fail-closed by default. **Catalog audit (2026-06-05)**: for the LocalBackend variant, rather than building from scratch, integrate an existing maintained Pi extension — candidates include [`pi-sandbox`](https://pi.dev/packages/pi-sandbox), [`@nqbao/pi-sandbox`](https://pi.dev/packages/@nqbao/pi-sandbox), [`pi-guard-sandbox`](https://pi.dev/packages/pi-guard-sandbox), [`@casualjim/pi-heimdall`](https://pi.dev/packages/@casualjim/pi-heimdall). |
| **Governance fields** | `spec.permissions` (data / network / secrets), `spec.approvals` (per-tool, interactive). The `requirements.sandbox` family is the first sub-set; the rest expand the surface. |
| **Additional runtime packs** | `agent-runtime-data` (Spark/SQL/catalog tools), `agent-runtime-coding` (git/test-runner), `agent-runtime-secure` (hardened sandbox profile) |
| **Schema stabilization** | v1alpha1 → v1beta1 → v1 with migration tooling and one-cycle deprecation windows |
| **Honesty extraction** | Pull `runtime/src/honesty.ts` into a standalone `pi-honesty-guardrail` package for community use |
| **Subagent extension swap** | Replace vendored `extensions/subagent/` with community `pi-subagents` per the [Pi catalog](https://pi.dev/packages) |
| **Resume + cancellation parity** | opencode `--resume` support; Pi adapter `cancelled` reason on SIGINT |

---

## Downstream adoption (parallel, opportunistic)

Private or internal pilots are welcome but **do not gate OSS milestones**. Such use cases live in a separate doc (not committed to this repo) and connect via the same Backend interface — no special-case code paths in upstream.

---

## What's NOT on the roadmap

To stay focused, these are explicitly OUT OF SCOPE through v0.8:

- One Docker image per agent (anti-pattern; runtime images are capability bundles, not per-agent builds)
- Vendor-specific backends as first-class citizens (Backend interface is the integration point)
- Web UI / IDE plugins
- Marketplace for agents
- The federated agent-orchestration camp (agents-as-MCP-servers calling each other) as the primary orchestration mode — see the v0.7 section for why declarative goes first
- Anything that breaks ADL portability across adapters

If you want any of the above, open an issue describing the use case before opening a PR — the answer is probably "great, after v1.0."

---

## Open questions (revisit during the linked release)

- **OTel platform for the v0.5 smoke** — MLflow (broad reach) vs BrainTrust (agent-native, clean UX) vs Langfuse (OSS-friendly) for the one-platform end-to-end gate. Decide at slice 5.1.
- **GenAI semconv churn budget** (v0.5.x) — how many patches we're willing to ship to track the spec before pinning to a particular semconv version.
- **AgentWorkflow scope** (v0.7) — does the declarative engine ship in-process only, or do we factor out an interface that Maestro / Temporal / Argo can implement against on day one?
- **Session store auth** (v0.6/v0.8) — OIDC vs API-key for `agentctl serve`; depends on what real-world pilots actually need.
- **`runtime.type` evolution** (v0.3) — keep forever as the explicit-binding shortcut, or deprecate when `RuntimeBinding` lands?
- **Governance field shape** — required vs optional fields, how strict the schema should be
- **Honesty guardrail packaging** — keep monorepo-internal or publish as standalone npm
- **Multi-version `agentctl` schema support** — should a future agentctl accept both ADL `v1alpha1` and `v1beta1`?

## Recorded design decisions

Decisions made during planning that are too small for a full ADR but worth pinning so future contributors don't re-litigate.

### v0.5 pivot: tracing, long-running, and orchestration over more backends

**Date:** 2026-06-10. **Status:** Committed.

After shipping the KubernetesBackend skeleton (slice 4.3, released as v0.4.0), the next direction was originally to keep filling out the K8s surface (slice 4.4 advanced binding fields, 4.5 sandboxing enforcement, 4.7 Docker peer). We're pivoting away from that.

**Reasoning:**

- **Diminishing returns on more backends.** Local + Kubernetes is enough to demonstrate the Backend interface. Adding Docker / serviceAccount fields / NetworkPolicy enforcement is polish, not capability — none of it changes what an *agent* can do, only where the agent runs.
- **The next-most-load-bearing primitives aren't backends.** Three things change what an agent fleet can actually do in production:
  1. **Observability** — without OTel traces wired through CLI / adapter / tool layers, debugging multi-step agents is guesswork. This is also where the project pays for itself in the medium term (every observability platform integration becomes free once GenAI semconv is in place).
  2. **Long-running agent shapes** — one-off jobs are a tiny fraction of real use. Interactive chat, persistent sessions, and server-mode agents are what users actually want to deploy.
  3. **Orchestration** — once individual agents work, the question is immediately "how do I get five of them to cooperate?" ADL today says nothing about that.
- **Tracing is a hard prerequisite.** You can't reason about a long-running agent without per-turn spans. You can't reason about a workflow of 5 agents without one trace that stitches all 5 together. Skipping v0.5 to do v0.6 or v0.7 first would mean throwing both away once tracing lands.
- **K8s slices aren't abandoned.** They move to opportunistic background. The moment a real user signals they need NetworkPolicy enforcement or imagePullSecrets, we ship that slice; we just don't gate other progress on them.

**Order of dependencies:** v0.5 (tracing) → v0.6 (REPL + durable sessions) → v0.7 (declarative AgentWorkflow) → v0.8 (HTTP/SSE server). v0.8 explicitly builds on v0.6's session store and v0.7's workflow engine, so the order is non-negotiable.

**What this decision does NOT change:**

- The Backend interface contract (LocalBackend + KubernetesBackend keep working as-is)
- The ADL schema (v0.5–v0.8 additions are additive: `spec.observability.tracing`, `spec.outputSchema`, new `AgentWorkflow` kind)
- The "no per-agent Docker image" stance — runtime images are still capability bundles
- The "no federated agents-calling-agents as primary orchestration mode" stance is *new*; recorded explicitly in the v0.7 section.

### v0.3.3 `Backend.Resolve()` capability matching: warn-but-proceed (not fail-closed)

When the Agent declares `spec.runtime.requirements: { sandbox: true }` and the selected `RuntimeBinding`'s `target` doesn't advertise that capability, the resolver in slice 3.3 will **emit a wire `warning` event and proceed with the run**, not abort.

Rationale:
- v0.3.1 introduced `requirements` as advisory; existing specs that declared it expected pass-through behavior. Flipping to fail-closed in v0.3.3 would silently break any spec that included a requirement the local target doesn't yet enforce.
- The Kubernetes target was originally planned to flip to fail-closed in v0.4.5 (sandboxing enforcement). After the 2026-06-10 pivot that slice is opportunistic background; the matcher stays warn-but-proceed by default until that slice actually ships.
- A separate LocalBackend sandbox fallback (sandbox-exec / unshare+seccomp) can also flip to fail-closed once it ships — also opportunistic background now.

Implementation: the resolver emits one `warning` event per unmet requirement at the start of the run, naming the requirement and the Binding that's missing the capability. The session then continues normally.

Strictness is an opt-in flag on the Binding (`spec.target.strict: true`), so operators who want fail-closed behavior get it without waiting for the K8s sandboxing slice. Default stays warn-but-proceed across all backends until either (a) the K8s sandboxing slice ships, or (b) a separate ADR commits to flipping the project-wide default.

---

## How this document is maintained

- Each minor release updates this file with what shipped, what slipped, and what changed in commitment.
- New "parallel tracks" entries require a one-paragraph design summary before merging, even if implementation is months away.
- Breaking changes to ADL, the wire protocol, or the schema namespace require a separate ADR (Architecture Decision Record) under `docs/architecture/`. We'll add the ADR template when the first one lands.
