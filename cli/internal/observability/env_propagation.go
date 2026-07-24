package observability

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// ExtractTraceContextFromEnv reads TRACEPARENT (and TRACESTATE if set)
// from the process environment, runs them through the globally-configured
// TextMapPropagator, and returns a child context carrying the extracted
// parent. When TRACEPARENT is unset the input ctx is returned unchanged.
//
// Used by the agentctl entrypoint to pick up the trace context that
// KubernetesBackend.Submit injects into the Pod container env (slice
// 5.2). Without this call the in-Pod agentctl would start a fresh root
// span instead of nesting under the host `agentctl.run` span, defeating
// the host→pod→adapter trace stitching codex pass on slice 5.2 caught.
//
// Safe to call unconditionally:
//   - tracing disabled at this level → no-op propagator, ctx unchanged
//   - TRACEPARENT unset → ctx unchanged
//   - malformed TRACEPARENT → the W3C propagator silently ignores it
//     (no panic, no half-applied state)
func ExtractTraceContextFromEnv(ctx context.Context) context.Context {
	tp := os.Getenv("TRACEPARENT")
	if tp == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{"traceparent": tp}
	if ts := os.Getenv("TRACESTATE"); ts != "" {
		carrier["tracestate"] = ts
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
