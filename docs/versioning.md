# Versioning Policy

> **Core rule:** `agentctl vX.Y.Z` does **not** imply `ADL vX.Y.Z`. Each artifact and each schema versions independently.

This document captures the version dimensions Agent Controller tracks separately and how they interact across releases.

---

## The dimensions

| Dimension | Identifier | Lifetime |
|---|---|---|
| **Agent Controller release** | `v0.2.0`, `v0.3.0`, … (umbrella) | Each minor cycle |
| **`agentctl` CLI** | Go module version | Bumped per CLI change |
| **`@agent-controller/runtime`** | npm version | Bumped per Pi-adapter change |
| **`@agent-controller/runtime-opencode`** | npm version | Bumped per opencode-adapter change |
| **ADL schema** | `agent-controller.dev/v1alpha1` (in `apiVersion`) | Bumped on breaking field changes |
| **Manifest schema** | `manifest.v1.json` (file format version; `apiVersion` shares the ADL namespace `agent-controller.dev/v1alpha1`) | Bumped on breaking manifest changes |
| **Wire protocol** | `v: 1` (NDJSON event envelope field) | Bumped on breaking event-shape changes |
| **Runtime image** | `agent-runtime-base:0.1.0` (planned v0.4+) | Independent of CLI |

The umbrella release version is what users see in GitHub Releases. Internally, the individual artifacts move independently — a v0.2.0 release might ship `agentctl v0.2.0` + `@agent-controller/runtime 0.1.5` + ADL schema `v1alpha1` (no bump) if only the CLI changed in a Pi-adapter-compatible way.

---

## Why the dimensions are separate

A single project-wide version conflates four different concerns:

1. **Consumer compatibility** — does my pinned schema still work?
2. **Operator change** — does the binary I deployed behave the same?
3. **Library change** — does the npm package I `require()` from my own code still export the same shape?
4. **On-the-wire format** — can my CI parser still read events from the new runtime?

By tracking these separately, downstream users only need to react to the dimensions they depend on. A schema-only consumer doesn't have to redeploy when the CLI gets a bug fix. A library consumer doesn't have to migrate when the wire-protocol envelope adds an optional field.

---

## ADL schema versioning

The ADL schema uses Kubernetes-style version qualifiers:

| Stage | Meaning | Field semantics |
|---|---|---|
| `v1alpha1` | Current state | Fields may be renamed, removed, or change meaning between minors |
| `v1beta1` | Planned ~v0.4 | Fields stable in name; semantics may still tighten |
| `v1` | Planned post-v1.0 release | Stable forever; deprecation requires a `v2alpha1` migration window |

The schema's `apiVersion` field in YAML controls which version a spec is validated against:

```yaml
apiVersion: agent-controller.dev/v1alpha1   # current
kind: Agent
```

The CLI's policy on multi-version support:

- **v0.2.x**: accepts only `v1alpha1`
- **v0.3.x**: still accepts only `v1alpha1` (no schema break planned in v0.3)
- **v0.4.x and later**: when `v1beta1` lands, `agentctl` accepts BOTH `v1alpha1` and `v1beta1` for at least one minor release; `v1alpha1` is then deprecated with a one-minor sunset window.

Breaking changes (renaming a field, changing a field's type or default) require a schema-version bump — never a silent mutation of an existing version.

---

## Wire protocol versioning

The NDJSON event envelope currently has shape:

```json
{
  "v": 1,
  "type": "tool.call",
  "ts": "2026-06-03T10:18:15Z",
  "sessionId": "s_abc",
  "data": { … }
}
```

The integer `v` is the protocol version. The CLI rejects any event with `v != 1`. This is sufficient for v0.2.x.

### Planned migration to `apiVersion` (v0.5+)

The roadmap (memo §9) calls for a richer event envelope:

```json
{
  "apiVersion": "agent-controller.dev/events/v1alpha1",
  "type": "tool.completed",
  "runId": "run-123",
  "timestamp": "2026-06-03T10:18:15Z",
  "payload": { … }
}
```

This is a v0.5+ track tied to the event-protocol expansion (split `tool.{started,completed,failed}`, add `audit.event`, `artifact.created`). Until it lands:

- **Current consumers** keep relying on `v: 1` + `type` + `data`.
- **`apiVersion` is NOT emitted today.** Don't write code that branches on its presence yet — it's not in the envelope.
- When the migration happens, the CLI will accept both shapes for at least one minor release, then deprecate the bare `v: 1` form.

The deferral is intentional: adding a parallel versioning field to the existing envelope would be cosmetic without a corresponding event-protocol redesign. Doing both at once when v0.5 ships keeps the migration coherent.

---

## Compatibility windows

Pre-v1.0 policy:

| Change type | Allowed? | Deprecation window |
|---|---|---|
| Patch release (bug fix only) | Yes, anytime | None |
| Minor release (backward-compatible feature) | Yes, anytime | None |
| Breaking change to CLI flag or behavior | Yes, with migration notes | None pre-v1.0, but call it out |
| ADL schema field rename / removal | Yes, with new schema version | New `v1betaN` accepted alongside `v1alpha1` for ≥1 minor |
| Wire-protocol envelope change | Yes, with new envelope version | Both shapes accepted for ≥1 minor |

Post-v1.0 policy (tentative; finalized in `v0.9.x`):

| Change type | Deprecation window |
|---|---|
| Breaking CLI change | One minor + ADR in `docs/architecture/` |
| Schema breaking change | One minor with both versions accepted; deprecation log on every use |
| Wire-protocol breaking change | One minor with both envelopes accepted |

---

## Example timeline

This is what the next two minor cycles might look like:

```text
v0.2.0 (current)
  agentctl                    v0.2.0
  @agent-controller/runtime   0.1.5
  runtime-opencode            0.1.0
  ADL schema                  v1alpha1
  Manifest schema             v1
  Wire protocol               v: 1

v0.3.0 (RuntimeBinding)
  agentctl                    v0.3.0   (adds runtime.requirements parsing)
  @agent-controller/runtime   0.1.6    (no breaking change, patch bump)
  runtime-opencode            0.2.0    (Resolve() hook needed)
  ADL schema                  v1alpha1 (still — requirements added additively)
  Manifest schema             v1
  Wire protocol               v: 1

v0.4.0 (Kubernetes backend)
  agentctl                    v0.4.0
  @agent-controller/runtime   0.1.7
  runtime-opencode            0.3.0
  ADL schema                  v1alpha1
  Manifest schema             v1
  Wire protocol               v: 1
  agent-runtime-base image    0.1.0    (new dimension introduced)

v0.5.0 (Event protocol expansion + governance)
  agentctl                    v0.5.0
  @agent-controller/runtime   0.2.0    (emits apiVersion envelope)
  runtime-opencode            0.4.0
  ADL schema                  v1alpha1 → v1beta1 if governance fields force it
  Manifest schema             v1
  Wire protocol               v: 1 + apiVersion/v1alpha1 (both accepted)
  agent-runtime-base image    0.2.0
```

Numbers are illustrative — actual per-component versions depend on what changes between releases.

---

## When to bump what

A practical checklist for contributors making a change:

1. **Touching the CLI?** Bump `agentctl` version (Go module).
2. **Touching `runtime/`?** Bump `@agent-controller/runtime`. If the change adds a new flag or env var consumer code may set, bump minor; if it's a bug fix, patch.
3. **Touching `runtime-opencode/`?** Same — independent npm version from `runtime/`.
4. **Touching `schemas/adl.v1alpha1.json` in a non-additive way?** This is a breaking schema change; introduce `v1beta1.json` instead, don't mutate `v1alpha1`.
5. **Touching the wire-event envelope shape?** Don't, pre-v0.5 — `v: 1` is stable. Adding new event `type` values is fine; reshaping the envelope is not.
6. **Touching only docs / examples / tests?** No version bump needed.

---

## Cross-references

- [`ROADMAP.md`](../ROADMAP.md) — what's coming in v0.3 / v0.4 / v0.5+
- [`docs/architecture/overview.md`](architecture/overview.md) — wire-protocol section explains the current `v: 1` envelope
- [`docs/architecture/harness-matrix.md`](architecture/harness-matrix.md) — per-feature support per adapter
- [`schemas/`](../schemas/) — actual schema files
