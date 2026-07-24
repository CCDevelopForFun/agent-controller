# `schemas/` — JSON Schemas

Language-neutral JSON Schema definitions for Agent Controller's declarative contracts. Consumed by both `agentctl` (Go, via `santhosh-tekuri/jsonschema/v6`) and the runtime adapters (TypeScript, via `@sinclair/typebox` for input/output validation in tools).

**These are the source of truth.** Do not maintain parallel typed schemas in either language; generate or mirror, never duplicate by hand.

## Files

| File | Required `apiVersion` value | Used by |
|---|---|---|
| [`adl.v1alpha1.json`](adl.v1alpha1.json) | `agent-controller.dev/v1alpha1` | `agentctl validate` / `agentctl compile` for documents with `kind: Agent` |
| [`runtimebinding.v1alpha1.json`](runtimebinding.v1alpha1.json) | `agent-controller.dev/v1alpha1` | `agentctl validate` for documents with `kind: RuntimeBinding` (added in v0.3.2). v0.3.3 (Backend.Resolve) will consume parsed bindings at run time. |
| [`manifest.v1.json`](manifest.v1.json) | `agent-controller.dev/v1alpha1` (shared namespace with ADL) | Manifest files under `tools/<name>/manifest.yaml`, `extensions/<name>/manifest.yaml`, etc. |

All three schemas live in the same `agent-controller.dev/v1alpha1` namespace today (`agentctl validate` dispatches by the `kind:` field). The filename `manifest.v1.json` reflects the **manifest file format version** (`v1`), which is independent of the ADL schema's `v1alpha1` stage — a future breaking change to the manifest format would be `manifest.v2.json` even if ADL stays on `v1alpha1`.

## Embedded copies and drift

Both schemas are **embedded** into the Go CLI binary via `go:embed`. The originals in this directory are the source of truth; the embedded copies live at:

```
cli/internal/adl/schemas/adl.v1alpha1.json
cli/internal/adl/schemas/runtimebinding.v1alpha1.json
cli/internal/registry/schemas/manifest.v1.json
```

`go:embed` reads from these *internal* paths, not from `schemas/` at the repo root. **A plain rebuild will not pick up changes to the root schemas** — you must copy the file manually into the embedded location before the CLI sees the new version:

```bash
# After editing schemas/adl.v1alpha1.json
cp schemas/adl.v1alpha1.json cli/internal/adl/schemas/adl.v1alpha1.json

# After editing schemas/manifest.v1.json
cp schemas/manifest.v1.json cli/internal/registry/schemas/manifest.v1.json

# Then run from the cli/ module so the drift test catches mistakes
go -C cli test ./...
```

A drift-detection test (added in v0.2 slice 1.2) compares the embedded bytes against the on-disk source on every `go test ./...`. The test FAILS if the two copies disagree — that's by design, so contributors can't accidentally ship divergent schemas.

## IDE integration

You can point your YAML language server at the schemas directly:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/CCDevelopForFun/agent-controller/main/schemas/adl.v1alpha1.json
apiVersion: agent-controller.dev/v1alpha1
kind: Agent
...
```

Stable URLs on `agent-controller.dev/schemas/` are planned for v0.2.0+ — see [`../ROADMAP.md`](../ROADMAP.md).

## Versioning

Schema versions move independently of the CLI / runtime versions. See [`../docs/versioning.md`](../docs/versioning.md) for the multi-dimension policy.

Current state:

| Schema | Stage | Stability |
|---|---|---|
| `adl.v1alpha1.json` | alpha | Fields may be renamed, removed, or change meaning between minors |
| `manifest.v1.json` | v1 | Stable; deprecation requires a `v2` migration window |

Breaking changes to ADL require a new schema version (e.g. `v1beta1.json`), not silent mutation of `v1alpha1`.

## Adding a field

When adding a new field to ADL:

1. Update `schemas/adl.v1alpha1.json` here (additive only — see [`../CONTRIBUTING.md`](../CONTRIBUTING.md))
2. Copy the updated file into `cli/internal/adl/schemas/adl.v1alpha1.json` (the embedded source)
3. Update `cli/internal/adl/compiler.go` (Go side)
4. Update `runtime/src/types.ts` AND `runtime-opencode/src/types.ts` (TS sides)
5. Update `docs/architecture/harness-matrix.md` with a row for the new field per adapter
6. Run `go -C cli test ./...` to confirm the schema-sync test passes

All six in the same commit.

## Cross-references

- [`../docs/versioning.md`](../docs/versioning.md) — version dimensions explained
- [`../docs/architecture/harness-matrix.md`](../docs/architecture/harness-matrix.md) — per-field adapter support
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md) — schema-change workflow
