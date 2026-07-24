import { describe, it, expect } from "vitest";
import { createTranslatorState, translateCodexLine } from "./event-translator.js";
import type { HallucinationMode } from "./event-translator.js";

describe("translateCodexLine", () => {
  it("thread.started -> session.started + captures threadId", () => {
    const st = createTranslatorState();
    const r = translateCodexLine('{"type":"thread.started","thread_id":"019f-uuid"}', "s1", st);
    expect(st.threadId).toBe("019f-uuid");
    expect(r.events[0].type).toBe("session.started");
  });
  it("agent_message item -> message", () => {
    const st = createTranslatorState();
    const r = translateCodexLine('{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"hi"}}', "s1", st);
    expect(r.events[0].type).toBe("message");
    expect((r.events[0].data as any).text).toBe("hi");
  });
  it("turn.completed -> session.ended(completed)", () => {
    const st = createTranslatorState();
    const r = translateCodexLine('{"type":"turn.completed","usage":{"output_tokens":5}}', "s1", st);
    expect(r.events[0].type).toBe("session.ended");
    expect((r.events[0].data as any).reason).toBe("completed");
  });
  it("blank / non-json / unknown -> no events, no throw", () => {
    const st = createTranslatorState();
    expect(translateCodexLine("", "s1", st).events).toEqual([]);
    expect(translateCodexLine("not json", "s1", st).events).toEqual([]);
    expect(translateCodexLine('{"type":"turn.started"}', "s1", st).events).toEqual([]);
  });
  it("ended-guard: after turn.completed, subsequent items emit no events", () => {
    const st = createTranslatorState();
    translateCodexLine('{"type":"turn.completed","usage":{"output_tokens":5}}', "s1", st);
    const r = translateCodexLine('{"type":"item.completed","item":{"type":"agent_message","text":"late"}}', "s1", st);
    expect(r.events).toEqual([]);
  });
  it("returned threadId: thread.started return value includes uuid", () => {
    const st = createTranslatorState();
    const r = translateCodexLine('{"type":"thread.started","thread_id":"019f-uuid"}', "s1", st);
    expect(r.threadId).toBe("019f-uuid");
  });
  it("other unknown item types yield no events", () => {
    const st = createTranslatorState();
    const r = translateCodexLine('{"type":"item.completed","item":{"type":"unknown_future_item"}}', "s1", st);
    expect(r.events).toEqual([]);
  });
});

// ── command_execution fixture tests (captured from real codex run) ─────────────

// Real fixture from: codex exec --json --skip-git-repo-check -s workspace-write
//   "Run the shell command: echo hello-from-codex"
// Captured 2026-07-08
const CMD_EXEC_FIXTURE = JSON.stringify({
  type: "item.completed",
  item: {
    id: "item_0",
    type: "command_execution",
    command: "/bin/zsh -lc 'echo hello-from-codex'",
    aggregated_output: "hello-from-codex\n",
    exit_code: 0,
    status: "completed",
  },
});

// Real fixture — failed/nonzero exit
const CMD_EXEC_NONZERO_FIXTURE = JSON.stringify({
  type: "item.completed",
  item: {
    id: "item_err",
    type: "command_execution",
    command: "/bin/zsh -lc 'exit 1'",
    aggregated_output: "",
    exit_code: 1,
    status: "completed",
  },
});

describe("command_execution item → tool.call + tool.result", () => {
  it("emits exactly two events: tool.call then tool.result", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(CMD_EXEC_FIXTURE, "s1", st);
    expect(r.events).toHaveLength(2);
    expect(r.events[0].type).toBe("tool.call");
    expect(r.events[1].type).toBe("tool.result");
  });

  it("tool.call has toolName='bash', callId=item.id, args.command=real command", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(CMD_EXEC_FIXTURE, "s1", st);
    const call = r.events[0].data as Record<string, unknown>;
    expect(call.toolName).toBe("bash");
    expect(call.callId).toBe("item_0");
    expect((call.args as Record<string, unknown>).command).toBe("/bin/zsh -lc 'echo hello-from-codex'");
  });

  it("tool.result has callId=item.id, isError=false, content=aggregated_output", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(CMD_EXEC_FIXTURE, "s1", st);
    const result = r.events[1].data as Record<string, unknown>;
    expect(result.callId).toBe("item_0");
    expect(result.isError).toBe(false);
    expect(result.content).toBe("hello-from-codex\n");
  });

  it("exit_code > 0 → isError=true in tool.result", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(CMD_EXEC_NONZERO_FIXTURE, "s1", st);
    const result = r.events[1].data as Record<string, unknown>;
    expect(result.isError).toBe(true);
    expect(result.callId).toBe("item_err");
  });

  it("sessionId is threaded through both events", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(CMD_EXEC_FIXTURE, "my-sess", st);
    expect(r.events[0].sessionId).toBe("my-sess");
    expect(r.events[1].sessionId).toBe("my-sess");
  });
});

// ── mcp_tool_call fixture tests (captured from real codex run) ────────────────

// Real fixture (status=completed) from:
//   codex exec --json --skip-git-repo-check -s workspace-write
//   "Use the filesystem MCP tool to list the directory /tmp"
// item_2: list_allowed_directories — successful call
// Captured 2026-07-08
const MCP_CALL_FIXTURE = JSON.stringify({
  type: "item.completed",
  item: {
    id: "item_2",
    type: "mcp_tool_call",
    server: "filesystem",
    tool: "list_allowed_directories",
    arguments: {},
    result: {
      content: [{ type: "text", text: "Allowed directories:\n/Users/zhenglincharlesc/charles_workplace" }],
      structured_content: { content: "Allowed directories:\n/Users/zhenglincharlesc/charles_workplace" },
    },
    error: null,
    status: "completed",
  },
});

// Real fixture (status=failed) — access denied error
// item_1 from same run: list_directory /tmp → access denied
const MCP_CALL_FAILED_FIXTURE = JSON.stringify({
  type: "item.completed",
  item: {
    id: "item_1",
    type: "mcp_tool_call",
    server: "filesystem",
    tool: "list_directory",
    arguments: { path: "/tmp" },
    result: {
      content: [{ type: "text", text: "Access denied - path outside allowed directories: /tmp not in /Users/zhenglincharlesc/charles_workplace" }],
      structured_content: null,
    },
    error: null,
    status: "failed",
  },
});

describe("mcp_tool_call item → tool.call + tool.result", () => {
  it("emits exactly two events: tool.call then tool.result", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FIXTURE, "s1", st);
    expect(r.events).toHaveLength(2);
    expect(r.events[0].type).toBe("tool.call");
    expect(r.events[1].type).toBe("tool.result");
  });

  it("tool.call has toolName=mcp_<server>_<tool>, callId=item.id, args=arguments", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FIXTURE, "s1", st);
    const call = r.events[0].data as Record<string, unknown>;
    expect(call.toolName).toBe("mcp_filesystem_list_allowed_directories");
    expect(call.callId).toBe("item_2");
    expect(call.args).toEqual({});
  });

  it("tool.result has callId=item.id, isError=false, content=text from result.content[0]", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FIXTURE, "s1", st);
    const result = r.events[1].data as Record<string, unknown>;
    expect(result.callId).toBe("item_2");
    expect(result.isError).toBe(false);
    expect(result.content).toBe("Allowed directories:\n/Users/zhenglincharlesc/charles_workplace");
  });

  it("status=failed → isError=true in tool.result", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FAILED_FIXTURE, "s1", st);
    const result = r.events[1].data as Record<string, unknown>;
    expect(result.isError).toBe(true);
    expect(result.callId).toBe("item_1");
  });

  it("tool.call args reflect item.arguments for parameterized calls", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FAILED_FIXTURE, "s1", st);
    const call = r.events[0].data as Record<string, unknown>;
    expect(call.toolName).toBe("mcp_filesystem_list_directory");
    expect((call.args as Record<string, unknown>).path).toBe("/tmp");
  });

  it("sessionId is threaded through both events", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(MCP_CALL_FIXTURE, "my-sess", st);
    expect(r.events[0].sessionId).toBe("my-sess");
    expect(r.events[1].sessionId).toBe("my-sess");
  });
});

// ── Hallucination guardrail tests ─────────────────────────────────────────────

const FAKE_XML_TEXT =
  "Sure! <function_calls><invoke name=\"bash\"><parameter name=\"command\">ls</parameter></invoke></function_calls>";

const CLEAN_TEXT = "Here is the answer to your question.";

function agentMessageLine(text: string): string {
  return JSON.stringify({
    type: "item.completed",
    item: { id: "i1", type: "agent_message", text },
  });
}

describe("hallucination guardrail on agent_message", () => {
  it("block mode: fabricated XML -> error event, no message event", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(agentMessageLine(FAKE_XML_TEXT), "s1", st, "block");
    const types = r.events.map((e) => e.type);
    expect(types).toContain("error");
    expect(types).not.toContain("message");
    const errEv = r.events.find((e) => e.type === "error");
    expect((errEv!.data as Record<string, unknown>).kind).toBe("hallucinated_tool_call");
  });

  it("block mode: fabricated XML -> session.ended with reason error", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(agentMessageLine(FAKE_XML_TEXT), "s1", st, "block");
    const ended = r.events.find((e) => e.type === "session.ended");
    expect(ended).toBeDefined();
    expect((ended!.data as Record<string, unknown>).reason).toBe("error");
  });

  it("warn mode: fabricated XML -> scrubbed message + warning event, no error", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(agentMessageLine(FAKE_XML_TEXT), "s1", st, "warn");
    const types = r.events.map((e) => e.type);
    expect(types).toContain("warning");
    expect(types).toContain("message");
    expect(types).not.toContain("error");
    const msgEv = r.events.find((e) => e.type === "message");
    const text = (msgEv!.data as Record<string, unknown>).text as string;
    expect(text).not.toMatch(/<function_calls/i);
    expect(text).not.toMatch(/<invoke/i);
  });

  it("warn mode: warning event has kind=hallucinated_tool_call", () => {
    const st = createTranslatorState();
    const r = translateCodexLine(agentMessageLine(FAKE_XML_TEXT), "s1", st, "warn");
    const warnEv = r.events.find((e) => e.type === "warning");
    expect((warnEv!.data as Record<string, unknown>).kind).toBe("hallucinated_tool_call");
  });

  it("clean message: no warning or error in any mode", () => {
    for (const mode of ["block", "warn", "correct"] as HallucinationMode[]) {
      const st = createTranslatorState();
      const r = translateCodexLine(agentMessageLine(CLEAN_TEXT), "s1", st, mode);
      const types = r.events.map((e) => e.type);
      expect(types).not.toContain("warning");
      expect(types).not.toContain("error");
      expect(types).toContain("message");
    }
  });

  it("default mode (no mode arg): fabricated XML blocks (default=block)", () => {
    const st = createTranslatorState();
    // No mode argument → should default to "block"
    const r = translateCodexLine(agentMessageLine(FAKE_XML_TEXT), "s1", st);
    const types = r.events.map((e) => e.type);
    expect(types).toContain("error");
    expect(types).not.toContain("message");
  });
});
