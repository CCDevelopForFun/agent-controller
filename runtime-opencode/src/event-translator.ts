/**
 * Translate opencode's SSE event stream into our wire-protocol events.
 *
 * Phase 2 slice 2.4 — this is the core mapping layer between the opencode
 * SDK's event model and the NDJSON wire protocol that `agentctl run` reads.
 *
 * Wire protocol reference: runtime/src/types.ts (WireEventType union) and
 * cli/internal/wire/events.go. Changes here must stay in sync with both.
 *
 * opencode's data model: text content arrives via `message.part.updated`
 * events (TextPart streaming), not from the Message object itself
 * (AssistantMessage.summary is a boolean flag, not text). We accumulate
 * text in a per-session buffer and emit the full assembled text as a wire
 * `message` event when `session.idle` signals the turn is complete. This
 * mirrors the Pi adapter's behaviour (emit whole-message events, not
 * per-token deltas).
 *
 * Events translated:
 *   message.part.updated (TextPart) → accumulated in textBuffer; flushed
 *                                      as wire `message` on session.idle
 *   message.part.updated (ToolPart, running)   → wire `tool.call`
 *   message.part.updated (ToolPart, completed) → wire `tool.result`
 *   message.part.updated (ToolPart, error)     → wire `tool.result` (isError=true)
 *   message.updated (user, with summary)       → wire `message` (role=user)
 *   session.idle                               → flushes text buffer + signals
 *                                                "turn complete" to caller
 *   session.error                              → signals terminal failure
 *
 * Events intentionally NOT translated:
 *   file.edited, session.compacted, todo.updated, session.diff
 *   pty.*, lsp.*, tui.*, etc.    — implementation noise, not ADL surface
 */
import { stamp } from "./wire.js";
import type { WireEvent } from "./types.js";
import { detectHallucinatedToolCalls, stripHallucinationXml } from "./honesty.js";
import type { GlobalEvent, ToolPart } from "@opencode-ai/sdk";

export type HallucinationMode = "block" | "warn" | "correct";

/**
 * Flush the current text buffer to wireEvents, running hallucination
 * detection first. Used both at session.idle (normal path) and when a
 * new messageID arrives mid-turn (multi-message turns in tool-using
 * sessions). Modifies state.textBuffer. Codex pass 30 of slice 2.4
 * caught that intermediate flushes bypassed guardrail checks.
 */
function flushBuffer(
  text: string,
  sessionId: string,
  hallucinationMode: HallucinationMode,
  wireEvents: WireEvent[],
): string | undefined /* sessionError */ {
  if (!text) return undefined;
  const findings = detectHallucinatedToolCalls(text);
  if (findings.length > 0) {
    if (hallucinationMode === "block") {
      const errMsg =
        `Assistant message contains fabricated tool-call XML: ${findings.join(", ")}. ` +
        `The model is hallucinating tool invocations instead of using the runtime's tool channel.`;
      wireEvents.push(stamp(sessionId, "error", {
        kind: "hallucinated_tool_call",
        mode: hallucinationMode,
        message: errMsg,
        patterns: findings,
      }));
      return `Assistant message contained fabricated tool-call XML (${findings.join(", ")})`;
    }
    const { text: scrubbed } = stripHallucinationXml(text);
    wireEvents.push(stamp(sessionId, "warning", {
      kind: "hallucinated_tool_call",
      mode: hallucinationMode,
      message: `Fabricated tool-call XML stripped from message: ${findings.join(", ")}.`,
      patterns: findings,
    }));
    wireEvents.push(stamp(sessionId, "message", { text: scrubbed, role: "assistant" }));
  } else {
    wireEvents.push(stamp(sessionId, "message", { text, role: "assistant" }));
  }
  return undefined;
}

/**
 * Per-session state shared across translateEvent calls. The caller owns
 * one instance per session and passes it on every call. Keeps the
 * function pure from the caller's perspective while allowing stateful
 * text accumulation.
 */
export interface TranslatorState {
  /**
   * Tracks the last seen `part.text` snapshot for each part ID. When
   * `properties.delta` is absent (optional in the SDK), we derive the
   * incremental text by diffing against the previous snapshot.
   * Codex pass 16 of slice 2.4 caught that `delta ?? ""` dropped all
   * output for full-part update events that opencode sends without delta.
   */
  partTextSnapshots: Map<string, string>;
  /** Accumulated assistant text parts for the current turn. */
  textBuffer: string;
  /** Message ID we're currently accumulating text for. */
  currentMessageId: string | undefined;
  /**
   * Role of the current message being accumulated. TextParts arrive via
   * message.part.updated and carry no role field; we learn the role from
   * message.updated events and track it here so we only buffer assistant
   * text. Codex pass 5 of slice 2.4 caught that user prompt/correction
   * text was being buffered as assistant output.
   */
  currentMessageRole: "user" | "assistant" | undefined;
  /**
   * Map from messageID to role, populated from message.updated events.
   * Used to look up role when a text part arrives before its message.updated.
   */
  messageRoles: Map<string, "user" | "assistant">;
  /**
   * Pre-role text buffer: keyed by messageID, accumulates deltas that
   * arrived before the corresponding message.updated role event. When the
   * role event arrives, we flush pre-role deltas into textBuffer (if
   * assistant) or discard them (if user). Codex pass 10 of slice 2.4
   * caught that "skip unknown-role text" permanently dropped early deltas
   * that the stream would not replay.
   */
  preRoleBuffer: Map<string, string>;
  /**
   * Set of tool callIDs for which we have already emitted a `tool.call`
   * wire event. Guards against emitting duplicate tool.call events when
   * opencode sends multiple `message.part.updated` events while the same
   * tool part stays in "running" status (streamed input JSON, progress
   * metadata, etc.). Codex pass 13 of slice 2.4 caught this.
   */
  emittedToolCalls: Set<string>;
}

export function createTranslatorState(): TranslatorState {
  return {
    partTextSnapshots: new Map(),
    textBuffer: "",
    currentMessageId: undefined,
    currentMessageRole: undefined,
    messageRoles: new Map(),
    preRoleBuffer: new Map(),
    emittedToolCalls: new Set(),
  };
}

/**
 * Result of translating a single opencode GlobalEvent into zero or more
 * wire events. `sessionIdle` signals the caller that the model has
 * finished its turn and no more events are coming for this prompt.
 * `sessionError` signals a terminal failure the caller should surface.
 */
export interface TranslationResult {
  wireEvents: WireEvent[];
  sessionIdle: boolean;
  sessionError: string | undefined;
}

/**
 * Translate one opencode GlobalEvent to wire events.
 *
 * @param gev           The raw GlobalEvent from the SSE stream.
 * @param sessionId     The stable session ID the caller selected.
 * @param targetSession The opencode session ID (from session.create) — we
 *                      ignore events for other sessions that might arrive
 *                      on the same SSE stream.
 * @param state         Mutable translator state (text accumulation).
 * @param hallucinationMode  Block/warn/correct guardrail, from spec.guardrails.
 */
export function translateEvent(
  gev: GlobalEvent,
  sessionId: string,
  targetSession: string,
  state: TranslatorState,
  hallucinationMode: HallucinationMode = "block",
): TranslationResult {
  const ev = gev.payload;
  const wireEvents: WireEvent[] = [];
  let sessionIdle = false;
  let sessionError: string | undefined;

  switch (ev.type) {
    case "message.updated": {
      const msg = ev.properties.info;
      if (msg.sessionID !== targetSession) break;

      const role = msg.role as "user" | "assistant";
      // Track role so text-accumulation in message.part.updated below
      // can decide whether to buffer (assistant only). Codex pass 5 caught
      // that text parts carry no role and user prompt text was being buffered.
      state.messageRoles.set(msg.id, role);

      // Flush any pre-role buffer that accumulated before this event.
      // Codex pass 10 caught that "skip unknown-role text" permanently
      // dropped early deltas — we now stash them and flush here.
      const preRoleText = state.preRoleBuffer.get(msg.id);
      if (preRoleText) {
        state.preRoleBuffer.delete(msg.id);
        if (role === "assistant") {
          // Retroactively add the pre-role text to the main buffer. If we
          // were already buffering a different message, flush it first with
          // guardrail checks — codex pass 30 caught the silent drop.
          if (state.currentMessageId !== msg.id) {
            if (state.textBuffer && state.currentMessageRole === "assistant") {
              const err = flushBuffer(state.textBuffer, sessionId, hallucinationMode, wireEvents);
              if (err) sessionError ??= err;
            }
            state.textBuffer = "";
            state.currentMessageId = msg.id;
            state.currentMessageRole = "assistant";
          }
          state.textBuffer += preRoleText;
        } else if (role === "user") {
          // Emit user pre-role text as a wire message event immediately.
          // Codex pass 26 of slice 2.4 caught that discarding it broke
          // trace parity — the known-role path emits user messages, so
          // the out-of-order case must too.
          wireEvents.push(stamp(sessionId, "message", { text: preRoleText, role: "user" }));
        }
      }

      // No wire event emitted from message.updated — text arrives via
      // message.part.updated deltas and is flushed on session.idle.
      break;
    }

    case "message.part.updated": {
      const part = ev.properties.part;
      if (!part || (part as { sessionID?: string }).sessionID !== targetSession) break;

      // Tool parts MUST be checked first. ToolPart updates can arrive with a
      // non-empty `properties.delta` (opencode streams the input JSON as the
      // model generates it). If we checked `isTextOrHasDelta` first, a
      // ToolPart with a delta would be erroneously buffered as assistant text
      // and the tool.call / tool.result wire events would be skipped.
      // Codex pass 7 of slice 2.4 caught this ordering bug.
      if (isToolPart(part)) {
        const toolPart = part as ToolPart;
        const toolState = toolPart.state;
        if (toolState.status === "running") {
          // Emit tool.call exactly once per callID. opencode can send
          // multiple "running" updates as input is streamed or metadata
          // changes. Downstream wire consumers expect a single tool.call
          // followed by tool.result — guard with the seen-IDs set.
          // Codex pass 13 of slice 2.4 caught the duplicate emission.
          if (!state.emittedToolCalls.has(toolPart.callID)) {
            state.emittedToolCalls.add(toolPart.callID);
            wireEvents.push(stamp(sessionId, "tool.call", {
              toolName: toolPart.tool,
              callId: toolPart.callID,
              args: toolState.input,
            }));
          }
        } else if (toolState.status === "completed") {
          wireEvents.push(stamp(sessionId, "tool.result", {
            callId: toolPart.callID,
            isError: false,
            content: toolState.output,
          }));
        } else if (toolState.status === "error") {
          const errState = toolState as { error?: string };
          const errOut = errState.error ?? "tool error (no detail)";
          wireEvents.push(stamp(sessionId, "tool.result", {
            callId: toolPart.callID,
            isError: true,
            content: errOut,
          }));
        }
        break;
      }

      // Text accumulation: only accumulate delta from TextParts. Reasoning
      // parts, thinking blocks, and other non-text part types also carry
      // `properties.delta`, but they must NOT be emitted as user-visible
      // message text. Codex pass 9 of slice 2.4 caught that the earlier
      // "any non-tool part with a delta" condition leaked reasoning into the
      // wire `message` event. Restrict strictly to part.type === "text".
      if (isTextPart(part)) {
        const msgID = part.messageID;
        if (!msgID) break;

        // Resolve the role for this text part. message.updated events are
        // supposed to arrive before message.part.updated for the same
        // message, but race conditions can occur.
        const role =
          state.messageRoles.get(msgID) ??
          (state.currentMessageId === msgID ? state.currentMessageRole : undefined);

        // Derive the incremental text first (before role checks) so all
        // branches can use it. Prefer `properties.delta` when present.
        // When delta is absent (SDK type makes it optional), derive from
        // the diff against the previous part.text snapshot. Codex pass
        // 16 of slice 2.4 caught that `delta ?? ""` dropped all output
        // for full-part update events without a delta field.
        const evDeltaOptional = (ev.properties as { delta?: string }).delta;
        const currText = part.text ?? "";
        // Always update snapshot so subsequent no-delta events compute
        // diffs from the correct baseline. Codex pass 17 caught staleness.
        const prevText = state.partTextSnapshots.get(part.id) ?? "";
        state.partTextSnapshots.set(part.id, currText);
        let delta: string;
        if (evDeltaOptional !== undefined) {
          delta = evDeltaOptional;
        } else {
          delta = currText.startsWith(prevText) ? currText.slice(prevText.length) : currText;
        }

        // Emit user-role text parts immediately as wire message events
        // so the audit trace matches the Pi adapter. User text is NOT run
        // through the hallucination detector. Codex pass 18 caught that
        // we were silently dropping user text.
        if (role === "user") {
          if (!part.synthetic && delta) {
            wireEvents.push(stamp(sessionId, "message", { text: delta, role: "user" }));
          }
          break;
        }

        // Only accumulate text when we KNOW the message is assistant.
        // Accumulating user prompt/correction text would trip the hallucination
        // detector (CORRECTION_PROMPT itself contains <invoke>-like XML).
        // Codex passes 5 + 8 of slice 2.4 refined this rule.
        if (role !== undefined && role !== "assistant") break;

        // (delta is already computed above)

        if (role === undefined) {
          // Role not yet known — stash in pre-role buffer (if non-synthetic).
          if (!part.synthetic && delta) {
            const prev = state.preRoleBuffer.get(msgID) ?? "";
            state.preRoleBuffer.set(msgID, prev + delta);
          }
          break;
        }
        const synthetic = part.synthetic;
        if (state.currentMessageId !== msgID) {
          // Flush the previous assistant buffer before switching — include
          // guardrail checks so intermediate messages are also scanned.
          // Codex pass 29 introduced the flush; pass 30 caught it skipped
          // hallucinaton detection.
          if (state.textBuffer && state.currentMessageRole === "assistant") {
            const err = flushBuffer(state.textBuffer, sessionId, hallucinationMode, wireEvents);
            if (err) sessionError ??= err;
          }
          state.textBuffer = "";
          state.currentMessageId = msgID;
          state.currentMessageRole = role;
        }
        if (!synthetic && delta) {
          state.textBuffer += delta;
        }
        break;
      }
      // Non-text, non-tool part (reasoning, file, step marker, etc.) — ignore.
      break;
    }

    case "session.idle": {
      if (ev.properties.sessionID !== targetSession) break;

      // Flush the final assistant text buffer via flushBuffer (shared helper
      // that also runs hallucination guardrails). This is the same path
      // used for intermediate messageID flushes in multi-message turns.
      const rawText = state.textBuffer;
      state.textBuffer = "";
      state.currentMessageId = undefined;

      if (rawText) {
        const err = flushBuffer(rawText, sessionId, hallucinationMode, wireEvents);
        if (err) sessionError ??= err;
      }

      sessionIdle = true;
      break;
    }

    case "session.error": {
      // Accept this error when either:
      //   (a) the sessionID matches our target session, OR
      //   (b) sessionID is absent — allowed by the SDK type; since we spawn
      //       a dedicated one-session server, unscoped errors are ours.
      // Codex pass 8 of slice 2.4 caught that strict equality dropped
      // terminal errors when sessionID was undefined.
      const errSessionID = ev.properties.sessionID;
      if (errSessionID !== undefined && errSessionID !== targetSession) break;
      const err = ev.properties.error;
      const errMsg =
        err && "data" in err
          ? String((err.data as { message?: string }).message ?? JSON.stringify(err))
          : "opencode session.error (no detail)";
      sessionError ??= errMsg;
      break;
    }

    // All other event types are intentionally ignored.
    default:
      break;
  }

  return { wireEvents, sessionIdle, sessionError };
}

// ── helpers ────────────────────────────────────────────────────────────────

function isToolPart(part: unknown): part is ToolPart {
  return (
    typeof part === "object" &&
    part !== null &&
    (part as { type?: string }).type === "tool"
  );
}

function isTextPart(
  part: unknown,
): part is { type: "text"; text: string; messageID: string; synthetic?: boolean } {
  return (
    typeof part === "object" &&
    part !== null &&
    (part as { type?: string }).type === "text" &&
    typeof (part as { text?: unknown }).text === "string"
  );
}
