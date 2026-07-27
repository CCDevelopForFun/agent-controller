import { describe, it, expect } from "vitest";
import {
  assertClaudeCompatible,
  buildPrompt,
  buildOptions,
  buildSubagents,
  deriveSdkSessionUuid,
} from "./claude-invocation.js";
import { HONESTY_PREAMBLE } from "./honesty.js";
import type { CompiledSpec } from "./types.js";

function baseSpec(over: Partial<CompiledSpec> = {}): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "t" },
    model: { provider: "anthropic", name: "claude-opus-4-6" },
    task: "say hi",
    tools: [],
    extensions: [],
    skills: [],
    runtime: { type: "local-claude" },
    ...over,
  };
}

describe("assertClaudeCompatible", () => {
  it("accepts an anthropic spec", () => {
    expect(() => assertClaudeCompatible(baseSpec())).not.toThrow();
  });

  it("rejects provider openai and names the field", () => {
    const s = baseSpec({ model: { provider: "openai", name: "gpt-5.5" } });
    expect(() => assertClaudeCompatible(s)).toThrow(/spec\.model\.provider/);
  });

  it("rejects provider google", () => {
    const s = baseSpec({ model: { provider: "google", name: "gemini" } });
    expect(() => assertClaudeCompatible(s)).toThrow(/anthropic/);
  });

  it("rejects non-empty extensions", () => {
    const s = baseSpec({ extensions: [{ name: "audit-log" }] });
    expect(() => assertClaudeCompatible(s)).toThrow(/spec\.extensions/);
  });

  it("rejects non-empty installs", () => {
    const s = baseSpec({ installs: ["npm:pi-mcp-extension"] });
    expect(() => assertClaudeCompatible(s)).toThrow(/spec\.installs/);
  });

  it("rejects custom Pi-extension tools and names them", () => {
    const s = baseSpec({ tools: [{ name: "get_time", entrypoint: "/abs/x.js" }] });
    expect(() => assertClaudeCompatible(s)).toThrow(/get_time/);
  });

  it("accepts Pi built-in tools", () => {
    const s = baseSpec({ tools: [{ name: "bash", builtin: true }] });
    expect(() => assertClaudeCompatible(s)).not.toThrow();
  });

  it("accepts subagents (natively supported)", () => {
    const s = baseSpec({ subagents: [{ name: "reviewer" }] });
    expect(() => assertClaudeCompatible(s)).not.toThrow();
  });
});

describe("buildPrompt", () => {
  it("starts with the honesty preamble", () => {
    const p = buildPrompt(baseSpec(), []);
    expect(p.startsWith(HONESTY_PREAMBLE)).toBe(true);
  });

  it("includes persona role and instructions in order", () => {
    const s = baseSpec({ persona: { role: "Reviewer", instructions: "Be terse." } });
    const p = buildPrompt(s, []);
    expect(p.indexOf("Reviewer")).toBeLessThan(p.indexOf("Be terse."));
  });

  it("inlines skill bodies", () => {
    const p = buildPrompt(baseSpec(), [{ name: "iso-time", body: "Use ISO-8601." }]);
    expect(p).toContain("Use ISO-8601.");
    expect(p).toContain("iso-time");
  });

  it("omits persona sections when absent", () => {
    const p = buildPrompt(baseSpec(), []);
    expect(p).not.toContain("undefined");
  });

  it("excludes spec.task — task is query()'s prompt argument, not a system-prompt section", () => {
    const s = baseSpec({ task: "SENTINEL_TASK_TEXT_7f3a9c" });
    const p = buildPrompt(s, []);
    expect(p).not.toContain("SENTINEL_TASK_TEXT_7f3a9c");
  });
});

describe("buildOptions", () => {
  it("always sets settingSources to the empty array", () => {
    const o = buildOptions(baseSpec(), "sys", {});
    expect(o.settingSources).toEqual([]);
  });

  it("passes the model name through", () => {
    const o = buildOptions(baseSpec(), "sys", {});
    expect(o.model).toBe("claude-opus-4-6");
  });

  it("maps Pi built-in tools onto SDK tool names", () => {
    const s = baseSpec({
      tools: [
        { name: "bash", builtin: true },
        { name: "read", builtin: true },
        { name: "edit", builtin: true },
        { name: "write", builtin: true },
      ],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.tools).toEqual(["Bash", "Read", "Edit", "Write"]);
    expect(o.allowedTools).toEqual(["Bash", "Read", "Edit", "Write"]);
  });

  // Replaces "omits allowedTools when no tools are declared", which codified
  // the pre-fix behavior: leaving `tools` undefined means "all default Claude
  // Code tools" (sdk.d.ts:1422-1434), so a `tools: []` spec kept a live Bash.
  it("grants no built-in tools for a tools: [] spec", () => {
    const o = buildOptions(baseSpec({ tools: [] }), "sys", {});
    expect(o.tools).toEqual([]);
    expect(o.allowedTools).toEqual([]);
  });

  it("never leaves Options.tools undefined, which would mean the full default toolset", () => {
    const o = buildOptions(baseSpec({ tools: [] }), "sys", {});
    expect(o.tools).toBeDefined();
    expect(o.tools).not.toEqual({ type: "preset", preset: "claude_code" });
  });

  it("grants exactly the declared built-ins and nothing else", () => {
    const s = baseSpec({
      tools: [
        { name: "bash", builtin: true },
        { name: "read", builtin: true },
      ],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.tools).toEqual(["Bash", "Read"]);
    expect(o.tools).not.toContain("Edit");
    expect(o.tools).not.toContain("Write");
    expect(o.allowedTools).not.toContain("Edit");
    expect(o.allowedTools).not.toContain("Write");
  });

  it("throws naming an unmapped Pi built-in instead of silently dropping it", () => {
    const s = baseSpec({ tools: [{ name: "glob", builtin: true }] });
    expect(() => buildOptions(s, "sys", {})).toThrow(/glob/);
    expect(() => buildOptions(s, "sys", {})).toThrow(/BUILTIN_TOOL_MAP/);
  });

  it("grants the Agent delegation tool when subagents are registered", () => {
    const agents = { reviewer: { description: "reviews", prompt: "You review." } };
    const s = baseSpec({ tools: [{ name: "read", builtin: true }] });
    const o = buildOptions(s, "sys", agents);
    expect(o.tools).toEqual(["Read", "Agent"]);
    expect(o.allowedTools).toContain("Agent");
  });

  it("does not grant the Agent delegation tool when no subagents are registered", () => {
    const s = baseSpec({ tools: [{ name: "read", builtin: true }] });
    const o = buildOptions(s, "sys", {});
    expect(o.tools).not.toContain("Agent");
    expect(o.allowedTools).not.toContain("Agent");
  });

  it("auto-approves only the MCP servers the spec declares", () => {
    const s = baseSpec({
      mcpServers: [
        { name: "time", transport: "stdio", command: "npx" },
        { name: "docs", transport: "sse", url: "https://x/sse" },
      ],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.allowedTools).toEqual(["mcp__time", "mcp__docs"]);
    expect(o.allowedTools).not.toContain("mcp__other");
    // MCP tools are not built-ins, so the restriction list stays empty.
    expect(o.tools).toEqual([]);
  });

  it("normalizes MCP server names the SDK would rewrite, so the grant still matches", () => {
    const s = baseSpec({
      mcpServers: [{ name: "my.server", transport: "stdio", command: "npx" }],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.allowedTools).toEqual(["mcp__my_server"]);
  });

  it("throws for an MCP server name that is ambiguous under the mcp__ rule grammar", () => {
    const s = baseSpec({
      mcpServers: [{ name: "a__b", transport: "stdio", command: "npx" }],
    });
    expect(() => buildOptions(s, "sys", {})).toThrow(/spec\.mcpServers\[\]\.name/);
  });

  it("maps stdio MCP servers", () => {
    const s = baseSpec({
      mcpServers: [
        { name: "time", transport: "stdio", command: "npx", args: ["-y", "srv"], env: { A: "1" } },
      ],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.mcpServers!.time).toEqual({
      type: "stdio",
      command: "npx",
      args: ["-y", "srv"],
      env: { A: "1" },
    });
  });

  it("maps streamable-http onto the SDK http transport with headers", () => {
    const s = baseSpec({
      mcpServers: [
        { name: "remote", transport: "streamable-http", url: "https://x/mcp", headers: { H: "v" } },
      ],
    });
    const o = buildOptions(s, "sys", {});
    expect(o.mcpServers!.remote).toEqual({
      type: "http",
      url: "https://x/mcp",
      headers: { H: "v" },
    });
  });

  it("maps sse onto the SDK sse transport", () => {
    const s = baseSpec({ mcpServers: [{ name: "s", transport: "sse", url: "https://x/sse" }] });
    const o = buildOptions(s, "sys", {});
    expect(o.mcpServers!.s).toEqual({ type: "sse", url: "https://x/sse", headers: undefined });
  });

  it("throws a descriptive error for a stdio server with no command", () => {
    const s = baseSpec({ mcpServers: [{ name: "broken", transport: "stdio" }] });
    expect(() => buildOptions(s, "sys", {})).toThrow(/broken/);
    expect(() => buildOptions(s, "sys", {})).toThrow(/command/);
  });

  it("throws a descriptive error for a streamable-http server with no url", () => {
    const s = baseSpec({ mcpServers: [{ name: "remote", transport: "streamable-http" }] });
    expect(() => buildOptions(s, "sys", {})).toThrow(/remote/);
    expect(() => buildOptions(s, "sys", {})).toThrow(/url/);
  });

  // The agentctl session id is NOT an SDK session id: `agentctl serve` mints
  // `s_<base36ms><8hex>` and `agentctl chat` mints `s_<hex>`, while
  // Options.sessionId "Must be a valid UUID" and Options.resume expects an id
  // the SDK issued. Passing the raw id to `resume` failed on turn 1 of every
  // serve/chat session.
  it("sets a derived UUID sessionId on the first turn, never the raw agentctl id", () => {
    const o = buildOptions(baseSpec({ sessionId: "s_mfaq1x2y3z4a5b6c" }), "sys", {});
    expect(o.sessionId).toBe(deriveSdkSessionUuid("s_mfaq1x2y3z4a5b6c"));
    expect(o.sessionId).not.toBe("s_mfaq1x2y3z4a5b6c");
    expect(o.resume).toBeUndefined();
  });

  it("resumes the SDK session id captured on a previous turn", () => {
    const prior = "11111111-2222-5333-8444-555555555555";
    const o = buildOptions(baseSpec({ sessionId: "s_abc" }), "sys", {}, prior);
    expect(o.resume).toBe(prior);
    // The SDK forbids sessionId and resume together (sdk.d.ts:1804-1809).
    expect(o.sessionId).toBeUndefined();
  });

  it("sets neither sessionId nor resume for a one-shot run", () => {
    const o = buildOptions(baseSpec(), "sys", {});
    expect(o.resume).toBeUndefined();
    expect(o.sessionId).toBeUndefined();
  });

  it("rejects a blank sessionId and names the field", () => {
    expect(() => buildOptions(baseSpec({ sessionId: "   " }), "sys", {})).toThrow(
      /spec\.sessionId/,
    );
  });

  it("registers subagents when provided", () => {
    const agents = { reviewer: { description: "reviews", prompt: "You review." } };
    const o = buildOptions(baseSpec(), "sys", agents);
    expect(o.agents).toEqual(agents);
  });

  it("omits agents when none are declared", () => {
    expect(buildOptions(baseSpec(), "sys", {}).agents).toBeUndefined();
  });
});

describe("deriveSdkSessionUuid", () => {
  const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

  it("produces a syntactically valid v5 UUID", () => {
    expect(deriveSdkSessionUuid("s_mfaq1x2y3z4a5b6c")).toMatch(UUID_RE);
    expect(deriveSdkSessionUuid("s_deadbeef")).toMatch(UUID_RE);
  });

  it("is deterministic — the same agentctl id always maps to the same UUID", () => {
    const a = deriveSdkSessionUuid("s_abc");
    const b = deriveSdkSessionUuid("s_abc");
    expect(a).toBe(b);
    // Pinned so an accidental change to the namespace or hash input is caught:
    // a drifting derivation would silently orphan every existing session.
    expect(a).toBe("c3dcb283-cfda-52bd-b6d0-d5abb2f76986");
  });

  it("maps different agentctl ids to different UUIDs", () => {
    expect(deriveSdkSessionUuid("s_abc")).not.toBe(deriveSdkSessionUuid("s_abd"));
  });

  it("throws naming spec.sessionId for an empty or blank id", () => {
    expect(() => deriveSdkSessionUuid("")).toThrow(/spec\.sessionId/);
    expect(() => deriveSdkSessionUuid("  \t ")).toThrow(/spec\.sessionId/);
  });
});

describe("buildSubagents", () => {
  it("maps parsed subagent bodies onto AgentDefinition", () => {
    const out = buildSubagents([
      { name: "reviewer", description: "reviews code", tools: ["Read"], prompt: "You review." },
    ]);
    expect(out).toEqual({
      reviewer: { description: "reviews code", tools: ["Read"], prompt: "You review." },
    });
  });

  it("omits an absent tools list so the agent inherits parent tools", () => {
    const out = buildSubagents([{ name: "a", description: "d", prompt: "p" }]);
    expect(out.a.tools).toBeUndefined();
  });

  it("maps a declared model through to AgentDefinition.model", () => {
    const out = buildSubagents([
      { name: "a", description: "d", prompt: "p", model: "claude-haiku-4-5" },
    ]);
    expect(out.a.model).toBe("claude-haiku-4-5");
  });

  it("omits model when absent so the subagent inherits the main model", () => {
    const out = buildSubagents([{ name: "a", description: "d", prompt: "p" }]);
    expect(out.a.model).toBeUndefined();
  });

  it("returns an empty map for no subagents", () => {
    expect(buildSubagents([])).toEqual({});
  });
});
