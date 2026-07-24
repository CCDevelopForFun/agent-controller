import { describe, it, expect } from "vitest";
import { buildOpencodeConfig } from "./opencode-config.js";
import type { CompiledSpec } from "./types.js";

function baseSpec(overrides: Partial<CompiledSpec> = {}): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "hello" },
    model: { provider: "anthropic", name: "claude-sonnet-4-20250514" },
    task: "say hi",
    tools: [],
    extensions: [],
    skills: [],
    runtime: { type: "local-opencode" },
    ...overrides,
  };
}

describe("buildOpencodeConfig", () => {
  it("produces the minimal shape for a tool-less spec", () => {
    const cfg = buildOpencodeConfig(baseSpec());
    expect(cfg.$schema).toBe("https://opencode.ai/config.json");
    // cfg.agent always includes the 4 disabled native agents
    // (general/explore/plan/build) plus the primary; user-declared
    // subagents would be added too. Codex pass 4 of slice 2.5.
    expect(Object.keys(cfg.agent).sort()).toEqual(
      ["build", "explore", "general", "hello", "plan", "scout"],
    );
    const agent = cfg.agent.hello;
    expect(agent.model).toBe("anthropic/claude-sonnet-4-20250514");
    expect(agent.description).toMatch(/^Agent hello/);
    // ADL allowlist semantic: tools: [] → all known opencode tools are
    // explicitly denied. opencode doesn't support a "*" wildcard so we
    // enumerate every concrete tool key. Codex pass 11 of slice 2.4.
    expect(agent.permission).toBeDefined();
    // Every supported key must be "deny" since no tools were declared.
    for (const val of Object.values(agent.permission!)) {
      expect(val).toBe("deny");
    }
    // All known permission keys are in the deny baseline (codex pass 15
    // confirmed opencode docs support more keys than the SDK types expose).
    expect(agent.permission!["bash"]).toBe("deny");
    expect(agent.permission!["edit"]).toBe("deny");
    expect(agent.permission!["webfetch"]).toBe("deny");
    expect(agent.permission!["read"]).toBe("deny");
    expect(agent.permission!["glob"]).toBe("deny");
    expect(agent.permission!["*"]).toBe("deny");
  });

  it("emits temperature when spec.model.temperature is set", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({ model: { provider: "anthropic", name: "x", temperature: 0.42 } }),
    );
    expect(cfg.agent.hello.temperature).toBe(0.42);
  });

  it("omits temperature when spec.model.temperature is absent", () => {
    const cfg = buildOpencodeConfig(baseSpec());
    expect(cfg.agent.hello.temperature).toBeUndefined();
  });

  it("uses spec.metadata.description when present", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({ metadata: { name: "hello", description: "MVP demo" } }),
    );
    expect(cfg.agent.hello.description).toBe("MVP demo");
  });

  it("composes the prompt with honesty preamble + persona role + instructions", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        persona: {
          role: "Helpful assistant",
          instructions: "Be friendly. Use markdown.",
        },
      }),
    );
    const p = cfg.agent.hello.prompt;
    expect(p).toMatch(/Honesty rules/);
    expect(p).toMatch(/# Role\nHelpful assistant/);
    expect(p).toMatch(/# Instructions\nBe friendly\. Use markdown\./);
    // Honesty must come first so the persona's instructions can't
    // override it. Same ordering invariant as the Pi adapter's
    // buildSystemPrompt() (asserted by runtime/src/adapter.test.ts).
    const honestyIdx = p.indexOf("Honesty rules");
    const roleIdx = p.indexOf("# Role");
    expect(honestyIdx).toBeGreaterThanOrEqual(0);
    expect(roleIdx).toBeGreaterThan(honestyIdx);
  });

  it("includes the honesty preamble even when persona is absent", () => {
    const cfg = buildOpencodeConfig(baseSpec());
    expect(cfg.agent.hello.prompt).toMatch(/Honesty rules/);
  });

  it("maps each Pi built-in to opencode's permission key set; write collapses onto edit", () => {
    // Codex pass 1 of slice 2.2: opencode does not expose a separate
    // `write` permission — file write/edit is controlled by `edit`.
    // Mapping Pi's `write` → opencode's `edit` is intentional and
    // documented in PI_TO_OPENCODE_PERMISSION_KEY's table comment.
    const cfg = buildOpencodeConfig(
      baseSpec({
        tools: [
          { name: "bash", builtin: true },
          { name: "read", builtin: true },
          { name: "edit", builtin: true },
          { name: "write", builtin: true },
        ],
      }),
    );
    expect(cfg.agent.hello.permission!["bash"]).toBe("allow");
    // "read" is now grantable per opencode docs (codex pass 15).
    expect(cfg.agent.hello.permission!["read"]).toBe("allow");
    expect(cfg.agent.hello.permission!["edit"]).toBe("allow");
    // Other supported keys default to "deny".
    expect(cfg.agent.hello.permission!["webfetch"]).toBe("deny");
    expect(cfg.agent.hello.permission!["doom_loop"]).toBe("deny");
  });

  it("write-only spec collapses to edit allow (no separate write permission)", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({ tools: [{ name: "write", builtin: true }] }),
    );
    expect(cfg.agent.hello.permission!["edit"]).toBe("allow");
    expect(cfg.agent.hello.permission!["bash"]).toBe("deny");
  });

  it("declaring both edit and write is idempotent on the edit grant", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        tools: [
          { name: "edit", builtin: true },
          { name: "write", builtin: true },
        ],
      }),
    );
    expect(cfg.agent.hello.permission!["edit"]).toBe("allow");
    expect(cfg.agent.hello.permission!["bash"]).toBe("deny");
  });

  it("only emits permission grants for the built-ins the spec declares (deny baseline always present)", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({ tools: [{ name: "bash", builtin: true }] }),
    );
    expect(cfg.agent.hello.permission!["bash"]).toBe("allow");
    expect(cfg.agent.hello.permission!["webfetch"]).toBe("deny");
  });

  it("rejects custom Pi-extension tools with a clear field-naming error", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          tools: [
            { name: "get_time", entrypoint: "/abs/tools/get_time/entrypoint.ts" },
            { name: "bash", builtin: true },
          ],
        }),
      ),
    ).toThrow(/get_time/);
  });

  it("rejects custom Pi-extension tools naming every offender", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          tools: [
            { name: "foo", entrypoint: "/abs/foo.ts" },
            { name: "bar", entrypoint: "/abs/bar.ts" },
          ],
        }),
      ),
    ).toThrow(/foo, bar/);
  });

  it("uses agentName override when provided", () => {
    const cfg = buildOpencodeConfig(baseSpec(), { agentName: "custom-name" });
    // Primary agent uses the override name; the 4 disabled natives are also
    // always present (codex pass 4 of slice 2.5).
    expect(cfg.agent["custom-name"]).toBeDefined();
    expect(cfg.agent["custom-name"].mode).toBeUndefined(); // not a subagent
  });

  it("preserves provider/model format for non-anthropic providers", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({ model: { provider: "openai", name: "gpt-5" } }),
    );
    expect(cfg.agent.hello.model).toBe("openai/gpt-5");
  });

  // ── slice 2.5: skill body inlining ────────────────────────────────────

  it("inlines each skill body into the primary agent's prompt, wrapped via wrapSkillBody", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      skillBodies: [
        { name: "format-times", body: "Always emit times as ISO-8601 UTC." },
        { name: "be-terse", body: "Prefer one-sentence answers." },
      ],
    });
    const prompt = cfg.agent.hello.prompt;
    // Both skill bodies appear, each wrapped by wrapSkillBody's header.
    expect(prompt).toContain("Always emit times as ISO-8601 UTC.");
    expect(prompt).toContain("Prefer one-sentence answers.");
    // wrapSkillBody names the skill so the model knows which one it's reading.
    expect(prompt).toContain("format-times");
    expect(prompt).toContain("be-terse");
  });

  it("skips empty skill bodies", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      skillBodies: [
        { name: "empty", body: "   \n\n   " },
        { name: "real", body: "Real content." },
      ],
    });
    expect(cfg.agent.hello.prompt).toContain("Real content.");
    expect(cfg.agent.hello.prompt).not.toContain("empty");
  });

  // ── slice 2.5: subagent wiring ────────────────────────────────────────

  it("adds subagent definitions to cfg.agent with mode='subagent'", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "code-reviewer",
          description: "Use to review code changes for correctness.",
          systemPrompt: "You are a code reviewer. Be precise.",
        },
      ],
    });
    // Primary + subagent + 4 disabled natives.
    expect(Object.keys(cfg.agent).sort()).toEqual(
      ["build", "code-reviewer", "explore", "general", "hello", "plan", "scout"],
    );
    const sa = cfg.agent["code-reviewer"];
    expect(sa.mode).toBe("subagent");
    expect(sa.description).toBe("Use to review code changes for correctness.");
    expect(sa.prompt).toBe("You are a code reviewer. Be precise.");
    // Inherits primary agent's model when not overridden.
    expect(sa.model).toBe("anthropic/claude-sonnet-4-20250514");
    // Primary agent does NOT get mode set (defaults to opencode's "all").
    expect(cfg.agent.hello.mode).toBeUndefined();
  });

  it("subagent uses its own model when frontmatter overrides", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "fast-helper",
          description: "Fast helper subagent.",
          model: "anthropic/claude-haiku-4-5",
          systemPrompt: "Fast and short.",
        },
      ],
    });
    expect(cfg.agent["fast-helper"].model).toBe("anthropic/claude-haiku-4-5");
  });

  it("subagent tools map to opencode permission keys with deny baseline", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "bash-only",
          description: "Bash-only subagent.",
          tools: ["bash"],
          systemPrompt: "Use bash.",
        },
      ],
    });
    const sa = cfg.agent["bash-only"];
    expect(sa.permission!["bash"]).toBe("allow");
    expect(sa.permission!["edit"]).toBe("deny");
    expect(sa.permission!["read"]).toBe("deny");
    expect(sa.permission!["*"]).toBe("deny");
  });

  it("rejects subagent name colliding with primary agent name", () => {
    expect(() =>
      buildOpencodeConfig(baseSpec(), {
        subagentDefinitions: [
          { name: "hello", description: "collision", systemPrompt: "x" },
        ],
      }),
    ).toThrow(/collides with the primary agent name/);
  });

  it("rejects duplicate subagent names", () => {
    expect(() =>
      buildOpencodeConfig(baseSpec(), {
        subagentDefinitions: [
          { name: "twin", description: "first", systemPrompt: "a" },
          { name: "twin", description: "second", systemPrompt: "b" },
        ],
      }),
    ).toThrow(/duplicate subagent name "twin"/);
  });

  it("rejects subagent that declares an unknown built-in tool", () => {
    expect(() =>
      buildOpencodeConfig(baseSpec(), {
        subagentDefinitions: [
          {
            name: "bad",
            description: "Has fake tool.",
            tools: ["nuke"],
            systemPrompt: "x",
          },
        ],
      }),
    ).toThrow(/declares tool "nuke"/);
  });

  // ── slice 2.5: MCP server wiring ──────────────────────────────────────

  it("maps stdio MCP server to cfg.mcp[name] with type='local'", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          {
            name: "filesystem",
            transport: "stdio",
            command: "npx",
            args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
            env: { FOO: "bar" },
          },
        ],
      }),
    );
    expect(cfg.mcp).toBeDefined();
    const fs = cfg.mcp!["filesystem"];
    expect(fs.type).toBe("local");
    expect((fs as { command: string[] }).command).toEqual([
      "npx",
      "-y",
      "@modelcontextprotocol/server-filesystem",
      "/tmp",
    ]);
    expect((fs as { environment?: Record<string, string> }).environment).toEqual({ FOO: "bar" });
  });

  it("maps streamable-http MCP server to cfg.mcp[name] with type='remote'", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          {
            name: "remote-svc",
            transport: "streamable-http",
            url: "https://mcp.example.com/v1",
            headers: { Authorization: "Bearer abc" },
          },
        ],
      }),
    );
    const r = cfg.mcp!["remote-svc"];
    expect(r.type).toBe("remote");
    expect((r as { url: string }).url).toBe("https://mcp.example.com/v1");
    expect((r as { headers?: Record<string, string> }).headers).toEqual({ Authorization: "Bearer abc" });
  });

  it("maps sse MCP server transport identically to streamable-http (both → remote)", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          { name: "sse-svc", transport: "sse", url: "https://sse.example.com" },
        ],
      }),
    );
    expect(cfg.mcp!["sse-svc"].type).toBe("remote");
  });

  it("omits cfg.mcp entirely when spec.mcpServers is empty or undefined", () => {
    const a = buildOpencodeConfig(baseSpec());
    expect(a.mcp).toBeUndefined();
    const b = buildOpencodeConfig(baseSpec({ mcpServers: [] }));
    expect(b.mcp).toBeUndefined();
  });

  it("rejects stdio MCP server with no command field", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [{ name: "broken", transport: "stdio" }],
        }),
      ),
    ).toThrow(/transport "stdio" but has no command field/);
  });

  it("rejects remote MCP server with no url field", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [{ name: "broken", transport: "streamable-http" }],
        }),
      ),
    ).toThrow(/has no url field/);
  });

  it("rejects unknown MCP transport", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          mcpServers: [{ name: "weird", transport: "carrier-pigeon" as any, url: "x" }],
        }),
      ),
    ).toThrow(/unknown transport "carrier-pigeon"/);
  });

  // ── slice 2.5 codex pass 1: implicit grants ───────────────────────────

  it("primary agent gets task: 'allow' when subagents are declared", () => {
    const withoutSubagents = buildOpencodeConfig(baseSpec());
    expect(withoutSubagents.agent.hello.permission!["task"]).toBe("deny");

    const withSubagents = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        { name: "helper", description: "delegate target", systemPrompt: "x" },
      ],
    });
    // Without 'task: allow', opencode would connect the subagent but the
    // primary agent could never invoke it.
    expect(withSubagents.agent.hello.permission!["task"]).toBe("allow");
  });

  it("primary agent gets <server>_* grant for each declared MCP server", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          { name: "filesystem", transport: "stdio", command: "x" },
          { name: "github", transport: "stdio", command: "y" },
        ],
      }),
    );
    expect(cfg.agent.hello.permission!["filesystem_*"]).toBe("allow");
    expect(cfg.agent.hello.permission!["github_*"]).toBe("allow");
    // The wildcard catch-all is still present and untouched.
    expect(cfg.agent.hello.permission!["*"]).toBe("deny");
  });

  it("subagent with bare model id gets primary provider prepended", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "bare",
          description: "uses bare model id",
          model: "claude-haiku-4-5", // no provider prefix
          systemPrompt: "x",
        },
      ],
    });
    // Primary spec provider is "anthropic" → bare id is normalized.
    expect(cfg.agent["bare"].model).toBe("anthropic/claude-haiku-4-5");
  });

  it("preserves the primary agent when metadata.name collides with a native agent name", () => {
    // Codex pass 5 of slice 2.5: if a user names their primary agent "plan",
    // it should NOT be overwritten by the disabled native stub. The other
    // three natives still get disabled.
    const cfg = buildOpencodeConfig(
      baseSpec({ metadata: { name: "plan" } }),
    );
    expect(cfg.agent["plan"]).toBeDefined();
    expect(cfg.agent["plan"].disable).toBeUndefined();
    // The other four natives are still disabled.
    expect(cfg.agent["build"].disable).toBe(true);
    expect(cfg.agent["explore"].disable).toBe(true);
    expect(cfg.agent["general"].disable).toBe(true);
    expect(cfg.agent["scout"].disable).toBe(true);
  });

  it("primary agent named like a constructor or __proto__ prototype key is safe", () => {
    // Codex pass 6 of slice 2.5: null-prototype maps mean these names work
    // as ordinary keys without prototype pollution or false duplicate hits.
    const cfg = buildOpencodeConfig(baseSpec({ metadata: { name: "constructor" } }));
    expect(cfg.agent["constructor"]).toBeDefined();
    expect(cfg.agent["constructor"].disable).toBeUndefined();
  });

  it("MCP server named 'constructor' is treated as a normal key (no prototype pollution)", () => {
    // 'constructor' is in the allowed [A-Za-z0-9._-] charset so it passes
    // rejectUnsafeMcpNameChars (codex pass 8), and the null-prototype map
    // means it doesn't collide with Object.prototype (codex pass 6).
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          { name: "constructor", transport: "stdio", command: "x" },
        ],
      }),
    );
    expect(cfg.mcp!["constructor"]).toBeDefined();
  });

  it("rejects MCP server names with glob metacharacters that would overmatch wildcards", () => {
    // Codex pass 8 of slice 2.5: `repo*` would emit `repo*_*` which matches
    // built-in `repo_clone` and re-enables tools outside the ADL allowlist.
    for (const name of ["repo*", "foo?", "ba[r]", "x{y}", "a b"]) {
      expect(() =>
        buildOpencodeConfig(
          baseSpec({
            mcpServers: [{ name, transport: "stdio", command: "x" }],
          }),
        ),
      ).toThrow(/contains characters outside the allowed set/);
    }
  });

  it("accepts MCP server names with dots, dashes, underscores (common identifier chars)", () => {
    // These should NOT be rejected — they're conventional MCP server names.
    for (const name of ["github.com", "my-mcp", "my_mcp", "v1.2.3", "ABC-xyz_42"]) {
      expect(() =>
        buildOpencodeConfig(
          baseSpec({
            mcpServers: [{ name, transport: "stdio", command: "x" }],
          }),
        ),
      ).not.toThrow();
    }
  });

  it("rejects two MCP servers that sanitize to the same key", () => {
    // Codex pass 5 of slice 2.5: "github.com" and "github_com" both
    // sanitize to "github_com" — opencode would generate tool ids in the
    // same `github_com_*` namespace, collapsing the servers.
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [
            { name: "github.com", transport: "stdio", command: "x" },
            { name: "github_com", transport: "stdio", command: "y" },
          ],
        }),
      ),
    ).toThrow(/both sanitize to "github_com"/);
  });

  it("disables opencode's native agents (general/explore/plan/build) so they can't be invoked via task", () => {
    // Codex pass 4 of slice 2.5: blanket task: allow would otherwise let
    // the primary agent delegate to opencode natives, bypassing the
    // ADL allowlist.
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        { name: "helper", description: "real subagent", systemPrompt: "x" },
      ],
    });
    for (const n of ["general", "explore", "plan", "build", "scout"]) {
      expect(cfg.agent[n]).toBeDefined();
      expect(cfg.agent[n].disable).toBe(true);
    }
    // The real subagent is still present and NOT disabled.
    expect(cfg.agent["helper"].disable).toBeUndefined();
  });

  it("native agents are disabled even with no subagents declared", () => {
    // The deny stays in place via task: deny, but we also disable the
    // natives unconditionally so opencode doesn't expose them via any
    // other code path. Defense in depth.
    const cfg = buildOpencodeConfig(baseSpec());
    for (const n of ["general", "explore", "plan", "build", "scout"]) {
      expect(cfg.agent[n]?.disable).toBe(true);
    }
  });

  it("rejects subagent name colliding with an opencode native agent", () => {
    for (const name of ["general", "explore", "plan", "build", "scout"]) {
      expect(() =>
        buildOpencodeConfig(baseSpec(), {
          subagentDefinitions: [
            { name, description: "x", systemPrompt: "x" },
          ],
        }),
      ).toThrow(/collides with an opencode native agent/);
    }
  });

  it("MCP server with non-alphanumeric chars emits BOTH raw and sanitized allow patterns", () => {
    // Codex passes 4 and 7 of slice 2.5 disagreed on whether opencode uses
    // raw or sanitized MCP tool ids. We emit both patterns defensively so
    // either interpretation is covered.
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          { name: "github.com", transport: "stdio", command: "x" },
        ],
      }),
    );
    expect(cfg.mcp!["github.com"]).toBeDefined();
    expect(cfg.agent.hello.permission!["github_com_*"]).toBe("allow");
    expect(cfg.agent.hello.permission!["github.com_*"]).toBe("allow");
  });

  it("alphanumeric-only MCP server name emits exactly one allow pattern (no duplicate)", () => {
    const cfg = buildOpencodeConfig(
      baseSpec({
        mcpServers: [
          { name: "filesystem", transport: "stdio", command: "x" },
        ],
      }),
    );
    expect(cfg.agent.hello.permission!["filesystem_*"]).toBe("allow");
    // sanitized form === raw form, so no additional key is emitted.
    const keys = Object.keys(cfg.agent.hello.permission!).filter((k) => k.includes("filesystem"));
    expect(keys).toEqual(["filesystem_*"]);
  });

  it("subagent accepts documented Pi tool names beyond bash/read/edit/write", () => {
    // Pi frontmatter convention: `tools: read, grep, find, ls`.
    // Codex pass 2 of slice 2.5 caught that the narrow built-in map
    // rejected these documented tool names.
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "searcher",
          description: "Searches the codebase.",
          tools: ["read", "grep", "find", "ls"],
          systemPrompt: "x",
        },
      ],
    });
    const sa = cfg.agent["searcher"];
    expect(sa.permission!["read"]).toBe("allow");
    expect(sa.permission!["grep"]).toBe("allow");
    expect(sa.permission!["glob"]).toBe("allow"); // `find` maps to glob
    expect(sa.permission!["list"]).toBe("allow"); // `ls` maps to list
  });

  it("subagent with fully qualified provider/model is passed through unchanged", () => {
    const cfg = buildOpencodeConfig(baseSpec(), {
      subagentDefinitions: [
        {
          name: "qualified",
          description: "already qualified",
          model: "openai/gpt-5",
          systemPrompt: "x",
        },
      ],
    });
    expect(cfg.agent["qualified"].model).toBe("openai/gpt-5");
  });

  it("rejects MCP server names that would broaden opencode built-in permissions via wildcard", () => {
    // "repo" + "_*" would match opencode's built-in `repo_clone` / `repo_overview`
    // and silently override their deny baseline. Codex pass 3 of slice 2.5.
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [{ name: "repo", transport: "stdio", command: "x" }],
        }),
      ),
    ).toThrow(/collides with the opencode built-in permission key "repo_/);
    // Same for "doom" (would match doom_loop) and "external" (external_directory).
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [{ name: "doom", transport: "stdio", command: "x" }],
        }),
      ),
    ).toThrow(/collides with the opencode built-in permission key "doom_loop"/);
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [{ name: "external", transport: "stdio", command: "x" }],
        }),
      ),
    ).toThrow(/collides with the opencode built-in permission key "external_directory"/);
  });

  it("rejects duplicate MCP server names", () => {
    expect(() =>
      buildOpencodeConfig(
        baseSpec({
          mcpServers: [
            { name: "dup", transport: "stdio", command: "a" },
            { name: "dup", transport: "stdio", command: "b" },
          ],
        }),
      ),
    ).toThrow(/duplicate MCP server name "dup"/);
  });

  // Slice 5.4: experimental.openTelemetry flag. opencode emits AI-SDK
  // telemetry spans inside its child process when this is true.
  describe("experimental.openTelemetry (slice 5.4)", () => {
    it("omits experimental block entirely when observability is absent", () => {
      const cfg = buildOpencodeConfig(baseSpec());
      expect(cfg.experimental).toBeUndefined();
    });

    it("omits experimental block when tracing is explicitly false", () => {
      const cfg = buildOpencodeConfig(
        baseSpec({ observability: { tracing: false } }),
      );
      expect(cfg.experimental).toBeUndefined();
    });

    it("flips experimental.openTelemetry on when spec.observability.tracing is true", () => {
      const cfg = buildOpencodeConfig(
        baseSpec({ observability: { tracing: true } }),
      );
      expect(cfg.experimental).toEqual({ openTelemetry: true });
    });

    it("flips on even without captureContent — content capture is independent", () => {
      // captureContent controls content payload attachment in the
      // ADAPTER's agent.session span (slice 5.3 pattern). opencode's
      // AI-SDK telemetry handles its own content capture; the flag
      // gate is just on `tracing`.
      const cfg = buildOpencodeConfig(
        baseSpec({ observability: { tracing: true, captureContent: false } }),
      );
      expect(cfg.experimental?.openTelemetry).toBe(true);
    });
  });
});
