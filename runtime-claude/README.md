# `@agent-controller/runtime-claude`

Claude Agent SDK runtime adapter for [agent-controller](https://github.com/CCDevelopForFun/agent-controller).
Selected by `runtime.type: local-claude`.

Reads a `CompiledSpec` JSON document on stdin, drives a session through
[`@anthropic-ai/claude-agent-sdk`](https://www.npmjs.com/package/@anthropic-ai/claude-agent-sdk),
and writes the versioned NDJSON wire-event stream to stdout.

## Requirements

- Node >= 22
- `ANTHROPIC_API_KEY` in the environment
- `spec.model.provider: anthropic`

Unlike the opencode and codex adapters, **no external CLI is required** — the
SDK ships its own executable.

## Supported ADL surface

| Field | Support |
|---|---|
| `persona`, `task` | composed into the system prompt / prompt |
| `tools[]` (Pi built-ins) | mapped to `allowedTools` |
| `skills[]` | bodies inlined into the system prompt |
| `subagents[]` | registered natively via the SDK `agents` option |
| `mcpServers[]` | stdio, sse, and streamable-http all supported |
| `guardrails` | adapter-side hallucination detector |
| `--resume <id>` | mapped to the SDK `resume` option |
| `extensions[]`, `installs[]` | rejected — Pi-only |
| `model.provider` != anthropic | rejected |

See [`docs/architecture/harness-matrix.md`](../docs/architecture/harness-matrix.md)
for the authoritative per-field matrix.

## Isolation

The adapter always passes `settingSources: []`, so the SDK loads **no** ambient
configuration — not `~/.claude/settings.json`, not project `.claude/`, not
`CLAUDE.md`. A spec's behavior therefore depends only on the spec.
