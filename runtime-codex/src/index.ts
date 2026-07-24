/**
 * Codex runtime adapter entrypoint — Task 5 (C.1), one-shot codex exec run.
 *
 * Reads a CompiledSpec JSON document from stdin (to EOF), validates it,
 * resolves skill bodies, spawns `codex exec` with the composed prompt, reads
 * its JSONL stdout line-by-line, translates each line into wire events via
 * event-translator.ts, and writes the NDJSON wire-event stream to stdout.
 *
 * On abnormal exit: emits a `session.ended{reason:"error"}` wire event (if
 * none was already emitted) and sets process.exitCode = 1.
 *
 * Wire protocol contract: cli/internal/wire/events.go
 * Event translation:      src/event-translator.ts
 * Prompt composition:     src/codex-invocation.ts (buildPrompt / buildCodexArgs)
 * Honesty guardrails:     src/honesty.ts
 * CODEX_HOME lifecycle:   src/codex-home.ts
 */

import { readFileSync, writeFileSync, rmSync, existsSync } from "node:fs";
import { createInterface } from "node:readline";
import { spawn } from "node:child_process";
import { join } from "node:path";

// ── thread-id persistence ──────────────────────────────────────────────────────

const THREAD_ID_FILENAME = ".agentctl-thread-id";

/**
 * Read the persisted codex thread_id from a stable CODEX_HOME, if it exists.
 * Returns undefined when the file is absent or cannot be read.
 */
function readPersistedThreadId(homeDir: string): string | undefined {
  const file = join(homeDir, THREAD_ID_FILENAME);
  if (!existsSync(file)) return undefined;
  try {
    return readFileSync(file, "utf8").trim() || undefined;
  } catch {
    return undefined;
  }
}

/**
 * Persist the codex thread_id captured during a run into the stable CODEX_HOME
 * so the next turn can resume from it. No-op if threadId is falsy.
 */
function persistThreadId(homeDir: string, threadId: string | undefined): void {
  if (!threadId) return;
  try {
    writeFileSync(join(homeDir, THREAD_ID_FILENAME), threadId, { encoding: "utf8", mode: 0o600 });
  } catch {
    // Non-fatal: next turn will start a fresh thread rather than resuming.
  }
}
import type { CompiledSpec, ResolvedRef } from "./types.js";
import { assertCodexCompatible, buildCodexArgs, buildConfigToml } from "./codex-invocation.js";
import type { SkillBody } from "./codex-invocation.js";
import { resolveCodexHome } from "./codex-home.js";
import { stamp, emit } from "./wire.js";
import { createTranslatorState, translateCodexLine } from "./event-translator.js";
import type { HallucinationMode } from "./event-translator.js";

// ── session ID ────────────────────────────────────────────────────────────────

/** Generate a short wire sessionId: "s_" + 8 base-36 chars. */
function makeSessionId(): string {
  return "s_" + Math.random().toString(36).slice(2, 10);
}

// ── hallucination mode resolution ─────────────────────────────────────────────

/**
 * Resolve the hallucination guardrail mode from the spec. Defaults to "block"
 * when absent or unknown — matches the opencode adapter's behaviour.
 */
function resolveHallucinationMode(spec: CompiledSpec): HallucinationMode {
  const raw = spec.guardrails?.hallucinationDetector;
  if (!raw) return "block";
  if (raw === "warn" || raw === "block" || raw === "correct") return raw;
  process.stderr.write(
    `[runtime-codex] WARNING: unknown spec.guardrails.hallucinationDetector value ` +
    `"${raw}"; falling back to "block".\n`,
  );
  return "block";
}

// ── codex auth seeding ────────────────────────────────────────────────────────

/**
 * Seed authentication into CODEX_HOME by running `codex login --with-api-key`
 * with the API key piped to stdin. This writes auth.json into the CODEX_HOME
 * directory so that `codex exec` can authenticate without a keychain.
 *
 * Skips the seed step if auth.json already exists (idempotent for stable homes).
 * Throws if the login process exits non-zero.
 */
async function seedCodexAuth(homeDir: string, apiKey: string): Promise<void> {
  const authFile = join(homeDir, "auth.json");
  if (existsSync(authFile)) {
    return; // already seeded
  }

  await new Promise<void>((resolve, reject) => {
    const loginProc = spawn("codex", ["login", "--with-api-key"], {
      env: { ...process.env, CODEX_HOME: homeDir, HOME: homeDir },
      stdio: ["pipe", "pipe", "pipe"],
    });

    const stderrChunks: string[] = [];
    loginProc.stderr?.on("data", (chunk: Buffer | string) => {
      stderrChunks.push(typeof chunk === "string" ? chunk : chunk.toString("utf8"));
    });

    loginProc.stdin!.write(apiKey + "\n");
    loginProc.stdin!.end();

    loginProc.on("close", (code) => {
      if (code !== 0) {
        const stderrTail = stderrChunks.join("").slice(-2048).trim();
        reject(new Error(`runtime-codex: codex login failed: ${stderrTail || `exit code ${code}`}`));
      } else {
        resolve();
      }
    });

    loginProc.on("error", (err) => {
      reject(new Error(`runtime-codex: codex login failed: ${err.message}`));
    });
  });
}

// ── stdin reading ─────────────────────────────────────────────────────────────

async function readSpecFromStdin(): Promise<CompiledSpec> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8").trim();
  if (raw.length === 0) {
    throw new Error("runtime-codex: stdin was empty; expected a JSON-encoded CompiledSpec");
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    throw new Error(`runtime-codex: failed to parse stdin as JSON: ${detail}`);
  }
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error("runtime-codex: stdin parsed to a non-object value");
  }
  const spec = parsed as Partial<CompiledSpec>;
  if (typeof spec.v !== "number" || spec.v !== 1) {
    throw new Error(`runtime-codex: unsupported CompiledSpec version ${spec.v}; expected 1`);
  }
  if (!spec.metadata?.name) {
    throw new Error("runtime-codex: CompiledSpec.metadata.name is required");
  }
  if (!spec.model?.provider || !spec.model?.name) {
    throw new Error("runtime-codex: CompiledSpec.model.provider and .name are required");
  }
  return spec as CompiledSpec;
}

// ── skill body resolution ─────────────────────────────────────────────────────

/**
 * Strip YAML frontmatter (--- ... ---) from a Markdown file and return the
 * body. Returns the input unchanged when no frontmatter delimiter is present.
 */
function stripFrontmatter(raw: string): string {
  return raw.replace(/^---\s*\n[\s\S]*?\n---\s*\n?/, "");
}

/**
 * Read each spec.skills[].entrypoint SKILL.md file and return pre-resolved
 * SkillBody objects. Missing/unreadable files emit a stderr warning and are
 * skipped (tolerant, mirrors opencode adapter behavior).
 */
function readSkillBodies(skills: ResolvedRef[]): SkillBody[] {
  const out: SkillBody[] = [];
  for (const s of skills) {
    if (!s.entrypoint) continue;
    try {
      const raw = readFileSync(s.entrypoint, "utf8");
      const body = stripFrontmatter(raw);
      if (body.trim().length > 0) out.push({ name: s.name, body });
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      process.stderr.write(
        `[runtime-codex] WARNING: could not read skill ${s.name} at ${s.entrypoint}: ${msg}\n`,
      );
    }
  }
  return out;
}

// ── main ──────────────────────────────────────────────────────────────────────

async function main(): Promise<void> {
  // 1. Parse stdin.
  let spec: CompiledSpec;
  try {
    spec = await readSpecFromStdin();
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    process.stderr.write(`[runtime-codex] FATAL: ${msg}\n`);
    process.exitCode = 2;
    return;
  }

  // 2. Defense-in-depth: reject unsupported features before spawning anything.
  const sessionId = makeSessionId();

  try {
    assertCodexCompatible(spec);
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    process.stderr.write(`[runtime-codex] ERROR: ${msg}\n`);
    emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
      reason: "error" as const,
      message: msg,
    }));
    process.exitCode = 1;
    return;
  }

  // 3. Check for OPENAI_API_KEY before doing anything that requires it.
  const apiKey = process.env.OPENAI_API_KEY;
  if (!apiKey) {
    const msg = "runtime-codex: OPENAI_API_KEY is required to authenticate the codex CLI";
    process.stderr.write(`[runtime-codex] ERROR: ${msg}\n`);
    emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
      reason: "error" as const,
      message: msg,
    }));
    process.exitCode = 1;
    return;
  }

  // 4. Resolve (or create) CODEX_HOME.
  const home = resolveCodexHome(spec.sessionId);

  try {
    // 5. Seed codex authentication into CODEX_HOME.
    try {
      await seedCodexAuth(home.dir, apiKey);
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      process.stderr.write(`[runtime-codex] ERROR: ${msg}\n`);
      emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
        reason: "error" as const,
        message: msg,
      }));
      process.exitCode = 1;
      return;
    }

    // 6. Write MCP server config into CODEX_HOME/config.toml (if any).
    //    buildCodexArgs omits --ignore-user-config when MCP servers are present,
    //    so codex will read this file. The clean CODEX_HOME provides isolation.
    const configToml = buildConfigToml(spec);
    if (configToml) {
      writeFileSync(join(home.dir, "config.toml"), configToml, "utf8");
    }

    // 7. Read skill bodies.
    const skillBodies = readSkillBodies(spec.skills ?? []);

    // 8. Build codex argv.
    //    For stable homes (spec.sessionId set), read any persisted thread_id from
    //    the previous turn so that codex can resume the conversation.
    //    For ephemeral homes (one-shot, no sessionId), never resume.
    const storedThreadId = !home.ephemeral ? readPersistedThreadId(home.dir) : undefined;
    const args = buildCodexArgs(spec, {
      cwd: process.cwd(),
      skills: skillBodies,
      resumeThreadId: storedThreadId,
    });

    // 9. Spawn `codex exec`.
    const child = spawn("codex", args, {
      env: { ...process.env, CODEX_HOME: home.dir, HOME: home.dir },
      cwd: process.cwd(),
      stdio: ["ignore", "pipe", "pipe"],
    });

    // Collect stderr tail for error reporting (last 2 KiB).
    const stderrChunks: string[] = [];
    child.stderr?.on("data", (chunk: Buffer | string) => {
      stderrChunks.push(typeof chunk === "string" ? chunk : chunk.toString("utf8"));
    });

    // 10. Translate JSONL stdout line-by-line.
    const hallucinationMode = resolveHallucinationMode(spec);
    const state = createTranslatorState();
    const rl = createInterface({ input: child.stdout!, crlfDelay: Infinity });

    rl.on("line", (line: string) => {
      const { events } = translateCodexLine(line, sessionId, state, hallucinationMode);
      for (const ev of events) {
        emit(process.stdout.write.bind(process.stdout), ev);
      }
    });

    // 11. Wait for child to exit.
    await new Promise<void>((resolve) => {
      child.on("close", (code) => {
        rl.close();
        if (!state.ended && code !== 0) {
          const stderrTail = stderrChunks.join("").slice(-2048);
          emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
            reason: "error" as const,
            message: stderrTail.trim() || `codex exited with code ${code}`,
          }));
          process.exitCode = 1;
        } else if (!state.ended) {
          // Exited 0 but no turn.completed was seen — emit a synthetic ended.
          emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
            reason: "completed" as const,
          }));
        }
        resolve();
      });

      // Handle spawn errors (e.g. codex not on PATH).
      child.on("error", (err) => {
        rl.close();
        if (!state.ended) {
          emit(process.stdout.write.bind(process.stdout), stamp(sessionId, "session.ended", {
            reason: "error" as const,
            message: err.message,
          }));
          state.ended = true;
        }
        process.exitCode = 1;
        resolve();
      });
    });

    // 12. Persist the captured thread_id for the next turn (stable homes only).
    //     Ephemeral homes are wiped in the finally block so persistence would be
    //     pointless; we skip it to avoid any I/O on a dir about to be deleted.
    if (!home.ephemeral) {
      persistThreadId(home.dir, state.threadId);
    }
  } finally {
    // 12. Wipe ephemeral CODEX_HOME.
    if (home.ephemeral) {
      try {
        rmSync(home.dir, { recursive: true, force: true });
      } catch {
        // Non-fatal — don't mask a real error.
      }
    }
  }
}

main().catch((err) => {
  process.stderr.write(`[runtime-codex] UNHANDLED: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exitCode = 1;
});
