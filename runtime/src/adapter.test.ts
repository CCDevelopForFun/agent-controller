import { describe, it, expect, vi, beforeEach } from "vitest";
import type { CompiledSpec, MCPServer, WireEvent } from "./types.js";

// ── source-bound extension fixture state ─────────────────────────────────────
// Controls what resolveSourceBoundExtension sees during tests.
const sourceFixture = {
  // Package names considered "already installed" (require/resolve succeeds).
  installedPackages: new Set<string>(),
  // spawnSync exit code for pi install (0 = success).
  piInstallExitCode: 0,
  // Whether spawnSync should report a spawn error.
  piInstallError: undefined as Error | undefined,
  // Package JSON content for installed packages (keyed by package name).
  // Defaults to a valid pi.extensions entry if not overridden.
  packageJsonContent: new Map<string, string>(),
};

// ── fs mock state ─────────────────────────────────────────────────────────────
// Controls the fake filesystem used by writeMcpJson and writeAgentFiles in adapter.ts.
const fakeFs = {
  // path → content for files that "exist" in the fake FS
  files: new Map<string, string>(),
  // directories that "exist" in the fake FS (mkdirSync populates this)
  dirs: new Set<string>(),
  // all writeFileSync calls: [path, content]
  writes: [] as Array<[string, string]>,
  // all mkdirSync calls
  mkdirs: [] as string[],
  // all copyFileSync calls: [src, dest]
  copies: [] as Array<[string, string]>,
  // all unlinkSync calls
  unlinks: [] as string[],
  // paths that should throw EACCES when unlink is attempted (for fail-closed tests)
  unlinkFails: new Set<string>(),
};

vi.mock("node:fs", () => ({
  existsSync: (p: string) => fakeFs.files.has(p) || fakeFs.dirs.has(p),
  readFileSync: (p: string, _enc: string) => {
    const c = fakeFs.files.get(p);
    if (c === undefined) throw Object.assign(new Error(`ENOENT: ${p}`), { code: "ENOENT" });
    return c;
  },
  mkdirSync: (p: string, _opts: any) => { fakeFs.mkdirs.push(p); fakeFs.dirs.add(p); },
  writeFileSync: (p: string, content: string, opts?: unknown) => {
    // Honor the exclusive-create flag ("wx") so tests exercise the atomic
    // writeMcpJson path (codex pass 5 of slice 7.5): a "x" flag on an
    // existing file throws EEXIST, matching Node's fs.
    const flag =
      typeof opts === "object" && opts !== null
        ? (opts as { flag?: string }).flag
        : undefined;
    if (typeof flag === "string" && flag.includes("x") && fakeFs.files.has(p)) {
      const err = new Error(`EEXIST: file already exists, open '${p}'`) as NodeJS.ErrnoException;
      err.code = "EEXIST";
      throw err;
    }
    fakeFs.files.set(p, content);
    fakeFs.writes.push([p, content]);
  },
  copyFileSync: (src: string, dest: string) => {
    // Copy from fake FS if src exists there; otherwise record an empty copy.
    const content = fakeFs.files.get(src) ?? "";
    fakeFs.files.set(dest, content);
    fakeFs.copies.push([src, dest]);
  },
  readdirSync: (p: string) => {
    // Return basenames of all files that are directly under the given dir.
    const entries: string[] = [];
    for (const filePath of fakeFs.files.keys()) {
      // A file is "in" the dir if its parent equals p.
      const parent = filePath.substring(0, filePath.lastIndexOf("/"));
      if (parent === p) {
        entries.push(filePath.substring(p.length + 1));
      }
    }
    return entries;
  },
  unlinkSync: (p: string) => {
    if (fakeFs.unlinkFails.has(p)) {
      throw Object.assign(new Error(`EACCES: ${p}`), { code: "EACCES" });
    }
    fakeFs.files.delete(p);
    fakeFs.unlinks.push(p);
  },
  // realpathSync: returns path as-is for test purposes (symlinks not used in fakeFs).
  realpathSync: (p: string) => {
    if (fakeFs.files.has(p) || fakeFs.dirs.has(p)) return p;
    throw Object.assign(new Error(`ENOENT: ${p}`), { code: "ENOENT" });
  },
}));

// Mock node:url so fileURLToPath works in the test environment.
// The adapter uses it to get __dirname for resolving the subagent extension path.
// pathToFileURL is also exposed because fake-provider (imported transitively
// via adapter.ts) destructures it; without the export, vitest's strict ESM
// loader can surface a missing-export error before the test suite runs.
// adapter.test.ts mocks pi-coding-agent, so the fake-provider's lazy loader
// never actually runs here, but the binding must exist for the import.
vi.mock("node:url", () => ({
  fileURLToPath: (_url: string) => {
    // Convert file:// URL to a path; in tests import.meta.url is a file URL.
    // We return a stable mock path so the subagent extension path is predictable.
    return "/mock/runtime/dist/adapter.js";
  },
  pathToFileURL: (p: string) => ({ href: `file://${p}` }),
}));

// Mock node:child_process for resolveSourceBoundExtension tests.
vi.mock("node:child_process", () => ({
  spawnSync: (cmd: string, args: string[], _opts: any) => {
    if (cmd === "which") {
      // No pi on PATH in tests by default.
      return { status: 1, stdout: "", stderr: "", error: undefined };
    }
    // For `pi install <source>` calls:
    if (sourceFixture.piInstallError) {
      return { status: null, error: sourceFixture.piInstallError, stdout: "", stderr: "" };
    }
    if (sourceFixture.piInstallExitCode === 0) {
      // Simulate install: add to installed packages so subsequent checks succeed.
      // The source arg is like "npm:my-pkg" → extract pkg name.
      const sourceArg = args[1] ?? "";
      const pkgName = sourceArg.startsWith("npm:") ? sourceArg.slice(4) : sourceArg;
      if (pkgName) {
        sourceFixture.installedPackages.add(pkgName);
        // Ensure package.json exists in fakeFs after install.
        // Pi installs to <agentDir>/npm/node_modules/<name>/ — mocked agentDir is /mock/agent/dir
        const pkgJsonPath = `/mock/agent/dir/npm/node_modules/${pkgName}/package.json`;
        if (!fakeFs.files.has(pkgJsonPath)) {
          const content = sourceFixture.packageJsonContent.get(pkgName) ?? JSON.stringify({
            name: pkgName,
            pi: { extensions: ["./src/index.ts"] },
          });
          fakeFs.files.set(pkgJsonPath, content);
        }
      }
    }
    return { status: sourceFixture.piInstallExitCode, stdout: "", stderr: "", error: undefined };
  },
}));

// Mock node:os for homedir (used by getAgentDir in source resolution).
vi.mock("node:os", () => ({
  homedir: () => "/mock/home",
  tmpdir: () => "/tmp",
}));

// Mock node:module so createRequire returns a fake resolver for pi-mcp-extension.
// We also handle the source-bound resolution path: pkg/package.json resolves to
// /mock/node_modules/<pkg>/package.json so resolveSourceBoundExtension can find it.
vi.mock("node:module", () => ({
  createRequire: () => {
    const resolver = (id: string) => {
      if (id === "pi-mcp-extension/src/index.ts") {
        return "/mock/node_modules/pi-mcp-extension/src/index.ts";
      }
      // Support <pkg>/package.json lookups for source-bound extension resolution.
      const pkgJsonSuffix = "/package.json";
      if (id.endsWith(pkgJsonSuffix)) {
        const pkgName = id.slice(0, -pkgJsonSuffix.length);
        // Only "installed" packages return a path; others throw.
        if (sourceFixture.installedPackages.has(pkgName)) {
          return `/mock/node_modules/${pkgName}/package.json`;
        }
      }
      throw new Error(`Cannot resolve: ${id}`);
    };
    resolver.resolve = resolver;
    return resolver;
  },
}));

// Capture-style mocks. Each test inspects what the adapter passed in.
const captured = {
  resourceLoaderArgs: undefined as any,
  createAgentArgs: undefined as any,
  promptCalls: [] as string[],
  subscribers: [] as Array<(ev: any) => void>,
  // Resolvers for any pending mocked session.prompt() calls. Tests call
  // `flushAndShutdown` to dispatch session_shutdown AND resolve the prompt
  // — matching real Pi semantics where session.prompt resolves only after
  // the turn is fully complete.
  promptResolvers: [] as Array<() => void>,
  modelGetCalls: [] as Array<[string, string]>,
  // The mocked agent object; tests can inspect onPayload after runSession.
  agentRef: undefined as any,
  // Controls whether getBuiltinModel returns a model or undefined (for error-path tests).
  modelReturnUndefined: false,
  // Records calls to SessionManager static factories.
  sessionManagerCalls: [] as Array<{ method: string; args: any[] }>,
};

vi.mock("@earendil-works/pi-ai/providers/all", () => ({
  getBuiltinModel: (provider: string, name: string) => {
    captured.modelGetCalls.push([provider, name]);
    if (captured.modelReturnUndefined) return undefined;
    return { __mocked: "model" };
  },
}));

// Tests can mutate this map to make getExtensions() return load errors for
// specific paths (or any extra synthetic entries).
const loaderErrors: { errors: Array<{ path: string; error: string }> } = { errors: [] };

vi.mock("@earendil-works/pi-coding-agent", () => {
  class DefaultResourceLoader {
    constructor(args: any) { captured.resourceLoaderArgs = args; }
    async reload() {}
    getExtensions() {
      return { errors: loaderErrors.errors };
    }
  }
  const createAgentSession = async (args: any) => {
    captured.createAgentArgs = args;
    // The adapter accesses session.agent.onPayload to forward temperature.
    const agent = { onPayload: undefined as any };
    captured.agentRef = agent;
    const session = {
      agent,
      subscribe: (fn: (ev: any) => void) => captured.subscribers.push(fn),
      bindExtensions: async (_bindings: any) => {},
      prompt: async (text: string) => {
        captured.promptCalls.push(text);
        await new Promise<void>((resolve) => {
          captured.promptResolvers.push(resolve);
        });
      },
      dispose: () => {},
    };
    return { session };
  };

  // Mock SessionManager with the two static factories the adapter uses.
  const mockSessionInstance = { __mocked: "sessionManager" };
  class SessionManager {
    static continueRecent(...args: any[]) {
      captured.sessionManagerCalls.push({ method: "continueRecent", args });
      return mockSessionInstance;
    }
    static inMemory(...args: any[]) {
      captured.sessionManagerCalls.push({ method: "inMemory", args });
      return mockSessionInstance;
    }
  }

  function getAgentDir() {
    return "/mock/agent/dir";
  }

  return { createAgentSession, DefaultResourceLoader, SessionManager, getAgentDir };
});

import { runSession } from "./adapter.js";

beforeEach(() => {
  captured.resourceLoaderArgs = undefined;
  captured.createAgentArgs = undefined;
  captured.promptCalls = [];
  captured.subscribers = [];
  captured.modelGetCalls = [];
  captured.agentRef = undefined;
  captured.modelReturnUndefined = false;
  loaderErrors.errors = [];
  captured.promptResolvers = [];
  captured.sessionManagerCalls = [];
  // Reset fake filesystem state between tests.
  fakeFs.files.clear();
  fakeFs.dirs.clear();
  fakeFs.writes = [];
  fakeFs.mkdirs = [];
  fakeFs.copies = [];
  fakeFs.unlinks = [];
  fakeFs.unlinkFails.clear();
  // Reset source-bound extension fixture state.
  sourceFixture.installedPackages.clear();
  sourceFixture.piInstallExitCode = 0;
  sourceFixture.piInstallError = undefined;
  sourceFixture.packageJsonContent.clear();
  // Clear env overrides that affect source resolution.
  delete process.env.AGENT_CONTROLLER_NO_AUTO_INSTALL;
  delete process.env.PI_BIN;
});

function fixture(): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "hello" },
    model: { provider: "anthropic", name: "claude-sonnet-5" },
    task: "Tell me the time",
    tools: [{ name: "get_time", entrypoint: "/abs/tools/get_time/entrypoint.ts" }],
    extensions: [{ name: "audit-log", entrypoint: "/abs/extensions/audit-log/entrypoint.ts", config: { path: "./audit.log" } }],
    skills: [{ name: "example-time-skill", entrypoint: "/abs/skills/example-time-skill/SKILL.md" }],
    runtime: { type: "local" },
  };
}

async function flushAndShutdown() {
  await new Promise((r) => setImmediate(r));
  for (const fn of captured.subscribers) fn({ type: "session_shutdown" });
  // Resolve any pending mocked session.prompt() calls so runSession completes.
  for (const r of captured.promptResolvers) r();
  captured.promptResolvers = [];
}

// v0.1.8 guardrail: the runtime hallucination detector imports cleanly.
import { detectHallucinatedToolCalls, stripHallucinationXml, CORRECTION_PROMPT } from "./honesty.js";

describe("honesty.detectHallucinatedToolCalls", () => {
  it("returns empty for clean text", () => {
    expect(detectHallucinatedToolCalls("Hello, the time is now.")).toEqual([]);
  });
  it("flags <invoke> Anthropic-style tool calls", () => {
    const r = detectHallucinatedToolCalls('I will use <invoke name="bash">...</invoke>');
    expect(r.length).toBe(1);
    expect(r[0]).toContain("invoke");
  });
  it("flags OpenAI-style <function_calls>", () => {
    const r = detectHallucinatedToolCalls("<function_calls>\n<invoke>...</invoke>\n</function_calls>");
    expect(r.length).toBe(2);
  });
  it("flags fabricated <function_result>", () => {
    const r = detectHallucinatedToolCalls('<function_result>{"name":"fake"}</function_result>');
    expect(r.length).toBe(1);
    expect(r[0]).toContain("function_result");
  });
  it("flags Claude Code <Skill> tool", () => {
    const r = detectHallucinatedToolCalls('<Skill name="whitepages"/>');
    expect(r.length).toBe(1);
    expect(r[0]).toContain("Skill");
  });
  it("flags truncated <function_calls / <function_result missing > (codex pass 7 regression)", () => {
    // The detector patterns must cover the same shapes as the scrubber.
    // Without \b, the literal `>` form silently missed truncated stream
    // cutoffs and warn/correct mode wouldn't fire even though the
    // scrubber tests pass.
    expect(detectHallucinatedToolCalls("leaking <function_result").length).toBeGreaterThan(0);
    expect(detectHallucinatedToolCalls("starting <function_calls").length).toBeGreaterThan(0);
    // And the well-formed cases still trigger.
    expect(detectHallucinatedToolCalls("<function_result>{}</function_result>").length).toBeGreaterThan(0);
    expect(detectHallucinatedToolCalls("<function_calls>x</function_calls>").length).toBeGreaterThan(0);
  });
});

describe("honesty.stripHallucinationXml", () => {
  it("returns clean text unchanged", () => {
    const r = stripHallucinationXml("Hello, the time is now.");
    expect(r.stripped).toBe(false);
    expect(r.text).toBe("Hello, the time is now.");
  });
  it("strips paired <function_calls>...</function_calls> block", () => {
    const r = stripHallucinationXml(
      "before\n<function_calls>\n<invoke>x</invoke>\n</function_calls>\nafter",
    );
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/function_calls/);
    expect(r.text).toMatch(/before/);
    expect(r.text).toMatch(/after/);
  });
  it("strips paired <function_result> blocks with attributes and preserves trailing text (codex pass 8 regression)", () => {
    // The detector matches <function_result\b (any attrs); the paired
    // scrub pattern must accept the same shape, otherwise the EOS
    // fallback strips legitimate text after the close.
    const r = stripHallucinationXml(
      '<function_result name="x">{"a":1}</function_result> final answer',
    );
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/<function_result/);
    expect(r.text).toMatch(/final answer/);
  });
  it("strips paired <function_calls> with whitespace/attrs and preserves trailing text", () => {
    const r = stripHallucinationXml(
      '<function_calls foo="bar">\n<invoke>x</invoke>\n</function_calls> continuing',
    );
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/function_calls/);
    expect(r.text).toMatch(/continuing/);
  });
  it("strips paired <invoke>...</invoke> block with attrs", () => {
    const r = stripHallucinationXml('use <invoke name="bash"><parameter>x</parameter></invoke> ok');
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/invoke/i);
    expect(r.text).toMatch(/use\s*ok/);
  });
  it("strips self-closing <Skill ... />", () => {
    const r = stripHallucinationXml('see <Skill name="whitepages" /> here');
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/Skill/);
  });
  it("strips self-closing tags whose attrs contain slashes (paths, URLs)", () => {
    // Regression test for codex finding round 1 [P2] (v0.1.10):
    // earlier [^/]* form stopped at the first slash inside an attribute
    // value, leaving the fabricated tag in the displayed text.
    const samples = [
      'check <Skill path="/tmp/foo" /> here',
      'edit <str_replace_editor path="/tmp/x.txt" /> then continue',
      'visit <Skill href="https://example.com/path" /> for more',
    ];
    for (const s of samples) {
      const r = stripHallucinationXml(s);
      expect(r.stripped).toBe(true);
      expect(r.text).not.toMatch(/<Skill|<str_replace_editor/);
    }
  });
  it("strips paired <function_result>...</function_result> block", () => {
    const r = stripHallucinationXml('<function_result>{"name":"fake"}</function_result> sigh');
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/function_result/);
    expect(r.text).toMatch(/sigh/);
  });
  it("strips orphan opening <invoke> tag and treats trailing text as fake body", () => {
    // Updated semantic (codex pass 6): when an orphan opening tag appears,
    // the rest of the message is presumed to be its fake body — that's
    // the most defensive scrub for malformed/truncated tool-call XML.
    // Anything that follows an orphan <invoke> is treated as fabricated
    // content; legitimate prose AFTER an orphan tag is not a realistic
    // model output (paired/self-closed forms exist for that). Prose
    // BEFORE the orphan tag is preserved — see the test below.
    const r = stripHallucinationXml('starting <invoke name="bash"> then prose');
    expect(r.stripped).toBe(true);
    expect(r.text).not.toMatch(/<invoke|then prose/);
    expect(r.text).toMatch(/starting/);
  });
  it("strips orphan opening <Skill> and <str_replace_editor> tags (codex pass 2 regression)", () => {
    // Models sometimes emit a truncated opening tag with no close — e.g.
    // `<Skill name="whitepages">` with no `/>` and no `</Skill>`. The
    // detector flags these; the scrubber must too, otherwise warn/correct
    // mode lies about what it scrubbed.
    const samples = [
      'starting <Skill name="whitepages"> then prose',
      'before <str_replace_editor path="/tmp/x" command="create"> after',
    ];
    for (const s of samples) {
      const r = stripHallucinationXml(s);
      expect(r.stripped).toBe(true);
      expect(r.text).not.toMatch(/<Skill|<str_replace_editor/);
    }
  });
  it("strips truncated mid-tag forms missing the closing > (codex pass 4 regression)", () => {
    // Stream cut off before the closing bracket. detectHallucinatedToolCalls
    // flags these because its pattern is just `<TagName\b`; the scrubber
    // must match the same shape via the `(?:>|$)` alternation. Each input
    // is constructed so the truncation reaches end-of-string.
    const samples = [
      'I will use <invoke name="bash"',
      'see <Skill name="whitepages"',
      'edit <str_replace_editor path="/tmp/x"',
      'starting <function_calls',
      'leaking <function_result',
    ];
    for (const s of samples) {
      const r = stripHallucinationXml(s);
      expect(r.stripped).toBe(true);
      expect(r.text).not.toMatch(/<invoke|<Skill|<str_replace_editor|<function_calls|<function_result/);
    }
  });
  it("strips orphan tag bodies through end-of-string (codex pass 6 regression)", () => {
    // Codex pass 6: tag-only orphan patterns left the body visible. The
    // defensive fallback now consumes the entire tail when a tag is
    // orphan/truncated. By the time these patterns run, paired forms are
    // gone — so consuming to end-of-string is the right semantics for
    // malformed/truncated XML.
    const samples = [
      '<function_result>{"fake":true}',
      '<invoke name="bash">rm -rf /',
      '<function_calls>\n<invoke>nested</invoke>\nfake body text here',
      '<Skill name="x">prose continues',
    ];
    for (const s of samples) {
      const r = stripHallucinationXml(s);
      expect(r.stripped).toBe(true);
      expect(r.text.trim()).toBe("");
    }
  });
  it("preserves preceding real prose when stripping orphan tag bodies", () => {
    const r = stripHallucinationXml(
      'I cannot run bash, but here is what the call would look like:\n<invoke name="bash">echo hi',
    );
    expect(r.stripped).toBe(true);
    expect(r.text).toMatch(/I cannot run bash/);
    expect(r.text).not.toMatch(/<invoke|echo hi/);
  });
  it("strips <parameter> children of truncated <invoke> (codex pass 5 regression)", () => {
    // Truncated Anthropic-style invoke that's missing the </invoke> close.
    // The opening tag would be stripped by the orphan-invoke pattern, but
    // the <parameter>...</parameter> child(ren) would survive into the
    // user-visible message without the dedicated <parameter> patterns.
    const samples = [
      '<invoke name="bash"><parameter name="command">echo x</parameter>',
      '<invoke name="bash">\n<parameter name="command">rm -rf /</parameter>\n<parameter name="cwd">/tmp</parameter>',
      'before <parameter name="command">ls</parameter> after',
    ];
    for (const s of samples) {
      const r = stripHallucinationXml(s);
      expect(r.stripped).toBe(true);
      expect(r.text).not.toMatch(/<invoke|<parameter/);
    }
  });
  it("collapses blank lines introduced by strips", () => {
    const r = stripHallucinationXml(
      "para1\n\n<function_calls>\nstuff\n</function_calls>\n\n\npara2",
    );
    expect(r.stripped).toBe(true);
    expect(r.text).toMatch(/para1\n\npara2/);
  });
});

describe("adapter — guardrails.hallucinationDetector", () => {
  function messageEndWithXml(): { type: string; message: any } {
    return {
      type: "message_end",
      message: {
        role: "assistant",
        content:
          "I'll look it up.\n<function_calls>\n<invoke name=\"bash\"><parameter name=\"command\">echo x</parameter></invoke>\n</function_calls>\nResult: { fake: true }",
        stopReason: "end_turn",
      },
    };
  }

  it("block mode (default when guardrails omitted): emits error event and ends with reason=error", async () => {
    const events: WireEvent[] = [];
    const ended = runSession(fixture(), (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    for (const fn of captured.subscribers) fn(messageEndWithXml());
    await flushAndShutdown();
    await ended;

    const errors = events.filter((e) => e.type === "error");
    const warnings = events.filter((e) => e.type === "warning");
    const ends = events.filter((e) => e.type === "session.ended");
    expect(errors.length).toBeGreaterThan(0);
    expect(warnings.length).toBe(0);
    expect((errors[0].data as any).kind).toBe("hallucinated_tool_call");
    expect((errors[0].data as any).mode).toBe("block");
    expect((ends[0].data as any).reason).toBe("error");
    expect(captured.promptCalls).toEqual(["Tell me the time"]); // no correction re-prompt
  });

  it("warn mode: emits warning event, scrubs XML from message text, ends completed", async () => {
    const spec = fixture();
    spec.guardrails = { hallucinationDetector: "warn" };
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    for (const fn of captured.subscribers) fn(messageEndWithXml());
    await flushAndShutdown();
    await ended;

    const errors = events.filter((e) => e.type === "error");
    const warnings = events.filter((e) => e.type === "warning");
    const messages = events.filter((e) => e.type === "message");
    const ends = events.filter((e) => e.type === "session.ended");
    expect(errors.length).toBe(0);
    expect(warnings.length).toBe(1);
    expect((warnings[0].data as any).kind).toBe("hallucinated_tool_call");
    expect((warnings[0].data as any).mode).toBe("warn");
    expect(messages.length).toBe(1);
    expect((messages[0].data as any).text).not.toMatch(/function_calls|<invoke/i);
    expect((messages[0].data as any).text).toMatch(/I'll look it up/);
    expect((ends[0].data as any).reason).toBe("completed");
    expect(captured.promptCalls).toEqual(["Tell me the time"]); // no correction re-prompt in warn mode
  });

  it("correct mode: warns, scrubs, AND dispatches a corrective re-prompt once", async () => {
    const spec = fixture();
    spec.guardrails = { hallucinationDetector: "correct" };
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    for (const fn of captured.subscribers) fn(messageEndWithXml());
    // Resolve the first prompt so the post-prompt block in runSession
    // can dispatch the correction. flushAndShutdown also drains the
    // second prompt's resolver.
    await flushAndShutdown();
    // After the first prompt resolves, runSession calls session.prompt
    // again with CORRECTION_PROMPT. That prompt queues a new resolver
    // that we need to drain a second time.
    await new Promise((r) => setImmediate(r));
    for (const r of captured.promptResolvers) r();
    captured.promptResolvers = [];
    await ended;

    const warnings = events.filter((e) => e.type === "warning");
    const ends = events.filter((e) => e.type === "session.ended");
    expect(warnings.length).toBe(1);
    expect((warnings[0].data as any).mode).toBe("correct");
    expect((ends[0].data as any).reason).toBe("completed");
    expect(captured.promptCalls.length).toBe(2);
    expect(captured.promptCalls[0]).toBe("Tell me the time");
    expect(captured.promptCalls[1]).toBe(CORRECTION_PROMPT);
  });

  it("correct mode: cap one correction even if multiple hallucinated message_ends arrive", async () => {
    const spec = fixture();
    spec.guardrails = { hallucinationDetector: "correct" };
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    // Three hallucinated assistant messages on the first turn.
    for (const fn of captured.subscribers) {
      fn(messageEndWithXml());
      fn(messageEndWithXml());
      fn(messageEndWithXml());
    }
    await flushAndShutdown();
    await new Promise((r) => setImmediate(r));
    for (const r of captured.promptResolvers) r();
    captured.promptResolvers = [];
    await ended;

    // Three warnings (one per message_end), but exactly one correction re-prompt.
    expect(events.filter((e) => e.type === "warning").length).toBe(3);
    expect(captured.promptCalls.length).toBe(2);
    expect(captured.promptCalls[1]).toBe(CORRECTION_PROMPT);
  });

  it("correct mode: does NOT re-prompt when the primary turn ended with an error (codex pass 5 regression)", async () => {
    // If Pi reports a terminal failure (stopReason=error) on a message_end
    // that also has hallucinated XML, correctionRequested is true but
    // errorMessage is also set. We must NOT issue a corrective re-prompt
    // after a terminal failure — the session is going to end with
    // reason=error regardless, so the extra prompt only burns tokens.
    const spec = fixture();
    spec.guardrails = { hallucinationDetector: "correct" };
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    for (const fn of captured.subscribers) {
      fn({
        type: "message_end",
        message: {
          role: "assistant",
          content: 'will use <invoke name="bash">...</invoke>',
          stopReason: "error",
        },
      });
    }
    await flushAndShutdown();
    await new Promise((r) => setImmediate(r));
    // Drain any second-prompt resolvers if the bug returns; if the gate
    // works correctly there will be none.
    for (const r of captured.promptResolvers) r();
    captured.promptResolvers = [];
    await ended;

    const ends = events.filter((e) => e.type === "session.ended");
    expect((ends[0].data as any).reason).toBe("error");
    // Exactly one prompt call (the original task) — NO correction prompt.
    expect(captured.promptCalls.length).toBe(1);
    expect(captured.promptCalls).toEqual(["Tell me the time"]);
  });

  it("ignores hallucinated XML in user-role message_end (codex pass 3 regression)", async () => {
    // In correct mode, the corrective re-prompt CORRECTION_PROMPT itself
    // mentions <invoke>, <function_calls>, and <Skill> by name as part of
    // its instructions to the model. Pi emits a user-role message_end for
    // that prompt. Without a role gate, the runtime would flag its own
    // corrective re-prompt as a hallucination and emit a spurious warning.
    const spec = fixture();
    spec.guardrails = { hallucinationDetector: "warn" };
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    // A user message_end carrying every XML pattern the detector knows
    // about. We expect zero warnings, zero errors, no scrubbing.
    for (const fn of captured.subscribers) {
      fn({
        type: "message_end",
        message: {
          role: "user",
          content:
            "Reminder: do not write <invoke>, <function_calls>, or <Skill> in your message.",
          stopReason: "end_turn",
        },
      });
    }
    await flushAndShutdown();
    await ended;

    expect(events.filter((e) => e.type === "warning")).toEqual([]);
    expect(events.filter((e) => e.type === "error")).toEqual([]);
    // And the message event we'd emit for it (if any) is unmodified.
    const messages = events.filter((e) => e.type === "message");
    if (messages.length > 0) {
      expect((messages[0].data as any).text).toMatch(/<invoke>/);
    }
  });

  it("unknown mode value falls back to block with a stderr warning", async () => {
    const spec = fixture();
    // Deliberate typo at the wire level — the schema rejects this at validate
    // time, but the runtime must fail safe if it ever sees a bad value.
    spec.guardrails = { hallucinationDetector: "loud" as any };
    const stderrSpy = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    const events: WireEvent[] = [];
    const ended = runSession(spec, (ev) => events.push(ev));
    await new Promise((r) => setImmediate(r));
    for (const fn of captured.subscribers) fn(messageEndWithXml());
    await flushAndShutdown();
    await ended;

    expect(stderrSpy).toHaveBeenCalled();
    const warnCall = stderrSpy.mock.calls.find((c) =>
      String(c[0]).includes("unknown spec.guardrails.hallucinationDetector"),
    );
    expect(warnCall).toBeDefined();
    // And the behavior matches block mode.
    expect(events.filter((e) => e.type === "error").length).toBeGreaterThan(0);
    expect(events.filter((e) => e.type === "warning").length).toBe(0);
    stderrSpy.mockRestore();
  });
});

describe("adapter", () => {
  it("constructs DefaultResourceLoader with resolved entrypoint paths", async () => {
    const events: WireEvent[] = [];
    const ended = runSession(fixture(), (ev) => events.push(ev));
    await flushAndShutdown();
    await ended;

    expect(captured.resourceLoaderArgs).toBeDefined();
    expect(captured.resourceLoaderArgs.additionalExtensionPaths).toEqual([
      "/abs/tools/get_time/entrypoint.ts",
      "/abs/extensions/audit-log/entrypoint.ts",
    ]);
  });

  it("passes the loader as resourceLoader to createAgentSession", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.createAgentArgs.resourceLoader).toBeDefined();
  });

  it("looks up the model via pi-ai getBuiltinModel(provider, name)", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.modelGetCalls).toEqual([["anthropic", "claude-sonnet-5"]]);
    // Use objectContaining so the assertion is stable whether or not
    // ANTHROPIC_BASE_URL is set (the adapter may add a baseUrl field to the
    // model object when the env var is present).
    expect(captured.createAgentArgs.model).toEqual(expect.objectContaining({ __mocked: "model" }));
  });

  it("submits spec.task via session.prompt exactly once", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.promptCalls).toEqual(["Tell me the time"]);
  });

  it("sets AGENT_CONTROLLER_EXT_CONFIG before session creation", async () => {
    delete process.env.AGENT_CONTROLLER_EXT_CONFIG;
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    const parsed = JSON.parse(process.env.AGENT_CONTROLLER_EXT_CONFIG ?? "{}");
    expect(parsed["audit-log"]).toEqual({ path: "./audit.log" });
  });

  it("includes spec.tools[].config entries in AGENT_CONTROLLER_EXT_CONFIG keyed by tool name", async () => {
    delete process.env.AGENT_CONTROLLER_EXT_CONFIG;
    const spec = fixture();
    // Patch the tool to carry a config block.
    spec.tools = [{ name: "get_time", entrypoint: "/abs/tools/get_time/entrypoint.ts", config: { zone: "UTC" } }];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const parsed = JSON.parse(process.env.AGENT_CONTROLLER_EXT_CONFIG ?? "{}");
    expect(parsed["get_time"]).toEqual({ zone: "UTC" });
  });

  it("extensions win over tools on name collision in AGENT_CONTROLLER_EXT_CONFIG", async () => {
    delete process.env.AGENT_CONTROLLER_EXT_CONFIG;
    const spec = fixture();
    // Give the tool and the extension the same name to force a collision.
    spec.tools = [{ name: "audit-log", entrypoint: "/abs/tools/audit-log/entrypoint.ts", config: { from: "tool" } }];
    spec.extensions = [{ name: "audit-log", entrypoint: "/abs/extensions/audit-log/entrypoint.ts", config: { from: "ext" } }];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const parsed = JSON.parse(process.env.AGENT_CONTROLLER_EXT_CONFIG ?? "{}");
    // Extension config wins.
    expect(parsed["audit-log"]).toEqual({ from: "ext" });
  });

  // Finding 1: tool allowlist enforcement (non-MCP path)
  it("passes the ADL spec.tools allowlist to createAgentSession when no mcpServers", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    // Fixture declares one tool: get_time. We expect tools: ["get_time"]
    // so Pi's tool-activation logic sees an explicit allowlist that
    // (a) excludes built-in read/bash/edit/write and
    // (b) activates the extension-registered get_time tool.
    expect(captured.createAgentArgs.tools).toEqual(["get_time"]);
    // noTools is not used in the non-MCP path.
    expect(captured.createAgentArgs.noTools).toBeUndefined();
  });

  // MCP tool allowlist fix: when mcpServers is non-empty, passing tools: []
  // sets allowedToolNames to an empty Set in Pi, which blocks ALL tools from
  // _toolRegistry — including MCP tools registered post-session-start by
  // pi-mcp-extension. The fix: use noTools: "builtin" instead of tools: [].
  it("uses noTools:'builtin' instead of tools:[] when mcpServers is non-empty (MCP tool allowlist fix)", async () => {
    // mcpFixture() has tools: [get_time] from fixture() PLUS mcpServers — the non-MCP
    // tools are present alongside MCP. With the fix, mcpServers presence switches
    // to noTools mode so MCP tools can register and activate.
    const spec = mcpFixture();
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    // MUST NOT pass tools: (any array) because an empty or partial allowlist
    // would block MCP tools from entering _toolRegistry.
    expect(captured.createAgentArgs.tools).toBeUndefined();
    // MUST pass noTools: "builtin" to suppress Pi's read/bash/edit/write.
    expect(captured.createAgentArgs.noTools).toBe("builtin");
  });

  it("uses noTools:'builtin' when mcpServers is non-empty and spec.tools is empty", async () => {
    // Pure MCP spec: no declared tools, just mcpServers.
    const spec = mcpFixture();
    spec.tools = [];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    expect(captured.createAgentArgs.tools).toBeUndefined();
    expect(captured.createAgentArgs.noTools).toBe("builtin");
  });

  // Finding 2: unknown model error
  it("throws a clear error when getBuiltinModel returns undefined", async () => {
    captured.modelReturnUndefined = true;
    const spec = fixture();
    spec.model = { provider: "openai", name: "does-not-exist" };
    await expect(runSession(spec, () => {})).rejects.toThrow(
      "Model openai/does-not-exist not found.",
    );
  });

  // Finding 3: persona system prompt
  it("passes persona as systemPrompt to DefaultResourceLoader when spec.persona is set", async () => {
    const spec = fixture();
    spec.persona = { role: "Test assistant", instructions: "Be concise." };
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.systemPrompt).toContain("# Role\nTest assistant");
    expect(captured.resourceLoaderArgs.systemPrompt).toContain("# Instructions\nBe concise.");
  });

  it("still sets systemPrompt to the honesty preamble when spec.persona is absent", async () => {
    // After the v0.1.8 guardrails, systemPrompt is never undefined — the
    // honesty preamble is always injected. This test pins that behaviour.
    const spec = fixture();
    delete (spec as any).persona;
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.systemPrompt).toContain("Honesty rules");
    // Persona sections are absent.
    expect(captured.resourceLoaderArgs.systemPrompt).not.toContain("# Role");
    expect(captured.resourceLoaderArgs.systemPrompt).not.toContain("# Instructions");
  });

  // v0.1.8 guardrails: honesty preamble is always at the start of systemPrompt.

  it("prepends the HONESTY_PREAMBLE before persona content", async () => {
    const spec = fixture();
    spec.persona = { role: "Test assistant", instructions: "Be concise." };
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const sp: string = captured.resourceLoaderArgs.systemPrompt;
    const honestyIdx = sp.indexOf("Honesty rules");
    const personaIdx = sp.indexOf("Test assistant");
    expect(honestyIdx).toBeGreaterThanOrEqual(0);
    expect(personaIdx).toBeGreaterThanOrEqual(0);
    expect(honestyIdx).toBeLessThan(personaIdx);
  });

  it("wraps each inlined skill body with the framing reminder", async () => {
    const spec = fixture();
    // Make sure the test sees a skill in the appendSystemPrompt path.
    spec.skills = [{ name: "fake-skill", entrypoint: "/tmp/fake-skill/SKILL.md" }];
    // Seed the fake skill body in the mocked filesystem.
    fakeFs.files.set("/tmp/fake-skill/SKILL.md", "---\nname: fake-skill\n---\nDo a thing.");
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const appended: string[] = captured.resourceLoaderArgs.appendSystemPrompt ?? [];
    expect(appended.length).toBeGreaterThan(0);
    const body = appended[0];
    expect(body).toContain("# Skill: fake-skill");
    expect(body).toContain("you do not have");
    expect(body).toContain("Do a thing.");
  });

  // Finding 1: ambient Pi extensions must be suppressed to enforce ADL allowlist.
  // noExtensions: true prevents DefaultResourceLoader.reload() from scanning
  // ~/.pi/agent/extensions/ and <cwd>/.pi/extensions/ for ambient extensions.
  it("passes noExtensions: true to DefaultResourceLoader to block ambient extensions", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.noExtensions).toBe(true);
  });

  // Milestone v0.1.2: Skill kind — additionalSkillPaths and noSkills wiring.
  it("passes additionalSkillPaths as parent dirs of SKILL.md entrypoints", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    // fixture() declares one skill with entrypoint /abs/skills/example-time-skill/SKILL.md.
    // The adapter should strip SKILL.md and pass the parent directory.
    expect(captured.resourceLoaderArgs.additionalSkillPaths).toEqual([
      "/abs/skills/example-time-skill",
    ]);
  });

  it("passes noSkills: true to block ambient skill loading from ~/.pi/agent/skills/", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.noSkills).toBe(true);
  });

  it("passes empty additionalSkillPaths when spec.skills is empty", async () => {
    const spec = fixture();
    spec.skills = [];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.additionalSkillPaths).toEqual([]);
  });

  // Finding 2: spec.model.temperature must reach the provider via onPayload —
  // BUT only when extended thinking is not enabled (Anthropic API requires
  // temperature=1 with thinking on, so we skip the merge to avoid silently
  // breaking tool calling).
  it("installs an onPayload hook that injects temperature when spec.model.temperature is set and thinking is off", async () => {
    const spec = fixture();
    spec.model = { provider: "anthropic", name: "claude-sonnet-5", temperature: 0.3 };
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    expect(captured.agentRef.onPayload).toBeTypeOf("function");
    // No `thinking` in the payload → temperature is merged in.
    const result = await captured.agentRef.onPayload({ model: "claude-3", messages: [] }, {});
    expect(result).toEqual({ model: "claude-3", messages: [], temperature: 0.3 });
  });

  // Codex finding round 4 [P2]: declared entrypoint load failures must abort.
  it("throws when a declared entrypoint fails to load", async () => {
    const spec = fixture();
    loaderErrors.errors = [
      { path: spec.tools[0].entrypoint, error: "module not found" },
    ];
    await expect(runSession(spec, () => {})).rejects.toThrow(
      /Failed to load ADL-declared extensions/,
    );
  });

  it("ignores load errors for paths the ADL did not declare", async () => {
    const spec = fixture();
    loaderErrors.errors = [
      { path: "/some/unrelated/extension/entrypoint.ts", error: "module not found" },
    ];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended; // resolves cleanly
  });

  // Codex finding round 4 [P1]: terminal Pi errors must surface as
  // session.ended { reason: "error" }, not "completed".
  it("emits session.ended reason=error when Pi message_end reports stopReason=error", async () => {
    const events: WireEvent[] = [];
    const ended = runSession(fixture(), (ev) => events.push(ev));
    // Wait for runSession to reach the prompt call (mocked prompt is now blocking).
    await new Promise((r) => setImmediate(r));
    // Simulate Pi reporting an error message_end BEFORE prompt resolves.
    for (const fn of captured.subscribers) {
      fn({ type: "message_end", message: { stopReason: "error" } });
    }
    // Now resolve the prompt and let runSession finish.
    for (const r of captured.promptResolvers) r();
    captured.promptResolvers = [];
    await ended;

    const endEv = events.find((e) => e.type === "session.ended");
    expect(endEv).toBeDefined();
    expect((endEv?.data as any).reason).toBe("error");
    // An explicit error event should also be present for the CLI's exit-code logic.
    expect(events.some((e) => e.type === "error")).toBe(true);
  });

  it("does NOT inject temperature when payload already has thinking enabled", async () => {
    const spec = fixture();
    spec.model = { provider: "anthropic", name: "claude-sonnet-5", temperature: 0.3 };
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    expect(captured.agentRef.onPayload).toBeTypeOf("function");
    // `thinking` present → temperature is left at whatever Pi/Anthropic default.
    const payload = { model: "claude-3", messages: [], thinking: { type: "enabled" }, tools: [{ name: "t" }] };
    const result = await captured.agentRef.onPayload(payload, {});
    expect((result as any).temperature).toBeUndefined();
    // Tools and thinking must survive untouched.
    expect((result as any).tools).toEqual([{ name: "t" }]);
    expect((result as any).thinking).toEqual({ type: "enabled" });
  });

  it("does NOT install an onPayload hook when spec.model.temperature is absent", async () => {
    const spec = fixture();
    // fixture() already omits temperature; verify the agent.onPayload stays undefined.
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.agentRef.onPayload).toBeUndefined();
  });

  // Session resumption tests (Milestone v0.1.1)

  it("uses SessionManager.inMemory when spec.sessionId is absent", async () => {
    const spec = fixture();
    // fixture() has no sessionId field — verify inMemory is called.
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const inMemoryCalls = captured.sessionManagerCalls.filter((c) => c.method === "inMemory");
    expect(inMemoryCalls).toHaveLength(1);
    // continueRecent must NOT be called.
    const continueCalls = captured.sessionManagerCalls.filter((c) => c.method === "continueRecent");
    expect(continueCalls).toHaveLength(0);
    // The sessionManager is forwarded to createAgentSession.
    expect(captured.createAgentArgs.sessionManager).toBeDefined();
  });

  it("uses SessionManager.continueRecent with agentctl/<id> dir when spec.sessionId is set", async () => {
    const spec = fixture();
    spec.sessionId = "my-demo-session";
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    const continueCalls = captured.sessionManagerCalls.filter((c) => c.method === "continueRecent");
    expect(continueCalls).toHaveLength(1);
    // First arg is cwd, second is the session dir path.
    const sessionDir: string = continueCalls[0].args[1];
    expect(sessionDir).toContain("agentctl");
    expect(sessionDir).toContain("my-demo-session");
    // The path should be under the mocked agentDir.
    expect(sessionDir).toContain("/mock/agent/dir");
    // inMemory must NOT be called.
    const inMemoryCalls = captured.sessionManagerCalls.filter((c) => c.method === "inMemory");
    expect(inMemoryCalls).toHaveLength(0);
    // The sessionManager is forwarded to createAgentSession.
    expect(captured.createAgentArgs.sessionManager).toBeDefined();
  });

  // Milestone v0.1.5: MCPServer kind — mcp.json and pi-mcp-extension wiring.

  function mcpFixture(): CompiledSpec {
    return {
      ...fixture(),
      mcpServers: [
        {
          name: "time-server",
          transport: "stdio",
          command: "npx",
          args: ["-y", "@modelcontextprotocol/server-time"],
          lifecycle: "eager",
        } satisfies MCPServer,
      ],
    };
  }

  it("writes .pi/mcp.json with the correct keyed-by-name structure when mcpServers is non-empty", async () => {
    const ended = runSession(mcpFixture(), () => {});
    await flushAndShutdown();
    await ended;

    // Exactly one write should have been made to .pi/mcp.json.
    const mcpWrite = fakeFs.writes.find(([p]) => p.endsWith(".pi/mcp.json"));
    expect(mcpWrite).toBeDefined();

    const written = JSON.parse(mcpWrite![1]);
    // Servers are keyed by name, not an array.
    expect(written.mcpServers).toBeDefined();
    expect(written.mcpServers["time-server"]).toBeDefined();
    expect(written.mcpServers["time-server"].transport).toBe("stdio");
    expect(written.mcpServers["time-server"].command).toBe("npx");
    expect(written.mcpServers["time-server"].args).toEqual(["-y", "@modelcontextprotocol/server-time"]);
    expect(written.mcpServers["time-server"].lifecycle).toBe("eager");
    // Settings block should be present with default toolPrefix.
    expect(written.settings?.toolPrefix).toBe("mcp");
  });

  it("appends pi-mcp-extension entrypoint to additionalExtensionPaths when mcpServers is non-empty", async () => {
    const ended = runSession(mcpFixture(), () => {});
    await flushAndShutdown();
    await ended;

    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    expect(paths).toContain("/mock/node_modules/pi-mcp-extension/src/index.ts");
  });

  it("does NOT write .pi/mcp.json and does NOT add pi-mcp-extension when mcpServers is empty", async () => {
    const spec = fixture();
    spec.mcpServers = [];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    const mcpWrite = fakeFs.writes.find(([p]) => p.endsWith(".pi/mcp.json"));
    expect(mcpWrite).toBeUndefined();

    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    expect(paths).not.toContain("/mock/node_modules/pi-mcp-extension/src/index.ts");
  });

  it("does NOT write .pi/mcp.json when mcpServers is absent", async () => {
    const spec = fixture();
    // mcpServers is undefined in the base fixture.
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    const mcpWrite = fakeFs.writes.find(([p]) => p.endsWith(".pi/mcp.json"));
    expect(mcpWrite).toBeUndefined();
  });

  it("throws when .pi/mcp.json already exists with different contents", async () => {
    // The single per-cwd .pi/mcp.json is NOT overwritten when it differs:
    // overwriting would let two overlapping runs from the same cwd race and
    // cross-load each other's MCP servers (codex pass 3 of slice 7.5). It
    // fails loudly instead. `agentctl run --workspace` relies on this —
    // reusing the same workspace is idempotent (identical content), a
    // different workspace from the same cwd fails here rather than racing.
    const cwd = process.cwd();
    const mcpPath = `${cwd}/.pi/mcp.json`;
    fakeFs.files.set(mcpPath, JSON.stringify({ mcpServers: { other: { transport: "stdio", command: "other" } } }));

    await expect(runSession(mcpFixture(), () => {})).rejects.toThrow(
      /Cannot write MCP config/,
    );
  });

  it("reports a concurrent-run error when .pi/mcp.json exists but is empty (partial write)", async () => {
    // The wx winner created the file but hasn't finished writing it yet
    // (a concurrent same-cwd run). The empty read must surface the
    // concurrency limitation, not a misleading "different contents".
    // Codex pass 6 of slice 7.5.
    const cwd = process.cwd();
    fakeFs.files.set(`${cwd}/.pi/mcp.json`, "");

    await expect(runSession(mcpFixture(), () => {})).rejects.toThrow(
      /being created by a concurrent run/,
    );
  });

  it("skips write (idempotent) when .pi/mcp.json already contains identical contents", async () => {
    const cwd = process.cwd();
    const mcpPath = `${cwd}/.pi/mcp.json`;
    // Pre-populate with the exact same content we would write.
    const spec = mcpFixture();
    // Build the expected content manually.
    const expected = JSON.stringify({
      settings: { toolPrefix: "mcp" },
      mcpServers: {
        "time-server": {
          transport: "stdio",
          lifecycle: "eager",
          command: "npx",
          args: ["-y", "@modelcontextprotocol/server-time"],
        },
      },
    }, null, 2);
    fakeFs.files.set(mcpPath, expected);

    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    // No new writes should have happened since content matches.
    const mcpWrites = fakeFs.writes.filter(([p]) => p.endsWith(".pi/mcp.json"));
    expect(mcpWrites).toHaveLength(0);
  });

  // Milestone v0.1.3: Subagent kind

  function subagentFixture(): CompiledSpec {
    return {
      ...fixture(),
      subagents: [
        { name: "sql-explorer", entrypoint: "/abs/agents/sql-explorer.md" },
      ],
    };
  }

  it("materializes subagent .md files to .pi/agents/ when subagents are declared", async () => {
    // Pre-populate fake FS so copyFileSync can read the source file.
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    // Exactly one copy should have been made to .pi/agents/sql-explorer.md.
    const agentCopy = fakeFs.copies.find(([_src, dest]) => dest.endsWith(".pi/agents/sql-explorer.md"));
    expect(agentCopy).toBeDefined();
    expect(agentCopy![0]).toBe("/abs/agents/sql-explorer.md");
  });

  it("appends the vendored subagent extension entrypoint to additionalExtensionPaths when subagents declared", async () => {
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    // The vendored extension path should end with extensions/subagent/entrypoint.ts.
    expect(paths.some((p) => p.endsWith("extensions/subagent/entrypoint.ts"))).toBe(true);
  });

  it("adds 'subagent' to the tool allowlist when subagents are declared", async () => {
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    expect(captured.createAgentArgs.tools).toContain("subagent");
    // Original declared tools should also be present.
    expect(captured.createAgentArgs.tools).toContain("get_time");
  });

  it("does NOT materialize agent files or add subagent tool when subagents is empty", async () => {
    const spec = fixture();
    spec.subagents = [];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    expect(fakeFs.copies).toHaveLength(0);
    expect(captured.createAgentArgs.tools).not.toContain("subagent");
  });

  it("skips copy (idempotent) when .pi/agents/ file already has identical content", async () => {
    const cwd = process.cwd();
    const agentPath = `${cwd}/.pi/agents/sql-explorer.md`;
    const content = "---\nname: sql-explorer\n---\nBody";
    // Pre-populate source and destination with identical content.
    fakeFs.files.set("/abs/agents/sql-explorer.md", content);
    fakeFs.files.set(agentPath, content);
    // Also pre-populate the tool extension destination so copyToolExtensionsToLocalAgentDir
    // skips it as well (idempotent). The destination is always named index.ts
    // (regardless of the source filename) so Pi's auto-discovery finds it.
    const toolContent = "tool entrypoint content";
    fakeFs.files.set("/abs/tools/get_time/entrypoint.ts", toolContent);
    fakeFs.files.set(`${cwd}/.pi/agent/extensions/get_time/index.ts`, toolContent);

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    // No new copy should have been made since content is identical.
    expect(fakeFs.copies).toHaveLength(0);
  });

  // ── Finding 2 (P2): tool copy must run even without ANTHROPIC_BASE_URL ──

  it("copies tool extensions to local agent dir even when ANTHROPIC_BASE_URL is not set", async () => {
    const savedBaseUrl = process.env.ANTHROPIC_BASE_URL;
    delete process.env.ANTHROPIC_BASE_URL;
    try {
      fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");
      fakeFs.files.set("/abs/tools/get_time/entrypoint.ts", "tool content");

      const ended = runSession(subagentFixture(), () => {});
      await flushAndShutdown();
      await ended;

      // copyFileSync should have been called for the tool, regardless of base URL.
      const toolCopy = fakeFs.copies.find(([_src, dest]) => dest.endsWith("extensions/get_time/index.ts"));
      expect(toolCopy).toBeDefined();
      expect(toolCopy![0]).toBe("/abs/tools/get_time/entrypoint.ts");
    } finally {
      if (savedBaseUrl !== undefined) process.env.ANTHROPIC_BASE_URL = savedBaseUrl;
    }
  });

  // ── Finding 3 (P2): skill dirs derived with path.dirname (platform-safe) ──

  it("derives skill dirs with path.dirname (no manual split)", async () => {
    const spec = fixture();
    // Use a Unix-style path — dirname should give the parent dir correctly.
    spec.skills = [{ name: "my-skill", entrypoint: "/some/deep/path/my-skill/SKILL.md" }];
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;
    expect(captured.resourceLoaderArgs.additionalSkillPaths).toEqual(["/some/deep/path/my-skill"]);
  });

  // ── Finding 4 (P2): stale .pi/agents/ entries are deleted before materializing ──

  it("removes stale .pi/agents/ .md files not in spec.subagents[] before writing", async () => {
    const cwd = process.cwd();
    const agentsDir = `${cwd}/.pi/agents`;
    const stale = `${agentsDir}/stale-agent.md`;
    const declared = `${agentsDir}/sql-explorer.md`;

    // Pre-populate fake FS: stale file exists, declared source exists.
    fakeFs.files.set(stale, "stale content");
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");
    // Mark the agentsDir as existing so readdirSync is called (existsSync returns true).
    fakeFs.dirs.add(agentsDir);

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    // The stale file should have been unlinked.
    expect(fakeFs.unlinks).toContain(stale);
    // The declared file should NOT be unlinked.
    expect(fakeFs.unlinks).not.toContain(declared);
    // The declared agent should have been copied.
    const agentCopy = fakeFs.copies.find(([_src, dest]) => dest.endsWith("sql-explorer.md"));
    expect(agentCopy).toBeDefined();
  });

  it("does not unlink non-.md files in .pi/agents/", async () => {
    const cwd = process.cwd();
    const agentsDir = `${cwd}/.pi/agents`;
    const nonMd = `${agentsDir}/README.txt`;

    fakeFs.files.set(nonMd, "not an agent file");
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");
    fakeFs.dirs.add(agentsDir);

    const ended = runSession(subagentFixture(), () => {});
    await flushAndShutdown();
    await ended;

    // Non-.md files must not be touched.
    expect(fakeFs.unlinks).not.toContain(nonMd);
  });

  // ── Codex pass-2 P2: fail closed if a stale agent cannot be removed ──

  it("throws when a stale .pi/agents/ file cannot be unlinked", async () => {
    const cwd = process.cwd();
    const agentsDir = `${cwd}/.pi/agents`;
    const stale = `${agentsDir}/stale-agent.md`;

    // Stale file exists, declared source exists, but unlinking the stale
    // file fails (e.g. read-only filesystem). Run must reject — leaving the
    // stale file would let the subagent extension's "project" scope still
    // discover it, defeating the ADL allowlist.
    fakeFs.files.set(stale, "stale content");
    fakeFs.files.set("/abs/agents/sql-explorer.md", "---\nname: sql-explorer\n---\nBody");
    fakeFs.dirs.add(agentsDir);
    fakeFs.unlinkFails.add(stale);

    await expect(runSession(subagentFixture(), () => {})).rejects.toThrow(
      /Failed to remove stale agent file/,
    );
  });

  // ── v0.1.6: spec.extensions[].source — source-bound extension tests ───────

  function sourceExtFixture(pkgName: string): CompiledSpec {
    return {
      ...fixture(),
      extensions: [
        {
          name: pkgName,
          entrypoint: "",
          source: `npm:${pkgName}`,
        },
      ],
    };
  }

  it("source-less extensions still load via entrypoint (regression)", async () => {
    const ended = runSession(fixture(), () => {});
    await flushAndShutdown();
    await ended;

    // fixture() has extensions: [{ name: "audit-log", entrypoint: "/abs/..." }]
    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    expect(paths).toContain("/abs/extensions/audit-log/entrypoint.ts");
  });

  it("source-bound extension triggers pi install when package is NOT installed", async () => {
    // pi-mcp-extension is NOT in installedPackages, so spawnSync will be called.
    // Provide a fake pi binary that existsSync returns true for.
    fakeFs.files.set("/fake/pi-bin", "#!/bin/sh");
    process.env.PI_BIN = "/fake/pi-bin";
    sourceFixture.piInstallExitCode = 0;
    const spec = sourceExtFixture("pi-mcp-extension");

    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    // After install the pi-managed path should have the package.json
    const pkgJsonPath = "/mock/agent/dir/npm/node_modules/pi-mcp-extension/package.json";
    expect(fakeFs.files.has(pkgJsonPath)).toBe(true);
    // The entrypoint should be in additionalExtensionPaths.
    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    expect(paths.some((p) => p.includes("pi-mcp-extension"))).toBe(true);
  });

  it("source-bound extension skips install when already installed in runtime node_modules", async () => {
    // Mark as installed via require.resolve path.
    sourceFixture.installedPackages.add("my-installed-ext");
    // Also ensure fakeFs has the package.json at the resolved path.
    fakeFs.files.set(
      "/mock/node_modules/my-installed-ext/package.json",
      JSON.stringify({ name: "my-installed-ext", pi: { extensions: ["./src/index.ts"] } }),
    );

    const spec = sourceExtFixture("my-installed-ext");
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    // spawnSync for pi install should NOT have been called (no pi-managed path was written).
    const pkgJsonPiPath = "/mock/agent/dir/npm/node_modules/my-installed-ext/package.json";
    expect(fakeFs.files.has(pkgJsonPiPath)).toBe(false);
    // Entrypoint is still in additionalExtensionPaths.
    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    expect(paths.some((p) => p.includes("my-installed-ext"))).toBe(true);
  });

  it("source-bound extension's resolved entrypoint joins additionalExtensionPaths", async () => {
    sourceFixture.installedPackages.add("cool-ext");
    fakeFs.files.set(
      "/mock/node_modules/cool-ext/package.json",
      JSON.stringify({ name: "cool-ext", pi: { extensions: ["./src/extension.ts"] } }),
    );

    const spec = sourceExtFixture("cool-ext");
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended;

    const paths: string[] = captured.resourceLoaderArgs.additionalExtensionPaths;
    // Should contain the resolved entrypoint from cool-ext's pi.extensions[0].
    expect(paths.some((p) => p.includes("cool-ext") && p.endsWith("extension.ts"))).toBe(true);
  });

  it("AGENT_CONTROLLER_NO_AUTO_INSTALL=1 causes a clear error when extension is missing", async () => {
    process.env.AGENT_CONTROLLER_NO_AUTO_INSTALL = "1";
    // Package is NOT installed.
    const spec = sourceExtFixture("missing-ext");

    await expect(runSession(spec, () => {})).rejects.toThrow(
      /Auto-installation is disabled/,
    );
  });

  it("AGENT_CONTROLLER_NO_AUTO_INSTALL=1 does NOT block an already-installed package", async () => {
    process.env.AGENT_CONTROLLER_NO_AUTO_INSTALL = "1";
    // Package IS already installed in Pi-managed path — found before guard is checked.
    fakeFs.files.set(
      "/mock/agent/dir/npm/node_modules/preinstalled-ext/package.json",
      JSON.stringify({ name: "preinstalled-ext", pi: { extensions: ["./src/index.ts"] } }),
    );

    const spec = sourceExtFixture("preinstalled-ext");
    const ended = runSession(spec, () => {});
    await flushAndShutdown();
    await ended; // should NOT throw
  });

  it("pi install failure propagates as a thrown error", async () => {
    sourceFixture.piInstallExitCode = 1; // non-zero → failure
    // Provide a real pi-like path so spawnSync is reached (not short-circuited).
    // We mark PI_BIN to something that existsSync returns true for via fakeFs.
    fakeFs.files.set("/fake/pi", "#!/bin/sh");
    process.env.PI_BIN = "/fake/pi";

    const spec = sourceExtFixture("fail-pkg");
    await expect(runSession(spec, () => {})).rejects.toThrow(
      /pi install npm:fail-pkg failed/,
    );
  });

  it("spec.installs[] non-empty triggers deprecation warning to stderr", async () => {
    const stderrWrites: string[] = [];
    const origWrite = process.stderr.write.bind(process.stderr);
    const spy = vi.spyOn(process.stderr, "write").mockImplementation((msg: any) => {
      stderrWrites.push(String(msg));
      return true;
    });

    try {
      const spec = fixture();
      (spec as any).installs = ["npm:some-old-pkg"];

      const ended = runSession(spec, () => {});
      await flushAndShutdown();
      await ended;

      const deprecationMsg = stderrWrites.find((m) => m.includes("DEPRECATION WARNING") && m.includes("spec.installs[]"));
      expect(deprecationMsg).toBeDefined();
    } finally {
      spy.mockRestore();
    }
  });

  it("spec.installs[] absent (or empty) produces no deprecation warning", async () => {
    const stderrWrites: string[] = [];
    const spy = vi.spyOn(process.stderr, "write").mockImplementation((msg: any) => {
      stderrWrites.push(String(msg));
      return true;
    });

    try {
      const spec = fixture(); // no installs field

      const ended = runSession(spec, () => {});
      await flushAndShutdown();
      await ended;

      const deprecationMsg = stderrWrites.find((m) => m.includes("DEPRECATION WARNING") && m.includes("spec.installs[]"));
      expect(deprecationMsg).toBeUndefined();
    } finally {
      spy.mockRestore();
    }
  });
});
