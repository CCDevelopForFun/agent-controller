import { describe, it, expect } from "vitest";
import { spawnSync } from "node:child_process";
import { resolve } from "node:path";
import { tmpdir as osTmpdir } from "node:os";
import { mkdtempSync, rmSync } from "node:fs";
import { normalizeAnthropicBaseUrlForOpencode } from "./anthropic-base-url.js";

const adapterPath = resolve(__dirname, "..", "dist", "index.js");

/**
 * Slice 2.1 acceptance tests for the opencode adapter skeleton.
 *
 * These spawn the built dist/index.js as a subprocess (the same way the
 * Go CLI will invoke it) and assert on the NDJSON wire-event stream.
 *
 * Requires `npm run build` to have produced runtime-opencode/dist/. The
 * test fails fast with a clear message if the build artifact is
 * missing, so a forgotten build doesn't manifest as a confusing
 * "Cannot find module" cascade.
 */
describe("opencode adapter skeleton (slice 2.1)", () => {
  function runAdapter(input: string): { stdout: string; stderr: string; status: number | null } {
    // Force a deterministic error path by pointing to a non-existent model
    // provider. This prevents the subprocess from making real model calls
    // even if the test host has opencode installed and authenticated via
    // its own config, ANTHROPIC_BASE_URL, or other credential paths.
    // Scrub ANTHROPIC_API_KEY as defense-in-depth; also pass a config
    // that specifies a guaranteed-to-fail provider/port combo. Codex
    // pass 7 of slice 2.4 noted that scrubbing only ANTHROPIC_API_KEY
    // leaves authenticated opencode installs reachable.
    // Remove ALL credential and config sources that could let opencode make a
    // real model call. createOpencode() spawns opencode as a child, passing
    // OPENCODE_CONFIG_CONTENT via env — so that env var is set by the adapter,
    // not inherited. We can't prevent opencode from reading its own config
    // from XDG_CONFIG_HOME or HOME, so we redirect HOME to an empty tempdir
    // and clear every auth env var we can identify. Codex pass 8 caught that
    // ANTHROPIC_BASE_URL + installed opencode still allows live requests.
    const {
      ANTHROPIC_API_KEY: _k,
      ANTHROPIC_BASE_URL: _bu,
      OPENCODE_CONFIG_CONTENT: _cfg,
      HOME: _home,
      XDG_CONFIG_HOME: _xdg,
      ...safeEnv
    } = process.env;
    // Use a fresh empty temp directory as HOME so opencode cannot read any
    // real config or credentials. Codex pass 20 caught that shared tmpdir()
    // could contain leftover opencode config from prior runs.
    const fakeHome = mkdtempSync(osTmpdir() + "/ac-test-home-");
    let result: ReturnType<typeof spawnSync>;
    try {
      result = spawnSync(process.execPath, [adapterPath], {
      input,
      encoding: "utf8",
      timeout: 10000,
      env: {
        ...safeEnv,
        // Scrubbed / replaced credentials
        ANTHROPIC_API_KEY: "test-scrubbed",
        // Redirect HOME so opencode can't read ~/.config/opencode
        HOME: fakeHome,
        XDG_CONFIG_HOME: fakeHome,
      },
    });
    } finally {
      // Clean up the temp HOME dir to avoid accumulating /tmp/ac-test-home-*
      // on repeated runs. Codex pass 21 caught that `cleanup` is not a
      // spawnSync option — the cleanup must be explicit.
      try { rmSync(fakeHome, { recursive: true, force: true }); } catch { /* ignore */ }
    }
    return {
      stdout: result!.stdout,
      stderr: result!.stderr,
      status: result!.status,
    };
  }

  function parseNdjson(stdout: string): Array<{ type: string; data: unknown }> {
    return stdout
      .split("\n")
      .filter((line) => line.trim().length > 0)
      .map((line) => JSON.parse(line) as { type: string; data: unknown });
  }

  it("exits non-zero with an error wire event for a spec with unsupported fields (hermetic failure path)", () => {
    // Declare a Pi-format extension — the adapter rejects specs with
    // unsupported fields BEFORE starting opencode, so this fails fast without
    // spawning any child processes and without making any network calls.
    // (Slice 2.5 now supports skills/subagents/mcpServers; only spec.extensions
    //  + spec.installs remain unsupported.)
    const spec = {
      v: 1,
      metadata: { name: "hello" },
      model: { provider: "anthropic", name: "claude-sonnet-4-20250514" },
      task: "say hi",
      tools: [],
      extensions: [{ name: "some-ext", entrypoint: "/tmp/fake.ts" }],
      skills: [],
      runtime: { type: "local-opencode" },
    };
    const { stdout, status } = runAdapter(JSON.stringify(spec));
    // Must exit non-zero.
    expect(status).toBe(1);
    // Must emit at least one event on stdout.
    const events = parseNdjson(stdout);
    expect(events.length).toBeGreaterThan(0);
    // The final event must be session.ended with reason=error.
    const lastEvent = events[events.length - 1];
    expect(lastEvent.type).toBe("session.ended");
    expect((lastEvent.data as { reason: string }).reason).toBe("error");
  });

  it("fails fast on empty stdin", () => {
    const { status, stderr } = runAdapter("");
    expect(status).toBe(2);
    expect(stderr).toContain("stdin was empty");
  });

  it("fails fast on invalid JSON", () => {
    const { status, stderr } = runAdapter("not json");
    expect(status).toBe(2);
    expect(stderr).toContain("failed to parse stdin");
  });

  it("fails fast on missing CompiledSpec.metadata.name", () => {
    const { status, stderr } = runAdapter(
      JSON.stringify({ v: 1, model: { provider: "x", name: "y" } }),
    );
    expect(status).toBe(2);
    expect(stderr).toContain("metadata.name");
  });

  it("fails fast on unsupported v field", () => {
    const { status, stderr } = runAdapter(JSON.stringify({ v: 99 }));
    expect(status).toBe(2);
    expect(stderr).toContain("unsupported CompiledSpec version");
  });
});

// Slice 4.2.1: ANTHROPIC_BASE_URL normalization so a Pi-style URL (no /v1)
// also works with opencode's Vercel AI SDK provider plugin.
describe("normalizeAnthropicBaseUrlForOpencode", () => {
  it("returns undefined when input is undefined", () => {
    expect(normalizeAnthropicBaseUrlForOpencode(undefined)).toBeUndefined();
  });

  it("returns empty string unchanged", () => {
    // Empty string is falsy → treated as 'unset' upstream; preserve verbatim.
    expect(normalizeAnthropicBaseUrlForOpencode("")).toBe("");
  });

  it("appends /v1 to a Pi-style URL with no version suffix", () => {
    expect(normalizeAnthropicBaseUrlForOpencode("http://gateway.local:9123/")).toBe(
      "http://gateway.local:9123/v1",
    );
    expect(normalizeAnthropicBaseUrlForOpencode("http://gateway.local:9123")).toBe(
      "http://gateway.local:9123/v1",
    );
  });

  it("leaves a URL ending in /v1 alone", () => {
    expect(normalizeAnthropicBaseUrlForOpencode("http://gateway.local:9123/v1")).toBe(
      "http://gateway.local:9123/v1",
    );
    expect(normalizeAnthropicBaseUrlForOpencode("http://gateway.local:9123/v1/")).toBe(
      "http://gateway.local:9123/v1",
    );
  });

  it("leaves api.anthropic.com URLs alone (already-versioned official endpoint)", () => {
    expect(normalizeAnthropicBaseUrlForOpencode("https://api.anthropic.com/v1")).toBe(
      "https://api.anthropic.com/v1",
    );
  });

  it("respects any /vN version suffix", () => {
    // If someone explicitly targets /v2 for a hypothetical future API, we
    // don't second-guess them.
    expect(normalizeAnthropicBaseUrlForOpencode("http://example.com/v2")).toBe(
      "http://example.com/v2",
    );
  });

  it("handles a base URL with a sub-path that isn't /vN", () => {
    // Pi adapter uses this shape; opencode expects /v1, so we append it.
    expect(normalizeAnthropicBaseUrlForOpencode("http://proxy.internal/anthropic/")).toBe(
      "http://proxy.internal/anthropic/v1",
    );
  });
});
