import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { translateEvent, createTranslatorState, type TranslatorState } from "./event-translator.js";
import type { GlobalEvent } from "@opencode-ai/sdk";

const SESSION = "oc-sess-001";
const WIRE_SESSION = "s_abc123";

function globalEvt(payload: object): GlobalEvent {
  return { directory: "/tmp", payload } as GlobalEvent;
}

/**
 * Send a message.updated event first to register the role, then send the
 * text part. This mirrors opencode's real event ordering and ensures the
 * role check in translateEvent passes (it now defaults to skipping rather
 * than buffering when role is unknown, per codex pass 8 of slice 2.4).
 */
function registerMessageRole(
  localState: TranslatorState,
  messageID = "m1",
  role: "user" | "assistant" = "assistant",
): void {
  localState.messageRoles.set(messageID, role);
}

function textPartEvt(
  text: string,
  opts: { messageID?: string; sessionID?: string; synthetic?: boolean; delta?: string } = {},
): GlobalEvent {
  // In single-chunk tests, delta === text (the whole chunk arrives at once).
  // In multi-chunk streaming, caller patches delta to the incremental piece.
  return globalEvt({
    type: "message.part.updated",
    properties: {
      part: {
        id: "tp1",
        sessionID: opts.sessionID ?? SESSION,
        messageID: opts.messageID ?? "m1",
        type: "text",
        text,
        synthetic: opts.synthetic ?? false,
      },
      // Default delta to text so single-chunk tests work without explicit patching.
      delta: opts.delta ?? text,
    },
  });
}

function toolPartEvt(
  status: "running" | "completed" | "error",
  callID = "call-abc",
): GlobalEvent {
  const stateMap: Record<string, object> = {
    running: { status: "running", input: { command: "ls" }, time: { start: 0 } },
    completed: { status: "completed", input: {}, output: "ok", title: "done", metadata: {}, time: { start: 0, end: 1 } },
    error: { status: "error", input: {}, output: "failed", time: { start: 0, end: 1 } },
  };
  return globalEvt({
    type: "message.part.updated",
    properties: {
      part: {
        id: "tp1",
        sessionID: SESSION,
        messageID: "m1",
        type: "tool",
        callID,
        tool: "bash",
        state: stateMap[status],
      },
    },
  });
}

function idleEvt(sessionID = SESSION): GlobalEvent {
  return globalEvt({ type: "session.idle", properties: { sessionID } });
}

let state: TranslatorState;
beforeEach(() => {
  state = createTranslatorState();
  // Default: register "m1" as an assistant message so text-accumulation
  // tests don't need to call registerMessageRole() explicitly. Tests that
  // want to test user-role or unknown-role paths call it themselves.
  registerMessageRole(state, "m1", "assistant");
});

function tr(gev: GlobalEvent, mode = "block" as const) {
  return translateEvent(gev, WIRE_SESSION, SESSION, state, mode);
}

describe("translateEvent — text accumulation", () => {
  it("accumulates text using the delta field, not part.text (no duplicate content)", () => {
    // Codex pass 1 of slice 2.4: opencode sends part.text as the full
    // running text, while properties.delta is the incremental addition.
    // Appending part.text every event produces "HelHelloHello world".
    // We must use delta ("Hel" + "lo" + " world" = "Hello world").
    const d1 = textPartEvt("Hel"); // full text "Hel", delta "Hel"
    const d2 = textPartEvt("Hello"); // full text "Hello", delta "lo"
    const d3 = textPartEvt("Hello world"); // full text "Hello world", delta " world"
    // Patch events to simulate opencode's delta behavior.
    (d1.payload as any).properties.delta = "Hel";
    (d2.payload as any).properties.delta = "lo";
    (d3.payload as any).properties.delta = " world";
    tr(d1); tr(d2); tr(d3);
    const idle = tr(idleEvt());
    const msg = idle.wireEvents.find((e) => e.type === "message");
    expect(msg).toBeDefined();
    expect((msg!.data as any).text).toBe("Hello world");
  });

  it("accumulates correctly when delta equals the full text (single-chunk case)", () => {
    // In the single-chunk case opencode sends delta=text (the whole thing
    // arrives at once). The test helper sets delta=text by default.
    tr(textPartEvt("Hello"));
    tr(textPartEvt(" world"));
    const idle = tr(idleEvt());
    expect(idle.sessionIdle).toBe(true);
    const msg = idle.wireEvents.find((e) => e.type === "message");
    expect(msg).toBeDefined();
    expect((msg!.data as any).text).toBe("Hello world");
  });

  it("emits message on session.idle and resets the buffer", () => {
    tr(textPartEvt("first turn"));
    tr(idleEvt());
    // Second turn
    tr(textPartEvt("second turn"));
    const idle2 = tr(idleEvt());
    const msgs = idle2.wireEvents.filter((e) => e.type === "message");
    expect(msgs).toHaveLength(1);
    expect((msgs[0].data as any).text).toBe("second turn");
  });

  it("skips synthetic TextParts", () => {
    tr(textPartEvt("real"));
    tr(textPartEvt(" (synth)", { synthetic: true }));
    const idle = tr(idleEvt());
    expect((idle.wireEvents.find((e) => e.type === "message")!.data as any).text).toBe("real");
  });

  it("ignores text parts for other sessions", () => {
    tr(textPartEvt("ignored", { sessionID: "other-session" }));
    const idle = tr(idleEvt());
    // No message emitted because the text was from another session
    expect(idle.wireEvents.filter((e) => e.type === "message")).toHaveLength(0);
    expect(idle.sessionIdle).toBe(true);
  });

  it("resets buffer on new messageID", () => {
    registerMessageRole(state, "m2", "assistant");
    tr(textPartEvt("msg1 text", { messageID: "m1" }));
    tr(textPartEvt("msg2 text", { messageID: "m2" }));
    const idle = tr(idleEvt());
    expect((idle.wireEvents.find((e) => e.type === "message")!.data as any).text).toBe("msg2 text");
  });

  it("session.idle for other sessions does not flush buffer", () => {
    tr(textPartEvt("pending"));
    const r = tr(globalEvt({ type: "session.idle", properties: { sessionID: "other" } }));
    expect(r.sessionIdle).toBe(false);
    expect(r.wireEvents.filter((e) => e.type === "message")).toHaveLength(0);
    // Buffer should still be "pending"
    expect(state.textBuffer).toBe("pending");
  });
});

describe("translateEvent — tool calls", () => {
  it("emits tool.call when a tool part transitions to running", () => {
    const r = tr(toolPartEvt("running"));
    expect(r.wireEvents).toHaveLength(1);
    expect(r.wireEvents[0].type).toBe("tool.call");
    const d = r.wireEvents[0].data as any;
    expect(d.toolName).toBe("bash");
    expect(d.callId).toBe("call-abc");
    expect(d.args).toEqual({ command: "ls" });
  });

  it("emits tool.result when a tool part completes", () => {
    const r = tr(toolPartEvt("completed"));
    expect(r.wireEvents[0].type).toBe("tool.result");
    expect((r.wireEvents[0].data as any).isError).toBe(false);
    expect((r.wireEvents[0].data as any).content).toBe("ok");
  });

  it("emits tool.result with isError=true and preserves the error message (codex pass 1 fix)", () => {
    // ToolStateError exposes the failure message in state.error, not state.output.
    // Codex pass 1 caught that we were looking for "output" and falling back
    // to a generic "tool error". The error field is now surfaced correctly.
    const r = tr(globalEvt({
      type: "message.part.updated",
      properties: {
        part: {
          id: "tp1", sessionID: SESSION, messageID: "m1", type: "tool",
          callID: "call-xyz", tool: "bash",
          state: {
            status: "error", input: {},
            error: "permission denied: /etc/shadow",
            metadata: {}, time: { start: 0, end: 1 },
          },
        },
      },
    }));
    expect((r.wireEvents[0].data as any).isError).toBe(true);
    expect((r.wireEvents[0].data as any).content).toBe("permission denied: /etc/shadow");
  });

  it("ignores tool parts for other sessions", () => {
    const r = tr(globalEvt({
      type: "message.part.updated",
      properties: {
        part: { id: "tp1", sessionID: "other", messageID: "m1", type: "tool", callID: "c", tool: "bash", state: { status: "running", input: {}, time: { start: 0 } } },
      },
    }));
    expect(r.wireEvents).toHaveLength(0);
  });
});

describe("translateEvent — session lifecycle", () => {
  it("sets sessionIdle=true and emits message on session.idle", () => {
    tr(textPartEvt("hello"));
    const r = tr(idleEvt());
    expect(r.sessionIdle).toBe(true);
    expect(r.wireEvents.some((e) => e.type === "message")).toBe(true);
  });

  it("does not set sessionIdle for other sessions", () => {
    const r = tr(globalEvt({ type: "session.idle", properties: { sessionID: "other" } }));
    expect(r.sessionIdle).toBe(false);
  });

  it("sets sessionError on session.error for target session", () => {
    const r = tr(globalEvt({
      type: "session.error",
      properties: { sessionID: SESSION, error: { name: "UnknownError", data: { message: "provider down" } } },
    }));
    expect(r.sessionError).toContain("provider down");
  });

  it("does not set sessionError for other sessions", () => {
    const r = tr(globalEvt({ type: "session.error", properties: { sessionID: "other", error: {} } }));
    expect(r.sessionError).toBeUndefined();
  });
});

describe("translateEvent — hallucination guardrails", () => {
  const xmlText = 'I will help. <invoke name="bash"><parameter>ls</parameter></invoke> Done.';

  it("block mode: emits wire error event on session.idle and sets sessionError", () => {
    tr(textPartEvt(xmlText)); // helper sets delta=text by default
    const r = tr(idleEvt(), "block");
    expect(r.wireEvents.some((e) => e.type === "error")).toBe(true);
    expect(r.sessionError).toBeDefined();
    expect(r.wireEvents.some((e) => e.type === "message")).toBe(false);
  });

  it("warn mode: emits warning + scrubbed message on session.idle, no sessionError", () => {
    tr(textPartEvt(xmlText), "warn");
    const r = tr(idleEvt(), "warn");
    expect(r.wireEvents.some((e) => e.type === "warning")).toBe(true);
    const msg = r.wireEvents.find((e) => e.type === "message");
    expect(msg).toBeDefined();
    expect((msg!.data as any).text).not.toMatch(/<invoke/);
    expect(r.sessionError).toBeUndefined();
  });

  it("clean text passes through without guardrail events", () => {
    tr(textPartEvt("clean response"));
    const r = tr(idleEvt(), "block");
    expect(r.wireEvents.filter((e) => e.type === "error")).toHaveLength(0);
    expect(r.wireEvents.filter((e) => e.type === "warning")).toHaveLength(0);
    expect((r.wireEvents.find((e) => e.type === "message")!.data as any).text).toBe("clean response");
  });
});

describe("translateEvent — unhandled event types", () => {
  it("silently ignores file.edited and other opencode-internal events", () => {
    const r = tr(globalEvt({ type: "file.edited", properties: { file: "/tmp/x.ts" } }));
    expect(r.wireEvents).toHaveLength(0);
    expect(r.sessionIdle).toBe(false);
    expect(r.sessionError).toBeUndefined();
  });
});
