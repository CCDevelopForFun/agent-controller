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
import {
  persistSdkSessionId,
  readPersistedSdkSessionId,
  sdkSessionStateDir,
} from "./claude-session.js";
import { readSkillBodies, readSubagentBodies } from "./registry.js";
import { createTranslatorState, translateSdkMessage } from "./event-translator.js";

const write = (s: string): void => void process.stdout.write(s);

/**
 * Record an SDK session id for later turns, warning on stderr if the write
 * fails. Not fatal: a one-shot run with no spec.sessionId never needs the
 * file. But the write is load-bearing when a session id IS set — see the
 * WRITE ORDERING note in claude-session.ts — so a failure must not be silent.
 */
function recordSdkSessionId(dir: string, sdkSessionId: string): void {
  const res = persistSdkSessionId(dir, sdkSessionId);
  if (!res.ok) {
    process.stderr.write(
      `[runtime-claude] WARNING: could not record the SDK session id at ` +
        `${res.file}: ${res.reason}. Later turns of this session will not be ` +
        `able to resume it.\n`,
    );
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
      const derived = deriveSdkSessionUuid(spec.sessionId);
      sessionStateDir = sdkSessionStateDir(derived);
      resumeSdkSessionId = readPersistedSdkSessionId(sessionStateDir);
      // First turn: record the id BEFORE query() can create its transcript.
      // Recording it afterwards permanently bricks the session if the process
      // dies in between — the SDK refuses to reuse a session id whose
      // transcript exists. See claude-session.ts's WRITE ORDERING note.
      if (!resumeSdkSessionId) recordSdkSessionId(sessionStateDir, derived);
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
    // Self-healing refinement, not the primary mechanism: the id was already
    // recorded before query() ran, and the SDK echoes Options.sessionId back on
    // `init`, so this is normally a no-op (persistSdkSessionId skips a rewrite
    // of the same value). It only does work if the SDK ever adopts an id other
    // than the one we asked for, in which case that id is what later turns must
    // resume. Runs on the error and fatal paths too.
    if (sessionStateDir && state.sdkSessionId) {
      recordSdkSessionId(sessionStateDir, state.sdkSessionId);
    }
  }

  if (!state.ended) {
    emit(write, stamp(sessionId, "session.ended", { reason: "completed" }));
  }
}

void main();
