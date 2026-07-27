import { describe, it, expect } from "vitest";
import { assertClaudeCompatible } from "./claude-invocation.js";
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
