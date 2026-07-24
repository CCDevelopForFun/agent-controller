/**
 * Invocation builder for the codex runtime adapter.
 *
 * Provides three pure functions:
 *
 *   - assertCodexCompatible(spec) — throws with a field-naming message when
 *     the spec declares features that codex cannot honour: non-openai provider,
 *     extensions, installs, subagents, or custom Pi-extension tools.
 *
 *   - buildPrompt(spec, skills) — composes the system prompt from the honesty
 *     preamble, optional persona role/instructions, inlined skill bodies, and
 *     the spec task. Mirrors buildPromptString() from runtime-opencode.
 *
 *   - buildCodexArgs(spec, opts) — returns the argv array (after the binary
 *     name) for `codex exec` / `codex exec resume`. The prompt is the last
 *     positional argument.
 *
 * No imports from runtime-opencode — this module is self-contained.
 */

import type { CompiledSpec, MCPServer, ResolvedRef } from "./types.js";
import { HONESTY_PREAMBLE, wrapSkillBody } from "./honesty.js";

/** Pre-resolved skill body, ready to be inlined into the system prompt. */
export interface SkillBody {
  name: string;
  body: string;
}

// ── assertCodexCompatible ──────────────────────────────────────────────────

/**
 * Throws with a descriptive, field-naming error when `spec` declares features
 * that the codex adapter cannot honour:
 *
 *   - model.provider !== "openai"  (codex only runs OpenAI models)
 *   - spec.extensions non-empty    (Pi-extension format unsupported)
 *   - spec.installs non-empty      (deprecated self-install unsupported)
 *   - spec.subagents non-empty     (subagent delegation unsupported)
 *   - any spec.tools[] entry with entrypoint set and builtin !== true
 *     (custom Pi-extension tool — cannot run in codex)
 *
 * Does nothing when the spec is fully compatible.
 */
export function assertCodexCompatible(spec: CompiledSpec): void {
  if (spec.model.provider !== "openai") {
    throw new Error(
      `runtime-codex: spec.model.provider is "${spec.model.provider}" but codex ` +
      `only supports provider "openai". Update the spec's model block.`,
    );
  }

  if ((spec.extensions ?? []).length > 0) {
    throw new Error(
      `runtime-codex: spec.extensions[] is non-empty. Pi-format extensions ` +
      `cannot run on the codex adapter. Remove extensions or target a ` +
      `different runtime.`,
    );
  }

  if ((spec.installs ?? []).length > 0) {
    throw new Error(
      `runtime-codex: spec.installs[] is non-empty. The codex adapter does ` +
      `not support the deprecated installs field. Remove installs or target ` +
      `a different runtime.`,
    );
  }

  if ((spec.subagents ?? []).length > 0) {
    throw new Error(
      `runtime-codex: spec.subagents[] is non-empty. The codex adapter does ` +
      `not support subagent delegation. Remove subagents or target a ` +
      `different runtime.`,
    );
  }

  const offenders: string[] = [];
  for (const t of spec.tools ?? []) {
    if (isCustomPiExtensionTool(t)) {
      offenders.push(t.name);
    }
  }
  if (offenders.length > 0) {
    throw new Error(
      `runtime-codex: spec.tools[] contains custom Pi-extension tools that ` +
      `cannot run on the codex adapter: ${offenders.join(", ")}. ` +
      `Only Pi built-in tools (bash, read, edit, write) are supported.`,
    );
  }
}

/**
 * Returns true when a ResolvedRef represents a custom Pi-extension tool
 * (entrypoint set, not a Pi built-in). Built-in tools (builtin: true) are
 * always safe, even if an entrypoint is incidentally set.
 */
function isCustomPiExtensionTool(t: ResolvedRef): boolean {
  return Boolean(t.entrypoint) && !t.builtin;
}

// ── buildConfigToml ────────────────────────────────────────────────────────

/**
 * Emit a `$CODEX_HOME/config.toml` fragment containing `[mcp_servers.<name>]`
 * blocks for every MCP server declared in spec.mcpServers[].
 *
 * Codex TOML schema (verified via `codex mcp add --help` and ~/.codex/config.toml):
 *
 *   stdio:
 *     [mcp_servers.<name>]
 *     command = "<binary>"
 *     args = ["<arg1>", ...]          # omitted when absent
 *
 *     [mcp_servers.<name>.env]        # omitted when env is absent
 *     KEY = "value"
 *
 *   streamable-http:
 *     [mcp_servers.<name>]
 *     url = "<url>"
 *
 *   sse: not supported by the codex config schema — throws.
 *
 * Returns "" when there are no MCP servers (caller skips writing config.toml).
 */
export function buildConfigToml(spec: CompiledSpec): string {
  const servers = spec.mcpServers ?? [];
  if (servers.length === 0) return "";

  const blocks: string[] = [];

  for (const srv of servers) {
    if (srv.transport === "sse") {
      throw new Error(
        `codex adapter: SSE MCP servers are not supported by the codex config schema ` +
        `(server "${srv.name}"). Use transport "streamable-http" or "stdio" instead.`,
      );
    }

    blocks.push(buildMcpServerBlock(srv));
  }

  return blocks.join("\n");
}

/** Identifier pattern for TOML section key components (server names, env keys). */
const TOML_KEY_RE = /^[A-Za-z0-9_-]+$/;

/** Build the TOML block for a single MCP server entry. */
function buildMcpServerBlock(srv: MCPServer): string {
  if (!TOML_KEY_RE.test(srv.name)) {
    throw new Error(
      `runtime-codex: MCP server name "${srv.name}" is invalid — must match [A-Za-z0-9_-]+`,
    );
  }

  const lines: string[] = [`[mcp_servers.${srv.name}]`];

  if (srv.transport === "streamable-http") {
    lines.push(`url = ${tomlString(srv.url ?? "")}`);
  } else {
    // stdio
    lines.push(`command = ${tomlString(srv.command ?? "")}`);
    if (srv.args !== undefined && srv.args.length > 0) {
      lines.push(`args = ${tomlArray(srv.args)}`);
    }
  }

  lines.push(""); // blank line after the section header block

  // Emit env subtable when present (stdio only, but we emit if set regardless)
  if (srv.env && Object.keys(srv.env).length > 0) {
    lines.push(`[mcp_servers.${srv.name}.env]`);
    for (const [k, v] of Object.entries(srv.env)) {
      if (!TOML_KEY_RE.test(k)) {
        throw new Error(
          `runtime-codex: env key "${k}" in server "${srv.name}" is invalid — must match [A-Za-z0-9_-]+`,
        );
      }
      lines.push(`${k} = ${tomlString(v)}`);
    }
    lines.push(""); // blank line after env block
  }

  return lines.join("\n");
}

/** TOML-quote a string value. */
function tomlString(s: string): string {
  // Escape backslashes, double-quotes, and control characters per TOML spec.
  return `"${s
    .replace(/\\/g, "\\\\")
    .replace(/"/g, '\\"')
    .replace(/\n/g, "\\n")
    .replace(/\r/g, "\\r")
    .replace(/\t/g, "\\t")
    .replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`)}"`;
}

/** TOML inline array of strings. */
function tomlArray(items: string[]): string {
  return `[${items.map(tomlString).join(", ")}]`;
}

// ── buildPrompt ────────────────────────────────────────────────────────────

/**
 * Compose the codex system prompt from:
 *   1. HONESTY_PREAMBLE
 *   2. Persona role (when present)
 *   3. Persona instructions (when present)
 *   4. Skill bodies (each wrapped via wrapSkillBody; empty bodies are skipped)
 *   5. spec.task
 *
 * Parts are joined with "\n\n". Mirrors buildPromptString() in
 * runtime-opencode/src/opencode-config.ts without importing from it.
 */
export function buildPrompt(spec: CompiledSpec, skills: SkillBody[]): string {
  const parts: string[] = [HONESTY_PREAMBLE];

  if (spec.persona?.role) {
    parts.push(`# Role\n${spec.persona.role}`);
  }
  if (spec.persona?.instructions) {
    parts.push(`# Instructions\n${spec.persona.instructions}`);
  }

  for (const s of skills) {
    if (!s.body.trim()) continue;
    parts.push(wrapSkillBody(s.name, s.body.trim()));
  }

  parts.push(spec.task);

  return parts.join("\n\n");
}

// ── buildCodexArgs ─────────────────────────────────────────────────────────

/**
 * Build the argv array (everything after the codex binary) for a `codex exec`
 * invocation.
 *
 * Normal (no MCP servers):
 *   ["exec","--json","--ignore-user-config","--skip-git-repo-check",
 *    "-C",cwd,"-m",model,"-s","workspace-write",
 *    ...(outputSchemaPath ? ["--output-schema", outputSchemaPath] : []),
 *    prompt]
 *
 * With MCP servers (spec.mcpServers non-empty):
 *   --ignore-user-config is omitted so that codex reads $CODEX_HOME/config.toml,
 *   which contains the [mcp_servers.*] blocks written by buildConfigToml(). The
 *   CODEX_HOME is already a clean isolated directory (see codex-home.ts), so
 *   dropping --ignore-user-config does not leak the user's ~/.codex config.
 *
 * Resume (resumeThreadId provided):
 *   ["exec","resume",resumeThreadId, ...flags]
 *
 * The prompt is composed via buildPrompt(spec, opts.skills ?? []). When
 * opts.skills is provided (pre-resolved SkillBody objects from index.ts),
 * they are inlined into the prompt. Omitting opts.skills produces the same
 * result as passing an empty array (no skill sections in the prompt).
 */
export function buildCodexArgs(
  spec: CompiledSpec,
  opts: { cwd: string; outputSchemaPath?: string; resumeThreadId?: string; skills?: SkillBody[] },
): string[] {
  const prompt = buildPrompt(spec, opts.skills ?? []);
  const hasMcpServers = (spec.mcpServers ?? []).length > 0;

  const flags: string[] = [
    "--json",
    // Omit --ignore-user-config when MCP servers are present: codex must read
    // $CODEX_HOME/config.toml (the [mcp_servers.*] blocks we wrote). The clean
    // CODEX_HOME already provides isolation from the user's ~/.codex config.
    ...(!hasMcpServers ? ["--ignore-user-config"] : []),
    "--skip-git-repo-check",
    "-C", opts.cwd,
    "-m", spec.model.name,
    "-s", "workspace-write",
    ...(opts.outputSchemaPath ? ["--output-schema", opts.outputSchemaPath] : []),
    prompt,
  ];

  if (opts.resumeThreadId !== undefined) {
    return ["exec", "resume", opts.resumeThreadId, ...flags];
  }

  return ["exec", ...flags];
}
