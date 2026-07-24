// Slice 5.4: adapter-side OTel for the opencode runtime.
//
// Different shape from the Pi adapter's observability module
// (runtime/src/observability.ts):
//
//   - opencode itself emits AI-SDK telemetry spans (LLM calls, tool
//     calls) when its config has `experimental.openTelemetry: true`.
//     We don't duplicate those — opencode runs as a separate child
//     process and has its own OTel SDK init.
//
//   - Our adapter owns one span: `agent.session`. It wraps the entire
//     opencode session, nests under the host `agentctl.run` span via
//     the TRACEPARENT delivered by slice 5.2 env injection, and
//     becomes the parent for opencode's AI-SDK spans by re-injecting
//     a fresh TRACEPARENT into `process.env` before the SDK spawns
//     the opencode child (createOpencodeServer spreads process.env).
//
//   - captureContent attaches `gen_ai.prompt` (spec.task) on
//     agent.session. opencode handles per-LLM-call content capture
//     itself via its own gen_ai.* attributes.
//
// The two-condition gate matches runtime/src/observability.ts and
// the host CLI:
//
//   spec.observability.tracing === true
//     AND
//   OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) set

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

/** Hard cap on content-attribute payloads. */
const MAX_CONTENT_BYTES = 64 * 1024;
const TRUNCATION_MARKER = "…[truncated]";
const TRUNCATION_MARKER_BYTES = Buffer.byteLength(TRUNCATION_MARKER, "utf8");

/** Shared shutdown budget — matches host + Pi adapter. */
const SHUTDOWN_BUDGET_MS = 5000;

/** Slice 5.2 namespace constant. */
export const EVENTS_API_VERSION_V1ALPHA1 = "agent-controller.dev/events/v1alpha1";

export interface AdapterTracing {
  /** Active TRACEPARENT for wire-envelope stamping; undefined when off. */
  getTraceparent(): string | undefined;
  /** Record a non-fatal warning on the session span. */
  onWarning(message: string): void;
  /** Record an error event on the session span. */
  onError(message: string): void;
  /** Closes the session span + flushes the OTel SDK. Idempotent. */
  end(reason: "completed" | "error", message?: string): Promise<void>;
}

const NOOP_TRACING: AdapterTracing = {
  getTraceparent: () => undefined,
  onWarning: () => {},
  onError: () => {},
  end: async () => {},
};

/**
 * Resolve the OTLP traces endpoint. Honors both the trace-specific env
 * var and the generic catch-all, with trace-specific winning. Mirrors
 * runtime/src/observability.ts so the two adapters have identical gate
 * semantics — operators don't have to learn two different env conventions.
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
  /** runtime-opencode package version, embedded as service.version. */
  packageVersion: string;
}

export function initAdapterTracing(args: InitTracingArgs): AdapterTracing {
  const tracingRequested = args.spec.observability?.tracing === true;
  if (!tracingRequested) return NOOP_TRACING;
  if (resolveOtlpTracesEndpoint() === undefined) return NOOP_TRACING;

  if (process.env.OTEL_LOG_LEVEL?.toLowerCase() === "debug") {
    diag.setLogger(new DiagConsoleLogger(), DiagLogLevel.DEBUG);
  }

  const resource = resourceFromAttributes({
    [ATTR_SERVICE_NAME]: "@agent-controller/runtime-opencode",
    [ATTR_SERVICE_VERSION]: args.packageVersion,
    "agent_controller.agent.name": args.spec.metadata.name,
    "agent_controller.runtime.type": args.spec.runtime.type,
    "agent_controller.session.id": args.sessionId,
  });

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

  const tracer = provider.getTracer("@agent-controller/runtime-opencode", args.packageVersion);
  const captureContent = args.spec.observability?.captureContent === true;

  // Slice 5.2 host injection delivers TRACEPARENT via env. Extract
  // here so the agent.session span we open below nests under the
  // host agentctl.run span. Missing TRACEPARENT means local-CLI run
  // with no outer trace — we still emit a detached agent.session root.
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

  // Per-prompt content capture (mirrors Pi adapter's behavior — the
  // user's task is a property of the run as a whole, so it goes on
  // the session span, not each LLM call).
  if (captureContent && args.spec.task) {
    sessionSpan.setAttribute("gen_ai.prompt", truncate(args.spec.task));
  }

  // Build the TRACEPARENT carrier from the session span so the
  // adapter can stamp it on wire events AND re-inject it into
  // process.env before spawning opencode. opencode inherits env from
  // the adapter; its experimental.openTelemetry path's AI-SDK spans
  // will see this as the parent context if opencode initializes its
  // own OTel SDK from env.
  const carrier: Record<string, string> = {};
  propagation.inject(sessionCtx, carrier);
  const sessionTraceparent = carrier.traceparent;
  const sessionTracestate = carrier.tracestate;

  // Mutate process.env so the @opencode-ai/sdk's `createOpencodeServer`
  // (which spreads `...process.env` into the opencode child env) picks
  // up our fresh TRACEPARENT. The host's injected TRACEPARENT is
  // overwritten — that's correct: opencode should nest under
  // agent.session (which itself nests under agentctl.run), not under
  // agentctl.run directly. If we left the host's TRACEPARENT in env,
  // opencode's spans would skip the agent.session level.
  if (sessionTraceparent) {
    process.env.TRACEPARENT = sessionTraceparent;
  }
  if (sessionTracestate) {
    process.env.TRACESTATE = sessionTracestate;
  } else {
    // Don't leak a stale tracestate from the host into the child —
    // mirrors the cleanup performed by injectTraceparent in
    // cli/internal/backend/trace_propagation.go (codex pass 2 of slice 5.2).
    delete process.env.TRACESTATE;
  }

  let ended = false;
  return {
    getTraceparent: () => sessionTraceparent,

    onWarning(message) {
      sessionSpan.addEvent("warning", { message });
    },

    onError(message) {
      sessionSpan.addEvent("exception", { "exception.message": message });
    },

    async end(reason, message) {
      if (ended) return;
      ended = true;

      if (reason === "error") {
        sessionSpan.setStatus({
          code: SpanStatusCode.ERROR,
          message: message ?? "session ended with error",
        });
      } else {
        sessionSpan.setStatus({ code: SpanStatusCode.OK });
      }
      sessionSpan.end();

      // Three layered 5s timeouts (exporter, BSP, this watchdog) —
      // matches the Pi adapter pattern. Promise.race alone wouldn't
      // bound process exit because it stops awaiting but doesn't
      // cancel pending I/O (codex pass 4 of slice 5.3 caught this).
      let watchdog: ReturnType<typeof setTimeout> | undefined;
      const timeoutPromise = new Promise<void>((_, reject) => {
        watchdog = setTimeout(
          () => reject(new Error("OTel shutdown timed out")),
          SHUTDOWN_BUDGET_MS,
        );
      });
      try {
        await Promise.race([provider.shutdown(), timeoutPromise]);
      } catch (err) {
        process.stderr.write(`[agent-controller] OTel shutdown error: ${String(err)}\n`);
      } finally {
        if (watchdog) clearTimeout(watchdog);
      }
    },
  };
}

/**
 * Cap content payloads. Identical contract to runtime/src/observability.ts —
 * reserves marker bytes BEFORE slicing so the returned string's byte
 * length is always <= MAX_CONTENT_BYTES, even on the truncation path.
 *
 * Exported for unit testing.
 */
export const __MAX_CONTENT_BYTES_FOR_TESTS = MAX_CONTENT_BYTES;
export function truncateForTests(s: string): string {
  return truncate(s);
}
function truncate(s: string): string {
  if (Buffer.byteLength(s, "utf8") <= MAX_CONTENT_BYTES) return s;
  const budget = MAX_CONTENT_BYTES - TRUNCATION_MARKER_BYTES;
  let out = s.slice(0, budget);
  while (Buffer.byteLength(out, "utf8") > budget) {
    out = out.slice(0, -1);
  }
  return out + TRUNCATION_MARKER;
}
