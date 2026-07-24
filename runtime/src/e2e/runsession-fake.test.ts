/**
 * End-to-end test: runSession() against a real Pi loaded from
 * node_modules, with the LLM stream provided by the fake provider.
 *
 * No vi.mock calls in this file — that's the point. Real DefaultResourceLoader,
 * real SessionManager, real createAgentSession, real api-registry; only
 * the model stream is faked.
 *
 * This is the v0.2-prep harness: closes debt #5 (hermetic CI without
 * API keys) and provides the regression net for the Phase 2 opencode
 * adapter — once we add `runtime-opencode/`, the same examples can be
 * asserted against both adapters to confirm wire-event parity.
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import * as os from "node:os";
import * as path from "node:path";
import * as fs from "node:fs";
import {
  clearFakeProvider,
  fauxAssistantMessage,
  fauxText,
  installFakeProvider,
  preloadFakeProvider,
} from "../testing/fake-provider.js";
import { runSession } from "../adapter.js";
import type { CompiledSpec, WireEvent } from "../types.js";

// Each test runs in its own temp cwd so any .agent-controller / .pi files
// the runtime materializes don't leak into the dev tree.
let originalCwd: string;
let testCwd: string;

beforeAll(async () => {
  // Warm the faux module cache so synchronous content helpers
  // (fauxText etc.) work in test setup.
  await preloadFakeProvider();
});

let originalAgentDir: string | undefined;
let originalAnthropicKey: string | undefined;

beforeEach(() => {
  originalCwd = process.cwd();
  testCwd = fs.mkdtempSync(path.join(os.tmpdir(), "ac-e2e-"));
  process.chdir(testCwd);
  process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER = "1";

  // Isolate Pi's agent directory under the temp workspace so the tests
  // never touch the user's real ~/.pi/agent (where auth.json / sessions
  // live). PI_CODING_AGENT_DIR is the canonical override; createAgentSession
  // reads it via getAgentDir(). Without this, hermetic CI runs that lack
  // a home directory (or that should not write to it) can fail or
  // contaminate global state.
  originalAgentDir = process.env.PI_CODING_AGENT_DIR;
  const localAgent = path.join(testCwd, ".pi-agent");
  fs.mkdirSync(localAgent, { recursive: true });
  process.env.PI_CODING_AGENT_DIR = localAgent;

  // Pi's auth check passes when ANTHROPIC_API_KEY is non-empty. The fake
  // provider never makes a network call, but Pi still validates env at
  // session construction. Use a sentinel that won't be mistaken for a real key.
  originalAnthropicKey = process.env.ANTHROPIC_API_KEY;
  if (!originalAnthropicKey) process.env.ANTHROPIC_API_KEY = "fake-test-only";
});

afterEach(() => {
  clearFakeProvider();
  delete process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER;
  // Restore PI_CODING_AGENT_DIR and ANTHROPIC_API_KEY.
  if (originalAgentDir === undefined) delete process.env.PI_CODING_AGENT_DIR;
  else process.env.PI_CODING_AGENT_DIR = originalAgentDir;
  if (originalAnthropicKey === undefined) delete process.env.ANTHROPIC_API_KEY;
  else process.env.ANTHROPIC_API_KEY = originalAnthropicKey;
  process.chdir(originalCwd);
  // Best-effort cleanup; ignore failures (something might still hold a
  // file handle on Windows-ish file systems).
  try {
    fs.rmSync(testCwd, { recursive: true, force: true });
  } catch {
    /* ignore */
  }
});

function minimalSpec(task: string): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "e2e-fake" },
    model: { provider: "anthropic", name: "claude-sonnet-4-20250514" },
    task,
    tools: [],
    extensions: [],
    skills: [],
    runtime: { type: "local" },
  };
}

describe("E2E: runSession against fake provider", () => {
  it("text-only turn produces the expected wire-event sequence", async () => {
    await installFakeProvider([
      fauxAssistantMessage(fauxText("Hello! The current time is 2026-05-29T10:00:00Z.")),
    ]);

    const events: WireEvent[] = [];
    await runSession(minimalSpec("Tell me the current UTC time."), (ev) => events.push(ev));

    // Coarse sequence assertions — exact ordering of model.request /
    // model.response and message events is provider-dependent, so we
    // assert on presence + final state rather than strict ordering.
    const started = events.find((e) => e.type === "session.started");
    const ended = events.find((e) => e.type === "session.ended");
    expect(started).toBeDefined();
    expect(ended).toBeDefined();
    expect((ended!.data as any).reason).toBe("completed");

    // No errors or warnings in a clean text turn.
    expect(events.filter((e) => e.type === "error")).toEqual([]);
    expect(events.filter((e) => e.type === "warning")).toEqual([]);

    // The assistant message wire event carries the scripted text.
    const assistantMessages = events
      .filter((e) => e.type === "message")
      .filter((e) => (e.data as any).role === "assistant");
    expect(assistantMessages.length).toBeGreaterThan(0);
    const text = (assistantMessages[assistantMessages.length - 1].data as any).text as string;
    expect(text).toContain("2026-05-29T10:00:00Z");
  });

  it("flags terminal session.ended reason=error when the script returns no match", async () => {
    // Empty responses list -> first turn has nothing to consume -> Pi
    // raises a streaming error. The adapter should propagate that as
    // reason=error.
    await installFakeProvider([]);

    const events: WireEvent[] = [];
    await runSession(minimalSpec("Anything at all."), (ev) => events.push(ev));

    const ended = events.find((e) => e.type === "session.ended");
    expect(ended).toBeDefined();
    expect((ended!.data as any).reason).toBe("error");
  });

  it("hallucinated XML in scripted text triggers the v0.1.10 detector (block mode)", async () => {
    // Same hallucination guardrail surface that adapter.test.ts asserts
    // against — but here it runs through the FULL pipeline: real Pi
    // dispatches the fake stream, message_end event carries the bad
    // text, detector fires, session ends with error.
    await installFakeProvider([
      fauxAssistantMessage(
        fauxText('Looking it up: <invoke name="bash"><parameter name="cmd">ls</parameter></invoke> Done.'),
      ),
    ]);

    const events: WireEvent[] = [];
    await runSession(minimalSpec("List the files."), (ev) => events.push(ev));

    const errors = events.filter((e) => e.type === "error");
    const ended = events.find((e) => e.type === "session.ended");
    expect(errors.length).toBeGreaterThan(0);
    expect((errors[0].data as any).kind).toBe("hallucinated_tool_call");
    expect((ended!.data as any).reason).toBe("error");
  });
});
