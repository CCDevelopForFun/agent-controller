# Contributing to Agent Controller

Thanks for considering a contribution. This document covers the dev setup, the slice-by-slice workflow we use, and the review expectations.

## Dev setup

Requirements:

- Go ≥ 1.25 (matches `cli/go.mod`; required for OTel SDK v1.44+ since slice 5.1; older versions will fail to build)
- Node.js ≥ 20 + npm
- (Optional) `opencode` CLI for `ADAPTER=opencode` E2E
- (Optional) `codex` CLI for mandatory pre-merge review

```bash
git clone https://github.com/CCDevelopForFun/agent-controller.git
cd agent-controller

# Build
(cd runtime && npm install --ignore-scripts && npm run build)
(cd runtime-opencode && npm install --ignore-scripts && npm run build)
(cd cli && go build -o bin/agentctl ./cmd/agentctl)

# Test
(cd runtime && npm test)
(cd runtime-opencode && npm test)
(cd cli && go test ./...)
```

For end-to-end coverage against a live model:

```bash
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ./e2e/run.sh                # Pi
ANTHROPIC_API_KEY=sk-ant-... AGENT_CONTROLLER_RUN_LIVE=1 ADAPTER=opencode ./e2e/run.sh   # opencode
```

Without `AGENT_CONTROLLER_RUN_LIVE=1`, `e2e/run.sh` is a no-op (safe to call in CI without credentials).

## How work gets done here: the slice-by-slice workflow

This project ships in small, testable slices rather than long-lived feature branches. Each slice:

1. Has a clear acceptance criterion (single sentence: "what does done look like?")
2. Touches a bounded set of files (typically 1-5)
3. Is reviewed by `codex review --uncommitted` before commit
4. Lands as a single commit with a descriptive message
5. Updates relevant docs (`docs/architecture/harness-matrix.md`, `ROADMAP.md`) in the same commit if behavior changes

Examples of "a slice":

- Slice 2.4 added opencode session dispatch + SSE event translation. Took 36 codex review passes (each finding addressed before the next ran).
- Slice 2.6 filled the harness capability matrix. 5 codex review passes; pass 5 clean.

Examples of NOT a slice:

- "Refactor all the things" — break into focused refactors.
- "Add a feature plus three follow-ups" — break into one slice per coherent unit.
- "Big PR with mixed code + doc updates" — separate the narratives.

## Commit messages

We use a lightweight conventional-commits style:

```
<area>: <one-line summary>

<paragraph explaining the why and the constraints>

<bullets of what changed if non-trivial>
```

Areas in use: `feat(opencode)`, `feat(pi)`, `docs`, `deps`, `test`, `fix`. New areas are fine when they make sense.

**Do not include** "Co-Authored-By: Claude" or similar trailers — the project owner's preference.

## Codex review (mandatory before merge)

Any non-trivial change (anything beyond a one-liner / typo fix / pure-comment edit) must pass `codex review --uncommitted` with no actionable findings before merging.

```bash
codex review --uncommitted    # takes 1-3 minutes
```

If codex finds something:

- Fix it now, or
- File it as a follow-up with a tracking note in the PR description, or
- Reject it with an explanation in the PR description.

Re-run codex after any non-trivial fix from a previous pass. The "clean signal" you're shooting for is: codex returns "I did not identify any discrete correctness, security, or maintainability issues."

## When implementing Pi-side features: catalog-first

Before writing code that touches Pi primitives (MCP, subagents, memory, skills, tools), check https://pi.dev/packages first. Many features already exist as maintained Pi extensions; vendoring or thin-wrapping one is usually better than rolling our own.

Decision (vendor / wrap / build from scratch) goes in the commit message.

This rule does NOT apply to opencode-side wiring (opencode has its own native primitives — Pi catalog packages don't run inside opencode).

## ADL changes

Any change that touches ADL fields requires:

1. The change to `schemas/adl.v1alpha1.json` (the source of truth)
2. Copy the file into the embedded location: `cp schemas/adl.v1alpha1.json cli/internal/adl/schemas/adl.v1alpha1.json` (the Go side reads from there via `go:embed`, not from the repo root)
3. A matching change to the Go compiler (`cli/internal/adl/compiler.go`)
4. A matching change to both runtime adapters (`runtime/src/types.ts` + `runtime-opencode/src/types.ts`)
5. A row update in `docs/architecture/harness-matrix.md`
6. A schema-sync test pass (`go -C cli test ./...`) — fails if the embedded copy drifts from the source

All in the same commit. Same workflow applies to `manifest.v1.json` (its embedded location is `cli/internal/registry/schemas/manifest.v1.json`).

## Releasing

The release workflow (`.github/workflows/release.yml`) fires on tag push (`v*.*.*`). Per-release checklist:

1. Bump versions to match the new tag in **every** publishable package:
   - `runtime/package.json` (`@agent-controller/runtime`)
   - `runtime-opencode/package.json` (`@agent-controller/runtime-opencode`)

   The `publish-npm` job asserts each `package.json.version` equals the tag's version (with the `v` prefix stripped) before publishing. Forgot-to-bump cases fail loudly.
2. Update `CHANGELOG.md` (new release section + reference-link footer).
3. Update `ROADMAP.md` (mark the release SHIPPED).
4. Tag two refs on the same commit: the umbrella tag (`vX.Y.Z`) and the Go-submodule tag (`cli/vX.Y.Z`) so `go install ...@vX.Y.Z` works.
5. Push the tags. The workflow builds the cross-platform `agentctl` binaries, publishes the two npm packages (if `NPM_TOKEN` is set), and cuts the GitHub Release.

Pre-release tags (with a hyphen, e.g. `v0.4.0-rc1`) skip the npm publish for now — npm supports dist-tags but the workflow doesn't wire that yet.

## Breaking changes

Pre-v1.0, breaking changes are allowed with migration notes in the commit message. Schema breaks bump the schema's version (`v1alpha1` → `v1beta1`), not the project version.

After v1.0, breaking changes go through an ADR (Architecture Decision Record) under `docs/architecture/` and ship behind a deprecation cycle of at least one minor release.

## What to work on

Look at:

- [`ROADMAP.md`](ROADMAP.md) — committed direction for v0.3, v0.4, v0.5+
- GitHub Issues — discrete work units
- "Phase 2 follow-ups" in [`docs/architecture/harness-matrix.md`](docs/architecture/harness-matrix.md) — concrete known gaps

If you want to propose something not on these lists, open an issue first describing the use case and design sketch — saves rework.

## Questions

Open a discussion on GitHub or reach out to the project owner (visible in `git log`).
