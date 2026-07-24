import { describe, it, expect, afterEach } from "vitest";
import { existsSync, mkdtempSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { resolveCodexHome } from "./codex-home.js";

describe("resolveCodexHome", () => {
  const clean: string[] = [];
  const orig = process.env.XDG_DATA_HOME;
  afterEach(() => {
    if (orig === undefined) delete process.env.XDG_DATA_HOME; else process.env.XDG_DATA_HOME = orig;
    for (const d of clean.splice(0)) rmSync(d, { recursive: true, force: true });
  });
  it("one-shot: ephemeral temp home", () => {
    const r = resolveCodexHome(undefined); clean.push(r.dir);
    expect(r.ephemeral).toBe(true);
    expect(r.dir).toContain("agent-controller-codex-");
    expect(existsSync(r.dir)).toBe(true);
  });
  it("resume: stable 0o700 home under XDG_DATA_HOME keyed by sessionId", () => {
    const base = mkdtempSync(join(tmpdir(), "xdg-")); clean.push(base);
    process.env.XDG_DATA_HOME = base;
    const r = resolveCodexHome("s_abc123");
    expect(r.ephemeral).toBe(false);
    expect(r.dir).toBe(join(base, "agent-controller", "codex-sessions", "s_abc123"));
    expect(statSync(r.dir).mode & 0o777).toBe(0o700);
  });
  it("rejects path-traversal session ids", () => {
    for (const bad of ["../../etc", "a/b", "..", "with space"]) {
      expect(() => resolveCodexHome(bad)).toThrow();
    }
  });
});
