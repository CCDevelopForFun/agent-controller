import { mkdirSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

export function makeTempCodexHome(): string {
  const rand = Math.random().toString(36).slice(2, 10);
  const dir = join(tmpdir(), `agent-controller-codex-${process.pid}-${Date.now()}-${rand}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

export function resolveCodexHome(sessionId?: string): { dir: string; ephemeral: boolean } {
  if (!sessionId) return { dir: makeTempCodexHome(), ephemeral: true };
  if (!/^[A-Za-z0-9_-]+$/.test(sessionId)) {
    throw new Error(`runtime-codex: invalid session id ${JSON.stringify(sessionId)} — must match [A-Za-z0-9_-]+`);
  }
  const base = process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share");
  const dir = join(base, "agent-controller", "codex-sessions", sessionId);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  return { dir, ephemeral: false };
}
