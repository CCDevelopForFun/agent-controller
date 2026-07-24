import { describe, it, expect, afterEach } from "vitest";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makeTempWorkdir, resolveDataDir } from "./workdir.js";

describe("workdir — opencode config/data separation + resume persistence", () => {
  const cleanup: string[] = [];
  const origXdg = process.env.XDG_DATA_HOME;

  afterEach(() => {
    if (origXdg === undefined) delete process.env.XDG_DATA_HOME;
    else process.env.XDG_DATA_HOME = origXdg;
    for (const d of cleanup.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  it("makeTempWorkdir creates a fresh, existing dir on every call", () => {
    const a = makeTempWorkdir();
    const b = makeTempWorkdir();
    cleanup.push(a, b);
    expect(a).not.toBe(b);
    expect(a).toContain("agent-controller-opencode-");
    expect(existsSync(a)).toBe(true);
    expect(existsSync(b)).toBe(true);
  });

  it("one-shot run (no session id): data dir is the ephemeral fallback", () => {
    const fallback = makeTempWorkdir();
    cleanup.push(fallback);
    expect(resolveDataDir(undefined, fallback)).toBe(fallback);
  });

  it("resume: data dir is a STABLE path keyed by session id under XDG_DATA_HOME", () => {
    const base = mkdtempSync(join(tmpdir(), "xdg-"));
    cleanup.push(base);
    process.env.XDG_DATA_HOME = base;
    const fallback = makeTempWorkdir();
    cleanup.push(fallback);

    const dir = resolveDataDir("s_abc123", fallback);
    expect(dir).toBe(join(base, "agent-controller", "opencode-sessions", "s_abc123"));
    expect(dir).not.toBe(fallback); // NOT the ephemeral config dir
    expect(existsSync(dir)).toBe(true);
  });

  it("resume: the persistent session dir is created private (0o700)", () => {
    const base = mkdtempSync(join(tmpdir(), "xdg-"));
    cleanup.push(base);
    process.env.XDG_DATA_HOME = base;

    const dir = resolveDataDir("s_perms", makeTempWorkdir());
    expect(statSync(dir).mode & 0o777).toBe(0o700);
  });

  it("resume: rejects path-traversal / unsafe session ids", () => {
    const base = mkdtempSync(join(tmpdir(), "xdg-"));
    cleanup.push(base);
    process.env.XDG_DATA_HOME = base;
    const fb = makeTempWorkdir();
    cleanup.push(fb);

    for (const bad of ["../../etc", "a/b", "..", "with space", "dot.dot"]) {
      expect(() => resolveDataDir(bad, fb)).toThrow();
    }
    // a real agentctl id (s_<base36>) is accepted
    expect(() => resolveDataDir("s_abc123", fb)).not.toThrow();
  });

  it("resume: same session id resolves the same dir; different ids differ", () => {
    const base = mkdtempSync(join(tmpdir(), "xdg-"));
    cleanup.push(base);
    process.env.XDG_DATA_HOME = base;
    const fb = makeTempWorkdir();
    cleanup.push(fb);

    expect(resolveDataDir("s_same", fb)).toBe(resolveDataDir("s_same", fb));
    expect(resolveDataDir("s_a", fb)).not.toBe(resolveDataDir("s_b", fb));
  });
});
