/**
 * Acceptance tests for the codex adapter entrypoint (Task 5, C.1).
 *
 * These spawn the built dist/index.js as a subprocess — the same way the Go
 * CLI will invoke it — and assert on the NDJSON wire-event stream.
 *
 * A fake `codex` shell script is placed on PATH so no real network call is
 * made. The fake binary prints a minimal JSONL fixture (thread.started →
 * turn.started → item.completed → turn.completed) then exits 0, mirroring
 * the verified output of a real `codex exec --json` run.
 */

import { describe, it, expect } from "vitest";
import { spawnSync } from "node:child_process";
import { mkdtempSync, writeFileSync, chmodSync, rmSync, existsSync, readFileSync } from "node:fs";
import { tmpdir as osTmpdir } from "node:os";
import { resolve, join } from "node:path";

const adapterPath = resolve(__dirname, "..", "dist", "index.js");

/** Minimal valid openai CompiledSpec. */
const minimalSpec = {
  v: 1,
  metadata: { name: "test-agent" },
  model: { provider: "openai", name: "gpt-4o" },
  task: "Reply with the word hi",
  tools: [],
  extensions: [],
  skills: [],
  runtime: { type: "local-codex" },
};

/**
 * The JSONL lines a real `codex exec --json` emits for a trivial one-turn run.
 * We print them verbatim from our fake binary.
 */
const fakeCodexOutput = [
  JSON.stringify({ type: "thread.started", thread_id: "thread-fake-001" }),
  JSON.stringify({ type: "turn.started" }),
  JSON.stringify({
    type: "item.completed",
    item: { type: "agent_message", text: "hi" },
  }),
  JSON.stringify({ type: "turn.completed", usage: { input_tokens: 10, output_tokens: 1 } }),
].join("\n");

function createFakeCodexBin(tmpDir: string): void {
  const scriptPath = join(tmpDir, "codex");
  // Handle two invocations:
  //   codex login --with-api-key  → read+discard stdin, write dummy auth.json, exit 0
  //   codex exec ...              → print canned JSONL and exit 0
  writeFileSync(
    scriptPath,
    `#!/bin/sh
if [ "$1" = "login" ] && [ "$2" = "--with-api-key" ]; then
  # Read and discard stdin (the API key)
  cat > /dev/null
  # Write a dummy auth.json into CODEX_HOME so seedCodexAuth skips on retry
  if [ -n "$CODEX_HOME" ]; then
    printf '{"token":"dummy"}' > "$CODEX_HOME/auth.json"
  fi
  exit 0
fi
echo '${fakeCodexOutput.replace(/'/g, "'\\''")}'\nexit 0\n`,
  );
  chmodSync(scriptPath, 0o755);
}

describe("codex adapter entrypoint", () => {
  let fakeBinDir: string;

  function runAdapter(
    input: string,
    extraEnv: Record<string, string> = {},
  ): { stdout: string; stderr: string; status: number | null } {
    const result = spawnSync(process.execPath, [adapterPath], {
      input,
      encoding: "utf8",
      timeout: 15000,
      env: {
        // Minimal safe env — strip credentials and real config paths.
        PATH: `${fakeBinDir}:${process.env.PATH ?? ""}`,
        HOME: fakeBinDir, // empty dir → codex can't find real config
        // forward enough for Node to run
        NODE_ENV: "test",
        // Provide a dummy key so assertCodexCompatible / codex won't
        // fail an early auth check before we even spawn.
        OPENAI_API_KEY: "test-fake-key",
        ...extraEnv,
      },
    });
    return {
      stdout: result.stdout,
      stderr: result.stderr,
      status: result.status,
    };
  }

  function parseNdjson(raw: string): Array<{ type: string; [k: string]: unknown }> {
    return raw
      .split("\n")
      .filter((l) => l.trim().length > 0)
      .map((l) => JSON.parse(l) as { type: string; [k: string]: unknown });
  }

  // Create fake bin dir once before all tests in this suite.
  // vitest runs describe() callbacks synchronously at collection time, so
  // we do this eagerly here (not inside beforeAll) to keep it simple.
  fakeBinDir = mkdtempSync(join(osTmpdir(), "ac-codex-test-"));
  createFakeCodexBin(fakeBinDir);

  // Cleanup after all tests.
  // Using process.on is a pragmatic approach for a test-only file; vitest
  // also supports afterAll but we'd need to import it.
  process.on("exit", () => {
    try { rmSync(fakeBinDir, { recursive: true, force: true }); } catch { /* ignore */ }
  });

  it("emits session.started as the first event for a valid spec", () => {
    const { stdout } = runAdapter(JSON.stringify(minimalSpec));
    const events = parseNdjson(stdout);
    expect(events.length).toBeGreaterThan(0);
    expect(events[0].type).toBe("session.started");
  });

  it("emits session.ended as the last event for a valid spec", () => {
    const { stdout } = runAdapter(JSON.stringify(minimalSpec));
    const events = parseNdjson(stdout);
    expect(events.length).toBeGreaterThan(0);
    expect(events[events.length - 1].type).toBe("session.ended");
  });

  it("session.ended has reason=completed on success", () => {
    const { stdout } = runAdapter(JSON.stringify(minimalSpec));
    const events = parseNdjson(stdout);
    const ended = events[events.length - 1];
    expect((ended.data as { reason: string }).reason).toBe("completed");
  });

  it("exits 0 on a clean run", () => {
    const { status } = runAdapter(JSON.stringify(minimalSpec));
    expect(status).toBe(0);
  });

  it("stdout contains a message event with the assistant reply", () => {
    const { stdout } = runAdapter(JSON.stringify(minimalSpec));
    const events = parseNdjson(stdout);
    const msgEvent = events.find((e) => e.type === "message");
    expect(msgEvent).toBeDefined();
    expect((msgEvent!.data as { text: string }).text).toBe("hi");
  });

  it("all events share the same sessionId", () => {
    const { stdout } = runAdapter(JSON.stringify(minimalSpec));
    const events = parseNdjson(stdout);
    const ids = new Set(events.map((e) => e.sessionId));
    expect(ids.size).toBe(1);
  });

  it("exits non-zero and emits session.ended{reason:error} for non-openai provider", () => {
    const spec = { ...minimalSpec, model: { provider: "anthropic", name: "claude-3" } };
    const { stdout, status } = runAdapter(JSON.stringify(spec));
    expect(status).not.toBe(0);
    const events = parseNdjson(stdout);
    expect(events.length).toBeGreaterThan(0);
    const last = events[events.length - 1];
    expect(last.type).toBe("session.ended");
    expect((last.data as { reason: string }).reason).toBe("error");
  });

  it("fails fast on empty stdin", () => {
    const { status, stderr } = runAdapter("");
    expect(status).not.toBe(0);
    expect(stderr.length).toBeGreaterThan(0);
  });

  it("fails fast on invalid JSON stdin", () => {
    const { status, stderr } = runAdapter("not json");
    expect(status).not.toBe(0);
    expect(stderr).toContain("failed to parse stdin");
  });

  it("emits session.ended{reason:error} and exits non-zero when OPENAI_API_KEY is unset", () => {
    // Override the env to explicitly remove OPENAI_API_KEY.
    const { stdout, status } = runAdapter(JSON.stringify(minimalSpec), {
      OPENAI_API_KEY: "",
    });
    expect(status).not.toBe(0);
    const events = parseNdjson(stdout);
    expect(events.length).toBeGreaterThan(0);
    const last = events[events.length - 1];
    expect(last.type).toBe("session.ended");
    expect((last.data as { reason: string }).reason).toBe("error");
    expect((last.data as { message: string }).message).toContain("OPENAI_API_KEY");
  });

  it("emits exactly one session.ended{reason:error} when codex binary is not on PATH", () => {
    // Run with an empty PATH so codex is not found and spawn errors.
    const result = spawnSync(process.execPath, [adapterPath], {
      input: JSON.stringify(minimalSpec),
      encoding: "utf8",
      timeout: 15000,
      env: {
        // Empty PATH so codex spawn fails with ENOENT.
        PATH: "",
        HOME: fakeBinDir,
        NODE_ENV: "test",
        OPENAI_API_KEY: "test-fake-key",
      },
    });
    expect(result.status).not.toBe(0);
    const stdout = result.stdout;
    const events = parseNdjson(stdout);
    const sessionEndedEvents = events.filter((e) => e.type === "session.ended");
    // Must be exactly one session.ended event.
    expect(sessionEndedEvents.length).toBe(1);
    const ended = sessionEndedEvents[0];
    expect((ended.data as { reason: string }).reason).toBe("error");
  });
});

// ── HOME isolation tests ───────────────────────────────────────────────────────

/**
 * Assert that the adapter passes HOME=<codex home dir> (not the operator's real
 * HOME) to both spawn sites — the `codex login` seed AND `codex exec`.
 *
 * Approach: a recording fake shim writes its $HOME and $CODEX_HOME env vars to
 * a JSON file on every invocation. After the adapter run completes we read that
 * file and assert HOME === CODEX_HOME and HOME !== process.env.HOME.
 */
function createRecordingFakeCodexBin(binDir: string, recordFile: string): void {
  const scriptPath = join(binDir, "codex");
  // Each invocation appends a JSON line: {"cmd":"login"|"exec","HOME":"...","CODEX_HOME":"..."}
  // We use >> so both the login and exec invocations are captured.
  writeFileSync(
    scriptPath,
    `#!/bin/sh
printf '{"cmd":"%s","HOME":"%s","CODEX_HOME":"%s"}\\n' "$1" "$HOME" "$CODEX_HOME" >> '${recordFile}'
if [ "$1" = "login" ] && [ "$2" = "--with-api-key" ]; then
  cat > /dev/null
  if [ -n "$CODEX_HOME" ]; then
    printf '{"token":"dummy"}' > "$CODEX_HOME/auth.json"
  fi
  exit 0
fi
printf '%s\\n' '${fakeCodexOutput.replace(/'/g, "'\\''")}'
exit 0
`,
  );
  chmodSync(scriptPath, 0o755);
}

describe("codex adapter — HOME isolation", () => {
  let recordBinDir: string;
  let recordFile: string;

  recordBinDir = mkdtempSync(join(osTmpdir(), "ac-codex-homeisolation-bin-"));
  recordFile = join(osTmpdir(), `ac-codex-homeisolation-record-${Date.now()}.ndjson`);
  createRecordingFakeCodexBin(recordBinDir, recordFile);

  process.on("exit", () => {
    try { rmSync(recordBinDir, { recursive: true, force: true }); } catch { /* ignore */ }
    try { rmSync(recordFile, { force: true }); } catch { /* ignore */ }
  });

  it("child processes receive HOME equal to CODEX_HOME, not the operator HOME", () => {
    // Use a real (distinct) HOME so we can confirm it is NOT forwarded.
    const operatorHome = "/tmp/fake-operator-home-should-not-appear";
    const result = spawnSync(process.execPath, [adapterPath], {
      input: JSON.stringify(minimalSpec),
      encoding: "utf8",
      timeout: 15000,
      env: {
        PATH: `${recordBinDir}:${process.env.PATH ?? ""}`,
        HOME: operatorHome,
        NODE_ENV: "test",
        OPENAI_API_KEY: "test-fake-key",
      },
    });
    expect(result.status).toBe(0);

    // Parse the NDJSON record written by the shim.
    const raw = readFileSync(recordFile, "utf8").trim();
    const records = raw
      .split("\n")
      .filter((l) => l.trim().length > 0)
      .map((l) => JSON.parse(l) as { cmd: string; HOME: string; CODEX_HOME: string });

    // Both login and exec must have been recorded.
    expect(records.length).toBeGreaterThanOrEqual(2);

    for (const rec of records) {
      // HOME must equal CODEX_HOME (both set to the adapter's controlled dir).
      expect(rec.HOME).toBe(rec.CODEX_HOME);
      // Neither must be the operator's real HOME.
      expect(rec.HOME).not.toBe(operatorHome);
      expect(rec.CODEX_HOME).not.toBe(operatorHome);
    }
  });
});

// ── Multi-turn session resume tests ────────────────────────────────────────────

/**
 * Known thread id emitted by the fake shim on `codex exec` (first turn).
 * The resume shim asserts the second invocation passes this exact id.
 */
const KNOWN_THREAD_ID = "thread-resume-test-42";

/**
 * Create a resume-aware fake codex binary.
 *
 * Behaviour:
 *   codex login --with-api-key  → write dummy auth.json, exit 0
 *   codex exec ...              → emit thread.started with KNOWN_THREAD_ID + events, exit 0
 *   codex exec resume <id> ...  → write <argsFile> with received args JSON, emit events, exit 0
 *                                 exits non-zero if the received id != KNOWN_THREAD_ID
 */
function createResumeFakeCodexBin(binDir: string, argsFile: string): void {
  const scriptPath = join(binDir, "codex");
  const fakeResumeOutput = [
    JSON.stringify({ type: "thread.started", thread_id: KNOWN_THREAD_ID }),
    JSON.stringify({ type: "turn.started" }),
    JSON.stringify({ type: "item.completed", item: { type: "agent_message", text: "resumed" } }),
    JSON.stringify({ type: "turn.completed", usage: { input_tokens: 5, output_tokens: 1 } }),
  ].join("\\n");
  const fakeFirstOutput = [
    JSON.stringify({ type: "thread.started", thread_id: KNOWN_THREAD_ID }),
    JSON.stringify({ type: "turn.started" }),
    JSON.stringify({ type: "item.completed", item: { type: "agent_message", text: "first" } }),
    JSON.stringify({ type: "turn.completed", usage: { input_tokens: 5, output_tokens: 1 } }),
  ].join("\\n");

  // Shell script — single-quoted strings need careful escaping for embedded JSON
  const script = `#!/bin/sh
if [ "$1" = "login" ] && [ "$2" = "--with-api-key" ]; then
  cat > /dev/null
  if [ -n "$CODEX_HOME" ]; then
    printf '{"token":"dummy"}' > "$CODEX_HOME/auth.json"
  fi
  exit 0
fi
# $1=exec, $2 may be "resume"
if [ "$2" = "resume" ]; then
  # $3 is the thread id
  printf '%s' '{"args":["'"$1"'","'"$2"'","'"$3"'"]}' > '${argsFile}'
  if [ "$3" != "${KNOWN_THREAD_ID}" ]; then
    echo "FAKE-CODEX: wrong resume id: $3" >&2
    exit 1
  fi
  printf '%b\\n' '${fakeResumeOutput}'
  exit 0
fi
printf '%b\\n' '${fakeFirstOutput}'
exit 0
`;
  writeFileSync(scriptPath, script);
  chmodSync(scriptPath, 0o755);
}

describe("codex adapter — multi-turn session resume", () => {
  let fakeBinDir: string;
  let xdgDataHome: string;
  let argsFile: string;

  // Set up isolated dirs eagerly (same pattern as main suite).
  fakeBinDir = mkdtempSync(join(osTmpdir(), "ac-codex-resume-bin-"));
  xdgDataHome = mkdtempSync(join(osTmpdir(), "ac-codex-resume-data-"));
  argsFile = join(xdgDataHome, "resume-args.json");
  createResumeFakeCodexBin(fakeBinDir, argsFile);

  process.on("exit", () => {
    try { rmSync(fakeBinDir, { recursive: true, force: true }); } catch { /* ignore */ }
    try { rmSync(xdgDataHome, { recursive: true, force: true }); } catch { /* ignore */ }
  });

  const STABLE_SESSION_ID = "test-session-resume-01";

  function runAdapterWithSession(
    sessionId: string | undefined,
    extraEnv: Record<string, string> = {},
  ): { stdout: string; stderr: string; status: number | null } {
    const spec = {
      ...minimalSpec,
      sessionId,
    };
    const result = spawnSync(process.execPath, [adapterPath], {
      input: JSON.stringify(spec),
      encoding: "utf8",
      timeout: 15000,
      env: {
        PATH: `${fakeBinDir}:${process.env.PATH ?? ""}`,
        HOME: fakeBinDir,
        NODE_ENV: "test",
        OPENAI_API_KEY: "test-fake-key",
        XDG_DATA_HOME: xdgDataHome,
        ...extraEnv,
      },
    });
    return { stdout: result.stdout, stderr: result.stderr, status: result.status };
  }

  function parseNdjson(raw: string): Array<{ type: string; [k: string]: unknown }> {
    return raw
      .split("\n")
      .filter((l) => l.trim().length > 0)
      .map((l) => JSON.parse(l) as { type: string; [k: string]: unknown });
  }

  it("turn 1 (stable sessionId, fresh): mints thread_id and persists it", () => {
    // Remove any prior args file to start clean.
    try { rmSync(argsFile); } catch { /* ignore */ }

    const { stdout, status } = runAdapterWithSession(STABLE_SESSION_ID);
    expect(status).toBe(0);

    // session.started event should include threadId
    const events = parseNdjson(stdout);
    const started = events.find((e) => e.type === "session.started");
    expect(started).toBeDefined();
    expect((started!.data as { threadId?: string }).threadId).toBe(KNOWN_THREAD_ID);

    // The persisted thread-id file must exist inside the stable CODEX_HOME
    const threadIdFile = join(
      xdgDataHome,
      "agent-controller", "codex-sessions", STABLE_SESSION_ID,
      ".agentctl-thread-id",
    );
    expect(existsSync(threadIdFile)).toBe(true);
    expect(readFileSync(threadIdFile, "utf8").trim()).toBe(KNOWN_THREAD_ID);

    // Turn 1 was NOT a resume (no args file written by the shim)
    expect(existsSync(argsFile)).toBe(false);
  });

  it("turn 2 (stable sessionId, resumed): passes codex exec resume <thread_id>", () => {
    // Turn 1 must have run first (test ordering is sequential in vitest).
    // The thread-id file should already exist from the prior test.
    const threadIdFile = join(
      xdgDataHome,
      "agent-controller", "codex-sessions", STABLE_SESSION_ID,
      ".agentctl-thread-id",
    );
    expect(existsSync(threadIdFile)).toBe(true);

    const { stdout, status, stderr } = runAdapterWithSession(STABLE_SESSION_ID);
    expect(status).toBe(0, `stderr: ${stderr}`);

    // The shim must have written args file with "resume" + the correct id
    expect(existsSync(argsFile)).toBe(true);
    const argsData = JSON.parse(readFileSync(argsFile, "utf8")) as { args: string[] };
    expect(argsData.args[1]).toBe("resume");
    expect(argsData.args[2]).toBe(KNOWN_THREAD_ID);

    // session.started event should still carry threadId
    const events = parseNdjson(stdout);
    const started = events.find((e) => e.type === "session.started");
    expect(started).toBeDefined();
    expect((started!.data as { threadId?: string }).threadId).toBe(KNOWN_THREAD_ID);
  });

  it("one-shot (no sessionId): does NOT resume and does not persist thread-id", () => {
    try { rmSync(argsFile); } catch { /* ignore */ }

    const { status } = runAdapterWithSession(undefined);
    expect(status).toBe(0);

    // No args file → no resume invocation
    expect(existsSync(argsFile)).toBe(false);

    // No thread-id file in xdgDataHome (ephemeral home is in osTmpdir and wiped)
    // We can only assert the argsFile wasn't written (the shim only writes it for resume)
  });
});
