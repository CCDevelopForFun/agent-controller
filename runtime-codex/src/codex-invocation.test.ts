import { describe, it, expect } from "vitest";
import { assertCodexCompatible, buildPrompt, buildCodexArgs } from "./codex-invocation.js";
import { HONESTY_PREAMBLE } from "./honesty.js";
import type { CompiledSpec } from "./types.js";

function baseSpec(overrides: Partial<CompiledSpec> = {}): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "my-agent" },
    model: { provider: "openai", name: "gpt-5-codex" },
    task: "Do the thing",
    tools: [],
    extensions: [],
    skills: [],
    runtime: { type: "local-opencode" },
    ...overrides,
  };
}

// ── assertCodexCompatible ──────────────────────────────────────────────────

describe("assertCodexCompatible", () => {
  it("does NOT throw for a clean openai spec", () => {
    expect(() => assertCodexCompatible(baseSpec())).not.toThrow();
  });

  it("does NOT throw for a spec with a built-in tool (builtin: true)", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ tools: [{ name: "bash", builtin: true }] }))
    ).not.toThrow();
  });

  it("does NOT throw for a spec with a built-in tool (no entrypoint, no builtin flag)", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ tools: [{ name: "read" }] }))
    ).not.toThrow();
  });

  it("throws when model.provider is not openai (e.g. anthropic)", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ model: { provider: "anthropic", name: "claude-3" } }))
    ).toThrow(/provider/);
  });

  it("throws when model.provider is google", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ model: { provider: "google", name: "gemini-2" } }))
    ).toThrow(/provider/);
  });

  it("throws when spec.extensions is non-empty", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ extensions: [{ name: "my-ext", entrypoint: "/abs/ext.ts" }] }))
    ).toThrow(/extensions/);
  });

  it("throws when spec.installs is non-empty", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ installs: ["npm:some-pkg"] }))
    ).toThrow(/installs/);
  });

  it("throws when spec.subagents is non-empty", () => {
    expect(() =>
      assertCodexCompatible(baseSpec({ subagents: [{ name: "worker", entrypoint: "/abs/worker.md" }] }))
    ).toThrow(/subagents/);
  });

  it("throws for a custom Pi-extension tool (entrypoint set, builtin: false)", () => {
    expect(() =>
      assertCodexCompatible(
        baseSpec({ tools: [{ name: "my-tool", entrypoint: "/abs/tools/my-tool.ts", builtin: false }] })
      )
    ).toThrow(/my-tool/);
  });

  it("throws for a custom Pi-extension tool (entrypoint set, no builtin flag)", () => {
    expect(() =>
      assertCodexCompatible(
        baseSpec({ tools: [{ name: "custom-x", entrypoint: "/abs/custom-x.ts" }] })
      )
    ).toThrow(/custom-x/);
  });

  it("names all offending custom tools in the error message", () => {
    expect(() =>
      assertCodexCompatible(
        baseSpec({
          tools: [
            { name: "foo", entrypoint: "/abs/foo.ts" },
            { name: "bar", entrypoint: "/abs/bar.ts" },
          ],
        })
      )
    ).toThrow(/foo.*bar|bar.*foo/);
  });

  it("does NOT throw when a tool has builtin: true even if entrypoint is somehow set", () => {
    expect(() =>
      assertCodexCompatible(
        baseSpec({ tools: [{ name: "bash", entrypoint: "/some/path.ts", builtin: true }] })
      )
    ).not.toThrow();
  });
});

// ── buildPrompt ────────────────────────────────────────────────────────────

describe("buildPrompt", () => {
  it("includes HONESTY_PREAMBLE", () => {
    const result = buildPrompt(baseSpec(), []);
    expect(result).toContain(HONESTY_PREAMBLE);
  });

  it("HONESTY_PREAMBLE comes first in the output", () => {
    const result = buildPrompt(baseSpec(), []);
    expect(result.startsWith(HONESTY_PREAMBLE)).toBe(true);
  });

  it("includes persona role when present", () => {
    const result = buildPrompt(
      baseSpec({ persona: { role: "Expert engineer" } }),
      []
    );
    expect(result).toMatch(/# Role\nExpert engineer/);
  });

  it("includes persona instructions when present", () => {
    const result = buildPrompt(
      baseSpec({ persona: { instructions: "Be concise." } }),
      []
    );
    expect(result).toMatch(/# Instructions\nBe concise\./);
  });

  it("includes both role and instructions, role before instructions", () => {
    const result = buildPrompt(
      baseSpec({ persona: { role: "My Role", instructions: "My Instructions" } }),
      []
    );
    const roleIdx = result.indexOf("# Role");
    const instrIdx = result.indexOf("# Instructions");
    expect(roleIdx).toBeGreaterThan(-1);
    expect(instrIdx).toBeGreaterThan(roleIdx);
  });

  it("omits persona section entirely when persona is absent", () => {
    const result = buildPrompt(baseSpec(), []);
    expect(result).not.toContain("# Role");
    expect(result).not.toContain("# Instructions");
  });

  it("includes wrapSkillBody output for each skill", () => {
    const result = buildPrompt(baseSpec(), [
      { name: "my-skill", body: "Do this specific thing." },
    ]);
    expect(result).toContain("# Skill: my-skill");
    expect(result).toContain("Do this specific thing.");
  });

  it("includes all skills when multiple are given", () => {
    const result = buildPrompt(baseSpec(), [
      { name: "skill-a", body: "Body A" },
      { name: "skill-b", body: "Body B" },
    ]);
    expect(result).toContain("# Skill: skill-a");
    expect(result).toContain("Body A");
    expect(result).toContain("# Skill: skill-b");
    expect(result).toContain("Body B");
  });

  it("skill-a appears before skill-b in declaration order", () => {
    const result = buildPrompt(baseSpec(), [
      { name: "skill-a", body: "Body A" },
      { name: "skill-b", body: "Body B" },
    ]);
    expect(result.indexOf("skill-a")).toBeLessThan(result.indexOf("skill-b"));
  });

  it("includes spec.task in the prompt", () => {
    const result = buildPrompt(baseSpec({ task: "Build the rocket" }), []);
    expect(result).toContain("Build the rocket");
  });

  it("parts are joined by double newlines", () => {
    const result = buildPrompt(
      baseSpec({ task: "my-task", persona: { role: "My Role" } }),
      []
    );
    expect(result).toContain(HONESTY_PREAMBLE + "\n\n# Role");
  });

  it("skips empty skill bodies", () => {
    const result = buildPrompt(baseSpec(), [{ name: "empty-skill", body: "   " }]);
    expect(result).not.toContain("# Skill: empty-skill");
  });
});

// ── buildCodexArgs ─────────────────────────────────────────────────────────

describe("buildCodexArgs", () => {
  const cwd = "/work/myproject";

  it("starts with exec", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args[0]).toBe("exec");
  });

  it("contains --json flag", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).toContain("--json");
  });

  it("contains --ignore-user-config flag", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).toContain("--ignore-user-config");
  });

  it("contains --skip-git-repo-check flag", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).toContain("--skip-git-repo-check");
  });

  it("contains -C with the cwd value", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    const idx = args.indexOf("-C");
    expect(idx).toBeGreaterThan(-1);
    expect(args[idx + 1]).toBe(cwd);
  });

  it("contains -m with the model name", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    const idx = args.indexOf("-m");
    expect(idx).toBeGreaterThan(-1);
    expect(args[idx + 1]).toBe("gpt-5-codex");
  });

  it("contains -s workspace-write", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    const idx = args.indexOf("-s");
    expect(idx).toBeGreaterThan(-1);
    expect(args[idx + 1]).toBe("workspace-write");
  });

  it("ends with the composed prompt as the last arg", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    const prompt = buildPrompt(baseSpec(), []);
    expect(args[args.length - 1]).toBe(prompt);
  });

  it("normal (no resume): does NOT contain 'resume'", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).not.toContain("resume");
  });

  it("resume variant: second arg is 'resume'", () => {
    const args = buildCodexArgs(baseSpec(), { cwd, resumeThreadId: "thread-abc123" });
    expect(args[0]).toBe("exec");
    expect(args[1]).toBe("resume");
  });

  it("resume variant: third arg is the thread ID", () => {
    const args = buildCodexArgs(baseSpec(), { cwd, resumeThreadId: "thread-abc123" });
    expect(args[2]).toBe("thread-abc123");
  });

  it("resume variant: still contains --json, -m, -s, -C flags", () => {
    const args = buildCodexArgs(baseSpec(), { cwd, resumeThreadId: "tid-999" });
    expect(args).toContain("--json");
    expect(args).toContain("-m");
    expect(args).toContain("-s");
    expect(args).toContain("workspace-write");
    expect(args).toContain("-C");
    expect(args).toContain(cwd);
  });

  it("resume variant: prompt is still the last arg", () => {
    const args = buildCodexArgs(baseSpec(), { cwd, resumeThreadId: "tid-x" });
    const prompt = buildPrompt(baseSpec(), []);
    expect(args[args.length - 1]).toBe(prompt);
  });

  it("outputSchemaPath variant: contains --output-schema flag and value", () => {
    const schemaPath = "/tmp/schema.json";
    const args = buildCodexArgs(baseSpec(), { cwd, outputSchemaPath: schemaPath });
    const idx = args.indexOf("--output-schema");
    expect(idx).toBeGreaterThan(-1);
    expect(args[idx + 1]).toBe(schemaPath);
  });

  it("outputSchemaPath appears before the prompt (last arg)", () => {
    const schemaPath = "/tmp/schema.json";
    const args = buildCodexArgs(baseSpec(), { cwd, outputSchemaPath: schemaPath });
    const schemaIdx = args.indexOf("--output-schema");
    expect(schemaIdx).toBeLessThan(args.length - 1);
  });

  it("without outputSchemaPath: does NOT contain --output-schema", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).not.toContain("--output-schema");
  });

  it("exact normal argv matches the spec", () => {
    const spec = baseSpec();
    const prompt = buildPrompt(spec, []);
    const args = buildCodexArgs(spec, { cwd });
    expect(args).toEqual([
      "exec",
      "--json",
      "--ignore-user-config",
      "--skip-git-repo-check",
      "-C", cwd,
      "-m", "gpt-5-codex",
      "-s", "workspace-write",
      prompt,
    ]);
  });

  it("exact resume argv matches the spec", () => {
    const spec = baseSpec();
    const prompt = buildPrompt(spec, []);
    const args = buildCodexArgs(spec, { cwd, resumeThreadId: "resume-tid" });
    expect(args).toEqual([
      "exec", "resume", "resume-tid",
      "--json",
      "--ignore-user-config",
      "--skip-git-repo-check",
      "-C", cwd,
      "-m", "gpt-5-codex",
      "-s", "workspace-write",
      prompt,
    ]);
  });

  it("exact argv with outputSchema matches the spec", () => {
    const spec = baseSpec();
    const prompt = buildPrompt(spec, []);
    const schemaPath = "/tmp/out.json";
    const args = buildCodexArgs(spec, { cwd, outputSchemaPath: schemaPath });
    expect(args).toEqual([
      "exec",
      "--json",
      "--ignore-user-config",
      "--skip-git-repo-check",
      "-C", cwd,
      "-m", "gpt-5-codex",
      "-s", "workspace-write",
      "--output-schema", schemaPath,
      prompt,
    ]);
  });

  it("inlines skill bodies into the prompt when opts.skills is provided", () => {
    const spec = baseSpec();
    const skills = [{ name: "my-skill", body: "Do the special thing." }];
    const args = buildCodexArgs(spec, { cwd, skills });
    const promptWithSkill = buildPrompt(spec, skills);
    expect(args[args.length - 1]).toBe(promptWithSkill);
    expect(args[args.length - 1]).toContain("# Skill: my-skill");
    expect(args[args.length - 1]).toContain("Do the special thing.");
  });

  it("prompt without skills equals prompt with empty skills array", () => {
    const spec = baseSpec();
    const argsNoSkills = buildCodexArgs(spec, { cwd });
    const argsEmptySkills = buildCodexArgs(spec, { cwd, skills: [] });
    expect(argsNoSkills[argsNoSkills.length - 1]).toBe(argsEmptySkills[argsEmptySkills.length - 1]);
  });

  it("omits --ignore-user-config when spec has MCP servers (so codex reads CODEX_HOME/config.toml)", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "npx", args: ["-y", "srv"] }],
    });
    const args = buildCodexArgs(spec, { cwd });
    expect(args).not.toContain("--ignore-user-config");
  });

  it("keeps --ignore-user-config when spec has no MCP servers", () => {
    const args = buildCodexArgs(baseSpec(), { cwd });
    expect(args).toContain("--ignore-user-config");
  });
});

// ── buildConfigToml ────────────────────────────────────────────────────────

import { buildConfigToml } from "./codex-invocation.js";

describe("buildConfigToml", () => {
  it("returns empty string when spec has no mcpServers", () => {
    expect(buildConfigToml(baseSpec())).toBe("");
  });

  it("returns empty string when spec.mcpServers is an empty array", () => {
    expect(buildConfigToml(baseSpec({ mcpServers: [] }))).toBe("");
  });

  it("contains [mcp_servers.time] section for a stdio server named 'time'", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "npx", args: ["-y", "srv"] }],
    });
    expect(buildConfigToml(spec)).toContain("[mcp_servers.time]");
  });

  it("contains command = \"npx\" for the stdio server", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "npx", args: ["-y", "srv"] }],
    });
    expect(buildConfigToml(spec)).toContain('command = "npx"');
  });

  it("contains args = [\"-y\", \"srv\"] for the stdio server", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "npx", args: ["-y", "srv"] }],
    });
    expect(buildConfigToml(spec)).toContain('args = ["-y", "srv"]');
  });

  it("emits env subtable when env is provided for a stdio server", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "mcp", transport: "stdio", command: "uvx", args: ["srv"], env: { TOKEN: "abc" } }],
    });
    const toml = buildConfigToml(spec);
    expect(toml).toContain("[mcp_servers.mcp.env]");
    expect(toml).toContain('TOKEN = "abc"');
  });

  it("omits env subtable when env is absent", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "mcp", transport: "stdio", command: "uvx", args: ["srv"] }],
    });
    expect(buildConfigToml(spec)).not.toContain("[mcp_servers.mcp.env]");
  });

  it("omits args line when args is absent", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "mcp", transport: "stdio", command: "uvx" }],
    });
    const toml = buildConfigToml(spec);
    expect(toml).not.toContain("args =");
  });

  it("contains url = ... for a streamable-http server", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "remote", transport: "streamable-http", url: "https://example.com/mcp" }],
    });
    expect(buildConfigToml(spec)).toContain('[mcp_servers.remote]');
    expect(buildConfigToml(spec)).toContain('url = "https://example.com/mcp"');
  });

  it("emits multiple [mcp_servers.*] blocks when multiple servers are declared", () => {
    const spec = baseSpec({
      mcpServers: [
        { name: "alpha", transport: "stdio", command: "npx", args: ["-y", "alpha"] },
        { name: "beta", transport: "streamable-http", url: "https://beta.example.com/mcp" },
      ],
    });
    const toml = buildConfigToml(spec);
    expect(toml).toContain("[mcp_servers.alpha]");
    expect(toml).toContain("[mcp_servers.beta]");
  });

  it("alpha section appears before beta section", () => {
    const spec = baseSpec({
      mcpServers: [
        { name: "alpha", transport: "stdio", command: "npx", args: ["-y", "alpha"] },
        { name: "beta", transport: "streamable-http", url: "https://beta.example.com/mcp" },
      ],
    });
    const toml = buildConfigToml(spec);
    expect(toml.indexOf("[mcp_servers.alpha]")).toBeLessThan(toml.indexOf("[mcp_servers.beta]"));
  });

  it("throws for an sse server (not supported by codex config)", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "legacy", transport: "sse", url: "https://example.com/sse" }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/sse.*not supported|codex adapter/i);
  });

  // ── TOML-injection guards ────────────────────────────────────────────────

  it("throws when server name contains ] (injection attempt)", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "foo]bar", transport: "stdio", command: "npx" }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/server name "foo\]bar" is invalid/);
  });

  it("throws when server name contains a dot (dotted-key injection)", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "github.com", transport: "stdio", command: "npx" }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/server name "github\.com" is invalid/);
  });

  it("throws when server name contains a newline", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "foo\n[evil]", transport: "stdio", command: "npx" }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/server name/);
  });

  it("throws when env key contains a disallowed character", () => {
    const spec = baseSpec({
      mcpServers: [{
        name: "time",
        transport: "stdio",
        command: "uvx",
        env: { "BAD KEY": "value" },
      }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/env key "BAD KEY" in server "time" is invalid/);
  });

  it("throws when env key contains ] (injection attempt)", () => {
    const spec = baseSpec({
      mcpServers: [{
        name: "time",
        transport: "stdio",
        command: "uvx",
        env: { "K]EY": "value" },
      }],
    });
    expect(() => buildConfigToml(spec)).toThrow(/env key "K\]EY" in server "time" is invalid/);
  });

  it("tomlString escapes a newline in a command value — no raw newline in output", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "foo\nbar" }],
    });
    const toml = buildConfigToml(spec);
    // The emitted TOML must not contain a raw newline inside a quoted value
    expect(toml).toContain('command = "foo\\nbar"');
    // Verify no raw newline appears between the opening and closing quote of the value
    const valueMatch = toml.match(/command = "([^"]*)"/);
    expect(valueMatch).not.toBeNull();
    expect(valueMatch![1]).not.toMatch(/\n/);
  });

  it("tomlString escapes a newline in an arg value", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "npx", args: ["line1\nline2"] }],
    });
    const toml = buildConfigToml(spec);
    expect(toml).toContain('"line1\\nline2"');
  });

  it("tomlString escapes CR and tab in values", () => {
    const spec = baseSpec({
      mcpServers: [{ name: "time", transport: "stdio", command: "a\rb\tc" }],
    });
    const toml = buildConfigToml(spec);
    expect(toml).toContain('command = "a\\rb\\tc"');
  });
});
