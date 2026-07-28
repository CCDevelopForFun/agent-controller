/**
 * Cross-turn SDK session-state persistence for the Claude Agent SDK adapter.
 *
 * The adapter is a fresh process per turn — `cli/internal/serve/run_turn.go`
 * and `cli/cmd/agentctl/chat.go` both re-dispatch with `spec.SessionID` set —
 * so "have I already opened an SDK session for this agentctl session?" cannot
 * live in memory. It lives in one small file on disk.
 *
 * Isolated in its own module, mirroring how `runtime-codex/src/codex-home.ts`
 * isolates exactly this concern, so the half of the session bridge that decides
 * whether a turn resumes or starts fresh is unit-testable. The
 * previously-inlined version of this logic was invisible to CI, and that is
 * where its ordering defect lived (see the WRITE ORDERING note below).
 *
 * ── WRITE ORDERING IS LOAD-BEARING ─────────────────────────────────────────
 *
 * The id must be recorded BEFORE `query()` runs, not after it finishes.
 *
 * The bundled CLI refuses to reuse a session id whose transcript already
 * exists: passing `--session-id <id>` a second time fails with
 * `Session ID <id> is already in use.` (observed; the local SDK path passes
 * neither `--fork-session` nor `--sdk-url`, which are the only exemptions).
 * The SDK creates that transcript at session init, before the first model
 * call.
 *
 * So if the id were only recorded after `query()` returned, any death in
 * between — SIGTERM from `backend.Stop`, which is how ordinary cancellation
 * works, or any hard crash — would leave a transcript with no record of it.
 * Every later turn would then re-take the first-turn branch, pass the same
 * derived id, and hard-fail. Permanently: the id is only ever recorded on a
 * successful init that can no longer happen.
 *
 * Recording the derived id up front is correct rather than speculative,
 * because it is exactly the id the SDK adopts (`Options.sessionId` is echoed
 * back on the `init` message). The remaining window is the inverse and far
 * smaller: if the SDK fails before init, the file names a session with no
 * transcript, and the next turn's `resume` returns a `result` with
 * `is_error: true` (a soft error, not a throw). That window spans a subprocess
 * spawn rather than a whole conversation.
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

/** Basename of the state file inside the per-session directory. */
export const SDK_SESSION_ID_FILENAME = "sdk-session-id";

/**
 * Canonical UUID shape. Deliberately does not pin the version or variant
 * nibbles: the adapter mints v5 ids, but the SDK is the authority on what it
 * hands back on `init`, and rejecting a valid id over a version digit we did
 * not anticipate would break resume for no good reason.
 */
const UUID_SHAPE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** Outcome of a persist attempt. Never throws — see persistSdkSessionId. */
export type PersistResult =
  | { ok: true; file: string }
  | { ok: false; file: string; reason: string };

/**
 * Directory holding one agentctl session's SDK state.
 *
 * Keyed by the *derived UUID*, never by the raw `spec.sessionId`, so no
 * unvalidated spec string reaches a path component and two different agentctl
 * sessions cannot collide (distinct ids derive to distinct UUIDs).
 */
export function sdkSessionStateDir(derivedUuid: string): string {
  const base = process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share");
  return join(base, "agent-controller", "claude-sessions", derivedUuid);
}

/**
 * Read the SDK session id recorded for this session, if any.
 *
 * Returns undefined — meaning "treat this as a first turn" — when the file is
 * absent, unreadable, empty, or does not hold a canonical UUID. Never throws:
 * an unreadable state file must not take down a turn that could otherwise run.
 *
 * Content that is present but malformed is ignored rather than forwarded to
 * `Options.resume`. Both outcomes are broken for an already-started session,
 * but ignoring it still lets a session with no transcript start cleanly,
 * whereas forwarding garbage always fails.
 */
export function readPersistedSdkSessionId(dir: string): string | undefined {
  const file = join(dir, SDK_SESSION_ID_FILENAME);
  if (!existsSync(file)) return undefined;
  let raw: string;
  try {
    raw = readFileSync(file, "utf8");
  } catch {
    return undefined;
  }
  const trimmed = raw.trim();
  if (!UUID_SHAPE.test(trimmed)) return undefined;
  return trimmed;
}

/**
 * Record the SDK session id for later turns.
 *
 * Returns a result instead of throwing or writing to stderr itself: the caller
 * decides how loud to be, and this module stays testable. A failure is not
 * fatal — a one-shot `agentctl run` with no session id never needs the file —
 * but it does mean later turns cannot resume, so the caller should say so.
 *
 * Idempotent: re-recording the same value is skipped, so the common case
 * (every turn observing the id it already asked for) does no I/O.
 */
export function persistSdkSessionId(dir: string, sdkSessionId: string): PersistResult {
  const file = join(dir, SDK_SESSION_ID_FILENAME);
  if (readPersistedSdkSessionId(dir) === sdkSessionId) return { ok: true, file };
  try {
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    writeFileSync(file, sdkSessionId, { encoding: "utf8", mode: 0o600 });
    return { ok: true, file };
  } catch (err) {
    return { ok: false, file, reason: err instanceof Error ? err.message : String(err) };
  }
}
