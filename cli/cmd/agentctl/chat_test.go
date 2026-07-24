package main

// Slice 6.3 unit tests for `agentctl chat`. The TTY-driven REPL loop
// is exercised end-to-end in slice 6.6's acceptance test against a
// real adapter; the unit tests here pin the testable pieces:
//
//   - openChatStore picks the right backing impl per flag
//   - openOrResumeChatSession creates / resumes / rejects cross-agent
//     reuse with the documented semantics
//   - runChatTurn dispatches one turn and updates session state
//
// Backend interaction is faked rather than mocked end-to-end — we
// stand up a minimal `chatTestBackend` that satisfies backend.Backend
// and returns scripted events. Avoids the spawn-Pi-subprocess
// requirement and keeps the test fast.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

func chatTestSpec(name string) adl.CompiledSpec {
	return adl.CompiledSpec{
		V:        1,
		Metadata: adl.SpecMetadata{Name: name},
		Model:    adl.Model{Provider: "anthropic", Name: "claude-sonnet-4-6"},
		Task:     "initial task",
		Runtime:  adl.RuntimeConfig{Type: "local"},
	}
}

// ── openChatStore ──────────────────────────────────────────────────────────

func TestOpenChatStoreInMemoryReturnsMemoryStore(t *testing.T) {
	store, err := openChatStore(true)
	if err != nil {
		t.Fatalf("openChatStore(true): %v", err)
	}
	defer store.Close()
	// Sanity: can we create + roundtrip? No assertion on the concrete
	// type — that's an implementation detail. We assert behavior.
	ctx := context.Background()
	sess := sessions.Session{
		ID: "s_mem", AgentName: "a", RuntimeType: "local",
		Status: sessions.StatusActive,
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		Spec: chatTestSpec("a"),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestOpenChatStoreDefaultUsesSQLite(t *testing.T) {
	// SQLite store lives at DefaultSQLiteStorePath; the test must
	// not touch the operator's real $XDG_DATA_HOME. Point XDG at a
	// per-test temp dir.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, err := openChatStore(false)
	if err != nil {
		t.Fatalf("openChatStore(false): %v", err)
	}
	defer store.Close()
	// Behavior assertion: a roundtrip survives Close + reopen, proving
	// we got a file-backed store and not in-memory.
	ctx := context.Background()
	sess := sessions.Session{
		ID: "s_disk", AgentName: "a", RuntimeType: "local",
		Status: sessions.StatusActive,
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		Spec: chatTestSpec("a"),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store2, err := openChatStore(false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	if _, err := store2.Get(ctx, "s_disk"); err != nil {
		t.Errorf("session lost across reopen — SQLite store should persist: %v", err)
	}
}

// ── openOrResumeChatSession ────────────────────────────────────────────────

func TestOpenOrResumeChatSessionCreatesNewWhenIDIsEmpty(t *testing.T) {
	store := sessions.NewMemoryStore()
	defer store.Close()
	spec := chatTestSpec("alice")

	sess, _, err := openOrResumeChatSession(context.Background(), store, spec, "")
	if err != nil {
		t.Fatalf("openOrResumeChatSession: %v", err)
	}
	if sess.ID == "" {
		t.Errorf("expected generated session id, got empty")
	}
	if !strings.HasPrefix(sess.ID, "s_") {
		t.Errorf("expected session id to start with s_, got %q", sess.ID)
	}
	if sess.AgentName != "alice" {
		t.Errorf("agent name not preserved: got %q", sess.AgentName)
	}
	if sess.Status != sessions.StatusActive {
		t.Errorf("new session should be Active, got %q", sess.Status)
	}
}

func TestOpenOrResumeChatSessionResumesExisting(t *testing.T) {
	store := sessions.NewMemoryStore()
	defer store.Close()
	ctx := context.Background()
	spec := chatTestSpec("alice")

	// Seed an existing session.
	createdAt := time.Now().UTC().Add(-time.Hour)
	prior := sessions.Session{
		ID: "s_prior", AgentName: "alice", RuntimeType: "local",
		Status: sessions.StatusEnded, // mimics a previously-completed chat
		CreatedAt: createdAt, LastActiveAt: createdAt,
		Spec: spec,
	}
	if err := store.Create(ctx, prior); err != nil {
		t.Fatalf("seed Create: %v", err)
	}

	got, _, err := openOrResumeChatSession(ctx, store, spec, "s_prior")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.ID != "s_prior" {
		t.Errorf("expected resumed id s_prior, got %q", got.ID)
	}
	// Resume must refresh Status to Active and bump LastActiveAt.
	if got.Status != sessions.StatusActive {
		t.Errorf("resumed session should be Active, got %q", got.Status)
	}
	if !got.LastActiveAt.After(createdAt) {
		t.Errorf("LastActiveAt should have been bumped: got %v want > %v",
			got.LastActiveAt, createdAt)
	}
	// CreatedAt must NOT change on resume.
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt should not change on resume: got %v want %v",
			got.CreatedAt, createdAt)
	}
}

func TestOpenOrResumeChatSessionRejectsCrossAgentReuse(t *testing.T) {
	// The model sees the prior agent's persona across turns; swapping
	// agents under the same session id would put it in a confused
	// state. Refuse with a clear error.
	store := sessions.NewMemoryStore()
	defer store.Close()
	ctx := context.Background()

	// Seed a session belonging to agent "alice".
	prior := sessions.Session{
		ID: "s_cross", AgentName: "alice", RuntimeType: "local",
		Status: sessions.StatusActive,
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(),
		Spec: chatTestSpec("alice"),
	}
	_ = store.Create(ctx, prior)

	// Try to resume with a spec for agent "bob" — must reject.
	_, _, err := openOrResumeChatSession(ctx, store, chatTestSpec("bob"), "s_cross")
	if err == nil {
		t.Fatal("expected cross-agent reuse to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "alice") || !strings.Contains(err.Error(), "bob") {
		t.Errorf("error should mention both agent names; got %q", err.Error())
	}
}

func TestSpecsEquivalentDetectsDrift(t *testing.T) {
	// Codex pass 5 of slice 6.3: --resume must use the stored Spec
	// when the freshly-parsed YAML disagrees. specsEquivalent is
	// the discriminator used to print the [resume] drift warning.
	a := chatTestSpec("alice")
	b := chatTestSpec("alice")
	if !specsEquivalent(a, b) {
		t.Errorf("identical specs should be equivalent")
	}

	// Model change → drift.
	c := chatTestSpec("alice")
	c.Model.Name = "claude-opus-4-7"
	if specsEquivalent(a, c) {
		t.Errorf("differing model should not be equivalent")
	}

	// Tools change → drift.
	d := chatTestSpec("alice")
	d.Tools = []adl.ResolvedRef{{Name: "bash", Builtin: true}}
	if specsEquivalent(a, d) {
		t.Errorf("differing tools should not be equivalent")
	}

	// Persona change → drift.
	e := chatTestSpec("alice")
	e.Persona = &adl.Persona{Role: "researcher"}
	if specsEquivalent(a, e) {
		t.Errorf("added persona should not be equivalent")
	}
}

func TestOpenOrResumeChatSessionReturnsErrorWhenResumeIDNotFound(t *testing.T) {
	store := sessions.NewMemoryStore()
	defer store.Close()
	_, _, err := openOrResumeChatSession(context.Background(), store, chatTestSpec("a"), "s_missing")
	if err == nil {
		t.Fatal("expected error when --resume id is unknown, got nil")
	}
	if !strings.Contains(err.Error(), "s_missing") {
		t.Errorf("error should name the missing id; got %q", err.Error())
	}
}

// ── runChatTurn ────────────────────────────────────────────────────────────

// chatTestBackend is a backend.Backend that returns scripted events.
// The events channel closes after the script runs, mirroring how a
// real backend signals end-of-turn.
type chatTestBackend struct {
	scripted     []wire.Event
	lastSpecTask string
	lastSpecID   string
	resolveCalls int
	submitCalls  int
}

func (b *chatTestBackend) Capabilities() backend.Caps {
	return backend.Caps{SupportsStreaming: true, MaxConcurrency: 1}
}
func (b *chatTestBackend) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	b.resolveCalls++
	b.lastSpecTask = spec.Task
	if spec.SessionID != nil {
		b.lastSpecID = *spec.SessionID
	}
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}
func (b *chatTestBackend) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	b.submitCalls++
	return "h", nil
}
func (b *chatTestBackend) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event, len(b.scripted))
	for _, ev := range b.scripted {
		ch <- ev
	}
	close(ch)
	return ch
}
func (b *chatTestBackend) Stop(_ backend.SessionHandle) error { return nil }

func TestRunChatTurnInjectsTaskAndSessionID(t *testing.T) {
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionStarted, Ts: time.Now()},
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"completed"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_abc", "hello, agent", nil, 1)
	if err != nil {
		t.Fatalf("runChatTurn: %v", err)
	}
	if be.lastSpecTask != "hello, agent" {
		t.Errorf("Task not injected: got %q want %q", be.lastSpecTask, "hello, agent")
	}
	if be.lastSpecID != "s_abc" {
		t.Errorf("SessionID not injected: got %q want %q", be.lastSpecID, "s_abc")
	}
}

func TestRunChatTurnDoesNotMutateCallerSpec(t *testing.T) {
	// Critical: each turn modifies Task and SessionID on a LOCAL copy
	// of the spec. If the caller's spec drifts across turns, the
	// second turn would inherit the first turn's Task — making chat
	// behavior depend on prior turn state. The store snapshots the
	// original spec; the in-memory spec must stay original too.
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"completed"}`)},
		},
	}
	spec := chatTestSpec("alice")
	originalTask := spec.Task

	cmd := &cobra.Command{}
	_ = runChatTurn(context.Background(), cmd, be, &spec, "s_id", "user input 1", nil, 1)

	if spec.Task != originalTask {
		t.Errorf("caller Task mutated: got %q want %q", spec.Task, originalTask)
	}
	if spec.SessionID != nil {
		t.Errorf("caller SessionID mutated: got %v want nil", spec.SessionID)
	}
}

func TestRunChatTurnSurfacesErrorEventAsTurnError(t *testing.T) {
	// Adapter emitting an `error` wire event ends the turn with a
	// non-nil error. The REPL loop catches this and prints it but
	// stays alive — that's the contract this test pins.
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventError, Ts: time.Now(), Data: []byte(`{"message":"boom"}`)},
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"error","message":"boom"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", "trigger error", nil, 1)
	if err == nil {
		t.Fatal("expected runChatTurn to return non-nil error on EventError")
	}
}

func TestRunChatTurnTreatsTerminalEndedReasonErrorAsFailure(t *testing.T) {
	// Codex pass 1 of slice 6.3: an adapter can end a turn with
	// `session.ended { reason: "error" }` WITHOUT first emitting a
	// separate `error` wire event. The terminal reason alone must
	// be enough for chat to report the turn as failed.
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(),
				Data: []byte(`{"reason":"error","message":"runtime panic"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", "trigger ended-error", nil, 1)
	if err == nil {
		t.Fatal("expected runChatTurn to return non-nil error on session.ended reason=error")
	}
	if !strings.Contains(err.Error(), "runtime panic") {
		t.Errorf("turn error should carry the adapter message; got %q", err.Error())
	}
}

func TestRunChatTurnTreatsTerminalEndedReasonCancelledAsFailure(t *testing.T) {
	// User Ctrl-C during a turn surfaces as `reason: "cancelled"`.
	// Mirror the run-command convention: report it as a turn error
	// so the REPL prints something rather than silently looping.
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(),
				Data: []byte(`{"reason":"cancelled"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", "", nil, 1)
	if err == nil {
		t.Fatal("expected runChatTurn to return non-nil error on session.ended reason=cancelled")
	}
}

func TestRunChatTurnCompletesCleanlyOnSessionEnded(t *testing.T) {
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventMessage, Ts: time.Now(), Data: []byte(`{"role":"assistant","text":"hi"}`)},
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"completed"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	if err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", "hello", nil, 1); err != nil {
		t.Errorf("expected clean completion, got: %v", err)
	}
}

func TestRunChatTurnPreservesRawInputWhitespace(t *testing.T) {
	// Codex pass 1 of slice 6.3, P3: prompts with intentional leading
	// whitespace (code blocks, indented snippets) must reach the
	// adapter unmodified. TrimSpace would have stripped them.
	be := &chatTestBackend{
		scripted: []wire.Event{
			{V: 1, Type: wire.EventSessionEnded, Ts: time.Now(), Data: []byte(`{"reason":"completed"}`)},
		},
	}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	indented := "    def fib(n):\n        return n if n < 2 else fib(n-1)+fib(n-2)"
	if err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", indented, nil, 1); err != nil {
		t.Fatalf("runChatTurn: %v", err)
	}
	if be.lastSpecTask != indented {
		t.Errorf("Task lost whitespace: got %q want %q", be.lastSpecTask, indented)
	}
}

func TestRunChatTurnPropagatesSubmitFailure(t *testing.T) {
	// Use a backend whose Submit returns an error to verify the
	// failure path doesn't panic / hang.
	be := failingChatBackend{}
	spec := chatTestSpec("alice")
	cmd := &cobra.Command{}

	err := runChatTurn(context.Background(), cmd, be, &spec, "s_id", "hello", nil, 1)
	if err == nil {
		t.Fatal("expected Submit failure to surface as runChatTurn error")
	}
	if !strings.Contains(err.Error(), "submit") {
		t.Errorf("error should mention submit; got %q", err.Error())
	}
}

type failingChatBackend struct{}

func (failingChatBackend) Capabilities() backend.Caps { return backend.Caps{} }
func (failingChatBackend) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}
func (failingChatBackend) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	return "", errors.New("synthetic submit failure")
}
func (failingChatBackend) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event)
	close(ch)
	return ch
}
func (failingChatBackend) Stop(_ backend.SessionHandle) error { return nil }
