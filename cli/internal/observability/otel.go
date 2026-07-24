// Package observability wires OpenTelemetry into agentctl.
//
// Slice 5.1 (v0.5.0 introduction):
//   - InitTracerProvider() sets up the global trace provider with an
//     OTLP/HTTP exporter. Endpoint + headers come from the standard
//     OTEL_EXPORTER_OTLP_* env vars so users don't learn a new config
//     surface; BrainTrust / MLflow / Langfuse / OTel Collector all use
//     the same vars.
//   - Tracer() returns the named tracer agentctl spans hang off of.
//   - StartRootSpan() opens a span at the root of agentctl's run path
//     and sets the GenAI semconv attributes (gen_ai.system,
//     gen_ai.request.model, gen_ai.operation.name).
//
// Later slices (5.2 onward) add wire-event TRACEPARENT propagation,
// adapter-side spans, and per-tool spans. This file keeps the host-side
// CLI plumbing in one place.
package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation-scope name attached to every span
// agentctl emits. Downstream observability platforms can filter on it.
const TracerName = "github.com/CCDevelopForFun/agent-controller/cli"

// InitTracerProvider sets up the global OTel tracer provider and
// returns a shutdown func the caller MUST defer to flush spans before
// the process exits. When OTel is disabled (no OTEL_EXPORTER_OTLP_*
// env vars set, or spec opt-in is off), this is a no-op that returns a
// no-op shutdown func — callers don't need to branch.
//
// Returns an error only when the OTLP exporter / TracerProvider fails
// to construct; missing env vars are NOT an error (they mean "don't
// trace this run").
func InitTracerProvider(ctx context.Context, serviceVersion string) (shutdown func(context.Context) error, err error) {
	if !isEnabled() {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName()),
			semconv.ServiceVersion(serviceVersion),
		),
		resource.WithFromEnv(),
		resource.WithHost(),
	)
	// resource.New returns ErrPartialResource (non-fatal) when some
	// attribute parsing fails — e.g. a malformed OTEL_RESOURCE_ATTRIBUTES
	// env value missing a `=` separator. The resource itself is still
	// usable; don't abort the run for imperfect telemetry metadata.
	// Codex pass 8 of slice 5.1.
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("build OTel resource: %w", err)
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("construct OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			// Tight batch settings keep p99 latency under control for
			// short agentctl runs. Long-running agents (v0.6+) inherit
			// the same defaults; a future slice can tune them.
			sdktrace.WithMaxExportBatchSize(64),
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		// Force-flush before shutdown so an interrupted run still
		// delivers spans it produced before SIGINT.
		var errs []error
		if err := tp.ForceFlush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("OTel flush: %w", err))
		}
		if err := tp.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("OTel shutdown: %w", err))
		}
		return errors.Join(errs...)
	}, nil
}

// Tracer returns the named tracer agentctl spans hang off of.
func Tracer() trace.Tracer {
	return otel.Tracer(TracerName)
}

// RunAttributes is the typed payload StartRootSpan annotates the root
// span with. Pulled out as a struct so callers don't have to remember
// which OTel attribute keys to use; we own the semconv mapping here.
//
// RuntimeType vs BackendType is the load-bearing distinction: a K8s
// binding sets RuntimeType=local|local-pi|local-opencode (what runs
// inside the Pod) but BackendType=kubernetes (where the host launched
// it). Operators filter on backend.type to separate cluster traces
// from local-dev traces. Codex pass 4 of slice 5.1 added the split.
type RunAttributes struct {
	AgentName     string
	ModelProvider string // e.g. "anthropic"
	ModelName     string // e.g. "claude-sonnet-4-20250514"
	RuntimeType   string // in-Pod / in-process adapter: "local", "local-opencode", …
	BackendType   string // Backend implementation: "local" (LocalBackend), "kubernetes" (KubernetesBackend)
	BindingName   string // empty when no --binding
	SessionID     string // empty when no --resume
}

// SetLateAttributes adds attributes to a span that weren't known at
// StartRootSpan time (binding name, session id, backend type). Safe to
// call after Start so the span lifecycle remains "start → enrich → end."
func SetLateAttributes(span trace.Span, attrs RunAttributes) {
	if attrs.BindingName != "" {
		span.SetAttributes(attribute.String("agent_controller.binding.name", attrs.BindingName))
	}
	if attrs.SessionID != "" {
		span.SetAttributes(attribute.String("agent_controller.session.id", attrs.SessionID))
	}
	if attrs.BackendType != "" {
		span.SetAttributes(attribute.String("agent_controller.backend.type", attrs.BackendType))
	}
}

// StartRootSpan opens the agentctl.run root span and sets the GenAI
// semconv attributes. The returned context carries the active span;
// the End func MUST be called when the run finishes (deferred is fine).
// Callers can enrich the span with binding/session/backend attrs later
// via SetLateAttributes once those values are known.
func StartRootSpan(ctx context.Context, attrs RunAttributes) (context.Context, trace.Span) {
	spanAttrs := []attribute.KeyValue{
		// agent-controller specific attributes — namespaced so they
		// don't collide with the OTel GenAI semconv as it stabilizes.
		attribute.String("agent_controller.agent.name", attrs.AgentName),
		attribute.String("agent_controller.runtime.type", attrs.RuntimeType),
	}
	if attrs.BindingName != "" {
		spanAttrs = append(spanAttrs, attribute.String("agent_controller.binding.name", attrs.BindingName))
	}
	if attrs.SessionID != "" {
		spanAttrs = append(spanAttrs, attribute.String("agent_controller.session.id", attrs.SessionID))
	}
	if attrs.BackendType != "" {
		spanAttrs = append(spanAttrs, attribute.String("agent_controller.backend.type", attrs.BackendType))
	}
	// OTel GenAI semconv. Keys are stable in semconv/v1.27.0 onward;
	// future semconv churn shows up here so the rest of the codebase
	// doesn't have to track it.
	if attrs.ModelProvider != "" {
		spanAttrs = append(spanAttrs, semconv.GenAISystemKey.String(attrs.ModelProvider))
	}
	if attrs.ModelName != "" {
		spanAttrs = append(spanAttrs, semconv.GenAIRequestModelKey.String(attrs.ModelName))
	}
	spanAttrs = append(spanAttrs, semconv.GenAIOperationNameKey.String("agent.run"))

	return Tracer().Start(ctx, "agentctl.run",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttrs...),
	)
}

// isEnabled reports whether OTel should be initialized for this run.
// We treat presence of OTEL_EXPORTER_OTLP_ENDPOINT (or the more
// specific OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) as the signal — same
// convention every OTel SDK uses. Spec-level opt-in
// (`spec.observability.tracing: true`) is enforced by the caller
// before they call InitTracerProvider.
func isEnabled() bool {
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		return true
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); v != "" {
		return true
	}
	return false
}

func serviceName() string {
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		return v
	}
	return "agentctl"
}
