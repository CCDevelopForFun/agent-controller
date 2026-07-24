package observability

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestInitTracerProviderNoOpWhenEnvUnset(t *testing.T) {
	// Defensive: clear both env vars so the test doesn't depend on the
	// developer's shell.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")

	shutdown, err := InitTracerProvider(context.Background(), "test")
	if err != nil {
		t.Fatalf("InitTracerProvider should not error in no-op mode: %v", err)
	}
	if shutdown == nil {
		t.Fatalf("shutdown func should never be nil")
	}
	// Shutdown should also be a no-op (no exporter, nothing to flush).
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown should not error: %v", err)
	}
}

func TestIsEnabledRespectsEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if isEnabled() {
		t.Errorf("isEnabled should be false when both env vars are empty")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	if !isEnabled() {
		t.Errorf("isEnabled should be true when OTEL_EXPORTER_OTLP_ENDPOINT is set")
	}

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://localhost:4318/v1/traces")
	if !isEnabled() {
		t.Errorf("isEnabled should be true when OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set")
	}
}

// withInMemoryExporter swaps the global provider for an in-memory one
// so assertions can inspect emitted spans without an OTLP collector.
func withInMemoryExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp
}

func TestStartRootSpanSetsGenAIAttributes(t *testing.T) {
	exp := withInMemoryExporter(t)

	ctx := context.Background()
	_, span := StartRootSpan(ctx, RunAttributes{
		AgentName:     "test-agent",
		ModelProvider: "anthropic",
		ModelName:     "claude-sonnet-4-20250514",
		RuntimeType:   "local",
		BindingName:   "test-binding",
		SessionID:     "ses_abc",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name != "agentctl.run" {
		t.Errorf("span name: got %q", s.Name)
	}

	attrs := map[string]string{}
	for _, a := range s.Attributes {
		attrs[string(a.Key)] = a.Value.Emit()
	}

	wantAttrs := map[string]string{
		"agent_controller.agent.name":   "test-agent",
		"agent_controller.runtime.type": "local",
		"agent_controller.binding.name": "test-binding",
		"agent_controller.session.id":   "ses_abc",
		"gen_ai.system":                 "anthropic",
		"gen_ai.request.model":          "claude-sonnet-4-20250514",
		"gen_ai.operation.name":         "agent.run",
	}
	for k, want := range wantAttrs {
		got, ok := attrs[k]
		if !ok {
			t.Errorf("missing attribute %q", k)
			continue
		}
		if got != want {
			t.Errorf("attribute %q: got %q, want %q", k, got, want)
		}
	}
}

func TestStartRootSpanOmitsEmptyOptionals(t *testing.T) {
	exp := withInMemoryExporter(t)

	ctx := context.Background()
	_, span := StartRootSpan(ctx, RunAttributes{
		AgentName:   "minimal",
		RuntimeType: "local",
	})
	span.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes {
		k := string(a.Key)
		if k == "agent_controller.binding.name" || k == "agent_controller.session.id" ||
			k == "gen_ai.system" || k == "gen_ai.request.model" {
			if strings.TrimSpace(a.Value.Emit()) != "" {
				t.Errorf("expected attribute %q to be omitted when source field is empty, got %q", k, a.Value.Emit())
			}
		}
	}
}
