# Examples

Runnable ADL specs. Each is self-contained unless noted; run them from the repo
root so the `tools/` `extensions/` `skills/` `agents/` registry resolves.

```bash
cli/bin/agentctl run examples/hello.yaml
```

## One per adapter

Same agent shape, one field different — the point of ADL.

| File | `runtime.type` | Notes |
|---|---|---|
| [`hello.yaml`](hello.yaml) | `local` (Pi) | `get_time` tool, `audit-log` extension, `example-time-skill` |
| [`hello-opencode.yaml`](hello-opencode.yaml) | `local-opencode` | needs the `opencode` CLI on `PATH` |
| [`hello-codex.yaml`](hello-codex.yaml) | `local-codex` | needs the `codex` CLI on `PATH`; `model.provider: openai` |
| [`hello-claude.yaml`](hello-claude.yaml) | `local-claude` | `model.provider: anthropic`; no extra CLI |

## By feature

| File | Demonstrates |
|---|---|
| [`mcp-time.yaml`](mcp-time.yaml) | MCP over stdio via `@modelcontextprotocol/server-time` |
| [`self-contained-mcp.yaml`](self-contained-mcp.yaml) | The same agent with `extensions[].source` auto-install |
| [`claude-skills-demo.yaml`](claude-skills-demo.yaml) | External skills inlined into the prompt, no tools |
| [`claude-skills-with-bash.yaml`](claude-skills-with-bash.yaml) | A skill plus the `bash` tool — runs a real shell command |
| [`subagent-demo.yaml`](subagent-demo.yaml) | Parent delegates to the `sql-explorer` child |
| [`bash-allowlist.yaml`](bash-allowlist.yaml) | Bash command allowlist via `@gotgenes/pi-permission-system` |
| [`tracing-demo.yaml`](tracing-demo.yaml) | OTel spans — run with `OTEL_EXPORTER_OTLP_ENDPOINT=...` |
| [`guardrails-block.yaml`](guardrails-block.yaml) · [`-warn`](guardrails-warn.yaml) · [`-correct`](guardrails-correct.yaml) | The three hallucination-detector modes, side by side |
| [`with-installs.yaml`](with-installs.yaml) | The deprecated `spec.installs[]` — prefer `extensions[].source` |

## Serving and scheduling

| Path | What it is |
|---|---|
| [`serve-test.yaml`](serve-test.yaml) | Minimal spec used by the `agentctl serve` E2E tests |
| [`serve-client.sh`](serve-client.sh) | Runnable client — creates a session, streams a turn over SSE |
| [`bindings/`](bindings/) | RuntimeBindings: `local-default`, `local-strict` (strict capability matching), `kubernetes-kind` |
| [`schedulers/`](schedulers/) | `agentctl run` as a Maestro / Airflow / Temporal task, over a shared [`text-classifier.yaml`](schedulers/text-classifier.yaml) |

## Related

- Field reference: [`../README.md#what-adl-can-declare`](../README.md#what-adl-can-declare)
- Per-adapter support: [`../docs/architecture/harness-matrix.md`](../docs/architecture/harness-matrix.md)
- `serve` HTTP reference: [`../docs/serve.md`](../docs/serve.md)
