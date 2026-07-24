package serve

// Slice 8.6: pin the per-turn root-span emission contract for RunTurn.
//
// Each call to RunTurn must open exactly one agentctl.run root span tagged
// with the session id. The adapter's own child spans nest under it via the
// TRACEPARENT env-var injection already wired in slice 5.2.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// withServeTracerProvider installs an in-memory OTel provider and restores the
// previous global provider on cleanup. Mirrors the pattern in chat_span_test.go.
func withServeTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return exporter
}

func TestRunTurn_EmitsOneRootSpanPerTurn(t *testing.T) {
	exporter := withServeTracerProvider(t)

	endedData, _ := json.Marshal(map[string]string{"reason": "completed"})
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		},
	}
	m := newTestManagerWithBackend(t, fb)

	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.RunTurn(context.Background(), sess.ID, "hello", func(wire.Event) error { return nil }); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	spans := exporter.GetSpans()

	// Exactly one agentctl.run span must be recorded.
	var rootSpans []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "agentctl.run" {
			rootSpans = append(rootSpans, s)
		}
	}
	if len(rootSpans) != 1 {
		t.Fatalf("got %d agentctl.run spans, want 1 (all spans: %d)", len(rootSpans), len(spans))
	}

	// The span must carry the session id attribute.
	attrs := map[string]any{}
	for _, kv := range rootSpans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got := attrs["agent_controller.session.id"]; got != sess.ID {
		t.Errorf("agent_controller.session.id = %v, want %q", got, sess.ID)
	}
}

func TestRunTurn_SecondTurnEmitsSecondRootSpan(t *testing.T) {
	// Two sequential turns must produce two distinct agentctl.run spans,
	// each scoped to exactly one turn — no span left unclosed between turns.
	exporter := withServeTracerProvider(t)

	endedData, _ := json.Marshal(map[string]string{"reason": "completed"})
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		},
	}
	m := newTestManagerWithBackend(t, fb)

	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		fb.events = []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		}
		if err := m.RunTurn(context.Background(), sess.ID, "turn", func(wire.Event) error { return nil }); err != nil {
			t.Fatalf("RunTurn[%d]: %v", i, err)
		}
	}

	var count int
	for _, s := range exporter.GetSpans() {
		if s.Name == "agentctl.run" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("got %d agentctl.run spans for 2 turns, want 2", count)
	}
}
