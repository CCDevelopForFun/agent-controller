/**
 * Canary tests for the subagent shim.
 *
 * `extensions/subagent/entrypoint.ts` no longer vendors Pi's subagent
 * extension — it loads `<pi>/examples/extensions/subagent/index.ts` in place
 * and overrides only the behaviors that conflict with agent-controller's model
 * (see that file's header). That buys us ~450 fewer copied lines, at the cost
 * of depending on a path inside pi's `examples/`, which is NOT a stable API
 * surface: a pi bump can move the file or reshape its default export.
 *
 * These tests exist so that failure lands here, at `npm test`, instead of at a
 * user's session start. Unlike adapter.test.ts they mock NOTHING — they load
 * the real shim through Pi's own `discoverAndLoadExtensions`, which is the same
 * jiti + alias-map path production uses.
 */

import { mkdtempSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { discoverAndLoadExtensions } from "@earendil-works/pi-coding-agent";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { applyProcessOverrides } from "../../extensions/subagent/entrypoint.ts";
import { resolveUpstreamSubagentPath } from "./adapter.ts";

const SHIM_PATH = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
  "extensions",
  "subagent",
  "entrypoint.ts",
);

/** Load the shim exactly as Pi does, returning the tools it registered. */
async function loadShim() {
  const scratch = mkdtempSync(join(tmpdir(), "ac-subagent-canary-"));
  const result = await discoverAndLoadExtensions([SHIM_PATH], scratch, scratch);
  return result;
}

describe("subagent shim over Pi's bundled example", () => {
  let savedUpstream: string | undefined;
  let savedPiBin: string | undefined;
  let savedAgentDir: string | undefined;

  beforeEach(() => {
    savedUpstream = process.env.AC_UPSTREAM_SUBAGENT_PATH;
    savedPiBin = process.env.AC_PI_BIN;
    savedAgentDir = process.env.AC_SUBAGENT_AGENT_DIR;
  });

  afterEach(() => {
    for (const [k, v] of [
      ["AC_UPSTREAM_SUBAGENT_PATH", savedUpstream],
      ["AC_PI_BIN", savedPiBin],
      ["AC_SUBAGENT_AGENT_DIR", savedAgentDir],
    ] as const) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });

  it("resolves Pi's bundled subagent example to a file that exists", () => {
    // If this fails, pi moved or stopped shipping examples/extensions/subagent.
    // The fallback is re-vendoring the fork — see the shim's header.
    const upstreamPath = resolveUpstreamSubagentPath();
    expect(upstreamPath).toMatch(/examples[/\\]extensions[/\\]subagent[/\\]index\.ts$/);
    expect(existsSync(upstreamPath)).toBe(true);
  });

  it("registers exactly one tool named 'subagent' through Pi's own loader", async () => {
    process.env.AC_UPSTREAM_SUBAGENT_PATH = resolveUpstreamSubagentPath();

    const { extensions, errors } = await loadShim();

    expect(errors).toEqual([]);
    expect(extensions).toHaveLength(1);
    expect([...extensions[0].tools.keys()]).toEqual(["subagent"]);
  });

  it("replaces upstream's model-visible description so no home path or scope escape leaks", async () => {
    process.env.AC_UPSTREAM_SUBAGENT_PATH = resolveUpstreamSubagentPath();

    const { extensions } = await loadShim();
    const description = extensions[0].tools.get("subagent")!.definition.description;

    // Upstream builds its description from getAgentDir() and tells the model to
    // set agentScope: "both" to reach project agents. Both must be gone.
    expect(description).not.toMatch(/agentScope: "both"/);
    expect(description).not.toMatch(/\.pi[/\\]agent/);
    expect(description).toContain("declared in this agent's spec");
  });

  it("fails loudly when AC_UPSTREAM_SUBAGENT_PATH is unset", async () => {
    delete process.env.AC_UPSTREAM_SUBAGENT_PATH;

    const { errors } = await loadShim();

    expect(errors).toHaveLength(1);
    expect(errors[0].error).toContain("AC_UPSTREAM_SUBAGENT_PATH");
  });

  it("forces agentScope=project and confirmProjectAgents=false, ignoring what the model asked for", async () => {
    process.env.AC_UPSTREAM_SUBAGENT_PATH = resolveUpstreamSubagentPath();
    // No AC_PI_BIN / AC_SUBAGENT_AGENT_DIR: this test only inspects the params
    // upstream received, and an unknown agent short-circuits before any spawn.
    delete process.env.AC_PI_BIN;
    delete process.env.AC_SUBAGENT_AGENT_DIR;

    const { extensions } = await loadShim();
    const tool = extensions[0].tools.get("subagent")!.definition;

    // The model asks for the user scope — the escape the shim exists to close.
    // "no-such-agent" is not in any scope, so upstream returns its
    // unknown-agent result without spawning a child pi.
    const scratch = mkdtempSync(join(tmpdir(), "ac-subagent-exec-"));
    const result: any = await (tool as any).execute(
      "call-1",
      { agent: "no-such-agent", task: "noop", agentScope: "user", confirmProjectAgents: true },
      undefined,
      undefined,
      { cwd: scratch, hasUI: false },
    );

    // details.agentScope is upstream's echo of the scope it actually resolved.
    expect(result.details.agentScope).toBe("project");
    // Reaching the unknown-agent path (rather than a UI confirm) also shows
    // confirmProjectAgents was not honored as `true`.
    expect(JSON.stringify(result.content)).toContain("no-such-agent");
  });

  // Pi runs a batch of tool calls concurrently by default, so two subagent
  // calls in one turn overlap. Per-call save/restore corrupted that: the second
  // call captured the first call's already-mutated argv[1] as the "original",
  // and the first call's restore repointed argv[1] at the adapter while the
  // second was still spawning children — which then hang on a CompiledSpec
  // read. Flagged by codex review; these assert the reference counting.
  describe("overlapping calls (Pi's parallel tool execution)", () => {
    const PI_BIN = "/mock/node_modules/.bin/pi.js";
    const AGENT_DIR = "/mock/project/.pi/agent";

    beforeEach(() => {
      process.env.AC_PI_BIN = PI_BIN;
      process.env.AC_SUBAGENT_AGENT_DIR = AGENT_DIR;
      delete process.env.PI_CODING_AGENT_DIR;
    });

    it("holds the overrides until the LAST overlapping call releases", () => {
      const originalArgv1 = process.argv[1];

      const releaseA = applyProcessOverrides();
      expect(process.argv[1]).toBe(PI_BIN);
      expect(process.env.PI_CODING_AGENT_DIR).toBe(AGENT_DIR);

      const releaseB = applyProcessOverrides();
      releaseA();

      // A is done but B is still spawning children — the overrides must hold.
      expect(process.argv[1]).toBe(PI_BIN);
      expect(process.env.PI_CODING_AGENT_DIR).toBe(AGENT_DIR);

      releaseB();
      expect(process.argv[1]).toBe(originalArgv1);
      expect(process.env.PI_CODING_AGENT_DIR).toBeUndefined();
    });

    it("ignores a double release so one call cannot restore under another", () => {
      const originalArgv1 = process.argv[1];

      const releaseA = applyProcessOverrides();
      const releaseB = applyProcessOverrides();
      releaseA();
      releaseA();

      expect(process.argv[1]).toBe(PI_BIN);

      releaseB();
      expect(process.argv[1]).toBe(originalArgv1);
    });

    it("restores the pre-existing PI_CODING_AGENT_DIR rather than deleting it", () => {
      process.env.PI_CODING_AGENT_DIR = "/operator/own/.pi/agent";

      const release = applyProcessOverrides();
      expect(process.env.PI_CODING_AGENT_DIR).toBe(AGENT_DIR);

      release();
      expect(process.env.PI_CODING_AGENT_DIR).toBe("/operator/own/.pi/agent");
    });
  });

  it("restores process.argv[1] and PI_CODING_AGENT_DIR after the call", async () => {
    process.env.AC_UPSTREAM_SUBAGENT_PATH = resolveUpstreamSubagentPath();
    process.env.AC_PI_BIN = "/mock/node_modules/.bin/pi.js";
    process.env.AC_SUBAGENT_AGENT_DIR = "/mock/project/.pi/agent";
    delete process.env.PI_CODING_AGENT_DIR;

    const { extensions } = await loadShim();
    const tool = extensions[0].tools.get("subagent")!.definition;
    const argvBefore = process.argv[1];

    const scratch = mkdtempSync(join(tmpdir(), "ac-subagent-restore-"));
    await (tool as any).execute(
      "call-1",
      { agent: "no-such-agent", task: "noop" },
      undefined,
      undefined,
      { cwd: scratch, hasUI: false },
    );

    // Both overrides are process-global, so leaking them past the call would
    // repoint the parent session's own getAgentDir() (auth.json, sessions).
    expect(process.argv[1]).toBe(argvBefore);
    expect(process.env.PI_CODING_AGENT_DIR).toBeUndefined();
  });
});
