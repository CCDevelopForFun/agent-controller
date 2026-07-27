/**
 * SDKMessage -> wire-event translation for the Claude Agent SDK adapter.
 *
 * The SDK's SDKMessage union has 37 variants and grows with minor releases,
 * so this translator matches only the variants the wire protocol needs and
 * ignores the rest. An unrecognized variant is never an error.
 *
 * model.request / model.response are deliberately not emitted: the SDK does
 * not surface per-model-request events, and synthesizing them would lose
 * fidelity. Same gap as the opencode and codex adapters.
 */

import type { HallucinationMode, WireEvent } from "./types.js";
import { stamp } from "./wire.js";
import { detectHallucinatedToolCalls, stripHallucinationXml } from "./honesty.js";

export interface TranslatorState {
  /** The SDK's own session id, captured from the init message for --resume. */
  sdkSessionId?: string;
  ended: boolean;
}

export function createTranslatorState(): TranslatorState {
  return { ended: false };
}

interface ContentBlock {
  type?: string;
  text?: string;
  id?: string;
  name?: string;
  input?: unknown;
  tool_use_id?: string;
  content?: unknown;
  is_error?: boolean;
}

/** Normalize a tool_result content payload to a string. */
function resultToString(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) => (b && typeof b === "object" && "text" in b ? String((b as ContentBlock).text ?? "") : ""))
      .join("");
  }
  return content == null ? "" : JSON.stringify(content);
}

export function translateSdkMessage(
  msg: unknown,
  sessionId: string,
  state: TranslatorState,
  mode: HallucinationMode = "block",
): { events: WireEvent[]; sdkSessionId?: string; fatal?: boolean } {
  if (state.ended) return { events: [] };
  if (!msg || typeof msg !== "object") return { events: [] };

  const m = msg as Record<string, unknown>;

  switch (m.type) {
    case "system": {
      const sdkSessionId = m.session_id as string | undefined;
      if (sdkSessionId) state.sdkSessionId = sdkSessionId;
      return {
        events: [stamp(sessionId, "session.started", { sdkSessionId, model: m.model })],
        sdkSessionId,
      };
    }

    case "assistant": {
      const message = m.message as { content?: ContentBlock[] } | undefined;
      const blocks = message?.content ?? [];
      const events: WireEvent[] = [];

      const text = blocks
        .filter((b) => b.type === "text")
        .map((b) => b.text ?? "")
        .join("");

      if (text) {
        const hits = detectHallucinatedToolCalls(text);
        if (hits.length > 0) {
          if (mode === "block") {
            state.ended = true;
            return {
              events: [
                stamp(sessionId, "error", {
                  kind: "hallucinated_tool_call",
                  message:
                    `assistant fabricated tool-call XML (${hits.join(", ")}); ` +
                    `guardrails.hallucinationDetector is "block"`,
                }),
                stamp(sessionId, "session.ended", { reason: "error" }),
              ],
              fatal: true,
            };
          }
          const { text: scrubbed } = stripHallucinationXml(text);
          events.push(
            stamp(sessionId, "warning", {
              kind: "hallucinated_tool_call",
              detected: hits,
              message: "fabricated tool-call XML scrubbed from assistant message",
            }),
          );
          events.push(stamp(sessionId, "message", { role: "assistant", text: scrubbed }));
        } else {
          events.push(stamp(sessionId, "message", { role: "assistant", text }));
        }
      }

      for (const b of blocks) {
        if (b.type === "tool_use") {
          events.push(
            stamp(sessionId, "tool.call", {
              toolName: b.name,
              callId: b.id,
              args: b.input,
            }),
          );
        }
      }

      return { events };
    }

    case "user": {
      const message = m.message as { content?: ContentBlock[] } | undefined;
      const blocks = message?.content ?? [];
      const events: WireEvent[] = [];
      for (const b of blocks) {
        if (b.type === "tool_result") {
          events.push(
            stamp(sessionId, "tool.result", {
              callId: b.tool_use_id,
              isError: Boolean(b.is_error),
              content: resultToString(b.content),
            }),
          );
        }
      }
      return { events };
    }

    case "result": {
      state.ended = true;
      const isError = Boolean(m.is_error) || m.subtype !== "success";
      if (isError) {
        return {
          events: [
            stamp(sessionId, "error", {
              kind: String(m.subtype ?? "error"),
              message: String(m.result ?? "session failed"),
            }),
            stamp(sessionId, "session.ended", { reason: "error" }),
          ],
          fatal: true,
        };
      }
      return {
        events: [
          stamp(sessionId, "session.ended", {
            reason: "completed",
            result: m.result,
            numTurns: m.num_turns,
          }),
        ],
      };
    }

    default:
      return { events: [] };
  }
}
