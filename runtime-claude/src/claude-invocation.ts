/**
 * Invocation builders for the Claude Agent SDK runtime adapter.
 *
 * This module is intentionally pure: every export is a function over plain
 * data with no I/O, so the whole surface is unit-testable without a live
 * model or an SDK session.
 */

import type { AgentDefinition, McpServerConfig, Options } from "@anthropic-ai/claude-agent-sdk";
import type { CompiledSpec, MCPServer, ResolvedRef } from "./types.js";
import { HONESTY_PREAMBLE, wrapSkillBody } from "./honesty.js";

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
 * Returns undefined for names with no SDK equivalent.
 */
const BUILTIN_TOOL_MAP: Record<string, string> = {
  bash: "Bash",
  read: "Read",
  edit: "Edit",
  write: "Write",
};

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
 */
export function buildOptions(
  spec: CompiledSpec,
  systemPrompt: string,
  subagents: Record<string, AgentDefinition>,
): Options {
  const opts: Options = {
    model: spec.model.name,
    systemPrompt,
    settingSources: [],
  };

  const allowed = (spec.tools ?? [])
    .filter((t) => t.builtin)
    .map((t) => BUILTIN_TOOL_MAP[t.name])
    .filter((n): n is string => Boolean(n));
  if (allowed.length > 0) opts.allowedTools = allowed;

  const servers = spec.mcpServers ?? [];
  if (servers.length > 0) {
    const map: Record<string, McpServerConfig> = {};
    for (const srv of servers) map[srv.name] = toMcpServerConfig(srv);
    opts.mcpServers = map;
  }

  if (Object.keys(subagents).length > 0) opts.agents = subagents;
  if (spec.sessionId) opts.resume = spec.sessionId;

  return opts;
}
