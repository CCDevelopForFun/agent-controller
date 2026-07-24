// Slice 5.3: adapter-side OTel emission for the Pi runtime.
//
// What this module does
// ---------------------
//   - Initializes a NodeTracerProvider with an OTLP/HTTP exporter when
//     `spec.observability.tracing === true` AND `OTEL_EXPORTER_OTLP_ENDPOINT`
//     is set in the environment.
//   - Extracts the host-side TRACEPARENT (delivered by slice 5.2's
//     KubernetesBackend / LocalBackend env injection) and uses it as
//     the parent for the adapter's root `agent.session` span.
//   - Exposes a thin callback surface (`AdapterTracing`) the adapter
//     can call from its existing Pi event subscriber. Pi-event-to-span
//     translation lives ENTIRELY here so adapter.ts stays focused on
//     wire-event emission and guardrails.
//
// What it deliberately does NOT do
// --------------------------------
//   - No OTel JS auto-instrumentation. The Pi adapter is a small
//     subprocess; auto-instrumentation's startup cost is not worth it.
//   - No metric or log emission yet. Slice 5.5+ will add those.
//   - No span emission when tracing is off. The factory returns a
//     no-op implementation so adapter.ts can call onLLMStart /
//     onToolStart / etc. unconditionally without `if (tracing)` guards
//     in the hot path.
//
// Span shape
// ----------
// The tree the adapter produces:
//
//   agent.session                    (adapter root, parent = host agentctl.run via TRACEPARENT)
//   ├── gen_ai.chat                  (one per Pi `turn_start` / `turn_end` pair — see note)
//   │   ├── gen_ai.tool.call (read)  (tool spans nest under the LLM turn that requested them)
//   │   └── gen_ai.tool.call (bash)
//   └── gen_ai.chat
//       └── gen_ai.tool.call (write)
//
// Why turn_start / turn_end and NOT before_provider_request /
// after_provider_response: the latter pair are extension-runner hooks
// surfaced via the Pi ExtensionAPI event bus — they never reach the
// session-level `subscribe()` callback. AgentSessionEvent (which is
// what session.subscribe receives) only contains the union from
// pi-agent-core's AgentEvent: agent_start, turn_start, message_*,
// tool_execution_*, turn_end, agent_end. Codex pass 1 of slice 5.3
// caught the original mis-wiring against extension hooks.
//
// Attributes follow OTel GenAI semantic conventions:
//   https://opentelemetry.io/docs/specs/semconv/gen-ai/
//
// Content capture (`spec.observability.captureContent`)
// -----------------------------------------------------
// Off by default. When on, the adapter attaches:
//   - `gen_ai.prompt` (assistant text) on `agent.session`
//   - `gen_ai.completion` (assistant text from message_end) on the
//     surrounding `gen_ai.chat` span
//   - `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result` on
//     `gen_ai.tool.call` spans, JSON-stringified and capped at 64 KiB
//     to protect the exporter pipeline from unbounded payloads.

import {
  context,
  diag,
  DiagConsoleLogger,
  DiagLogLevel,
  propagation,
  ROOT_CONTEXT,
  SpanStatusCode,
  trace,
  type Context,
  type Span,
  type Tracer,
} from "@opentelemetry/api";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import {
  ATTR_SERVICE_NAME,
  ATTR_SERVICE_VERSION,
} from "@opentelemetry/semantic-conventions";

import type { CompiledSpec } from "./types.js";

/** Hard cap on content-attribute payloads (per attribute, post-JSON). */
const MAX_CONTENT_BYTES = 64 * 1024;
/** Marker appended when content is truncated. Counted against the cap. */
const TRUNCATION_MARKER = "…[truncated]";
const TRUNCATION_MARKER_BYTES = Buffer.byteLength(TRUNCATION_MARKER, "utf8");

/**
 * Shutdown budget shared by the BatchSpanProcessor's `exportTimeoutMillis`,
 * the OTLP exporter's `timeoutMillis`, and the AdapterTracing.end() watchdog.
 * Matches the host-side flush budget in cli/cmd/agentctl/main.go.
 *
 * Configuring the SDK's own timeouts (not just our outer Promise.race) is
 * required to actually bound process exit: without these, the BSP's default
 * 30s export-timeout and the exporter's default 10s request-timeout keep
 * unref'd-but-pending HTTP work alive past our race, holding the adapter
 * process open beyond the intended 5s. Codex pass 4 of slice 5.3 caught
 * this — Promise.race only stops *awaiting*, it doesn't cancel the
 * underlying work.
 */
const SHUTDOWN_BUDGET_MS = 5000;

/** Slice 5.2 namespace constant. Stamped on wire events when tracing is on. */
export const EVENTS_API_VERSION_V1ALPHA1 = "agent-controller.dev/events/v1alpha1";

/** What the adapter calls. The no-op variant is returned when tracing is off. */
export interface AdapterTracing {
  /** Active TRACEPARENT for stamping on the slice-5.2 wire envelope; undefined when off. */
  getTraceparent(): string | undefined;
  /** Fires on Pi `turn_start`. Opens a `gen_ai.chat` span. */
  onLLMStart(): void;
  /**
   * Fires on Pi `turn_end`. Closes the `gen_ai.chat` span and records
   * usage / stopReason pulled from the assistant message's `usage`
   * (input/output token counts per pi-ai's `Usage` interface) and
   * `stopReason` ("stop" | "length" | "toolUse" | "error" | "aborted").
   */
  onLLMEnd(args: {
    inputTokens?: number;
    outputTokens?: number;
    stopReason?: string;
  }): void;
  onToolStart(toolName: string, callId: string, args: unknown): void;
  onToolEnd(callId: string, isError: boolean, content: unknown): void;
  /** Records the role+text on the active LLM span as completion attrs (captureContent=true only). */
  onAssistantMessage(role: string, text: string): void;
  /** Records an error event on the session span. */
  onError(message: string): void;
  /** Closes the session span. Idempotent — repeat calls are no-ops. */
  end(reason: "completed" | "error", message?: string): Promise<void>;
}

const NOOP_TRACING: AdapterTracing = {
  getTraceparent: () => undefined,
  onLLMStart: () => {},
  onLLMEnd: () => {},
  onToolStart: () => {},
  onToolEnd: () => {},
  onAssistantMessage: () => {},
  onError: () => {},
  end: async () => {},
};

/**
 * Resolve the OTLP traces endpoint. Honors both the trace-specific env
 * var and the generic catch-all, in the same priority order the OTel
 * SDK uses internally (specific wins). Matches the host-side
 * `isEnabled()` in cli/internal/observability/otel.go — codex pass 1 of
 * slice 5.3 caught that we previously only checked the generic var,
 * silently no-op'ing on the trace-specific config.
 *
 * Exported for unit testing — direct test against `initAdapterTracing`
 * for the live-tracer path would have to stand up an OTLPExporter and
 * wait through its retries, which doesn't unit-test cleanly.
 */
export function resolveOtlpTracesEndpoint(): string | undefined {
  for (const name of ["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"]) {
    const v = process.env[name];
    if (v && v.trim() !== "") return v;
  }
  return undefined;
}

export interface InitTracingArgs {
  spec: CompiledSpec;
  sessionId: string;
  /** runtime package version, embedded as service.version resource attr. */
  packageVersion: string;
}

/**
 * Initialize OTel for this adapter run. Returns a no-op tracer when
 * either `spec.observability.tracing` is off OR
 * `OTEL_EXPORTER_OTLP_ENDPOINT` is unset — both conditions match the
 * host-side gate in `cli/internal/observability/otel.go`, keeping the
 * "tracing is on" semantics consistent across the host/adapter boundary.
 */
export function initAdapterTracing(args: InitTracingArgs): AdapterTracing {
  const tracingRequested = args.spec.observability?.tracing === true;
  if (!tracingRequested) return NOOP_TRACING;

  if (resolveOtlpTracesEndpoint() === undefined) {
    // Matches the host-side behavior: spec opted in but no collector
    // reachable. Better to silently no-op than spam stderr per run.
    return NOOP_TRACING;
  }

  // Honor OTEL_LOG_LEVEL for diagnostics — same env var the host uses.
  if (process.env.OTEL_LOG_LEVEL?.toLowerCase() === "debug") {
    diag.setLogger(new DiagConsoleLogger(), DiagLogLevel.DEBUG);
  }

  const resource = resourceFromAttributes({
    [ATTR_SERVICE_NAME]: "@agent-controller/runtime",
    [ATTR_SERVICE_VERSION]: args.packageVersion,
    "agent_controller.agent.name": args.spec.metadata.name,
    "agent_controller.runtime.type": args.spec.runtime.type,
    "agent_controller.session.id": args.sessionId,
  });

  // Match the BSP's exportTimeoutMillis + the OTLP exporter's
  // timeoutMillis to SHUTDOWN_BUDGET_MS so they don't outlive our
  // shutdown race and keep the Node event loop alive past intent.
  const exporter = new OTLPTraceExporter({ timeoutMillis: SHUTDOWN_BUDGET_MS });
  const provider = new NodeTracerProvider({
    resource,
    spanProcessors: [
      new BatchSpanProcessor(exporter, {
        maxExportBatchSize: 64,
        scheduledDelayMillis: 2000,
        exportTimeoutMillis: SHUTDOWN_BUDGET_MS,
      }),
    ],
  });
  provider.register({ propagator: new W3CTraceContextPropagator() });

  const tracer = provider.getTracer("@agent-controller/runtime", args.packageVersion);
  const captureContent = args.spec.observability?.captureContent === true;

  // Extract the host-delivered parent (slice 5.2). When TRACEPARENT is
  // absent the propagator returns ROOT_CONTEXT unchanged and we end up
  // with a detached root — that's the local-CLI case where the user
  // ran agentctl directly without an outer trace.
  const parentCtx = propagation.extract(ROOT_CONTEXT, {
    traceparent: process.env.TRACEPARENT ?? "",
    tracestate: process.env.TRACESTATE ?? "",
  });

  const sessionSpan = tracer.startSpan(
    "agent.session",
    {
      attributes: {
        "gen_ai.system": args.spec.model.provider,
        "gen_ai.request.model": args.spec.model.name,
        "gen_ai.operation.name": "agent.session",
        "agent_controller.agent.name": args.spec.metadata.name,
        "agent_controller.session.id": args.sessionId,
      },
    },
    parentCtx,
  );
  const sessionCtx = trace.setSpan(parentCtx, sessionSpan);

  // Codex pass 2 of slice 5.3: when captureContent is on, attach the
  // user prompt (spec.task) immediately so the schema's
  // "prompt + completion + tool args + tool result" contract holds.
  // We stamp it on the session span specifically (NOT each gen_ai.chat
  // turn) because the prompt is a property of the agent run as a whole
  // — every turn within the run sees the same user task. If a future
  // slice surfaces additional user messages mid-run (e.g. interactive
  // mode), those will go on the relevant gen_ai.chat span instead.
  if (captureContent && args.spec.task) {
    sessionSpan.setAttribute("gen_ai.prompt", truncate(args.spec.task));
  }

  // Capture the TRACEPARENT carried by the session span; the adapter
  // stamps it onto every emitted wire event so downstream consumers
  // (CLI watchers, logging pipelines) can correlate.
  const carrier: Record<string, string> = {};
  propagation.inject(sessionCtx, carrier);
  const sessionTraceparent = carrier.traceparent;

  return makeTracing({
    tracer,
    sessionSpan,
    sessionCtx,
    sessionTraceparent,
    captureContent,
    modelAttrs: {
      "gen_ai.system": args.spec.model.provider,
      "gen_ai.request.model": args.spec.model.name,
    },
    shutdown: () => provider.shutdown(),
  });
}

interface LiveState {
  tracer: Tracer;
  sessionSpan: Span;
  sessionCtx: Context;
  sessionTraceparent: string | undefined;
  captureContent: boolean;
  /**
   * Model attrs copied onto every `gen_ai.chat` span. OTel attributes
   * don't inherit through the span tree — backends that filter LLM
   * spans by `gen_ai.system` / `gen_ai.request.model` need them
   * directly on the chat span. Codex pass 5 of slice 5.3 caught this.
   */
  modelAttrs: Record<string, string>;
  shutdown: () => Promise<void>;
}

function makeTracing(state: LiveState): AdapterTracing {
  let activeLLM: { span: Span; ctx: Context } | null = null;
  const liveTools = new Map<string, Span>();
  let ended = false;

  return {
    getTraceparent: () => state.sessionTraceparent,

    onLLMStart() {
      // If a prior turn span never closed, close it as ERROR before
      // opening a new one. Defensive — Pi should always pair turn_start
      // with turn_end, but a crash mid-turn would leak an open span
      // otherwise.
      if (activeLLM) {
        activeLLM.span.setStatus({
          code: SpanStatusCode.ERROR,
          message: "LLM span superseded without matching turn_end",
        });
        activeLLM.span.end();
        activeLLM = null;
      }
      const span = state.tracer.startSpan(
        "gen_ai.chat",
        {
          attributes: {
            "gen_ai.operation.name": "chat",
            ...state.modelAttrs,
          },
        },
        state.sessionCtx,
      );
      activeLLM = { span, ctx: trace.setSpan(state.sessionCtx, span) };
    },

    onLLMEnd({ inputTokens, outputTokens, stopReason }) {
      if (!activeLLM) return;
      if (inputTokens !== undefined) {
        activeLLM.span.setAttribute("gen_ai.usage.input_tokens", inputTokens);
      }
      if (outputTokens !== undefined) {
        activeLLM.span.setAttribute("gen_ai.usage.output_tokens", outputTokens);
      }
      if (stopReason) {
        // OTel GenAI semconv expects an array on finish_reasons.
        activeLLM.span.setAttribute("gen_ai.response.finish_reasons", [stopReason]);
        if (stopReason === "error" || stopReason === "aborted") {
          activeLLM.span.setStatus({
            code: SpanStatusCode.ERROR,
            message: `turn ended with stopReason=${stopReason}`,
          });
        }
      }
      activeLLM.span.end();
      activeLLM = null;
    },

    onToolStart(toolName, callId, args) {
      // Parent = active LLM span if one is open, else the session span.
      // The LLM-active case is normal mid-turn execution; the
      // session-parent case covers tool calls that fire outside a
      // generation (e.g. lifecycle hooks).
      const parentCtx = activeLLM ? activeLLM.ctx : state.sessionCtx;
      const span = state.tracer.startSpan(
        `gen_ai.tool.call ${toolName}`,
        {
          attributes: {
            "gen_ai.operation.name": "execute_tool",
            "gen_ai.tool.name": toolName,
            "gen_ai.tool.call.id": callId,
            // `gen_ai.system` is required on all gen_ai.* spans per
            // semconv; backends filtering by system would otherwise
            // miss tool spans.
            "gen_ai.system": state.modelAttrs["gen_ai.system"],
          },
        },
        parentCtx,
      );
      if (state.captureContent) {
        span.setAttribute("gen_ai.tool.call.arguments", truncateJson(args));
      }
      liveTools.set(callId, span);
    },

    onToolEnd(callId, isError, content) {
      const span = liveTools.get(callId);
      if (!span) return;
      liveTools.delete(callId);
      if (isError) {
        span.setStatus({ code: SpanStatusCode.ERROR });
      }
      if (state.captureContent) {
        span.setAttribute("gen_ai.tool.call.result", truncateJson(content));
      }
      span.end();
    },

    onAssistantMessage(role, text) {
      // Only assistant messages carry the model's completion. user/tool
      // messages are inputs from the agent runtime side; capturing them
      // would be redundant with `gen_ai.tool.call.result`.
      if (!state.captureContent || role !== "assistant" || !text) return;
      const target = activeLLM?.span ?? state.sessionSpan;
      target.setAttribute("gen_ai.completion", truncate(text));
    },

    onError(message) {
      state.sessionSpan.addEvent("exception", {
        "exception.message": message,
      });
    },

    async end(reason, message) {
      if (ended) return;
      ended = true;

      // Sweep any spans the Pi event stream left open. In a healthy
      // session both maps are already empty; this is a safety net for
      // crash paths so the BatchSpanProcessor doesn't drop unended
      // spans on shutdown.
      for (const [, span] of liveTools) {
        span.setStatus({ code: SpanStatusCode.ERROR, message: "tool span left open at session end" });
        span.end();
      }
      liveTools.clear();
      if (activeLLM) {
        activeLLM.span.setStatus({ code: SpanStatusCode.ERROR, message: "LLM span left open at session end" });
        activeLLM.span.end();
        activeLLM = null;
      }

      if (reason === "error") {
        state.sessionSpan.setStatus({
          code: SpanStatusCode.ERROR,
          message: message ?? "session ended with error",
        });
      } else {
        state.sessionSpan.setStatus({ code: SpanStatusCode.OK });
      }
      state.sessionSpan.end();

      // Drain BatchSpanProcessor. Three layered timeouts ALL set to
      // SHUTDOWN_BUDGET_MS:
      //   - OTLPExporter timeoutMillis (per HTTP request)
      //   - BatchSpanProcessor exportTimeoutMillis (per export call)
      //   - this watchdog Promise.race (per shutdown await)
      // Just the outer race is not enough — Promise.race stops
      // awaiting but does not cancel pending I/O. Codex pass 4 of
      // slice 5.3 caught the gap: with the SDK defaults (10s/30s),
      // the adapter process could stay alive 30s past intended exit.
      //
      // The watchdog timer is cleared in finally so the pending
      // setTimeout doesn't itself keep the event loop alive after a
      // fast shutdown (codex pass 1 P2 fix).
      let watchdog: ReturnType<typeof setTimeout> | undefined;
      const timeoutPromise = new Promise<void>((_, reject) => {
        watchdog = setTimeout(
          () => reject(new Error("OTel shutdown timed out")),
          SHUTDOWN_BUDGET_MS,
        );
      });
      try {
        await Promise.race([state.shutdown(), timeoutPromise]);
      } catch (err) {
        process.stderr.write(`[agent-controller] OTel shutdown error: ${String(err)}\n`);
      } finally {
        if (watchdog) clearTimeout(watchdog);
      }
    },
  };
}

/**
 * Cap content payloads. The byte-limit protects the OTLP exporter from
 * a single span ballooning to MBs when an agent run produces a large
 * tool result (e.g. a 1 MB grep output). 64 KiB matches the host-side
 * stdout scanner buffer cap in cli/internal/backend/local.go.
 *
 * The truncation marker counts against the cap — the returned string's
 * byte length is always <= MAX_CONTENT_BYTES. Codex pass 4 of slice
 * 5.3 caught the original implementation: it sliced to the full cap
 * and THEN appended the marker, exceeding the cap by 13 UTF-8 bytes
 * and risking rejection from exact-limit OTLP exporter guards.
 *
 * Exported for unit testing — testing via the public tracer would
 * require a live OTel SDK + an OTLP collector.
 */
export const __MAX_CONTENT_BYTES_FOR_TESTS = MAX_CONTENT_BYTES;
export function truncateForTests(s: string): string {
  return truncate(s);
}
function truncate(s: string): string {
  if (Buffer.byteLength(s, "utf8") <= MAX_CONTENT_BYTES) return s;
  const budget = MAX_CONTENT_BYTES - TRUNCATION_MARKER_BYTES;
  // Trim by characters first (cheap), then re-check bytes — UTF-8
  // multi-byte chars can push back over the limit when we cut on a
  // byte boundary that's not a char boundary.
  let out = s.slice(0, budget);
  while (Buffer.byteLength(out, "utf8") > budget) {
    out = out.slice(0, -1);
  }
  return out + TRUNCATION_MARKER;
}

function truncateJson(value: unknown): string {
  try {
    return truncate(JSON.stringify(value) ?? "null");
  } catch {
    // Circular refs, BigInt, etc. — better to ship a marker than the
    // span fail to export.
    return "[unserializable]";
  }
}
