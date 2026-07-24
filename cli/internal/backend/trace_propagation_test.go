package backend

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// withTestPropagator installs the W3C TraceContext propagator + a
// recording tracer provider just for this test, and restores the
// previous globals via t.Cleanup. The restore matters because one of
// the tests below temporarily sets the propagator to an empty
// composite ("tracing off"); without cleanup that leaks to whichever
// test runs after it, including under `go test -shuffle=on`.
func withTestPropagator(t *testing.T) {
	t.Helper()
	prevProp := otel.GetTextMapPropagator()
	prevTP := otel.GetTracerProvider()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevProp)
		otel.SetTracerProvider(prevTP)
	})
}

func TestInjectTraceparentNoOpWhenNoActiveSpan(t *testing.T) {
	// No active span in the context → propagator writes nothing.
	withTestPropagator(t)
	env := injectTraceparent(context.Background(), []string{"FOO=bar"})
	if len(env) != 1 || env[0] != "FOO=bar" {
		t.Errorf("expected env unchanged when no active span; got %v", env)
	}
}

func TestInjectTraceparentEmitsTraceparentForActiveSpan(t *testing.T) {
	withTestPropagator(t)
	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-root")
	defer span.End()

	env := injectTraceparent(ctx, []string{"FOO=bar"})
	var seenTraceparent bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "TRACEPARENT=") {
			seenTraceparent = true
			// W3C TraceContext format: `00-<32-hex-trace-id>-<16-hex-span-id>-<2-hex-flags>`
			if !strings.HasPrefix(kv, "TRACEPARENT=00-") {
				t.Errorf("TRACEPARENT not in W3C format: %s", kv)
			}
		}
	}
	if !seenTraceparent {
		t.Errorf("expected TRACEPARENT in env, got %v", env)
	}
	// Original env entries must still be there.
	if env[0] != "FOO=bar" {
		t.Errorf("original env entry was clobbered: %v", env)
	}
}

func TestInjectTraceparentStripsStaleTraceEnvWhenEmitting(t *testing.T) {
	// Codex pass 2 of slice 5.2: when the active span has its own
	// TRACEPARENT but the caller's env still carries a stale
	// TRACEPARENT/TRACESTATE pair from a parent shell, the child must
	// not see a fresh TRACEPARENT paired with a vendor TRACESTATE that
	// belongs to a different trace.
	withTestPropagator(t)
	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-root")
	defer span.End()

	stale := []string{
		"FOO=bar",
		"TRACEPARENT=00-00000000000000000000000000000001-0000000000000002-01",
		"TRACESTATE=vendor=stale",
		"PATH=/usr/bin",
	}
	env := injectTraceparent(ctx, stale)

	var traceparentCount, tracestateCount int
	for _, kv := range env {
		if strings.HasPrefix(kv, "TRACEPARENT=") {
			traceparentCount++
			if strings.Contains(kv, "00000000000000000000000000000001") {
				t.Errorf("stale TRACEPARENT leaked through: %s", kv)
			}
		}
		if strings.HasPrefix(kv, "TRACESTATE=") {
			tracestateCount++
			if strings.Contains(kv, "vendor=stale") {
				t.Errorf("stale TRACESTATE leaked through: %s", kv)
			}
		}
	}
	if traceparentCount != 1 {
		t.Errorf("expected exactly one TRACEPARENT, got %d: %v", traceparentCount, env)
	}
	// Active span has no tracestate → no TRACESTATE in output. Critical:
	// the stale one must be gone too, not just shadowed.
	if tracestateCount != 0 {
		t.Errorf("expected zero TRACESTATE entries (active span has none), got %d: %v", tracestateCount, env)
	}
	// Non-trace env must survive untouched.
	hasFoo, hasPath := false, false
	for _, kv := range env {
		if kv == "FOO=bar" {
			hasFoo = true
		}
		if kv == "PATH=/usr/bin" {
			hasPath = true
		}
	}
	if !hasFoo || !hasPath {
		t.Errorf("non-trace env entries were dropped: %v", env)
	}
}

func TestInjectTraceparentLeavesEnvAloneWhenTracingOff(t *testing.T) {
	// When the propagator yields nothing, we MUST NOT clobber the
	// caller's env — they may rely on inherited TRACEPARENT for some
	// other propagation scheme outside agentctl's control.
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) // empty composite = effectively no-op
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	stale := []string{
		"FOO=bar",
		"TRACEPARENT=00-00000000000000000000000000000001-0000000000000002-01",
	}
	env := injectTraceparent(context.Background(), stale)
	if len(env) != len(stale) {
		t.Errorf("expected env unchanged when propagator emits nothing, got %v", env)
	}
}

func TestInjectTraceparentSafeOnNilEnv(t *testing.T) {
	// Used by KubernetesBackend which passes nil to extract a fresh slice.
	withTestPropagator(t)
	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "test-root")
	defer span.End()

	env := injectTraceparent(ctx, nil)
	if len(env) == 0 {
		t.Errorf("expected at least TRACEPARENT in env, got empty")
	}
}
