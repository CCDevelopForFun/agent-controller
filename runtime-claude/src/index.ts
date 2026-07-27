/**
 * Claude Agent SDK runtime adapter entrypoint.
 *
 * Reads a CompiledSpec JSON document from stdin (to EOF), validates it against
 * the adapter's capability set, resolves skill and subagent bodies from the
 * local registry, drives a session through @anthropic-ai/claude-agent-sdk's
 * query(), translates each SDKMessage into wire events, and writes the NDJSON
 * wire-event stream to stdout.
 *
 * On abnormal exit: emits session.ended{reason:"error"} (if none was emitted
 * already) and sets process.exitCode = 1.
 *
 * Wire protocol contract: cli/internal/wire/events.go
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import type { CompiledSpec, HallucinationMode } from "./types.js";
import { emit, stamp } from "./wire.js";
import {
  assertClaudeCompatible,
  buildOptions,
  buildPrompt,
  buildSubagents,
  deriveSdkSessionUuid,
} from "./claude-invocation.js";
import { readSkillBodies, readSubagentBodies } from "./registry.js";
import { createTranslatorState, translateSdkMessage } from "./event-translator.js";

const write = (s: string): void => void process.stdout.write(s);

// ── SDK session-id persistence ────────────────────────────────────────────────
//
// The adapter is a fresh process per turn (cli/internal/serve/run_turn.go and
// cli/cmd/agentctl/chat.go both re-dispatch with spec.SessionID set), so
// "have I seen this agentctl session before?" cannot live in memory. Mirrors
// runtime-codex's `$CODEX_HOME/.agentctl-thread-id` file.
//
// The directory is keyed by the derived UUID rather than the raw agentctl id
// so no unvalidated spec string ever reaches a path component.

const SDK_SESSION_ID_FILENAME = "sdk-session-id";

function sdkSessionStateDir(derivedUuid: string): string {
  const base = process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share");
  return join(base, "agent-controller", "claude-sessions", derivedUuid);
}

/** Read the SDK session id captured on a previous turn, if any. */
function readPersistedSdkSessionId(dir: string): string | undefined {
  const file = join(dir, SDK_SESSION_ID_FILENAME);
  if (!existsSync(file)) return undefined;
  try {
    return readFileSync(file, "utf8").trim() || undefined;
  } catch {
    return undefined;
  }
}

/**
 * Persist the SDK session id for the next turn. Non-fatal on failure: the
 * next turn falls back to the deterministic first-turn id.
 */
function persistSdkSessionId(dir: string, sdkSessionId: string): void {
  try {
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    writeFileSync(join(dir, SDK_SESSION_ID_FILENAME), sdkSessionId, {
      encoding: "utf8",
      mode: 0o600,
    });
  } catch {
    // Non-fatal.
  }
}

/** Generate a short wire sessionId: "s_" + 8 base-36 chars. */
function makeSessionId(): string {
  return "s_" + Math.random().toString(36).slice(2, 10);
}

/** Resolve the hallucination guardrail mode, defaulting to "block". */
function resolveHallucinationMode(spec: CompiledSpec): HallucinationMode {
  const raw = spec.guardrails?.hallucinationDetector;
  if (raw === "warn" || raw === "block" || raw === "correct") return raw;
  if (raw !== undefined) {
    process.stderr.write(
      `[runtime-claude] WARNING: unknown spec.guardrails.hallucinationDetector ` +
        `"${raw}"; falling back to "block".\n`,
    );
  }
  return "block";
}

async function readStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function main(): Promise<void> {
  const sessionId = makeSessionId();
  const state = createTranslatorState();

  let spec: CompiledSpec;
  try {
    spec = JSON.parse(await readStdin()) as CompiledSpec;
  } catch (err) {
    emit(write, stamp(sessionId, "error", {
      kind: "invalid_spec",
      message: `runtime-claude: could not parse CompiledSpec from stdin: ${String(err)}`,
    }));
    emit(write, stamp(sessionId, "session.ended", { reason: "error" }));
    process.exitCode = 1;
    return;
  }

  try {
    assertClaudeCompatible(spec);
  } catch (err) {
    emit(write, stamp(sessionId, "error", {
      kind: "unsupported_spec",
      message: err instanceof Error ? err.message : String(err),
    }));
    emit(write, stamp(sessionId, "session.ended", { reason: "error" }));
    process.exitCode = 1;
    return;
  }

  const root = process.cwd();
  const mode = resolveHallucinationMode(spec);

  // buildOptions() throws with a field-naming message for specs it cannot map
  // (malformed MCP entries, an unmapped built-in tool, a blank sessionId), and
  // deriveSdkSessionUuid() throws for the same reason. Both are spec problems,
  // not runtime failures, so they get the same rejection shape as
  // assertClaudeCompatible rather than an unhandled rejection.
  let options: Options;
  let sessionStateDir: string | undefined;
  try {
    const skills = readSkillBodies(root, spec.skills ?? []);
    const subagentBodies = readSubagentBodies(root, spec.subagents ?? []);
    const systemPrompt = buildPrompt(spec, skills);
    const subagents = buildSubagents(subagentBodies);

    let resumeSdkSessionId: string | undefined;
    if (spec.sessionId) {
      sessionStateDir = sdkSessionStateDir(deriveSdkSessionUuid(spec.sessionId));
      resumeSdkSessionId = readPersistedSdkSessionId(sessionStateDir);
    }

    options = buildOptions(spec, systemPrompt, subagents, resumeSdkSessionId);
  } catch (err) {
    emit(write, stamp(sessionId, "error", {
      kind: "unsupported_spec",
      message: err instanceof Error ? err.message : String(err),
    }));
    emit(write, stamp(sessionId, "session.ended", { reason: "error" }));
    process.exitCode = 1;
    return;
  }

  if (spec.model.temperature !== undefined) {
    process.stderr.write(
      `[runtime-claude] NOTE: spec.model.temperature is not supported by the ` +
        `Claude Agent SDK and is ignored.\n`,
    );
  }

  try {
    for await (const msg of query({ prompt: spec.task, options })) {
      const { events, fatal } = translateSdkMessage(msg, sessionId, state, mode);
      for (const ev of events) emit(write, ev);
      if (fatal) {
        process.exitCode = 1;
        return;
      }
    }
  } catch (err) {
    if (!state.ended) {
      emit(write, stamp(sessionId, "error", {
        kind: "runtime_error",
        message: err instanceof Error ? err.message : String(err),
      }));
      emit(write, stamp(sessionId, "session.ended", { reason: "error" }));
    }
    process.exitCode = 1;
    return;
  } finally {
    // Record the SDK's own session id so the next turn resumes it instead of
    // starting over. Runs on the error and fatal paths too — the transcript
    // exists either way, and losing the id would silently fork the session.
    if (sessionStateDir && state.sdkSessionId) {
      persistSdkSessionId(sessionStateDir, state.sdkSessionId);
    }
  }

  if (!state.ended) {
    emit(write, stamp(sessionId, "session.ended", { reason: "completed" }));
  }
}

void main();
