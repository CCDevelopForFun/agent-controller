package main

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// Slice 6.5: pin the per-turn span emission contract.
//
// runChatTurn opens a `chat.turn` span as a child of whatever ctx
// the caller passes, with `chat.turn.index` and prompt-size
// attributes. The adapter dispatch uses the turn span's ctx so
// TRACEPARENT propagation (slice 5.2) parents adapter spans under
// the turn — not the chat-root.

func withChatTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
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

func TestRunChatTurnEmitsChatTurnSpanWithIndex(t *testing.T) {
	// Install an in-memory tracer so runChatTurn's span lands
	// somewhere we can inspect. The fake backend's events are
	// scripted to a clean session.ended so the turn span closes
	// with OK status.
	exporter := withChatTracerProvider(t)
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(),
				Data: []byte(`{"reason":"completed"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	if err := runChatTurn(context.Background(), cmd, be, &spec, "s_span", "hello, traced agent", nil, 42); err != nil {
		t.Fatalf("runChatTurn: %v", err)
	}

	spans := exporter.GetSpans()
	var turnSpan *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "chat.turn" {
			turnSpan = &spans[i]
			break
		}
	}
	if turnSpan == nil {
		t.Fatalf("expected a chat.turn span; got %d spans", len(spans))
	}

	// Attribute contract.
	attrs := map[string]any{}
	for _, kv := range turnSpan.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got := attrs["chat.turn.index"]; got != int64(42) {
		t.Errorf("chat.turn.index = %v, want 42", got)
	}
	if got := attrs["chat.turn.prompt.bytes"]; got != int64(len("hello, traced agent")) {
		t.Errorf("chat.turn.prompt.bytes = %v, want %d", got, len("hello, traced agent"))
	}
	if got := attrs["agent_controller.session.id"]; got != "s_span" {
		t.Errorf("agent_controller.session.id = %v, want s_span", got)
	}
}

func TestRunChatTurnTurnSpanMarkedErrorOnTurnFailure(t *testing.T) {
	// When the adapter emits session.ended { reason: "error" } the
	// turn span should be Status=ERROR so observability tools
	// surface failed turns. Mirrors the chat-root pattern for early
	// failures.
	exporter := withChatTracerProvider(t)
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(),
				Data: []byte(`{"reason":"error","message":"boom"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_err", "trigger", nil, 1)
	if err == nil {
		t.Fatalf("expected runChatTurn to return error on session.ended reason=error")
	}

	var turnSpan *tracetest.SpanStub
	spans := exporter.GetSpans()
	for i := range spans {
		if spans[i].Name == "chat.turn" {
			turnSpan = &spans[i]
			break
		}
	}
	if turnSpan == nil {
		t.Fatalf("chat.turn span missing")
	}
	if turnSpan.Status.Code != codes.Error {
		t.Errorf("turn span status = %v, want Error", turnSpan.Status.Code)
	}
}
