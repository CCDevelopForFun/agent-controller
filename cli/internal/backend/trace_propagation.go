package backend

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// injectTraceparent extracts the active OTel trace context from ctx via
// the globally-configured propagator (W3C TraceContext + Baggage, set
// by observability.InitTracerProvider) and emits TRACEPARENT
// (+ TRACESTATE when present) through the provided env slice.
//
// Used by Backend.Submit implementations to thread the host-side trace
// context into the spawned adapter subprocess (LocalBackend) or Pod
// container env (KubernetesBackend). The adapter then initializes its
// own OTel SDK with that context as the parent, so per-tool/per-model
// spans nest under the host's `agentctl.run` span.
//
// Behavior contract:
//
//   - When the global propagator yields a fresh TRACEPARENT, any
//     pre-existing TRACEPARENT/TRACESTATE entries in env are stripped
//     before the fresh pair is appended. This prevents the codex pass
//     2 of slice 5.2 finding: a stale TRACESTATE inherited from a
//     wrapper script's env getting paired with a fresh TRACEPARENT
//     from the active span — vendor-tracestate corruption on the
//     receiving end.
//
//   - When the propagator yields nothing (tracing off — the SDK's
//     default no-op propagator), env is returned unchanged. That
//     keeps the helper transparent for callers with no OTel context
//     who may still want a parent-inherited TRACEPARENT to pass
//     through untouched.
func injectTraceparent(ctx context.Context, env []string) []string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	newTP := carrier["traceparent"]
	newTS := carrier["tracestate"]
	if newTP == "" {
		return env
	}

	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "TRACEPARENT=") || strings.HasPrefix(kv, "TRACESTATE=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TRACEPARENT="+newTP)
	if newTS != "" {
		out = append(out, "TRACESTATE="+newTS)
	}
	return out
}
