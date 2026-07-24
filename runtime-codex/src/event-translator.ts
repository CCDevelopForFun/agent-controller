import type { WireEvent } from "./types.js";
import { stamp } from "./wire.js";
import { detectHallucinatedToolCalls, stripHallucinationXml } from "./honesty.js";

export type HallucinationMode = "block" | "warn" | "correct";

export interface TranslatorState {
  threadId?: string;
  ended: boolean;
}

export function createTranslatorState(): TranslatorState {
  return { ended: false };
}

export function translateCodexLine(
  line: string,
  sessionId: string,
  state: TranslatorState,
  hallucinationMode: HallucinationMode = "block",
): { events: WireEvent[]; threadId?: string } {
  if (state.ended) return { events: [] };
  if (!line.trim()) return { events: [] };

  let obj: Record<string, unknown>;
  try {
    obj = JSON.parse(line) as Record<string, unknown>;
  } catch {
    return { events: [] };
  }

  const type = obj.type as string | undefined;

  switch (type) {
    case "thread.started": {
      const threadId = obj.thread_id as string | undefined;
      if (threadId) state.threadId = threadId;
      const ev = stamp(sessionId, "session.started", {
        threadId,
      });
      return { events: [ev], threadId };
    }

    case "item.completed": {
      const item = obj.item as Record<string, unknown> | undefined;
      if (!item) return { events: [] };

      // command_execution: shell command run by codex → tool.call + tool.result
      if (item.type === "command_execution") {
        const id = item.id as string;
        const command = item.command as string;
        const output = (item.aggregated_output as string) ?? "";
        const exitCode = item.exit_code as number | null;
        const isError = exitCode !== 0;
        return {
          events: [
            stamp(sessionId, "tool.call", {
              toolName: "bash",
              callId: id,
              args: { command },
            }),
            stamp(sessionId, "tool.result", {
              callId: id,
              isError,
              content: output,
            }),
          ],
        };
      }

      // mcp_tool_call: MCP server invocation → tool.call + tool.result
      if (item.type === "mcp_tool_call") {
        const id = item.id as string;
        const server = item.server as string;
        const tool = item.tool as string;
        const args = item.arguments as Record<string, unknown>;
        const status = item.status as string;
        const isError = status === "failed" || status === "error";
        // Extract text content from result.content[0].text (real codex wire shape)
        const result = item.result as Record<string, unknown> | null | undefined;
        const contentArr = result?.content as Array<Record<string, unknown>> | undefined;
        const content =
          (contentArr && contentArr.length > 0 ? (contentArr[0].text as string) : undefined) ??
          String(item.error ?? "");
        return {
          events: [
            stamp(sessionId, "tool.call", {
              toolName: `mcp_${server}_${tool}`,
              callId: id,
              args,
            }),
            stamp(sessionId, "tool.result", {
              callId: id,
              isError,
              content,
            }),
          ],
        };
      }

      if (item.type !== "agent_message") return { events: [] };
      const text = item.text as string;

      // Run hallucination guardrail on agent_message text.
      const findings = detectHallucinatedToolCalls(text);
      if (findings.length > 0) {
        if (hallucinationMode === "block") {
          // Emit error + session.ended(error); no message event.
          const errMsg =
            `Assistant message contains fabricated tool-call XML: ${findings.join(", ")}. ` +
            `The model is hallucinating tool invocations instead of using the runtime's tool channel.`;
          state.ended = true;
          return {
            events: [
              stamp(sessionId, "error", {
                kind: "hallucinated_tool_call",
                mode: hallucinationMode,
                message: errMsg,
                patterns: findings,
              }),
              stamp(sessionId, "session.ended", {
                reason: "error" as const,
                message: errMsg,
              }),
            ],
          };
        }
        // warn and correct: scrub XML, emit warning + scrubbed message.
        // correct mode: same as warn for v1 (a re-prompt in codex would require
        // a new `codex exec resume` invocation which is out of scope for this
        // single-shot adapter; document as a v1 limitation).
        const { text: scrubbed } = stripHallucinationXml(text);
        return {
          events: [
            stamp(sessionId, "warning", {
              kind: "hallucinated_tool_call",
              mode: hallucinationMode,
              message: `Fabricated tool-call XML stripped from message: ${findings.join(", ")}.`,
              patterns: findings,
            }),
            stamp(sessionId, "message", {
              text: scrubbed,
              role: "assistant" as const,
            }),
          ],
        };
      }

      // Clean message — emit normally.
      const ev = stamp(sessionId, "message", {
        text,
        role: "assistant" as const,
      });
      return { events: [ev] };
    }

    case "turn.completed": {
      const usage = obj.usage;
      const ev = stamp(sessionId, "session.ended", {
        reason: "completed" as const,
        usage,
      });
      state.ended = true;
      return { events: [ev] };
    }

    // turn.started, command_execution, mcp_tool_call, and anything else
    default:
      return { events: [] };
  }
}
