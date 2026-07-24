/**
 * Opencode adapter entrypoint — Phase 2 of the v0.2 execution plan.
 *
 * This adapter accepts a CompiledSpec on stdin (one JSON document, then
 * EOF), spawns an opencode server via @opencode-ai/sdk, drives a session
 * with spec.task as the prompt, translates opencode's SSE events into our
 * wire-protocol NDJSON on stdout, and exits with a non-zero code when the
 * session ends in error.
 *
 * Wire protocol contract: cli/internal/wire/events.go
 * Event translation: src/event-translator.ts (pure, testable separately)
 * Config mapping:    src/opencode-config.ts (pure, no SDK dependency)
 * Honesty guardrails: src/honesty.ts (mirrored from runtime/)
 *
 * Operational notes:
 *   - opencode is spawned as a child process by createOpencode(). On
 *     normal exit, server.close() is called in the finally block.
 *   - The SSE event stream is global (all sessions on the opencode
 *     instance); translateEvent filters to our session ID.
 *   - The hallucination guardrail mode from spec.guardrails is forwarded
 *     to translateEvent. The "correct" mode re-prompts once when the
 *     model fabricates XML (mirrors Pi adapter behavior).
 *   - This adapter does NOT yet handle: skill body inlining, MCP servers,
 *     subagents. Those land in slice 2.5. When a spec declares those
 *     features the adapter currently ignores them (the effective behavior
 *     is that the model runs without those tools/skills). Slice 2.5 will
 *     either wire them or emit a compile-time rejection.
 */
import { mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { makeTempWorkdir, resolveDataDir } from "./workdir.js";
import { createOpencode } from "@opencode-ai/sdk";
import { normalizeAnthropicBaseUrlForOpencode } from "./anthropic-base-url.js";
import { stamp } from "./wire.js";
import type { CompiledSpec, HallucinationMode, ResolvedRef, WireEvent } from "./types.js";
import type { SkillBody, SubagentDefinition } from "./opencode-config.js";
import { buildOpencodeConfig } from "./opencode-config.js";
import { translateEvent, createTranslatorState } from "./event-translator.js";
import { CORRECTION_PROMPT } from "./honesty.js";
import {
  EVENTS_API_VERSION_V1ALPHA1,
  initAdapterTracing,
  type AdapterTracing,
} from "./observability.js";
import { fileURLToPath } from "node:url";

/** Read once at module load — embedded as service.version in OTel spans. */
const RUNTIME_PACKAGE_VERSION: string = (() => {
  try {
    const pkgPath = fileURLToPath(new URL("../package.json", import.meta.url));
    return JSON.parse(readFileSync(pkgPath, "utf8")).version as string;
  } catch {
    return "0.0.0";
  }
})();

// ── stdin reading ──────────────────────────────────────────────────────────

async function readSpecFromStdin(): Promise<CompiledSpec> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8").trim();
  if (raw.length === 0) {
    throw new Error("runtime-opencode: stdin was empty; expected a JSON-encoded CompiledSpec");
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    const detail = err instanceof Error ? err.message : String(err);
    throw new Error(`runtime-opencode: failed to parse stdin as JSON: ${detail}`);
  }
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error("runtime-opencode: stdin parsed to a non-object value");
  }
  const spec = parsed as Partial<CompiledSpec>;
  if (typeof spec.v !== "number" || spec.v !== 1) {
    throw new Error(`runtime-opencode: unsupported CompiledSpec version ${spec.v}; expected 1`);
  }
  if (!spec.metadata?.name) {
    throw new Error("runtime-opencode: CompiledSpec.metadata.name is required");
  }
  if (!spec.model?.provider || !spec.model?.name) {
    throw new Error("runtime-opencode: CompiledSpec.model.provider and .name are required");
  }
  return spec as CompiledSpec;
}

// ── wire event emitter ────────────────────────────────────────────────────

/**
 * Slice 5.4: when tracing is active for this run, every emitted wire
 * event picks up the slice-5.2 envelope additions (`apiVersion` +
 * `traceparent`). When off, this stays `undefined` and `emit()` falls
 * back to the legacy v0.x envelope shape — no per-event overhead and
 * existing wire consumers see no change.
 *
 * Module-level rather than threaded through `runOpencode` because
 * `emit()` is referenced from many call sites across the file and the
 * runtime adapter only handles ONE session per process. A worker pool
 * would need to scope this differently, but that's not a concern here.
 */
let activeTraceparent: string | undefined;

/**
 * Slice 5.4 codex pass 4: set synchronously by the signal handler so
 * `main()`'s outer catch can suppress cancellation-induced terminal
 * events even when the underlying SDK request rejects with a generic
 * `TypeError: fetch failed` (which doesn't match the "AbortError" /
 * "aborted" string check). The signal handler still owns the wire
 * stream's terminator and tracing flush in that case.
 *
 * Module-level rather than scoped to `runOpencode` because the throw
 * propagates up to `main()` after `runOpencode` has already returned
 * via its finally; main's catch needs the flag too.
 */
let cancelledByUserGlobal = false;

/**
 * Slice 5.4 codex pass 5: flipped TRUE right after a terminator (whether
 * cancelled, error, or completed) is emitted. emit() drops any later
 * event so the wire stream has at most one terminal `session.ended`.
 *
 * Why a separate flag from cancelledByUserGlobal: even on the success
 * path, the bounded-5s `tracing.end()` flush is a window where in-
 * flight SSE events could still be calling emit() — we want them
 * dropped there too. cancelledByUserGlobal is specifically about
 * suppressing main()'s terminator-emit when the handler already wrote
 * one; sessionTerminated is the broader "no more events after the
 * terminator" guarantee.
 */
let sessionTerminated = false;

function emit(ev: WireEvent): void {
  if (sessionTerminated) return;
  if (activeTraceparent) {
    ev = {
      ...ev,
      apiVersion: EVENTS_API_VERSION_V1ALPHA1,
      traceparent: activeTraceparent,
    };
  }
  process.stdout.write(JSON.stringify(ev) + "\n");
  if (ev.type === "session.ended") {
    sessionTerminated = true;
  }
}

// ── hallucination mode resolver ───────────────────────────────────────────

function resolveHallucinationMode(spec: CompiledSpec): HallucinationMode {
  const raw = spec.guardrails?.hallucinationDetector;
  if (!raw) return "block";
  if (raw === "warn" || raw === "block" || raw === "correct") return raw;
  process.stderr.write(
    `[runtime-opencode] WARNING: unknown spec.guardrails.hallucinationDetector value ` +
    `"${raw}"; falling back to "block".\n`,
  );
  return "block";
}

// ── skill + subagent resolution (slice 2.5) ───────────────────────────────

/**
 * Strip YAML frontmatter from the head of a Markdown file. Returns the body
 * portion only. If no frontmatter delimiter is present, returns the input
 * unchanged. Matches the same regex Pi adapter uses for skill body extraction.
 */
function stripFrontmatter(raw: string): string {
  return raw.replace(/^---\s*\n[\s\S]*?\n---\s*\n?/, "");
}

/**
 * Parse YAML frontmatter from the head of a Markdown file into a flat
 * Record<string, unknown>. Supports the small subset Pi's subagent format
 * uses: string keys with string values OR YAML list values
 * (e.g. `tools:\n  - bash\n  - read`). Quoted values are de-quoted.
 *
 * Intentionally tiny: we avoid pulling a full YAML dependency into runtime-
 * opencode for one config-file parser. Frontmatter that uses richer YAML
 * features (anchors, multiline strings, nested maps) will fall through
 * with the lines preserved verbatim — callers that need richer data should
 * extend this parser.
 */
function parseFrontmatter(raw: string): Record<string, unknown> {
  const match = raw.match(/^---\s*\n([\s\S]*?)\n---\s*\n?/);
  if (!match) return {};
  const body = match[1];
  const out: Record<string, unknown> = {};
  const lines = body.split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    // Skip blank lines and comment lines.
    if (!line.trim() || line.trim().startsWith("#")) { i++; continue; }
    const kv = line.match(/^([A-Za-z0-9_-]+):\s*(.*)$/);
    if (!kv) { i++; continue; }
    const key = kv[1];
    const rest = kv[2];
    // Reject YAML block-scalar indicators. Parsing `description: >-\n  multi\n  line`
    // would require a full YAML implementation. Rather than silently capture the
    // indicator as the value, throw so the spec author knows to inline.
    // Codex pass 5 of slice 2.5 caught block scalars being silently accepted.
    if (/^[|>][-+]?\s*$/.test(rest.trim())) {
      throw new Error(
        `runtime-opencode: frontmatter field "${key}" uses a YAML block-scalar ` +
        `indicator ("${rest.trim()}"). Block scalars are not supported by the ` +
        `adapter's inline frontmatter parser — please use a single-line value or ` +
        `quote the string.`,
      );
    }
    if (rest === "" || rest === undefined) {
      // Possible list:
      //   key:
      //     - foo
      //     - bar
      const list: string[] = [];
      let j = i + 1;
      while (j < lines.length) {
        const item = lines[j].match(/^\s*-\s+(.*)$/);
        if (!item) break;
        list.push(dequote(item[1].trim()));
        j++;
      }
      if (list.length > 0) {
        out[key] = list;
        i = j;
        continue;
      }
      out[key] = "";
      i++;
      continue;
    }
    out[key] = dequote(rest.trim());
    i++;
  }
  return out;
}

function dequote(s: string): string {
  if ((s.startsWith("\"") && s.endsWith("\"")) || (s.startsWith("'") && s.endsWith("'"))) {
    return s.slice(1, -1);
  }
  return s;
}

/**
 * Read each spec.skills[].entrypoint and return pre-resolved SkillBody
 * objects ready to be inlined into the system prompt by buildOpencodeConfig.
 * Missing/unreadable files emit a stderr warning and are skipped, matching
 * Pi adapter's tolerant behavior for malformed skill refs.
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
        `[runtime-opencode] WARNING: could not read skill ${s.name} at ${s.entrypoint}: ${msg}\n`,
      );
    }
  }
  return out;
}

/**
 * Read each spec.subagents[].entrypoint (.md file with YAML frontmatter),
 * parse the frontmatter, and return SubagentDefinition objects. Errors
 * (unreadable file, missing required frontmatter fields) throw — the spec
 * declared this subagent and the run cannot honor it without the data,
 * so failing fast is the right call.
 */
function readSubagentDefinitions(subagents: ResolvedRef[]): SubagentDefinition[] {
  const out: SubagentDefinition[] = [];
  for (const s of subagents) {
    if (!s.entrypoint) {
      throw new Error(
        `runtime-opencode: subagent "${s.name}" has no entrypoint. The compiler should ` +
        `resolve every spec.subagents[] ref to an absolute .md path.`,
      );
    }
    let raw: string;
    try {
      raw = readFileSync(s.entrypoint, "utf8");
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      throw new Error(`runtime-opencode: could not read subagent ${s.name} at ${s.entrypoint}: ${msg}`);
    }
    const fm = parseFrontmatter(raw);
    const fmName = typeof fm.name === "string" ? fm.name : s.name;
    const fmDescription = typeof fm.description === "string" ? fm.description : "";
    if (!fmDescription) {
      throw new Error(
        `runtime-opencode: subagent ${s.name} at ${s.entrypoint} is missing a ` +
        `"description" field in its YAML frontmatter. opencode requires a description ` +
        `to know when to invoke the subagent.`,
      );
    }
    const fmModel = typeof fm.model === "string" && fm.model.length > 0 ? fm.model : undefined;
    // Subagent frontmatter `tools` accepts two formats per Pi's loader:
    //   tools:
    //     - bash
    //     - read
    // OR the comma-separated scalar form:
    //   tools: bash,read
    // We accept both so existing subagent .md files continue to work
    // unchanged. Codex pass 1 of slice 2.5 caught that we only accepted
    // the array form, silently dropping the scalar form.
    let fmTools: string[] | undefined;
    if (Array.isArray(fm.tools)) {
      fmTools = fm.tools.filter((t): t is string => typeof t === "string");
    } else if (typeof fm.tools === "string" && fm.tools.length > 0) {
      fmTools = fm.tools.split(",").map((t) => t.trim()).filter((t) => t.length > 0);
    }
    const systemPrompt = stripFrontmatter(raw).trim();
    out.push({
      name: fmName,
      description: fmDescription,
      ...(fmTools ? { tools: fmTools } : {}),
      ...(fmModel ? { model: fmModel } : {}),
      systemPrompt,
    });
  }
  return out;
}

// ── main session loop ─────────────────────────────────────────────────────

async function runOpencode(
  spec: CompiledSpec,
  sessionId: string,
  tracing: AdapterTracing,
): Promise<boolean> {
  // Normalize ANTHROPIC_BASE_URL before any opencode subprocess inherits
  // process.env. Without this, a Pi-style URL (no /v1) makes opencode
  // request `${base}/messages` and the gateway returns 404. Slice 4.2.1
  // adds this so operators can set one env var for both adapters.
  const rawBaseUrl = process.env.ANTHROPIC_BASE_URL;
  if (rawBaseUrl) {
    const normalized = normalizeAnthropicBaseUrlForOpencode(rawBaseUrl);
    if (normalized && normalized !== rawBaseUrl) {
      process.env.ANTHROPIC_BASE_URL = normalized;
    }
  }

  const hallucinationMode = resolveHallucinationMode(spec);
  let configDir: string | undefined;
  let server: { url: string; close(): void } | undefined;

  // SIGINT/SIGTERM handler: when agentctl stops this process via
  // LocalBackend.Stop, Node exits without running the finally block.
  // We emit a `session.ended { reason: "cancelled" }` wire event so the CLI's
  // event loop observes a clean terminal event (not a synthetic runtime error),
  // then close the opencode server child so port 4096 isn't occupied by a
  // zombie for subsequent runs. Codex pass 4 caught the resource leak;
  // codex pass 5 caught the missing cancellation wire event.
  // AbortController for createOpencode startup. If SIGINT/SIGTERM arrives
  // during the `await createOpencode()` call, the SDK will abort the server
  // spawn attempt so we don't orphan the opencode child process. Without
  // this, `server` is still `undefined` when shutdownOnSignal fires and the
  // spawned process is left running. Codex pass 16 of slice 2.4 caught.
  const abortController = new AbortController();

  // Slice 5.4 codex pass 1: the original handler called process.exit(130)
  // synchronously, which bypassed the finally block AND the new OTel
  // shutdown. We now defer the exit through an async IIFE so the OTel
  // BatchSpanProcessor has its bounded-5s shutdown budget before the
  // process actually terminates. A re-entry guard prevents a second
  // signal during the flush from re-running the handler body.
  //
  // Slice 5.4 codex pass 2 added `cancelledByUser` — set synchronously
  // so the SSE main loop (which will start seeing the abort below
  // close its stream) can skip its own terminal-event emits and let
  // the handler's `cancelled` terminator be the only one on the wire.
  // Without this, the main loop would observe an abort-induced SSE
  // close, set errorMessage = "SSE stream closed without session.idle",
  // and append a duplicate `error` + `session.ended(error)` AFTER the
  // handler's `cancelled` terminator.
  let shutdownInProgress = false;
  const shutdownOnSignal = () => {
    if (shutdownInProgress) return;
    shutdownInProgress = true;
    cancelledByUserGlobal = true;
    abortController.abort(); // cancel any in-flight createOpencode() call
    // Clean up temp workdir so cancelled runs don't leak agent-controller-
    // opencode-* dirs in /tmp. Codex pass 35 caught that process.exit(130)
    // bypasses the finally block that does this cleanup normally.
    if (configDir) {
      try { rmSync(configDir, { recursive: true, force: true }); } catch { /* ignore */ }
    }
    void (async () => {
      try {
        // Use emit() rather than a raw stdout.write so the wire
        // terminator picks up apiVersion + traceparent when tracing
        // is active. Codex pass 1 of slice 5.4 caught the bypass.
        // emit() flips sessionTerminated=true after this call, so any
        // in-flight SSE event that lost the race won't appear after
        // the terminator (codex pass 5).
        emit(stamp(sessionId, "session.ended", { reason: "cancelled" }));
      } catch { /* ignore — best-effort wire stream */ }
      // process.exit() does NOT drain stdout. For small writes
      // `writableNeedDrain` stays false even though bytes are still
      // queued, so the previous codex-pass-2 drain check would have
      // let us exit before the cancelled terminator shipped — and
      // the CLI would miss the ONLY terminal event for the run.
      // The write-callback pattern is the reliable way: post an
      // empty write and only resolve when its callback fires, which
      // happens after all prior writes (including the cancelled
      // line above) have been handed to the OS. Codex pass 3 of
      // slice 5.4 caught the regression against the original
      // callback-based drain that lived here before slice 5.4.
      await new Promise<void>((resolve) => {
        process.stdout.write("", () => resolve());
      });
      // Close the opencode server BEFORE the bounded-5s tracing flush
      // (codex pass 5). If we awaited tracing.end first, the opencode
      // child could keep doing tool work for up to 5s after the user
      // pressed Ctrl-C — visible to the user as "ignored cancellation"
      // — and SSE events still arriving over the open connection
      // could in principle invoke emit() racing the terminator (the
      // sessionTerminated flag set above closes that race, but
      // closing the server is the more direct fix and cuts the wait).
      server?.close();
      // Flush OTel — bounded by the 5s shutdown budget inside end().
      // tracing.end() is idempotent so if a later path also calls it,
      // those are no-ops.
      activeTraceparent = undefined;
      await tracing.end("error", "cancelled");
      process.exit(130);
    })();
  };
  process.once("SIGINT", shutdownOnSignal);
  process.once("SIGTERM", shutdownOnSignal);

  try {
    // Config + cwd + HOME are ALWAYS an ephemeral temp dir (wiped on exit) so
    // no ambient config — and no config a prior turn may have written — leaks
    // into this run. Only opencode's DATA store (XDG_DATA_HOME, set below) is
    // stable per-session, which is what enables resume without leaking config.
    configDir = makeTempWorkdir();
    const dataDir = resolveDataDir(spec.sessionId, configDir);

    // Build the opencode config from the ADL spec. The SDK passes this to
    // opencode via the OPENCODE_CONFIG_CONTENT env var (not a config file).
    //
    // Slice 2.5 wires three previously-rejected fields:
    //   - spec.skills[]   → inline SKILL.md bodies into the system prompt
    //                       (no opencode-native concept; same pattern as Pi)
    //   - spec.subagents[] → opencode cfg.agent[name] with mode="subagent"
    //                        (opencode-native subagent support)
    //   - spec.mcpServers[] → opencode cfg.mcp[name] (opencode-native MCP)
    //
    // Fields that remain rejected (Pi-specific, no opencode equivalent):
    //   - spec.extensions[] — Pi extension JS modules don't run in opencode
    //   - spec.installs[]    — deprecated; use spec.extensions[].source
    //
    // As of v0.3.4 the canonical rejection lives in
    // cli/internal/adl/compiler.go::checkOpencodeIncompatibilities, so
    // `agentctl compile` catches these before any adapter starts. The
    // runtime checks below stay as defense-in-depth: a hand-crafted
    // CompiledSpec that bypasses the compiler (e.g. piped straight
    // into the adapter binary) still gets rejected here.
    const unsupportedFields: string[] = [];
    const allExtensions = spec.extensions ?? [];
    if (allExtensions.length > 0) {
      unsupportedFields.push(`spec.extensions (${allExtensions.length} declared) — Pi extension modules cannot run in opencode; the opencode adapter does not support custom Pi-format extensions`);
    }
    if ((spec.installs ?? []).length > 0) {
      unsupportedFields.push(`spec.installs (${spec.installs!.length} entries) — deprecated; use spec.extensions[].source on the Pi adapter (runtime.type: local)`);
    }
    if (unsupportedFields.length > 0) {
      throw new Error(
        `runtime-opencode: spec declares capabilities not supported by the opencode adapter:\n` +
        unsupportedFields.map((f) => `  - ${f}`).join("\n") + "\n" +
        "Either remove these from the spec or use runtime.type: local (Pi adapter) which supports them.",
      );
    }

    // Resolve skill bodies + subagent definitions BEFORE calling buildOpencodeConfig.
    // Both involve fs reads of the per-ref entrypoint paths the compiler resolved.
    const skillBodies = readSkillBodies(spec.skills ?? []);
    const subagentDefinitions = readSubagentDefinitions(spec.subagents ?? []);

    const cfg = buildOpencodeConfig(spec, { skillBodies, subagentDefinitions });

    // Isolate opencode from ALL ambient user config: skills, MCP servers,
    // plugins, CLAUDE.md files, ~/.opencode, XDG data dirs, etc. Redirect
    // HOME, XDG_CONFIG_HOME, and XDG_DATA_HOME to our temp workspace so the
    // spawned opencode process sees only our ADL-derived config.
    //
    // Auth implication: opencode must get credentials from environment variables
    // (ANTHROPIC_API_KEY, etc.) rather than from auth.json files in the real
    // HOME. This is the correct behavior for an ADL-governed agent — auth
    // comes from the operator's environment, not user-specific dotfiles. If
    // auth is stored only in ~/.opencode/auth.json, the local-opencode adapter
    // will fail to authenticate; set ANTHROPIC_API_KEY instead.
    //
    // Codex passes 32-34 escalated the isolation requirement: XDG_CONFIG_HOME
    // alone doesn't prevent opencode from loading ~/.opencode or CLAUDE.md.
    const opencodeXdgConfigDir = join(configDir, ".config", "opencode");
    mkdirSync(opencodeXdgConfigDir, { recursive: true });
    writeFileSync(
      join(opencodeXdgConfigDir, "opencode.json"),
      JSON.stringify(cfg, null, 2),
      "utf8",
    );
    const originalXdgConfigHome = process.env.XDG_CONFIG_HOME;
    const originalXdgDataHome = process.env.XDG_DATA_HOME;
    const originalHome = process.env.HOME;
    process.env.XDG_CONFIG_HOME = configDir;
    process.env.XDG_DATA_HOME = dataDir;
    process.env.HOME = configDir;

    // Spawn an opencode server + connected client. Use port 0 to let the OS
    // allocate a free ephemeral port instead of always binding to 4096.
    // Without dynamic ports, two concurrent local-opencode runs (or a dev's
    // existing opencode instance) would collide and fail at server start.
    // Codex pass 5 of slice 2.4 caught the fixed-port issue.
    // Preflight: ensure the `opencode` binary is available before spawning
    // the server. `which` is POSIX-only and fails on Windows or minimal
    // images. Instead we probe by running `opencode --version` with
    // `shell: true` so the OS (including cmd.exe) can find `.cmd` / `.bat`
    // variants via PATH. Codex pass 24 added the check; pass 25 caught the
    // cross-platform gap.
    const probeResult = spawnSync("opencode", ["--version"], {
      encoding: "utf8",
      shell: true,          // lets cmd.exe resolve opencode.cmd on Windows
      timeout: 5000,        // don't wait forever if the binary hangs
    });
    if (probeResult.error || (probeResult.status !== 0 && probeResult.status !== null)) {
      throw new Error(
        "runtime-opencode: the `opencode` CLI is not installed or not on PATH. " +
        "Install it with `npm install -g opencode-ai` or follow the setup guide at " +
        "https://opencode.ai/docs/. The local-opencode adapter requires the CLI to be " +
        "available as a separate process.",
      );
    }
    // Also chdir to configDir before spawning so the opencode server process
    // starts with configDir as its cwd. opencode scans cwd (and ancestors)
    // for project-level `.opencode/` or `opencode.json`; by starting in an
    // isolated temp dir that has no such files, we prevent project config
    // from leaking into the session. Sessions still reference projectCwd via
    // the `directory` query param per-request. Codex pass 33 caught that
    // XDG_CONFIG_HOME alone didn't isolate project-level configs.
    const originalCwd = process.cwd();
    process.chdir(configDir);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const oc = await createOpencode({ config: cfg as any, port: 0, signal: abortController.signal });
    // Restore cwd, HOME, and XDG env vars after the server spawns.
    process.chdir(originalCwd);
    if (originalHome === undefined) delete process.env.HOME;
    else process.env.HOME = originalHome;
    if (originalXdgConfigHome === undefined) delete process.env.XDG_CONFIG_HOME;
    else process.env.XDG_CONFIG_HOME = originalXdgConfigHome;
    if (originalXdgDataHome === undefined) delete process.env.XDG_DATA_HOME;
    else process.env.XDG_DATA_HOME = originalXdgDataHome;
    server = oc.server;
    const client = oc.client;

    // The session's working directory must be the caller's project root, not
    // the temp config dir. Specs that grant read/edit/bash tools need to
    // see the project files; editing in an empty temp dir then deleting it
    // would lose all work. Codex pass 2 of slice 2.4 caught the wrong cwd.
    const projectCwd = process.cwd();

    // (resume check was moved earlier, to the top of the try block, before
    // createOpencode() — codex pass 20 of slice 2.4 caught the ordering.)

    // Resume-or-create the opencode session. When agentctl passes a session
    // id, the persisted opencode store (under our stable workdir) may already
    // hold this session's base session from a prior turn — reuse it so the
    // conversation continues; otherwise create a fresh one. Mirrors opencode's
    // own `run --continue` (list → reuse the base session).
    let opencodeSessionId: string;
    if (spec.sessionId) {
      // opencode scopes `list` by `directory` server-side, so within our
      // per-session stable workdir this returns the session(s) for this
      // project. Pick the base session (no parentID); sub-agent children have
      // a parentID. Verified: a freshly-spawned server resolves the prior
      // turn's session from the persisted store this way.
      const listed = await client.session.list({ query: { directory: projectCwd } });
      const base = (listed.data ?? []).find((s) => !s.parentID);
      if (base) {
        opencodeSessionId = base.id;
      } else {
        const created = await client.session.create({ query: { directory: projectCwd } });
        if (!created.data) {
          throw new Error("opencode: session.create returned no data");
        }
        opencodeSessionId = created.data.id;
      }
    } else {
      const created = await client.session.create({ query: { directory: projectCwd } });
      if (!created.data) {
        throw new Error("opencode: session.create returned no data");
      }
      opencodeSessionId = created.data.id;
    }

    // SSE producer-consumer: subscribe to the global event stream and
    // immediately start a concurrent consumer (fire-and-forget IIFE) that
    // pushes events to an internal queue. This ensures the HTTP subscription
    // is active BEFORE we call promptAsync, so we never miss events.
    //
    // The SDK's client.global.event() returns a lazy generator; the HTTP
    // connection only starts on the first .next() call. By starting the IIFE
    // before promptAsync, the IIFE's for-await queues the first .next() in
    // the same microtask batch. By the time the promptAsync network round-trip
    // completes, the SSE connection is already live.
    //
    // P2 fix (codex pass 23): pass sseMaxRetryAttempts to cap retries so a
    // permanently-broken SSE connection terminates instead of retrying forever.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const sseStream = await client.global.event({ ...(({ sseMaxRetryAttempts: 3 }) as any) });

    const sseQueue: unknown[] = [];
    let sseEnded = false;
    let sseWaiter: (() => void) | undefined;

    // Track when the SSE connection is live so we can wait before prompting.
    let sseConnected = false;
    let sseConnectedResolve: (() => void) | undefined;
    const sseConnectedPromise = new Promise<void>((resolve) => {
      sseConnectedResolve = resolve;
    });

    (async () => {
      try {
        for await (const ev of sseStream.stream) {
          // The `server.connected` event signals the SSE subscription is live.
          // GlobalEvent has shape { directory, payload: { type, properties } }
          // — the type lives in ev.payload.type, NOT ev.type. Codex pass 29
          // caught the wrong path; pass 28 identified the race condition.
          const evPayloadType = (ev as { payload?: { type?: string } }).payload?.type;
          if (evPayloadType === "server.connected" && !sseConnected) {
            sseConnected = true;
            sseConnectedResolve?.();
            sseConnectedResolve = undefined;
          }
          sseQueue.push(ev);
          sseWaiter?.(); sseWaiter = undefined;
        }
      } catch (err) {
        sseConnectedResolve?.(); sseConnectedResolve = undefined;
        sseQueue.push({ __sseConnectionError: err instanceof Error ? err.message : String(err) });
        sseWaiter?.(); sseWaiter = undefined;
      } finally {
        sseConnectedResolve?.(); sseConnectedResolve = undefined;
        sseEnded = true;
        sseWaiter?.(); sseWaiter = undefined;
      }
    })();

    // Wait until the SSE subscription is live before prompting. If the
    // server.connected event doesn't arrive within 3s, we throw rather
    // than proceeding — a fast model turn could complete before the
    // adapter is subscribed, causing us to miss all events and report a
    // false success. Codex pass 31 caught the silent proceed-on-timeout.
    // Clear the timer when server.connected arrives before the timeout so
    // Node's event loop is not kept alive for the remainder of the 3 seconds.
    // Codex pass 35 of slice 2.4 caught the missing clearTimeout/unref.
    let sseConnectTimedOut = false;
    let sseConnectTimer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      sseConnectedPromise,
      new Promise<void>((resolve) => {
        sseConnectTimer = setTimeout(() => { sseConnectTimedOut = true; resolve(); }, 3000);
        // unref so the timer alone doesn't keep Node alive in normal exit paths
        sseConnectTimer.unref?.();
      }),
    ]);
    if (sseConnectTimer) { clearTimeout(sseConnectTimer); sseConnectTimer = undefined; }
    if (!sseConnected) {
      throw new Error(
        "runtime-opencode: timed out waiting for SSE server.connected event " +
        "(3s). opencode may be starting slowly or the global event stream is " +
        "unavailable. Try again or check the opencode server logs.",
      );
    }
    void sseConnectTimedOut; // suppress unused-variable warning

    // Wait until at least one event is in the queue or the stream ends/errors.
    async function waitForSseEvent(): Promise<void> {
      if (sseQueue.length > 0 || sseEnded) return;
      return new Promise<void>((resolve) => { sseWaiter = resolve; });
    }

    // Emit session.started now that opencode is up and the session exists.
    emit(stamp(sessionId, "session.started", {
      agentName: spec.metadata.name,
      model: spec.model,
    }));

    // promptAsync: submit the prompt after the SSE subscription is confirmed live.
    const promptResp = await client.session.promptAsync({
      path: { id: opencodeSessionId },
      query: { directory: projectCwd },
      body: {
        agent: spec.metadata.name,
        parts: [{ type: "text", text: spec.task }],
      },
    });

    // Check for HTTP-level errors on the prompt submission.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    if ((promptResp as any).error) {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const detail = (promptResp as any).error?.message ?? JSON.stringify((promptResp as any).error);
      throw new Error(`opencode: session.promptAsync failed: ${detail}`);
    }

    // Drive the event loop until session.idle (turn complete) or
    // session.error (terminal failure).
    let errorMessage: string | undefined;
    let correctionRequested = false;
    let correctionSent = false;
    const translatorState = createTranslatorState();

    // Helper to process one raw event from the SSE stream.
    function processRawEvent(rawEvent: unknown): { idle: boolean; error: string | undefined } {
      const gev = (rawEvent as { directory?: string; payload?: unknown }).payload
        ? (rawEvent as { directory: string; payload: unknown }) as import("@opencode-ai/sdk").GlobalEvent
        : undefined;
      if (!gev) return { idle: false, error: undefined };

      const result = translateEvent(gev, sessionId, opencodeSessionId, translatorState, hallucinationMode);

      for (const wev of result.wireEvents) {
        emit(wev);
        if (wev.type === "warning" && (wev.data as { kind?: string }).kind === "hallucinated_tool_call") {
          if (hallucinationMode === "correct" && !correctionSent) correctionRequested = true;
        }
        if (wev.type === "error") {
          errorMessage ??= (wev.data as { message?: string }).message ?? "opencode error";
        }
      }
      return { idle: result.sessionIdle, error: result.sessionError };
    }

    let mainLoopSawIdle = false;
    mainLoop: while (!errorMessage) {
      await waitForSseEvent();
      while (sseQueue.length > 0) {
        const rawEvent = sseQueue.shift();
        // Handle synthetic SSE connection error.
        if (rawEvent && typeof rawEvent === "object" && "__sseConnectionError" in (rawEvent as object)) {
          errorMessage ??= `SSE connection error: ${(rawEvent as { __sseConnectionError: string }).__sseConnectionError}`;
          break mainLoop;
        }
        const { idle, error } = processRawEvent(rawEvent);
        if (error) { errorMessage ??= error; break mainLoop; }
        if (idle) {
          if (correctionRequested && !correctionSent && !errorMessage) {
            correctionSent = true;
            const corrResp = await client.session.promptAsync({
              path: { id: opencodeSessionId },
              query: { directory: projectCwd },
              body: {
                agent: spec.metadata.name,
                parts: [{ type: "text", text: CORRECTION_PROMPT }],
              },
            });
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            if ((corrResp as any).error) {
              // eslint-disable-next-line @typescript-eslint/no-explicit-any
              errorMessage = `correction re-prompt failed: ${(corrResp as any).error?.message ?? "unknown"}`;
              break mainLoop;
            }
            // Keep looping for the second turn's idle.
            break; // break from inner while, outer while continues
          }
          mainLoopSawIdle = true;
          break mainLoop;
        }
      }
      // If the queue is now empty and the SSE stream ended, we're done waiting.
      if (sseEnded && sseQueue.length === 0) break;
    }

    // If the main loop exited without observing session.idle (or a
    // session.error which breaks with errorMessage set), the SSE stream closed
    // unexpectedly (server exit, EOF, network drop) before the turn completed.
    // Treat as an error — the turn is unfinished, not successfully completed.
    // Codex pass 13 of slice 2.4 caught this false-positive-success path.
    if (!errorMessage && !mainLoopSawIdle) {
      errorMessage = "opencode: SSE stream closed without session.idle — server may have exited before the turn completed";
    }

    // Slice 5.4 codex pass 2: if the user cancelled, the signal
    // handler's async IIFE is already running — it emitted the
    // `cancelled` terminator, owns the tracing flush, and will
    // process.exit(130). Skip our own terminal emits + tracing.end()
    // to avoid duplicating either, and return so the finally block
    // runs cleanup. The IIFE eventually wins the race to exit.
    if (cancelledByUserGlobal) {
      return false;
    }

    if (errorMessage) {
      // Codex pass 2 of slice 5.4: remove signal listeners BEFORE
      // tracing.end() so a SIGINT during the bounded-5s flush can't
      // fire shutdownOnSignal and produce a second terminal event.
      // Codex pass 5 of slice 5.4: ALSO close the opencode server
      // before the flush. If we left it running and Node received an
      // unhandled SIGINT during tracing.end (handlers already gone),
      // the default-exit path would skip the `finally` block and
      // orphan the opencode child. Closing now means even a hard
      // default-exit can't leak the child.
      process.removeListener("SIGINT", shutdownOnSignal);
      process.removeListener("SIGTERM", shutdownOnSignal);
      tracing.onError(errorMessage);
      emit(stamp(sessionId, "error", { message: errorMessage }));
      emit(stamp(sessionId, "session.ended", { reason: "error", message: errorMessage }));
      server?.close();
      activeTraceparent = undefined;
      await tracing.end("error", errorMessage);
      return false;
    }

    // Same listener removal + server close on the success path: the
    // bounded flush below is the last 5s we hold open in runOpencode
    // and a SIGINT during it must not (a) produce a `cancelled`
    // terminator on a completed run, nor (b) orphan the opencode
    // child if Node's default-exit skips the finally.
    process.removeListener("SIGINT", shutdownOnSignal);
    process.removeListener("SIGTERM", shutdownOnSignal);
    emit(stamp(sessionId, "session.ended", { reason: "completed" }));
    server?.close();
    activeTraceparent = undefined;
    await tracing.end("completed");
    return true;
  } finally {
    // Remove signal listeners so they don't fire again on normal exit.
    process.removeListener("SIGINT", shutdownOnSignal);
    process.removeListener("SIGTERM", shutdownOnSignal);
    server?.close();
    if (configDir) {
      try { rmSync(configDir, { recursive: true, force: true }); } catch { /* best-effort */ }
    }
    // Slice 5.4 codex pass 1: do NOT call tracing.end() here. The
    // happy/error paths inside this try block already do explicit
    // tracing.end(reason, message) with the real error message, and
    // the setup-failure path is handled by main()'s outer catch with
    // access to the actual exception. Calling end() here would race
    // those callers: this finally runs BEFORE main()'s catch sees the
    // throw, so a generic "terminated unexpectedly" message would
    // overwrite the real exception on the span and the terminal wire
    // events emitted from main() would lose the apiVersion+traceparent
    // envelope because activeTraceparent would already be cleared.
  }
}

// ── entry point ───────────────────────────────────────────────────────────

async function main(): Promise<number> {
  let spec: CompiledSpec;
  try {
    spec = await readSpecFromStdin();
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    process.stderr.write(`[runtime-opencode] ${message}\n`);
    return 2;
  }

  const sessionId = `s_${Date.now().toString(36)}`;

  // Slice 5.4: init OTel here (BEFORE runOpencode) so the setup-failure
  // path below — main's catch — can:
  //   (a) emit terminal wire events with the v1alpha1 envelope
  //       (apiVersion + traceparent) still attached via activeTraceparent,
  //   (b) call tracing.end("error", realMessage) with the actual
  //       exception text, not a generic placeholder.
  //
  // Codex pass 1 of slice 5.4 caught the original placement inside
  // runOpencode: setup throws (createOpencode, missing CLI, bad spec)
  // ended the span before main() could see the real failure.
  const tracing: AdapterTracing = initAdapterTracing({
    spec,
    sessionId: spec.sessionId ?? sessionId,
    packageVersion: RUNTIME_PACKAGE_VERSION,
  });
  activeTraceparent = tracing.getTraceparent();

  try {
    const ok = await runOpencode(spec, sessionId, tracing);
    return ok ? 0 : 1;
  } catch (err) {
    // Cancellation suppression. Three overlapping signals can land us
    // here during a user-cancelled run:
    //   - AbortError directly (e.g. `await createOpencode({signal})`)
    //   - "aborted" in the message (older SDK wrappers)
    //   - `TypeError: fetch failed` from an in-flight SDK request
    //     (session.create, promptAsync) whose underlying connection
    //     was severed by abortController.abort() — does NOT match the
    //     two patterns above
    // The cancelledByUserGlobal flag is the canonical signal: set
    // synchronously by the signal handler, so by the time main's catch
    // sees ANY of those errors, the handler IIFE is already running and
    // owns the terminator + flush. Codex pass 4 of slice 5.4 caught the
    // fetch-failed gap. The string checks remain as defense-in-depth.
    if (
      cancelledByUserGlobal ||
      (err instanceof Error && (err.name === "AbortError" || err.message.includes("aborted")))
    ) {
      return 130;
    }
    const message = err instanceof Error ? err.message : String(err);
    // Emit a terminal error on the wire so the CLI event loop sees it.
    // activeTraceparent is still set, so these stamp apiVersion + traceparent.
    emit(stamp(sessionId, "error", { message }));
    emit(stamp(sessionId, "session.ended", { reason: "error", message }));
    process.stderr.write(`[runtime-opencode] ${message}\n`);
    // Now flush tracing with the real exception message — the span's
    // status text carries the actual cause instead of "terminated
    // unexpectedly".
    activeTraceparent = undefined;
    await tracing.end("error", message);
    return 1;
  }
}

main()
  .then((code) => {
    // Use process.exitCode + allow Node to drain stdout naturally rather than
    // calling process.exit() directly. process.exit() does not wait for
    // buffered writes to flush, which can cause the CLI to miss session.ended.
    // Codex pass 27 of slice 2.4 caught this drain issue.
    process.exitCode = code;
  })
  .catch((err) => {
    const message = err instanceof Error ? err.message : String(err);
    process.stderr.write(`[runtime-opencode] uncaught: ${message}\n`);
    process.exitCode = 1;
  });
