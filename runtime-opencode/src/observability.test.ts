// Slice 5.4 tests for the opencode adapter's OTel surface. Mirrors
// runtime/src/observability.test.ts but does NOT stand up a live OTel
// SDK against a real OTLP endpoint — see the comment at the top of
// the Pi adapter's test file for why (BatchSpanProcessor + OTLPExporter
// leak worker handles inside vitest workers).
//
// What we DO verify:
//   - No-op gating: tracing must be opted-in via spec AND via env.
//   - resolveOtlpTracesEndpoint precedence (trace-specific wins).
//   - truncate() byte-cap invariant including the marker.
//   - process.env.TRACEPARENT / TRACESTATE rewriting semantics, since
//     opencode reads them via env to inherit the parent trace context.
//
// Live emission is verified end-to-end in slice 5.6 against a real
// OTLP collector.

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
    runtime: { type: "local-opencode" },
    ...overrides,
  };
}

describe("initAdapterTracing (opencode)", () => {
  const savedGeneric = process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
  const savedTraces = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
  const savedTP = process.env.TRACEPARENT;
  const savedTS = process.env.TRACESTATE;
  beforeEach(() => {
    delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
    delete process.env.TRACEPARENT;
    delete process.env.TRACESTATE;
  });
  afterEach(() => {
    if (savedGeneric === undefined) delete process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_ENDPOINT = savedGeneric;
    if (savedTraces === undefined) delete process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
    else process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = savedTraces;
    if (savedTP === undefined) delete process.env.TRACEPARENT;
    else process.env.TRACEPARENT = savedTP;
    if (savedTS === undefined) delete process.env.TRACESTATE;
    else process.env.TRACESTATE = savedTS;
  });

  it("returns a no-op tracer when spec.observability is absent", async () => {
    const t = initAdapterTracing({
      spec: baseSpec(),
      sessionId: "s1",
      packageVersion: "0.0.0",
    });
    expect(t.getTraceparent()).toBeUndefined();
    t.onWarning("noop");
    t.onError("noop");
    await t.end("completed");
  });

  it("returns a no-op tracer when OTLP env is unset (matches host gate)", async () => {
    const t = initAdapterTracing({
      spec: baseSpec({ observability: { tracing: true } }),
      sessionId: "s2",
      packageVersion: "0.0.0",
    });
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });

  it("returns a no-op tracer when OTLP endpoint is whitespace-only", async () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "   ";
    const t = initAdapterTracing({
      spec: baseSpec({ observability: { tracing: true } }),
      sessionId: "s3",
      packageVersion: "0.0.0",
    });
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });

  it("returns a no-op tracer when captureContent is set but tracing is off", async () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://localhost:4318/v1/traces";
    const t = initAdapterTracing({
      spec: baseSpec({ observability: { tracing: false, captureContent: true } }),
      sessionId: "s4",
      packageVersion: "0.0.0",
    });
    expect(t.getTraceparent()).toBeUndefined();
    await t.end("completed");
  });

  it("no-op tracer does not mutate process.env TRACEPARENT/TRACESTATE", async () => {
    // When tracing is off, the opencode child should see whatever
    // TRACEPARENT was inherited from the host (slice 5.2). The
    // adapter must not clobber it.
    process.env.TRACEPARENT = "00-host-host-host-host-host-host-host-h-host-host-host-001";
    process.env.TRACESTATE = "vendor=keep-me";
    const t = initAdapterTracing({
      spec: baseSpec(),
      sessionId: "s5",
      packageVersion: "0.0.0",
    });
    expect(process.env.TRACEPARENT).toBe("00-host-host-host-host-host-host-host-h-host-host-host-001");
    expect(process.env.TRACESTATE).toBe("vendor=keep-me");
    await t.end("completed");
  });
});

describe("resolveOtlpTracesEndpoint (opencode)", () => {
  // Same contract as the Pi adapter's resolver — operators learning
  // one set of env conventions can rely on both adapters honoring
  // the same precedence.
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

  it("undefined when neither var is set", () => {
    expect(resolveOtlpTracesEndpoint()).toBeUndefined();
  });

  it("trace-specific endpoint wins when both are set", () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://generic.example";
    process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = "http://traces.example";
    expect(resolveOtlpTracesEndpoint()).toBe("http://traces.example");
  });

  it("falls back to the generic endpoint when only it is set", () => {
    process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://generic.example";
    expect(resolveOtlpTracesEndpoint()).toBe("http://generic.example");
  });
});

describe("truncate (opencode content-cap)", () => {
  // Marker-byte reservation: returned string must satisfy
  //   byteLength(out) <= MAX_CONTENT_BYTES strictly,
  // even on the truncation path. Matches the Pi adapter contract.
  it("returns input unchanged when under the cap", () => {
    expect(truncateForTests("hi")).toBe("hi");
  });

  it("fits within the cap on ASCII over-cap input", () => {
    const huge = "x".repeat(__MAX_CONTENT_BYTES_FOR_TESTS * 2);
    const out = truncateForTests(huge);
    expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(__MAX_CONTENT_BYTES_FOR_TESTS);
  });

  it("fits within the cap on UTF-8 multi-byte over-cap input", () => {
    const huge = "🎯".repeat(__MAX_CONTENT_BYTES_FOR_TESTS);
    const out = truncateForTests(huge);
    expect(Buffer.byteLength(out, "utf8")).toBeLessThanOrEqual(__MAX_CONTENT_BYTES_FOR_TESTS);
  });
});
