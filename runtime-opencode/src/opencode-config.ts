/**
 * Map a CompiledSpec to opencode's `opencode.json` agent-config shape.
 *
 * Phase 2 slice 2.2 (this file): a pure function that builds the agent
 * config opencode expects. No SDK invocation, no file I/O — the caller
 * (slice 2.3+ will be runtime-opencode/src/index.ts after the SDK is
 * wired in) is responsible for writing the JSON to disk and pointing
 * opencode at it.
 *
 * Source of truth for the opencode config shape:
 *   https://opencode.ai/docs/config/
 *   https://opencode.ai/docs/agents/
 *
 * Key differences between this adapter's mapping and the Pi adapter:
 *
 *   - Pi extension-format tools (anything declared in spec.tools[] with
 *     `entrypoint` but not `builtin`) are NOT translatable to opencode.
 *     Opencode loads its own native tools and MCP-discovered tools;
 *     it does not execute Pi-format extension JS modules. We REJECT
 *     specs that declare custom Pi-extension tools when targeting
 *     opencode. This matches the harness-matrix ❌ cell — "rejected at
 *     compile time with a field-naming error."
 *
 *   - opencode's tool registry is `permissions: { <tool>: "allow"|"ask"|"deny" }`.
 *     The old top-level `tools: {...}` field is deprecated. We emit the
 *     new permission shape.
 *
 *   - opencode's model id is `<provider>/<modelId>`. Pi's getModel takes
 *     (provider, name) separately. We concatenate.
 *
 *   - opencode's `prompt` field accepts either an inline string or a
 *     path to a prompt file. Inline is simpler and avoids a transient
 *     file-write; we always inline. The honesty preamble + persona
 *     instructions + skill bodies (when slice 2.3 lands) are joined
 *     with double newlines into the single prompt string.
 *
 * What slice 2.2 does NOT yet do (deferred to 2.3+):
 *   - Skill body inlining (the v0.1.7 "active by default" lesson) —
 *     skills require fs reads and we keep this slice pure.
 *   - MCP server config — opencode has native MCP; mapping is small but
 *     belongs in slice 2.3.
 *   - Subagents — opencode has its own subagent format; slice 2.3.
 *   - Extensions with `source: npm:...` — handled by opencode's package
 *     install path or rejected; slice 2.3 design decision.
 */
import type { CompiledSpec, MCPServer, ResolvedRef } from "./types.js";
import { HONESTY_PREAMBLE, wrapSkillBody } from "./honesty.js";

/**
 * Pre-resolved skill body, ready to be inlined into the system prompt.
 * The caller (index.ts) reads spec.skills[].entrypoint from disk, strips
 * YAML frontmatter, and passes the resulting bodies in.
 *
 * Kept out of buildOpencodeConfig so that function remains testable without
 * fs fixtures — same I/O separation Pi adapter uses.
 */
export interface SkillBody {
  name: string;
  body: string;
}

/**
 * Pre-resolved subagent definition, ready to be added to opencode's
 * `cfg.agent` map. The caller reads spec.subagents[].entrypoint (a .md file
 * with YAML frontmatter), parses it, and passes the structured fields in.
 *
 * Field semantics mirror Pi's AgentConfig (see extensions/subagent/agents.ts):
 *   - name: agent identifier
 *   - description: when the model should invoke this subagent
 *   - tools: optional Pi built-in tool names the subagent may use; mapped to
 *     opencode permission keys the same way the primary agent's tools are
 *   - model: optional override of provider/name; falls back to primary model
 *   - systemPrompt: the subagent's system prompt body (markdown after the
 *     frontmatter)
 */
export interface SubagentDefinition {
  name: string;
  description: string;
  tools?: string[];
  model?: string;
  systemPrompt: string;
}

/**
 * The subset of opencode's agent config shape we currently target.
 * Intentionally narrow — opencode supports many more knobs (temperature,
 * mode, model rules, etc.) but we only emit what ADL drives today. Add
 * fields here as the corresponding ADL surface is mapped.
 *
 * The string keys are opencode's; do NOT rename without checking the
 * opencode docs and the SDK's TypeScript types (slice 2.3 will pull
 * those in).
 */
export interface OpencodeAgentConfig {
  description: string;
  model: string;
  prompt: string;
  temperature?: number;
  /**
   * Subagent mode: opencode treats agents with mode="subagent" as
   * delegate-only — they can be invoked by the primary agent via the
   * task tool but won't appear as a top-level chat interface. The
   * primary agent (the one named in spec.metadata.name) omits this
   * field so opencode treats it as mode="all" by default. Codex
   * pass guidance: opencode supports "subagent" | "primary" | "all".
   */
  mode?: "subagent" | "primary" | "all";
  permission?: Record<string, "allow" | "ask" | "deny">;
  /**
   * Disable this agent entry entirely so opencode skips it. Used to
   * mask out opencode's native agents (general/explore/plan/build)
   * so they cannot be invoked via task tool. Codex pass 4 of slice 2.5.
   */
  disable?: boolean;
}

/** opencode MCP server config — local stdio. */
export interface OpencodeMcpLocal {
  type: "local";
  command: string[];
  environment?: Record<string, string>;
  enabled?: boolean;
}

/** opencode MCP server config — remote HTTP/SSE. */
export interface OpencodeMcpRemote {
  type: "remote";
  url: string;
  headers?: Record<string, string>;
  enabled?: boolean;
}

export type OpencodeMcpConfig = OpencodeMcpLocal | OpencodeMcpRemote;

export interface OpencodeRootConfig {
  $schema: string;
  agent: Record<string, OpencodeAgentConfig>;
  /**
   * MCP (Model Context Protocol) servers. Each key is the server name and
   * the value is a stdio or remote transport config. Tools the MCP server
   * exposes appear in opencode under permission keys of the form
   * `<server>_<tool>`; ADL's allowlist already denies "*" so these are
   * inactive unless the spec explicitly grants `mcp_servers` access.
   * Optional — omitted when spec.mcpServers is absent or empty.
   */
  mcp?: Record<string, OpencodeMcpConfig>;
  /**
   * Experimental / opt-in opencode features. Slice 5.4 only sets the
   * `openTelemetry` flag (turns on AI-SDK telemetry spans for LLM and
   * tool calls inside the opencode child); other fields are passed
   * through unchanged or left absent.
   *
   * opencode reads OTEL_EXPORTER_OTLP_ENDPOINT / TRACEPARENT from env
   * for span destination + parent context — the runtime-opencode adapter
   * already inherits both via slice 5.2 env injection, and the
   * @opencode-ai/sdk's createOpencodeServer spreads process.env into the
   * opencode child, so no extra wiring is needed beyond flipping this flag.
   */
  experimental?: {
    openTelemetry?: boolean;
  };
}

/**
 * Map Pi built-in tool names to the opencode permission key that
 * controls them. The mapping is NOT identity:
 *
 *   bash  → bash       (1:1)
 *   read  → read       (1:1)
 *   edit  → edit       (1:1, controls opencode's `edit` and `apply_patch`)
 *   write → edit       (opencode does not expose a separate `write`
 *                        permission; file creation/overwrite is also
 *                        gated by `edit`. Codex pass 1 of slice 2.2
 *                        caught the earlier 1:1 mapping, which would
 *                        have left write-only specs unable to modify
 *                        any files.)
 *
 * If a spec declares BOTH `edit` and `write` (a common combination),
 * both map to "edit: allow" — the duplicate is idempotent and
 * harmless. If opencode later adds a distinct `write` permission this
 * table will need to revisit the mapping.
 *
 * Source: https://opencode.ai/docs/config/ — only read, edit, glob,
 * grep, list, bash, task, external_directory, lsp, and skill accept
 * the shorthand-or-pattern form; `write` is not on opencode's
 * permission key list.
 */
/**
 * Maps Pi built-in tool names to the opencode AgentConfig.permission key
 * that controls them. Only keys in OPENCODE_PERMISSION_KEYS_SUPPORTED are
 * actionable; others compile but are silently ignored.
 *
 * Notable gaps:
 *   read → no supported opencode permission key (read is always available
 *          in opencode; it cannot be denied via the permission config).
 *          Omitted intentionally; declaring it in spec.tools[] adds it to
 *          the grant list but has no effect on opencode's actual behavior.
 *   write → edit (opencode gates file-write via the `edit` permission).
 */
/**
 * Maps Pi built-in tool names to the opencode permission key.
 * Per opencode.ai/docs config reference (and confirmed by codex pass 15
 * of slice 2.4), `read` IS a supported permission key. Earlier codex
 * passes had flagged it as unsupported based on the SDK types alone,
 * but the runtime accepts it via the AgentConfig index signature.
 */
const PI_TO_OPENCODE_PERMISSION_KEY: Record<string, string> = {
  // Pi built-in names (the four ADL currently recognizes for primary agents).
  bash: "bash",
  read: "read",
  edit: "edit",
  write: "edit",
  // Additional names that appear in Pi subagent frontmatter (e.g.
  // `tools: read, grep, find, ls`). These aren't ADL primary-agent built-
  // ins, but subagent .md files are parsed independently and may list any
  // tool Pi's registry exposes. Map each to its opencode equivalent so
  // the subagent permission map is correct. Codex pass 2 of slice 2.5 caught.
  grep: "grep",
  find: "glob",
  ls: "list",
  // Accept opencode-native names too so frontmatter authors targeting
  // opencode directly don't have to translate.
  glob: "glob",
  list: "list",
  webfetch: "webfetch",
  websearch: "websearch",
  task: "task",
};

/**
 * Build an opencode root config from a CompiledSpec. Pure — does no
 * file I/O and reads no env. Throws on inputs that cannot be safely
 * mapped (e.g. custom Pi-extension tools targeting opencode), with an
 * error message that names the unsupported field.
 *
 * `agentName` defaults to spec.metadata.name; callers can override
 * (e.g. for hermetic test fixtures) when needed.
 */
export function buildOpencodeConfig(
  spec: CompiledSpec,
  options: {
    agentName?: string;
    /**
     * Pre-resolved skill bodies, in declaration order. Each body is
     * appended to the system prompt wrapped via wrapSkillBody() so the
     * model can see what skill it's reading.
     *
     * Caller (index.ts) is responsible for reading spec.skills[].entrypoint
     * (SKILL.md files) and stripping YAML frontmatter before passing here.
     */
    skillBodies?: SkillBody[];
    /**
     * Pre-resolved subagent definitions. Each becomes an entry in
     * opencode's `cfg.agent` map with `mode: "subagent"`.
     */
    subagentDefinitions?: SubagentDefinition[];
  } = {},
): OpencodeRootConfig {
  rejectUnsupportedTools(spec);

  const agentName = options.agentName ?? spec.metadata.name;
  const prompt = buildPromptString(spec, options.skillBodies ?? []);
  const subagentDefs = options.subagentDefinitions ?? [];
  const mcpServers = spec.mcpServers ?? [];
  const permissions = buildPermissions(spec.tools ?? [], {
    hasSubagents: subagentDefs.length > 0,
    mcpServerNames: mcpServers.map((s) => s.name),
  });

  const agent: OpencodeAgentConfig = {
    description:
      spec.metadata.description ??
      `Agent ${spec.metadata.name} (compiled from ADL by agent-controller).`,
    model: `${spec.model.provider}/${spec.model.name}`,
    prompt,
    ...(spec.model.temperature !== undefined ? { temperature: spec.model.temperature } : {}),
    permission: permissions,
  };

  // Build the agent map: primary agent first, then any subagents.
  // Null-prototype object so user-supplied names like "constructor" or
  // "__proto__" don't collide with Object.prototype. Codex pass 6 of slice 2.5.
  const agentMap: Record<string, OpencodeAgentConfig> = Object.create(null);
  agentMap[agentName] = agent;
  for (const sa of options.subagentDefinitions ?? []) {
    if (sa.name === agentName) {
      throw new Error(
        `runtime-opencode: subagent name "${sa.name}" collides with the primary ` +
        `agent name. Rename the subagent or the spec metadata.name.`,
      );
    }
    if ((NATIVE_OPENCODE_AGENTS as readonly string[]).includes(sa.name)) {
      throw new Error(
        `runtime-opencode: subagent name "${sa.name}" collides with an opencode ` +
        `native agent. The adapter disables these natives to preserve ADL's ` +
        `allowlist contract. Rename the subagent. ` +
        `(Reserved names: ${NATIVE_OPENCODE_AGENTS.join(", ")}.)`,
      );
    }
    if (Object.hasOwn(agentMap, sa.name)) {
      throw new Error(
        `runtime-opencode: duplicate subagent name "${sa.name}" in spec.subagents[].`,
      );
    }
    agentMap[sa.name] = buildSubagentConfig(sa, spec);
  }
  // Disable opencode's native agents so they cannot be delegated to via
  // task tool. SKIP any native whose name collides with the primary agent
  // (codex pass 5 of slice 2.5: the ADL schema allows metadata.name = "plan"
  // / "build" / "general" / "explore", and the primary agent must remain
  // executable in that case). Subagent names colliding with natives are
  // already rejected in the loop above, so the only overlap to defend
  // against here is the primary agent name.
  for (const nativeName of NATIVE_OPENCODE_AGENTS) {
    if (nativeName === agentName) continue;
    agentMap[nativeName] = {
      description: "(disabled by agent-controller)",
      model: `${spec.model.provider}/${spec.model.name}`,
      prompt: "(disabled)",
      disable: true,
    };
  }

  const root: OpencodeRootConfig = {
    $schema: "https://opencode.ai/config.json",
    agent: agentMap,
  };

  // MCP servers (optional). Omit the field entirely when the spec declares
  // none — keeps the config minimal and matches opencode's "absent ≠ empty"
  // expectation for top-level optional sections.
  const mcp = buildMcpConfig(spec.mcpServers ?? []);
  if (Object.keys(mcp).length > 0) root.mcp = mcp;

  // Slice 5.4: flip opencode's experimental.openTelemetry flag when the
  // spec opts into tracing. opencode emits AI-SDK telemetry spans inside
  // its own child process; for those spans to actually ship, the
  // operator must also set OTEL_EXPORTER_OTLP_ENDPOINT in the parent
  // env (which the SDK propagates into the opencode child). Setting
  // this flag without a configured collector is a no-op on opencode's
  // side, which is why we don't gate it on the endpoint env var here
  // — keeping the flag conditional only on the spec opt-in.
  if (spec.observability?.tracing === true) {
    root.experimental = { openTelemetry: true };
  }

  return root;
}

/**
 * Map a pre-resolved SubagentDefinition into opencode's AgentConfig shape.
 * The subagent inherits the primary spec's model when its own model is
 * not specified — keeping behavior predictable when the spec author
 * doesn't pin a model per subagent.
 */
function buildSubagentConfig(sa: SubagentDefinition, spec: CompiledSpec): OpencodeAgentConfig {
  // Subagent tools are a subset of Pi built-ins — map them through the same
  // PI_TO_OPENCODE_PERMISSION_KEY table the primary agent uses. Anything not
  // in the table is an unknown built-in and we throw (caller's responsibility
  // to validate the .md file before passing in).
  const grants: Record<string, "allow" | "ask" | "deny"> = { "*": "deny" };
  for (const key of OPENCODE_PERMISSION_KEYS_DENY_BASELINE) {
    grants[key] = "deny";
  }
  for (const toolName of sa.tools ?? []) {
    if (!(toolName in PI_TO_OPENCODE_PERMISSION_KEY)) {
      throw new Error(
        `runtime-opencode: subagent "${sa.name}" declares tool ${JSON.stringify(toolName)} ` +
        `which is not a known Pi built-in. Allowed: ${Object.keys(PI_TO_OPENCODE_PERMISSION_KEY).join(", ")}.`,
      );
    }
    grants[PI_TO_OPENCODE_PERMISSION_KEY[toolName]] = "allow";
  }

  // Normalize the subagent's model to opencode's `provider/model` format.
  // Pi convention often writes just the bare model id (`claude-sonnet-4-...`),
  // which opencode cannot resolve. If the subagent's model contains a `/`
  // it's already fully qualified; otherwise prepend the primary spec's
  // provider. Codex pass 1 of slice 2.5 caught the missing normalization.
  const subagentModel = sa.model
    ? (sa.model.includes("/") ? sa.model : `${spec.model.provider}/${sa.model}`)
    : `${spec.model.provider}/${spec.model.name}`;

  return {
    description: sa.description,
    model: subagentModel,
    prompt: sa.systemPrompt,
    mode: "subagent",
    permission: grants,
  };
}

/**
 * Reject MCP server names whose `<name>_*` allow pattern would override the
 * deny baseline for an opencode built-in. opencode matches permission keys
 * as wildcard patterns, so a server named "repo" would have `repo_*: "allow"`
 * which also matches the built-in `repo_clone` / `repo_overview` keys —
 * effectively granting them despite the baseline deny.
 *
 * Built into the deny baseline today: doom_loop, external_directory,
 * repo_clone, repo_overview. The forbidden MCP-name prefixes are therefore:
 * doom, external, repo. The check is derived from the baseline (not
 * hardcoded) so it stays accurate if the baseline grows.
 *
 * Codex pass 3 of slice 2.5 caught this allowlist-broadening bug.
 */
function rejectCollidingMcpName(name: string): void {
  // We emit both raw `<name>_*` and sanitized `<sanitized>_*` allow patterns
  // (because opencode's actual tool-id format isn't fully determined). Check
  // both prefixes against the built-in deny baseline so neither pattern can
  // accidentally re-grant a built-in.
  const sanitized = sanitizeMcpName(name);
  const candidatePrefixes = sanitized === name ? [`${name}_`] : [`${name}_`, `${sanitized}_`];
  for (const builtinKey of OPENCODE_PERMISSION_KEYS_DENY_BASELINE) {
    for (const candidatePrefix of candidatePrefixes) {
      if (builtinKey.startsWith(candidatePrefix)) {
        throw new Error(
          `runtime-opencode: MCP server name "${name}" collides with the opencode ` +
          `built-in permission key "${builtinKey}". The implicit "${candidatePrefix}*" allow ` +
          `grant for this server's tools would also match the built-in, breaking the ` +
          `ADL deny-by-default contract. Rename the MCP server in spec.mcpServers[].`,
        );
      }
    }
  }
}

/**
 * Mirror opencode's MCP-tool-id sanitization rule: replace every character
 * outside [A-Za-z0-9] with `_`. opencode generates tool ids of the form
 * `<sanitized-server-name>_<tool-name>`; our permission keys must use the
 * same sanitization to match. Codex pass 4 of slice 2.5 caught the
 * mismatch for server names like "github.com".
 */
function sanitizeMcpName(name: string): string {
  return name.replace(/[^A-Za-z0-9]/g, "_");
}

/**
 * Reject MCP server names that contain characters which would create
 * overmatching wildcard patterns in opencode's permission grants. We emit
 * the raw name as `<name>_*` in the permission map; if the name contains
 * glob metacharacters (`*`, `?`, `[`, `]`, `{`, `}`), the resulting pattern
 * could match arbitrary opencode built-ins (e.g. `repo*_*` matches `repo_clone`)
 * and re-enable tools outside the ADL allowlist. Codex pass 8 of slice 2.5.
 *
 * Allowed character set is the same one MCP servers conventionally use for
 * identifiers: alphanumerics plus `.`, `-`, `_`. Anything else throws.
 */
function rejectUnsafeMcpNameChars(name: string): void {
  if (!/^[A-Za-z0-9._-]+$/.test(name)) {
    throw new Error(
      `runtime-opencode: MCP server name "${name}" contains characters outside ` +
      `the allowed set [A-Za-z0-9._-]. Glob metacharacters (* ? [ ]) and other ` +
      `non-identifier characters could create overmatching opencode permission ` +
      `wildcards that re-enable built-in tools outside the ADL allowlist. ` +
      `Rename the MCP server in spec.mcpServers[].`,
    );
  }
}

/**
 * Map ADL spec.mcpServers[] to opencode's cfg.mcp shape.
 *
 * Transport mapping:
 *   - "stdio"             → { type: "local", command: [command, ...args], environment: env }
 *   - "streamable-http"   → { type: "remote", url, headers }
 *   - "sse"               → { type: "remote", url, headers }
 *
 * opencode does not distinguish streamable-http from sse — both become
 * type="remote". The transport-specific behavior is negotiated by the
 * remote endpoint at connect time.
 *
 * lifecycle (eager/lazy) has no direct opencode equivalent; we set
 * `enabled: true` for both ("eager" matches; "lazy" is approximated since
 * opencode lazily fetches tools anyway).
 */
function buildMcpConfig(servers: MCPServer[]): Record<string, OpencodeMcpConfig> {
  // Null-prototype object: ADL only constrains MCP names to non-empty strings,
  // so names like "constructor", "toString", or "__proto__" must work as plain
  // map keys without colliding with Object.prototype or mutating the prototype
  // chain. Codex pass 6 of slice 2.5.
  const out: Record<string, OpencodeMcpConfig> = Object.create(null);
  const sanitizedNames = new Map<string, string>(); // sanitized → raw
  for (const s of servers) {
    rejectUnsafeMcpNameChars(s.name);
    if (Object.hasOwn(out, s.name)) {
      throw new Error(
        `runtime-opencode: duplicate MCP server name "${s.name}" in spec.mcpServers[].`,
      );
    }
    const sanitized = sanitizeMcpName(s.name);
    const existing = sanitizedNames.get(sanitized);
    if (existing !== undefined && existing !== s.name) {
      throw new Error(
        `runtime-opencode: MCP server names "${existing}" and "${s.name}" both ` +
        `sanitize to "${sanitized}". opencode generates tool ids under the sanitized ` +
        `name, so the two servers' tools would collide in the same namespace. ` +
        `Rename one in spec.mcpServers[].`,
      );
    }
    sanitizedNames.set(sanitized, s.name);
    rejectCollidingMcpName(s.name);
    if (s.transport === "stdio") {
      if (!s.command) {
        throw new Error(
          `runtime-opencode: MCP server "${s.name}" uses transport "stdio" but has no command field.`,
        );
      }
      const local: OpencodeMcpLocal = {
        type: "local",
        command: [s.command, ...(s.args ?? [])],
      };
      if (s.env && Object.keys(s.env).length > 0) local.environment = s.env;
      out[s.name] = local;
    } else if (s.transport === "streamable-http" || s.transport === "sse") {
      if (!s.url) {
        throw new Error(
          `runtime-opencode: MCP server "${s.name}" uses transport "${s.transport}" but has no url field.`,
        );
      }
      const remote: OpencodeMcpRemote = {
        type: "remote",
        url: s.url,
      };
      if (s.headers && Object.keys(s.headers).length > 0) remote.headers = s.headers;
      out[s.name] = remote;
    } else {
      throw new Error(
        `runtime-opencode: MCP server "${s.name}" has unknown transport "${s.transport}". ` +
        `Expected one of: stdio, streamable-http, sse.`,
      );
    }
  }
  return out;
}

/**
 * Reject specs that declare custom Pi-extension tools when targeting
 * opencode. The check is: any spec.tools[] entry that has `entrypoint`
 * set and `builtin` unset is a Pi-extension tool. opencode cannot load
 * those, so we fail with a clear error that names every offending tool.
 *
 * spec.extensions[] is intentionally not validated here — slice 2.3
 * will design extension handling (likely also a reject for opencode,
 * since extension JS modules are Pi-specific). For now we silently
 * pass extensions through; the prompt construction ignores them.
 */
function rejectUnsupportedTools(spec: CompiledSpec): void {
  const offenders: string[] = [];
  for (const t of spec.tools ?? []) {
    if (isCustomPiExtensionTool(t)) {
      offenders.push(t.name);
    }
  }
  if (offenders.length > 0) {
    throw new Error(
      `runtime-opencode: spec.tools[] contains custom Pi-extension tools ` +
      `that cannot run on opencode: ${offenders.join(", ")}. ` +
      `Only Pi built-in tools (bash, read, edit, write) are supported on ` +
      `the opencode adapter today. See docs/architecture/harness-matrix.md ` +
      `for the supported-feature table; custom Pi extensions are documented ` +
      `as ❌ for opencode.`,
    );
  }
}

function isCustomPiExtensionTool(t: ResolvedRef): boolean {
  // A Pi built-in declared in spec.tools[] has builtin=true and no
  // entrypoint. A custom Pi-extension tool has entrypoint set and
  // builtin not set. Anything else (e.g. entrypoint set + builtin
  // true) shouldn't happen given the compiler, but we treat as not-
  // custom to err on the permissive side rather than reject a
  // valid-looking spec.
  return Boolean(t.entrypoint) && !t.builtin;
}

/**
 * Build the prompt string opencode uses as the agent's system prompt.
 *
 * Composition (in order):
 *   1. HONESTY_PREAMBLE — same anti-hallucination preamble as Pi
 *      adapter. Even though opencode is also susceptible to
 *      <invoke>/<function_calls> XML in message bodies, having the
 *      preamble at session start lowers the rate. Slice 2.4 will add
 *      the runtime XML detector to catch what slips through.
 *   2. Persona role and instructions (when present), formatted to
 *      match Pi adapter's buildSystemPrompt() so spec authors don't
 *      have to mentally branch on which adapter they're targeting.
 *
 * Skill bodies will be appended here in slice 2.3 once we add fs
 * reads to the mapping pipeline. For now this slice is pure.
 */
function buildPromptString(spec: CompiledSpec, skillBodies: SkillBody[]): string {
  const parts: string[] = [HONESTY_PREAMBLE];
  if (spec.persona?.role) parts.push(`# Role\n${spec.persona.role}`);
  if (spec.persona?.instructions) parts.push(`# Instructions\n${spec.persona.instructions}`);
  // Skill bodies (when present). Wrapped with the same "skill may describe
  // tools you lack" preamble Pi adapter uses via wrapSkillBody(). This is
  // the v0.1.7 "active by default" pattern: opencode doesn't have a skills
  // concept, so we inline the bodies so the model unconditionally sees them.
  for (const s of skillBodies) {
    if (!s.body.trim()) continue;
    parts.push(wrapSkillBody(s.name, s.body.trim()));
  }
  return parts.join("\n\n");
}

/**
 * All opencode permission keys to set in the deny baseline.
 *
 * This list combines:
 *   (a) Keys typed in AgentConfig.permission in @opencode-ai/sdk:
 *       edit, bash, webfetch, doom_loop, external_directory
 *   (b) Keys documented in opencode.ai/docs config reference that the SDK
 *       types do not yet expose but the runtime accepts via AgentConfig's
 *       index signature `[key: string]: unknown`:
 *       read, glob, grep, list, task, skill, lsp, question, websearch
 *   (c) A wildcard "*": "deny" baseline as a belt-and-suspenders catch-all.
 *       Codex pass 11 raised that this was a no-op; codex pass 15 said the
 *       opencode runtime does support it per the docs config reference.
 *       We include it because if it becomes effective in a future opencode
 *       release, having it present is exactly what we want; if it's still
 *       a no-op today, the explicit key denials above cover the known tools.
 *
 * When the opencode SDK types are updated to expose more keys, remove those
 * keys from the undocumented-but-runtime-supported list and keep them here.
 */
/**
 * opencode ships these named agents as natives. They are present in
 * cfg.agent by default (see AgentConfig types in the SDK). If we don't
 * explicitly disable them, a spec that grants `task: allow` (to invoke its
 * own declared subagents) can also delegate to these natives — which carry
 * their own tool defaults, bypassing the ADL allowlist contract.
 * Codex pass 4 of slice 2.5 caught this bypass.
 *
 * We disable all four unconditionally. Specs that want to use them can
 * re-export them via spec.subagents[] with explicit grants.
 */
const NATIVE_OPENCODE_AGENTS: ReadonlyArray<string> = [
  "plan",
  "build",
  "general",
  "explore",
  // Experimental native agent enabled by OPENCODE_EXPERIMENTAL_SCOUT. Listed
  // here defensively so the allowlist contract holds even when the operator
  // has scout enabled at the opencode level. Codex pass 6 of slice 2.5.
  "scout",
];

const OPENCODE_PERMISSION_KEYS_DENY_BASELINE: ReadonlyArray<string> = [
  // Typed in AgentConfig.permission (definite SDK support)
  "edit",
  "bash",
  "webfetch",
  "doom_loop",
  "external_directory",
  // Documented in opencode.ai/docs (runtime support, not yet in SDK types)
  "read",
  "glob",
  "grep",
  "list",
  "task",
  "skill",
  "lsp",
  "question",
  "websearch",
  // Additional keys present in the bundled SDK v2 types (codex pass 26).
  "todowrite",
  "repo_clone",
  "repo_overview",
  // Wildcard catch-all
  "*",
];

/**
 * Build the permissions map for opencode from spec.tools[].
 *
 * Implements ADL's allowlist semantic: start with explicit "deny" for
 * every known opencode tool, then overlay "allow" for the ones the spec
 * declares. This ensures that a `tools: []` spec actually runs with no
 * tools, matching Pi adapter behavior.
 *
 * Always returns a non-empty object (at minimum all-deny). We omit the
 * `undefined` return to make the caller simpler and the intent explicit.
 */
function buildPermissions(
  tools: ResolvedRef[],
  context: { hasSubagents: boolean; mcpServerNames: string[] } = { hasSubagents: false, mcpServerNames: [] },
): Record<string, "allow" | "ask" | "deny"> {
  // Start with the wildcard catch-all FIRST, then enumerate specific denies.
  // opencode uses last-match-wins semantics for permission rules, so if "*":
  // "deny" is serialized AFTER specific allows, it overrides them and renders
  // all explicitly granted tools unusable.
  const grants: Record<string, "allow" | "ask" | "deny"> = { "*": "deny" };
  for (const key of OPENCODE_PERMISSION_KEYS_DENY_BASELINE) {
    grants[key] = "deny";
  }

  // Overlay "allow" for Pi built-ins the spec explicitly declared.
  for (const t of tools) {
    if (!t.builtin) continue;
    if (!(t.name in PI_TO_OPENCODE_PERMISSION_KEY)) {
      throw new Error(
        `runtime-opencode: Pi built-in tool ${JSON.stringify(t.name)} is not in ` +
        `PI_TO_OPENCODE_PERMISSION_KEY. Add an entry.`,
      );
    }
    if (t.config && Object.keys(t.config).length > 0) {
      // As of v0.1.11 the canonical rejection for built-in-with-config lives
      // in the Go compiler (cli/internal/adl/compiler.go) so `agentctl
      // validate`/`compile` catches the bug before any adapter starts. We
      // keep this runtime check as defense-in-depth: if a future schema
      // change or a path that bypasses the compiler injects a built-in
      // with config, the opencode adapter still refuses to start with a
      // clear error. The Pi adapter relies entirely on the compile-time
      // rejection (no equivalent runtime check today).
      throw new Error(
        `runtime-opencode: spec.tools declares built-in "${t.name}" with config ` +
        `(${JSON.stringify(Object.keys(t.config))}). This should have been rejected at ` +
        `compile time by the Go CLI; reaching this code path means either an outdated ` +
        `CLI binary or a hand-crafted CompiledSpec. For bash command allowlisting, use ` +
        `the @gotgenes/pi-permission-system extension via spec.extensions[].source: ` +
        `npm:@gotgenes/pi-permission-system.`,
      );
    }
    const opencodeKey = PI_TO_OPENCODE_PERMISSION_KEY[t.name];
    grants[opencodeKey] = "allow";
  }

  // Slice 2.5 codex pass 1: when the spec declares subagents, the primary
  // agent needs the `task` permission to invoke them via opencode's task
  // tool. Without this, opencode connects all child agents but the parent
  // is denied access to delegate. Declaring subagents IS the implicit grant.
  if (context.hasSubagents) {
    grants["task"] = "allow";
  }

  // Slice 2.5: each MCP server's tools appear in opencode under permission
  // keys of the form `<server-prefix>_<tool>`. Codex passes 4 and 7 of this
  // slice gave contradictory accounts of whether opencode uses the raw name
  // (e.g. `my-mcp_*`) or a sanitized form (e.g. `my_mcp_*`). Rather than guess
  // and risk leaving the allow grant ineffective for one interpretation,
  // emit BOTH patterns. For alphanumeric-only names the two collapse to the
  // same key; for names with punctuation, whichever form opencode actually
  // generates is covered. A non-matching pattern is a harmless no-op.
  for (const serverName of context.mcpServerNames) {
    grants[`${serverName}_*`] = "allow";
    const sanitized = sanitizeMcpName(serverName);
    if (sanitized !== serverName) {
      grants[`${sanitized}_*`] = "allow";
    }
  }

  return grants;
}
