// Tests for the slice 5.3 adapter-side OTel emission.
//
// We avoid spinning up a real OTel SDK in unit tests because:
//   (a) BatchSpanProcessor + OTLPExporter have nontrivial startup
//       (workers, AbortControllers, etc.) that don't tear down cleanly
//       inside vitest's worker — they leak and the run hangs.
//   (b) We don't need to verify OTel SDK plumbing; the SDK has its own
//       tests upstream. What WE care about is:
//         - tracing is OFF unless both spec.observability.tracing
//           AND OTEL_EXPORTER_OTLP_ENDPOINT are set,
//         - the no-op tracer's surface contract matches AdapterTracing,
//         - the inline truncation cap behaves correctly.
//
// The live-emit path is verified end-to-end in slice 5.6 against a
// real OTLP collector, the same way slice 5.1 verified the host-side
// tracer.

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  __MAX_CONTENT_BYTES_FOR_TESTS,
  initAdapterTracing,
  resolveOtlpTracesEndpoint,
  truncateForTests,
} from "./observability.js";
import type { CompiledSpec } from "./types.js";

function baseSpec(overrides: Partial<CompiledSpec> = {}): CompiledSpec {
  return {
    v: 1,
    metadata: { name: "test-agent" },
    model: { provider: "anthropic", name: "claude-sonnet-4-6" },
    task: "do a thing",
    tools: [],
    extensions: [],
    skills: [],
    runtime: { type: "local-pi" },
    ...overrides,
  };
}

describe("initAdapterTracing", () => {
  const savedEnv = process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
  const savedTracesEnv = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
  beforeEach(() => {
    delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
    delete process.env.TRACEPARENT;
    delete process.env.TRACESTATE;
  });
  afterEach(() => {
    if (savedEnv === undefined) delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_ENDPOINT = savedEnv;
    if (savedTracesEnv === undefined) delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = savedTracesEnv;
  });

  it("returns a no-op tracer when spec.observability is absent", async () => {
    const spec = baseSpec();
    const t = initAdapterTracing({ spec, sessionId: "s1", packageVersion: "0.0.0" });
    expect(t.getTraceparent()).toBeUndefined();
    // Calls must be no-ops (no throw, no side effects).
    t.onLLMStart(0);
    t.onLLMEnd({ tokensIn: 1, tokensOut: 1, finishReason: "stop" });
    t.onToolStart("read", "c1", { file: "x" });
    t.onToolEnd("c1", false, "ok");
    t.onAssistantMessage("assistant", "hi");
    t.onError("err");
    await t.end("completed");
  });

  it("returns a no-op tracer when spec opts in but OTLP env is unset", async () => {
    // Spec wants tracing, but the operator hasn't configured a
    // collector. Matches the host-side gate in
    // cli/internal/observability/otel.go — silently no-op rather than
    // spam stderr per run.
    const spec = baseSpec({ observability: { tracing: true } });
    const t = initAdapterTracing({ spec, sessionId: "s2", packageVersion: "0.0.0" });
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });

  it("returns a no-op tracer when OTLP endpoint is whitespace-only", async () => {
    // Defense against a half-configured env (e.g. `export
    // OTEL_EXPORTER_OTLP_ENDPOINT=` in a shell). Without this check
    // we'd try to ship to "" and the SDK would silently retry forever.
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    const spec = baseSpec({ observability: { tracing: true } });
    const t = initAdapterTracing({ spec, sessionId: "s3", packageVersion: "0.0.0" });
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });

  it("returns a no-op tracer when tracing is opted out explicitly", async () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://localhost:4318/v1/traces";
    const spec = baseSpec({ observability: { tracing: false, captureContent: true } });
    const t = initAdapterTracing({ spec, sessionId: "s4", packageVersion: "0.0.0" });
    // captureContent=true alone is not enough — tracing must be on too.
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });
});

describe("truncate (content-cap enforcement)", () => {
  // Codex pass 4 of slice 5.3 caught the cap being exceeded by the
  // truncation marker. These tests anchor the contract:
  // `byteLength(out) <= MAX_CONTENT_BYTES` strictly, even on the
  // truncation path.
  it("returns input unchanged when under the cap", () => {
    const small = "hello";
    expect(truncateForTests(small)).toBe(small);
  });

  it("returned string fits within the byte cap when truncating ASCII", () => {
    const huge = "x".repeat(__MAX_CONTENT_BYTES_FOR_TESTS * 2);
    const out = truncateForTests(huge);
    expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(__MAX_CONTENT_BYTES_FOR_TESTS);
    expect(out.endsWith("…[truncated]")).toBe(true);
  });

  it("returned string fits within the byte cap on UTF-8 multi-byte content", () => {
    // 4-byte UTF-8 char repeated past the cap — exercise the
    // byte-aware trimming loop that prevents the slice cut from
    // landing mid-char.
    const huge = "🎯".repeat(__MAX_CONTENT_BYTES_FOR_TESTS);
    const out = truncateForTests(huge);
    expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(__MAX_CONTENT_BYTES_FOR_TESTS);
    expect(out.endsWith("…[truncated]")).toBe(true);
  });
});

describe("resolveOtlpTracesEndpoint", () => {
  // Codex pass 1 of slice 5.3 caught a config-recognition gap: the
  // gate only checked OTEL_EXPORTER_OTLP_ENDPOINT, silently no-op'ing
  // when an operator set the trace-specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
  // alone — a documented OTel convention used in many real deployments.
  const savedGeneric = process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
  const savedTraces = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
  beforeEach(() => {
    delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
  });
  afterEach(() => {
    if (savedGeneric === undefined) delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_ENDPOINT = savedGeneric;
    if (savedTraces === undefined) delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = savedTraces;
  });

  it("returns undefined when neither env var is set", () => {
    expect(resolveOtlpTracesEndpoint()).toBeUndefined();
  });

  it("recognizes the trace-specific endpoint env var", () => {
    process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = "http://collector.example/v1/traces";
    expect(resolveOtlpTracesEndpoint()).toBe("http://collector.example/v1/traces");
  });

  it("recognizes the generic endpoint env var", () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://collector.example";
    expect(resolveOtlpTracesEndpoint()).toBe("http://collector.example");
  });

  it("trace-specific wins when both are set (OTel SDK convention)", () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://generic.example";
    process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = "http://traces.example";
    expect(resolveOtlpTracesEndpoint()).toBe("http://traces.example");
  });

  it("ignores whitespace-only values from a half-configured env", () => {
    process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = "   ";
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "";
    expect(resolveOtlpTracesEndpoint()).toBeUndefined();
  });
});
