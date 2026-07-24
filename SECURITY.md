# Security Policy

## Supported versions

Agent Controller is pre-1.0. Only the latest minor release receives security fixes; once a new minor lands, the previous one is end-of-life.

| Version | Supported |
|---|---|
| `v0.2.x` | ✅ |
| `v0.1.x` and earlier | ❌ (no further fixes) |

Once `v1.0.0` ships, the supported-versions window will widen to at least the current minor and the one immediately preceding it.

## Reporting a vulnerability

**Please do not open public GitHub issues for security vulnerabilities.**

To report a vulnerability:

1. Open a private security advisory on GitHub: https://github.com/CCDevelopForFun/agent-controller/security/advisories/new
2. If you can't use GitHub Security Advisories, send an email to the project owner (visible in `git log`).
3. Include enough detail to reproduce: spec / config that triggers it, expected vs. actual behavior, any logs or stack traces.

You should receive an acknowledgement within **5 business days**. We aim to triage within 10 business days and publish a fix or disclosure timeline within 30 days for high-severity issues.

## Threat model

Agent Controller is a wrapper around language-model-driven agents that execute tools on the operator's machine or in remote runtimes. The threats we take seriously, in order of priority:

1. **ADL allowlist bypass.** Specs declare a closed set of tools / extensions / subagents / MCP servers. The runtime must refuse to expose anything not in that set. Examples we've already caught and closed (see `runtime-opencode/src/opencode-config.ts` history):
   - MCP server names that overmatch built-in permissions via wildcard
   - opencode native subagents (`general`, `explore`, etc.) reachable via `task` tool when subagents are declared
   - Skill bodies smuggling fabricated `<invoke>` XML past the guardrail
2. **Hallucinated tool calls.** The model writing fake tool-call XML in message text instead of using the runtime's tool channel. Defended in three layers: honesty preamble, skill-body framing, runtime XML detector (`block` / `warn` / `correct` modes).
3. **Ambient config leakage.** A user's `~/.opencode` / `~/.claude` / `~/.pi` config silently affecting what the agent can do. The opencode adapter isolates `HOME` / `XDG_*` / cwd to a fresh temp dir per session.
4. **Prototype pollution via user-supplied names.** ADL field values become map keys (MCP server names, subagent names). The **opencode adapter** uses null-prototype maps and `Object.hasOwn` checks (slice 2.5 codex pass 6) so keys like `__proto__`, `constructor`, or `toString` are inert. The **Pi adapter does NOT yet have the same defense** — it builds maps from `spec.mcpServers[].name` using plain `{}` objects today. Adding the same null-prototype hardening to the Pi adapter is a tracked v0.3 follow-up; until then, treat Pi-adapter inputs as if they had no prototype-pollution guard.

Out of scope (for now):

- Sandboxing the model itself or the tools it invokes — that's the runtime's (Pi / opencode / Kubernetes pod) responsibility.
- Anything that happens after a tool call legitimately granted by the spec runs. If you give an agent `bash` access and ask it to `rm -rf /`, the agent does what you asked.
- Multi-tenant isolation. Pre-v1.0, the model is "one operator, one session" — multi-tenancy lives at the deployment layer (Kubernetes RBAC, etc.).

## Coordinated disclosure

We follow industry-standard coordinated disclosure:

1. Reporter sends private report.
2. We acknowledge, triage, and develop a fix on a private branch.
3. We agree a disclosure date with the reporter (typically 30-90 days from report).
4. On disclosure day: fix ships in a patch release, GitHub Security Advisory is published, reporter is credited (unless they prefer anonymity).

We will not pursue legal action against good-faith researchers who follow this process.
