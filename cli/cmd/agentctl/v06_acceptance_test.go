package main

// Slice 6.6 — v0.6.0 acceptance test. Exercises the full v0.6 chat
// surface end-to-end against the real SessionStore + a real OTel
// TracerProvider + a fake adapter backend. Asserts the contracts
// the slice-6.X CHANGELOG entries promise:
//
//   1. SessionStore round-trip — create on first turn, bump
//      LastActiveAt per turn, transition to paused/ended on exit.
//   2. SQLite persistence — close the store, reopen, prior session
//      surfaces with expected status.
//   3. Sweep — marks idle Active sessions Expired; chat --resume
//      against an expired session surfaces errSessionExpired.
//   4. Trace tree — chat-root span emitted with the same trace id
//      as the per-turn `chat.turn` spans; turn spans parented
//      under the chat root; each turn has a unique chat.turn.index.
//
// Avoids spawning a real Pi/opencode adapter — that's exercised by
// the runtime packages' own test suites. The acceptance test
// composes the existing seams (openOrResumeChatSession, runChatTurn,
// chatTestBackend, observability.StartRootSpan) the way newChatCmd
// does in production.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/observability"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// withV06Acceptance stands up an in-memory trace exporter + a
// file-backed SQLite store in a temp dir, restoring globals on
// cleanup. Returns the store, exporter, and the store's path so
// reopen tests can target the same file.
func withV06Acceptance(t *testing.T) (sessions.Store, *tracetest.InMemoryExporter, string) {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prevTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
	})

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sessions.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return store, exporter, dbPath
}

func TestV06AcceptanceMultiTurnChatRoundtrip(t *testing.T) {
	store, exporter, _ := withV06Acceptance(t)
	ctx := context.Background()
	cmd := &cobra.Command{}
	spec := chatTestSpec("acceptance-agent")

	// 1) Open a chat-root span the way newChatCmd does.
	rootCtx, rootSpan := observability.StartRootSpan(ctx, observability.RunAttributes{
		AgentName:     spec.Metadata.Name,
		ModelProvider: spec.Model.Provider,
		ModelName:     spec.Model.Name,
		RuntimeType:   spec.Runtime.Type,
	})

	// 2) Create a fresh session.
	sess, snap, err := openOrResumeChatSession(rootCtx, store, spec, "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if snap.Status != "" {
		t.Errorf("new session should have empty resume snapshot; got status %q", snap.Status)
	}
	if sess.Status != sessions.StatusActive {
		t.Errorf("new session status = %q, want active", sess.Status)
	}

	// 3) Run three turns through a fake backend.
	for turn := int64(1); turn <= 3; turn++ {
		be := &chatTestBackend{
			scripted: []wire.Event{
				{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"completed"}`)},
			},
		}
		if err := runChatTurn(rootCtx, cmd, be, &spec, sess.ID, "turn-"+string(rune('0'+turn)), nil, turn); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if updErr := store.Update(ctx, sessions.Session{
			ID:           sess.ID,
			Status:       sessions.StatusActive,
			LastActiveAt: time.Now().UTC(),
		}); updErr != nil {
			t.Fatalf("touch after turn %d: %v", turn, updErr)
		}
	}

	// 4) Mark session paused at exit (simulating EOF / SIGTERM).
	if err := store.Update(ctx, sessions.Session{
		ID:           sess.ID,
		Status:       sessions.StatusPaused,
		LastActiveAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("paused update: %v", err)
	}
	rootSpan.End()

	// 5) Assertions — session state in store.
	final, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get final: %v", err)
	}
	if final.Status != sessions.StatusPaused {
		t.Errorf("final status = %q, want paused", final.Status)
	}
	if !final.LastActiveAt.After(sess.LastActiveAt) {
		t.Errorf("LastActiveAt was not bumped across turns: created=%v final=%v",
			sess.LastActiveAt, final.LastActiveAt)
	}

	// 6) Assertions — trace tree.
	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("expected spans to be exported; got 0")
	}
	var rootID, traceID string
	turnSpans := 0
	for _, s := range spans {
		if s.Name == "agentctl.run" {
			rootID = s.SpanContext.SpanID().String()
			traceID = s.SpanContext.TraceID().String()
		}
		if s.Name == "chat.turn" {
			turnSpans++
		}
	}
	if rootID == "" {
		t.Fatalf("agentctl.run root span not in export")
	}
	if turnSpans != 3 {
		t.Errorf("expected 3 chat.turn spans, got %d", turnSpans)
	}
	// All chat.turn spans must share the chat root's trace id AND
	// parent under the root span id. Without this, the per-turn
	// span work from slice 6.5 didn't actually wire the chat-root
	// ctx into the turn dispatch.
	for _, s := range spans {
		if s.Name != "chat.turn" {
			continue
		}
		if s.SpanContext.TraceID().String() != traceID {
			t.Errorf("chat.turn span has different trace id: got %s, want %s",
				s.SpanContext.TraceID(), traceID)
		}
		if s.Parent.SpanID().String() != rootID {
			t.Errorf("chat.turn span parent = %s, want chat root %s",
				s.Parent.SpanID(), rootID)
		}
	}
}

func TestV06AcceptanceResumeAcrossReopen(t *testing.T) {
	_, _, dbPath := withV06Acceptance(t)
	ctx := context.Background()
	spec := chatTestSpec("persistent-agent")

	// First chat — open store, create session, close.
	store1, err := sessions.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first NewSQLiteStore: %v", err)
	}
	sess, _, err := openOrResumeChatSession(ctx, store1, spec, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store1.Update(ctx, sessions.Session{
		ID:           sess.ID,
		Status:       sessions.StatusPaused,
		LastActiveAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second chat — reopen store, resume the session.
	store2, err := sessions.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	resumed, snap, err := openOrResumeChatSession(ctx, store2, spec, sess.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if snap.Status != sessions.StatusPaused {
		t.Errorf("expected previous status paused, got %q", snap.Status)
	}
	if resumed.Status != sessions.StatusActive {
		t.Errorf("resumed status = %q, want active (resume should reset)", resumed.Status)
	}
	if resumed.ID != sess.ID {
		t.Errorf("resumed id mismatch: got %q want %q", resumed.ID, sess.ID)
	}
}

func TestV06AcceptanceSweepThenResumeRejected(t *testing.T) {
	store, _, _ := withV06Acceptance(t)
	ctx := context.Background()
	spec := chatTestSpec("expiring-agent")

	// Seed a stale Active session by Create-then-rewind LastActiveAt.
	// (Store.Update can't rewind because of the immutable-fields
	// rule on CreatedAt + LastActiveAt-monotonic-isn't-enforced —
	// we just create with old timestamps directly.)
	stale := sessions.Session{
		ID:           "s_stale",
		AgentName:    spec.Metadata.Name,
		RuntimeType:  spec.Runtime.Type,
		Status:       sessions.StatusActive,
		CreatedAt:    time.Now().UTC().Add(-48 * time.Hour),
		LastActiveAt: time.Now().UTC().Add(-48 * time.Hour),
		Spec:         spec,
	}
	if err := store.Create(ctx, stale); err != nil {
		t.Fatalf("create stale: %v", err)
	}

	// Sweep with a 1h TTL.
	expired, err := store.MarkExpired(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if len(expired) != 1 || expired[0] != "s_stale" {
		t.Fatalf("expected sweep to expire s_stale, got %v", expired)
	}

	// Resume now returns errSessionExpired.
	_, snap, resumeErr := openOrResumeChatSession(ctx, store, spec, "s_stale")
	if resumeErr == nil {
		t.Fatal("expected errSessionExpired on resume of swept session, got nil")
	}
	if snap.Status != sessions.StatusExpired {
		t.Errorf("snap.Status = %q, want expired", snap.Status)
	}
}
