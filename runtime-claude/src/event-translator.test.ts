import { describe, it, expect } from "vitest";
import { createTranslatorState, translateSdkMessage } from "./event-translator.js";

const SID = "s_test";

describe("translateSdkMessage", () => {
  it("maps a system init message to session.started and captures the SDK session id", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      { type: "system", subtype: "init", session_id: "sdk-1", model: "claude-opus-4-6" },
      SID,
      st,
      "block",
    );
    expect(r.events.map((e) => e.type)).toEqual(["session.started"]);
    expect(r.sdkSessionId).toBe("sdk-1");
    expect(st.sdkSessionId).toBe("sdk-1");
  });

  it("maps assistant text to a message event", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      { type: "assistant", session_id: "sdk-1", message: { content: [{ type: "text", text: "hi" }] } },
      SID,
      st,
      "block",
    );
    expect(r.events).toHaveLength(1);
    expect(r.events[0].type).toBe("message");
    expect(r.events[0].data).toMatchObject({ role: "assistant", text: "hi" });
  });

  it("uses `text` (not `content`) as the message payload key, matching the other adapters", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      { type: "assistant", session_id: "sdk-1", message: { content: [{ type: "text", text: "hi" }] } },
      SID,
      st,
      "block",
    );
    expect(r.events[0].data).toEqual({ role: "assistant", text: "hi" });
  });

  it("emits tool.call for each tool_use block", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      {
        type: "assistant",
        session_id: "sdk-1",
        message: {
          content: [
            { type: "tool_use", id: "tu_1", name: "Bash", input: { command: "ls" } },
            { type: "tool_use", id: "tu_2", name: "Read", input: { file_path: "/a" } },
          ],
        },
      },
      SID,
      st,
      "block",
    );
    expect(r.events.map((e) => e.type)).toEqual(["tool.call", "tool.call"]);
    expect(r.events[0].data).toMatchObject({ toolName: "Bash", callId: "tu_1" });
  });

  it("emits tool.result for user tool_result blocks", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      {
        type: "user",
        session_id: "sdk-1",
        message: {
          content: [
            { type: "tool_result", tool_use_id: "tu_1", content: "a.txt", is_error: false },
          ],
        },
      },
      SID,
      st,
      "block",
    );
    expect(r.events).toHaveLength(1);
    expect(r.events[0].type).toBe("tool.result");
    expect(r.events[0].data).toMatchObject({ callId: "tu_1", isError: false, content: "a.txt" });
  });

  it("maps a success result to session.ended completed", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      { type: "result", subtype: "success", is_error: false, result: "done", session_id: "sdk-1" },
      SID,
      st,
      "block",
    );
    expect(r.events.map((e) => e.type)).toEqual(["session.ended"]);
    expect(r.events[0].data).toMatchObject({ reason: "completed" });
    expect(st.ended).toBe(true);
  });

  it("maps an error result to error + session.ended error", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      {
        type: "result",
        subtype: "error_during_execution",
        is_error: true,
        result: "boom",
        session_id: "sdk-1",
      },
      SID,
      st,
      "block",
    );
    expect(r.events.map((e) => e.type)).toEqual(["error", "session.ended"]);
    expect(r.events[1].data).toMatchObject({ reason: "error" });
    expect(r.fatal).toBe(true);
  });

  it("blocks hallucinated tool-call XML in block mode", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      {
        type: "assistant",
        session_id: "sdk-1",
        message: {
          content: [{ type: "text", text: "<function_calls><invoke name=\"Bash\"></invoke></function_calls>" }],
        },
      },
      SID,
      st,
      "block",
    );
    expect(r.events.map((e) => e.type)).toEqual(["error", "session.ended"]);
    expect(r.fatal).toBe(true);
  });

  it("scrubs and warns on hallucinated XML in warn mode", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage(
      {
        type: "assistant",
        session_id: "sdk-1",
        message: {
          content: [{ type: "text", text: "ok <function_calls><invoke name=\"Bash\"></invoke></function_calls>" }],
        },
      },
      SID,
      st,
      "warn",
    );
    const types = r.events.map((e) => e.type);
    expect(types).toContain("warning");
    expect(types).toContain("message");
    expect(r.fatal).toBeFalsy();
  });

  it("ignores unknown message variants", () => {
    const st = createTranslatorState();
    const r = translateSdkMessage({ type: "status", session_id: "x" }, SID, st, "block");
    expect(r.events).toEqual([]);
  });

  it("emits nothing once ended", () => {
    const st = createTranslatorState();
    st.ended = true;
    const r = translateSdkMessage(
      { type: "assistant", session_id: "x", message: { content: [{ type: "text", text: "late" }] } },
      SID,
      st,
      "block",
    );
    expect(r.events).toEqual([]);
  });
});
