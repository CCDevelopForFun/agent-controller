/**
 * Invocation builders for the Claude Agent SDK runtime adapter.
 *
 * This module is intentionally pure: every export is a function over plain
 * data with no I/O, so the whole surface is unit-testable without a live
 * model or an SDK session.
 */

import { createHash } from "node:crypto";
import type { AgentDefinition, McpServerConfig, Options } from "@anthropic-ai/claude-agent-sdk";
import type { CompiledSpec, MCPServer, ResolvedRef } from "./types.js";
import { HONESTY_PREAMBLE, wrapSkillBody } from "./honesty.js";

/**
 * Namespace for the agentctl-session-id -> SDK-session-UUID derivation.
 *
 * Itself UUIDv5(DNS, "agent-controller.dev"), so the constant is reproducible
 * from a documented input rather than being an arbitrary blob.
 */
const CLAUDE_SESSION_NAMESPACE = "0b66266c-ec3e-5b64-841f-aae1422cf01f";

/**
 * RFC 4122 §4.3 name-based UUID (SHA-1 / version 5), implemented locally so
 * the adapter takes no dependency for sixteen bytes of hashing.
 */
function uuidV5(namespace: string, name: string): string {
  const nsBytes = Buffer.from(namespace.replace(/-/g, ""), "hex");
  const digest = createHash("sha1").update(nsBytes).update(Buffer.from(name, "utf8")).digest();
  const b = Buffer.from(digest.subarray(0, 16));
  b[6] = (b[6] & 0x0f) | 0x50; // version 5
  b[8] = (b[8] & 0x3f) | 0x80; // RFC 4122 variant
  const h = b.toString("hex");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/**
 * Bridge an agentctl session id onto an SDK-native session id.
 *
 * `spec.sessionId` is an agentctl-owned opaque key, not something the SDK
 * minted: `agentctl serve` generates `s_<base36ms><8hex>`
 * (cli/internal/serve/manager.go) and `agentctl chat` generates `s_<hex>`
 * (cli/cmd/agentctl/chat.go). `Options.sessionId` "Must be a valid UUID"
 * (sdk.d.ts:1804-1809) and `Options.resume` expects an id the SDK itself
 * issued, so passing the agentctl id to either is a category error.
 *
 * The mapping is deterministic — same agentctl id always yields the same UUID
 * — so a later turn can find the same SDK session without carrying state.
 * The sibling adapters take the same posture of treating `spec.sessionId` as
 * an opaque key and deriving backend-native storage from it
 * (runtime-codex/src/codex-home.ts::resolveCodexHome).
 */
export function deriveSdkSessionUuid(agentctlSessionId: string): string {
  if (typeof agentctlSessionId !== "string" || agentctlSessionId.trim() === "") {
    throw new Error(
      `runtime-claude: spec.sessionId is set but blank ` +
        `(${JSON.stringify(agentctlSessionId)}). The Claude Agent SDK addresses ` +
        `sessions by UUID, and the agentctl session id is what that UUID is ` +
        `derived from, so it must be a non-empty string.`,
    );
  }
  return uuidV5(CLAUDE_SESSION_NAMESPACE, agentctlSessionId);
}

/**
 * Returns true when a ResolvedRef is a custom Pi-extension tool (entrypoint
 * set, not a Pi built-in). Built-ins are always safe even if an entrypoint is
 * incidentally set.
 */
function isCustomPiExtensionTool(t: ResolvedRef): boolean {
  return Boolean(t.entrypoint) && !t.builtin;
}

/**
 * Throws with a field-naming error when `spec` declares features the Claude
 * Agent SDK adapter cannot honour. Mirrors
 * cli/internal/adl/compiler.go::checkClaudeIncompatibilities — the compiler is
 * the canonical gate; this is defense in depth for hand-crafted CompiledSpecs
 * that bypass `agentctl compile`.
 *
 * Note: spec.subagents[] is NOT rejected — the SDK registers them natively.
 */
export function assertClaudeCompatible(spec: CompiledSpec): void {
  if (spec.model.provider !== "anthropic") {
    throw new Error(
      `runtime-claude: spec.model.provider is "${spec.model.provider}" but the ` +
        `Claude Agent SDK adapter supports only provider "anthropic". ` +
        `Use runtime.type: local / local-pi / local-opencode for openai/google.`,
    );
  }

  if ((spec.extensions ?? []).length > 0) {
    throw new Error(
      `runtime-claude: spec.extensions[] is non-empty. Pi-format extension JS ` +
        `modules cannot run inside the Claude Agent SDK. Remove extensions or ` +
        `target runtime.type: local.`,
    );
  }

  if ((spec.installs ?? []).length > 0) {
    throw new Error(
      `runtime-claude: spec.installs[] is non-empty. The claude adapter does not ` +
        `support the deprecated installs field. Use spec.extensions[].source on ` +
        `the Pi adapter instead.`,
    );
  }

  const offenders = (spec.tools ?? []).filter(isCustomPiExtensionTool).map((t) => t.name);
  if (offenders.length > 0) {
    throw new Error(
      `runtime-claude: spec.tools[] contains custom Pi-extension tools that ` +
        `cannot run on the claude adapter: ${offenders.sort().join(", ")}. ` +
        `Only Pi built-in tools (bash, read, edit, write) are supported.`,
    );
  }
}

/** A pre-resolved skill body, ready to inline into the system prompt. */
export interface SkillBody {
  name: string;
  body: string;
}

/** A pre-parsed subagent definition from <root>/agents/<slug>.md. */
export interface SubagentBody {
  name: string;
  description: string;
  tools?: string[];
  model?: string;
  prompt: string;
}

/**
 * Maps an ADL Pi built-in tool name onto the Claude Agent SDK tool name.
 *
 * This map now decides what the model can *use*, not merely what is
 * auto-approved (see buildOptions), so an unmapped name must fail loudly —
 * silently dropping it would hand back an Options that grants less than the
 * spec declares. Mirrors PI_TO_OPENCODE_PERMISSION_KEY's "Add an entry."
 * throw in runtime-opencode/src/opencode-config.ts.
 */
const BUILTIN_TOOL_MAP: Record<string, string> = {
  bash: "Bash",
  read: "Read",
  edit: "Edit",
  write: "Write",
};

/**
 * The SDK tool that delegates to a subagent registered via `Options.agents`.
 *
 * The name is `Agent`, not `Task`. Verified from the SDK's own types rather
 * than assumed: `AgentDefinition`'s doc comment is "Definition for a custom
 * subagent that can be invoked via the Agent tool" (sdk.d.ts:36, repeated at
 * :1353), and sdk-tools.d.ts declares `AgentInput`/`AgentOutput` with no
 * `TaskInput` counterpart. `Task` survives only as a legacy alias the SDK
 * rewrites to `Agent` internally, so listing `Agent` is the durable form.
 */
const SUBAGENT_DELEGATION_TOOL = "Agent";

/**
 * Map the spec's Pi built-in tools onto SDK tool names, preserving spec order.
 *
 * Non-built-ins are skipped: custom Pi-extension tools (entrypoint set) are
 * already rejected by assertClaudeCompatible and by the Go compiler.
 */
function mapBuiltinTools(tools: ResolvedRef[]): string[] {
  const out: string[] = [];
  for (const t of tools) {
    if (!t.builtin) continue;
    const mapped = BUILTIN_TOOL_MAP[t.name];
    if (!mapped) {
      throw new Error(
        `runtime-claude: spec.tools[] declares Pi built-in ${JSON.stringify(t.name)} ` +
          `which is not in BUILTIN_TOOL_MAP. Add an entry. Known built-ins: ` +
          `${Object.keys(BUILTIN_TOOL_MAP).join(", ")}.`,
      );
    }
    out.push(mapped);
  }
  return out;
}

/**
 * Build the permission-rule string that auto-approves every tool exposed by
 * one declared MCP server.
 *
 * The SDK's allow-rule grammar splits on `__`: `mcp__<server>` grants the whole
 * server, `mcp__<server>__<tool>` a single tool (sdk.d.ts:48 documents the same
 * server-level forms for `disallowedTools`; sdk.mjs's rule validator lists
 * `mcp__<server>` among the valid examples and rejects wildcards outside the
 * tool position in allow rules). Two consequences are handled here:
 *
 *  - Server names are normalized by the SDK when it builds tool names —
 *    "non-[a-zA-Z0-9_-] becomes _" (sdk.d.ts:3509) — so the rule must use the
 *    normalized form or it would silently match nothing.
 *  - A normalized name that still contains `__` is ambiguous under that
 *    grammar (`a__b` would be read as server `a`, tool `b`, potentially
 *    granting a *different* server). That cannot be expressed safely, so it
 *    throws instead of emitting a rule that grants the wrong thing.
 */
function mcpServerAllowRule(name: string): string {
  const normalized = name.replace(/[^a-zA-Z0-9_-]/g, "_");
  if (normalized.includes("__")) {
    throw new Error(
      `runtime-claude: MCP server name ${JSON.stringify(name)} normalizes to ` +
        `${JSON.stringify(normalized)}, which contains "__". The SDK's permission-rule ` +
        `grammar reads "mcp__<server>__<tool>", so such a name cannot be granted ` +
        `server-wide without also matching a different server. Rename the server in ` +
        `spec.mcpServers[].name.`,
    );
  }
  return `mcp__${normalized}`;
}

/**
 * Compose the system prompt: honesty preamble, then persona role, then persona
 * instructions, then each skill body wrapped via wrapSkillBody(). The spec's
 * `task` is NOT included here — it is the `prompt` argument to query().
 *
 * The persona/skills section order matches
 * runtime-codex/src/codex-invocation.ts::buildPrompt; codex additionally
 * appends `spec.task` as a final section, which this adapter deliberately
 * omits since `task` is passed separately to query().
 */
export function buildPrompt(spec: CompiledSpec, skills: SkillBody[]): string {
  const sections: string[] = [HONESTY_PREAMBLE];

  if (spec.persona?.role) {
    sections.push(`# Role\n\n${spec.persona.role}`);
  }
  if (spec.persona?.instructions) {
    sections.push(`# Instructions\n\n${spec.persona.instructions}`);
  }
  for (const skill of skills) {
    sections.push(wrapSkillBody(skill.name, skill.body));
  }

  return sections.join("\n\n");
}

/**
 * Build the SDK `agents` map from parsed subagent bodies. `tools` is omitted
 * when absent so the subagent inherits the parent's tool set (SDK semantics).
 */
export function buildSubagents(bodies: SubagentBody[]): Record<string, AgentDefinition> {
  const out: Record<string, AgentDefinition> = {};
  for (const b of bodies) {
    const def: AgentDefinition = { description: b.description, prompt: b.prompt };
    if (b.tools && b.tools.length > 0) def.tools = b.tools;
    // AgentDefinition.model accepts an alias ("opus") or a full id
    // ("claude-opus-4-6"); omitting it inherits the main model. Dropping a
    // declared model would silently ignore the subagent's frontmatter.
    if (b.model) def.model = b.model;
    out[b.name] = def;
  }
  return out;
}

/**
 * Map one ADL MCPServer onto the SDK's McpServerConfig union.
 *
 * schemas/adl.v1alpha1.json already requires `command` for stdio servers and
 * `url` for http/sse servers, so a spec that passed `agentctl validate`
 * cannot reach the throws below. This is defense-in-depth for hand-crafted
 * CompiledSpecs that bypass the compiler (see assertClaudeCompatible above
 * for the same posture) — mirrors runtime-opencode/src/opencode-config.ts's
 * stdio/http field checks so a missing field fails loudly and names the
 * offending server and field, instead of silently producing an empty-string
 * command/url that only surfaces as an opaque SDK error much later.
 */
function toMcpServerConfig(srv: MCPServer): McpServerConfig {
  switch (srv.transport) {
    case "stdio":
      if (!srv.command) {
        throw new Error(
          `runtime-claude: MCP server "${srv.name}" uses transport "stdio" but has no command field.`,
        );
      }
      return {
        type: "stdio",
        command: srv.command,
        args: srv.args,
        env: srv.env,
      };
    case "streamable-http":
    case "sse":
      if (!srv.url) {
        throw new Error(
          `runtime-claude: MCP server "${srv.name}" uses transport "${srv.transport}" but has no url field.`,
        );
      }
      return srv.transport === "streamable-http"
        ? { type: "http", url: srv.url, headers: srv.headers }
        : { type: "sse", url: srv.url, headers: srv.headers };
  }
}

/**
 * Build the SDK Options object.
 *
 * `settingSources: []` is unconditional and load-bearing: without it the SDK
 * loads ~/.claude/settings.json, project .claude/, and CLAUDE.md files, making
 * a declarative spec's behavior depend on the operator's machine and silently
 * widening the tool surface past what the spec declares.
 *
 * Tool gating uses BOTH SDK options, which do different jobs:
 *
 *  - `Options.tools` is the *restriction*: "Specify the base set of available
 *    built-in tools … `[]` (empty array) — Disable all built-in tools"
 *    (sdk.d.ts:1422-1434). It is always set, never left undefined, because
 *    undefined means "all default Claude Code tools" — which is how a
 *    `tools: []` spec previously kept a live Bash. Same contract
 *    runtime-opencode states for its deny-baseline: a `tools: []` spec must
 *    actually run with no tools.
 *  - `Options.allowedTools` is *auto-approval*: "tool names that are
 *    auto-allowed without prompting … To restrict which tools are available,
 *    use the `tools` option instead" (sdk.d.ts:1368-1375). Runs here are
 *    non-interactive and no `canUseTool` callback is supplied, so a tool that
 *    is available but not auto-approved makes the SDK raise a control-request
 *    error ("canUseTool callback is not provided."). Everything granted is
 *    therefore also auto-approved.
 *
 * MCP tools are not built-ins and so are unaffected by `Options.tools`; they
 * are granted per declared server via `mcp__<server>` allow rules. Anything
 * not declared in `spec.mcpServers[]` is never auto-approved.
 *
 * `resumeSdkSessionId` carries the SDK-native session id captured on a
 * previous turn of the same agentctl session (see index.ts). Its presence is
 * what distinguishes a resumed turn from a first turn — the SDK forbids
 * `sessionId` and `resume` together (sdk.d.ts:1804-1809).
 */
export function buildOptions(
  spec: CompiledSpec,
  systemPrompt: string,
  subagents: Record<string, AgentDefinition>,
  resumeSdkSessionId?: string,
): Options {
  const opts: Options = {
    model: spec.model.name,
    systemPrompt,
    settingSources: [],
  };

  const granted = mapBuiltinTools(spec.tools ?? []);

  // Subagents are delegated to through a tool. Registering them via
  // `Options.agents` without granting that tool would leave the delegation
  // silently unreachable — the same coupling runtime-opencode encodes as
  // `grants["task"] = "allow"` when the spec declares subagents.
  if (Object.keys(subagents).length > 0) granted.push(SUBAGENT_DELEGATION_TOOL);

  opts.tools = granted;

  const servers = spec.mcpServers ?? [];
  const allowed = [...granted, ...servers.map((srv) => mcpServerAllowRule(srv.name))];
  opts.allowedTools = allowed;

  if (servers.length > 0) {
    const map: Record<string, McpServerConfig> = {};
    for (const srv of servers) map[srv.name] = toMcpServerConfig(srv);
    opts.mcpServers = map;
  }

  if (Object.keys(subagents).length > 0) opts.agents = subagents;

  if (spec.sessionId) {
    if (resumeSdkSessionId) {
      opts.resume = resumeSdkSessionId;
    } else {
      opts.sessionId = deriveSdkSessionUuid(spec.sessionId);
    }
  }

  return opts;
}
