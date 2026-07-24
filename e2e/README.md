# E2E Test Harness

This directory hosts the project's shell-driven E2E harness. There are now **two layers** of E2E coverage, and they are not yet wired to use the same provider strategy.

## Layer 1 — vitest E2E against a fake provider (added v0.2 prep, slice 1.1)

`runtime/src/e2e/runsession-fake.test.ts` runs the real `runSession` against an unmocked Pi loaded from `node_modules`, with the LLM stream provided by pi-ai's built-in `registerFauxProvider`. No API key, no network. Runs locally as part of `npm test` in the `runtime/` directory. No CI workflow is checked in yet — adding one (a `.github/workflows/test.yml` that runs `go -C cli test ./...` — the Go module is nested under `cli/`, not at the repo root — and `npm --prefix runtime test`) is a tracked v0.2 follow-up.

This closes the original concern below ("Why a fake provider is not possible at MVP") — that note was authored before pi-ai exposed its faux provider helper via `@earendil-works/pi-ai`'s api-registry. The fake-provider lives at `runtime/src/testing/fake-provider.ts`.

Cross-package resolution note: pi-coding-agent ships its own nested copy of pi-ai under its `node_modules/`. Each copy has a distinct module-level api-registry. The fake-provider's lazy loader (`runtime/src/testing/fake-provider.ts`) probes for the nested copy first so registrations land in the same registry pi-coding-agent reads from. Without that step, the agent loop errors with `No API provider registered for api: fake-test`.

## Layer 2 — shell harness against the live model (`e2e/run.sh`)

`e2e/run.sh` exercises the full CLI subprocess path: build runtime + CLI, invoke `agentctl run examples/hello.yaml`, capture NDJSON output, assert on event types. It still requires `AGENT_CONTROLLER_RUN_LIVE=1` and a real `ANTHROPIC_API_KEY` because it has not yet been retrofitted onto the fake-provider path.

```bash
# Pi adapter (default)
AGENT_CONTROLLER_RUN_LIVE=1 ANTHROPIC_API_KEY=<your-key> ./e2e/run.sh

# opencode adapter (slice 2.6+)
ADAPTER=opencode AGENT_CONTROLLER_RUN_LIVE=1 ANTHROPIC_API_KEY=<your-key> ./e2e/run.sh
```

Without `AGENT_CONTROLLER_RUN_LIVE=1`, the script exits 0 and prints a skip message — safe to call in CI without credentials.

The `ADAPTER` env var selects which runtime to exercise, and each adapter ships its own example spec:
- `ADAPTER=pi` (default) — builds `runtime/` and runs `examples/hello.yaml` (with `get_time` tool + `audit-log` extension + `example-time-skill`) against the Pi adapter.
- `ADAPTER=opencode` — builds `runtime-opencode/`, requires the `opencode` CLI on PATH, and runs `examples/hello-opencode.yaml` (persona + task only, `tools: []`, no extensions / skills) against the opencode adapter. The two spec files are pre-baked rather than substituted at runtime; the opencode-compatible spec exists because the opencode adapter rejects custom Pi-extension tools and any `spec.extensions[]` at startup.

Note: the opencode path asserts `session.started` and `session.ended` only. Because `hello-opencode.yaml` declares `tools: []`, no `tool.call` / `tool.result` is expected. The Pi path also asserts `tool.call` / `tool.result` because `hello.yaml` reliably invokes `get_time` on Pi's prompting stack.

## Follow-up: hermetic E2E for both adapters

The subprocess-level E2E (Layer 2) currently requires live model credentials. There are two distinct hermetic-coverage gaps to close, each with a different shape:

### Pi adapter (Layer 2 + fake provider)

Wiring `run.sh --adapter pi` to use the fake provider — likely via `AGENT_CONTROLLER_USE_FAKE_PROVIDER=1` plus a way for the subprocess to load a response script — would let CI run the full subprocess flow hermetically. The in-process API (`installFakeProvider([...])`) doesn't fit a subprocess boundary; sketch options:
- `AGENT_CONTROLLER_FAKE_RESPONSES_JSON=<path>` env var pointing the runtime at a JSON file of FauxResponseStep entries.
- A small "loader extension" that reads the JSON at session start and calls `installFakeProvider` itself.

### opencode adapter (Anthropic-API mock server)

The fake-provider api-registry trick doesn't reach opencode because opencode runs in a separate subprocess that bypasses pi-ai entirely. A hermetic opencode E2E requires a different shape: an Anthropic-API-compatible mock server (handling `/v1/messages` streaming SSE) pointed to via `ANTHROPIC_BASE_URL=http://localhost:NNNN`. The mock can replay scripted responses the same way the fake provider does today.

Both gaps are tracked as v0.3 follow-ups in `docs/architecture/harness-matrix.md` (Phase 2 follow-up #1).

## Historical note

The original version of this README documented why a fake provider was thought to be impossible — that note depended on the `before_provider_request` extension hook, which doesn't support response substitution. The actual solution is at the pi-ai layer (`registerApiProvider` / `registerFauxProvider`), bypassing extension hooks entirely. The original note is preserved in git history.
