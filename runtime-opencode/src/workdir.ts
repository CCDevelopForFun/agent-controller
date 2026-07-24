// ── opencode working-directory management ─────────────────────────────────
//
// The opencode adapter separates two concerns that must NOT share a directory:
//
//   - CONFIG + cwd + HOME  → an ephemeral temp dir, fresh per invocation and
//     wiped on exit. This isolates opencode from ambient user config AND
//     prevents any config a prior turn wrote (e.g. via bash/write tools) from
//     leaking into the next turn.
//   - DATA (XDG_DATA_HOME)  → holds opencode's on-disk session store. For a
//     one-shot `run` it points at the same ephemeral dir (no persistence). For
//     a multi-turn session (`--resume` / `chat` / `serve`) it is a STABLE dir
//     keyed by the agentctl session id, so a freshly-spawned opencode server
//     resumes the persisted session (verified: opencode stores sessions under
//     XDG_DATA_HOME, so keeping only this dir stable is sufficient).

import { mkdirSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";

/**
 * Create an ephemeral temp dir for opencode's config/cwd/HOME. Fresh per call
 * (pid+timestamp+random so same-process/same-ms calls never collide) and the
 * caller is responsible for wiping it on exit.
 */
export function makeTempWorkdir(): string {
  const rand = Math.random().toString(36).slice(2, 10);
  const dir = join(tmpdir(), `agent-controller-opencode-${process.pid}-${Date.now()}-${rand}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

/**
 * Resolve XDG_DATA_HOME (opencode's session store root) for this run.
 *
 * Without a session id (one-shot run) the data lives in the ephemeral dir
 * (`ephemeralFallback`), so nothing persists. With a session id it is a STABLE
 * per-session dir under `$XDG_DATA_HOME/agent-controller/opencode-sessions/<id>`
 * (falling back to `~/.local/share`), created private (0o700) since it holds
 * conversation transcripts across runs — matching the SQLite session store.
 */
export function resolveDataDir(sessionId: string | undefined, ephemeralFallback: string): string {
  if (!sessionId) {
    return ephemeralFallback;
  }
  // sessionId comes from --resume / the CompiledSpec and is used as a path
  // component, so it must not be able to escape the session store. agentctl
  // ids are `s_<base36>`; reject anything with path separators, `..`, or other
  // unexpected characters rather than silently aliasing another directory.
  if (!/^[A-Za-z0-9_-]+$/.test(sessionId)) {
    throw new Error(
      `runtime-opencode: invalid session id ${JSON.stringify(sessionId)} — ` +
        "must match [A-Za-z0-9_-]+ (no path separators or '..').",
    );
  }
  const base = process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share");
  const dir = join(base, "agent-controller", "opencode-sessions", sessionId);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  return dir;
}
