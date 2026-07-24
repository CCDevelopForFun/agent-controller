package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func withW3CPropagator(t *testing.T) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

func TestExtractTraceContextFromEnvNoOpWhenUnset(t *testing.T) {
	withW3CPropagator(t)
	t.Setenv("TRACEPARENT", "")
	t.Setenv("TRACESTATE", "")

	ctx := ExtractTraceContextFromEnv(context.Background())
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Errorf("expected no remote parent in ctx when TRACEPARENT is unset")
	}
}

func TestExtractTraceContextFromEnvRecoversParent(t *testing.T) {
	withW3CPropagator(t)
	// Synthesize a valid W3C TraceContext header (the canonical example
	// from https://www.w3.org/TR/trace-context/).
	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	t.Setenv("TRACEPARENT", traceparent)

	ctx := ExtractTraceContextFromEnv(context.Background())
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatalf("expected valid remote span context, got invalid")
	}
	if got := sc.TraceID().String(); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("traceID mismatch: got %s", got)
	}
	if got := sc.SpanID().String(); got != "b7ad6b7169203331" {
		t.Errorf("spanID mismatch: got %s", got)
	}
	if !sc.IsRemote() {
		t.Errorf("expected IsRemote()=true on extracted span context")
	}
}

func TestExtractTraceContextFromEnvIgnoresMalformedHeader(t *testing.T) {
	withW3CPropagator(t)
	t.Setenv("TRACEPARENT", "not-a-valid-traceparent-header")

	// The W3C propagator silently drops malformed input — verify we
	// don't panic and the returned ctx has no valid parent.
	ctx := ExtractTraceContextFromEnv(context.Background())
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Errorf("expected no valid parent for malformed TRACEPARENT")
	}
}
