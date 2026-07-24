package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
)

// Slice 6.2: shared Store-interface conformance harness. Every Store
// impl (MemoryStore in slice 6.1, SQLiteStore in slice 6.2, future
// Postgres / Redis in v0.8) runs through this same battery so both
// implementations are pinned to identical behavior.
//
// Adding a new impl: write a small *_test.go that calls
//   func TestXxxStoreContract(t *testing.T) {
//     runStoreContract(t, func(t *testing.T) Store {
//       // open the impl; t.Cleanup the resources
//     })
//   }
//
// The factory receives `t` so it can register cleanup (db files,
// network handles, etc.). Each contract subtest gets its own factory
// invocation — impls don't have to scrub between subtests.

// storeFactory returns a fresh, empty Store for a single subtest. The
// returned store's lifetime is the subtest; the factory must call
// t.Cleanup to release any resources before the subtest exits.
type storeFactory func(t *testing.T) Store

func newTestSpec(name string) adl.CompiledSpec {
	return adl.CompiledSpec{
		V:        1,
		Metadata: adl.SpecMetadata{Name: name},
		Model:    adl.Model{Provider: "anthropic", Name: "claude-sonnet-4-6"},
		Task:     "test task",
		Runtime:  adl.RuntimeConfig{Type: "local"},
	}
}

func newTestSession(id, agentName string, status SessionStatus, lastActive time.Time) Session {
	return Session{
		ID:           id,
		AgentName:    agentName,
		RuntimeType:  "local",
		Status:       status,
		CreatedAt:    lastActive.Add(-time.Hour),
		LastActiveAt: lastActive,
		Spec:         newTestSpec(agentName),
	}
}

func runStoreContract(t *testing.T, mk storeFactory) {
	t.Helper()
	t.Run("CreateThenGetRoundtrip", func(t *testing.T) {
		store := mk(t)
		ctx := context.Background()
		s := newTestSession("s_001", "alice", StatusActive, time.Now().UTC())
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, "s_001")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != s.ID || got.AgentName != s.AgentName {
			t.Errorf("roundtrip mismatch: got %+v want %+v", got, s)
		}
		if got.Spec.Metadata.Name != "alice" {
			t.Errorf("spec snapshot lost agent name: got %q", got.Spec.Metadata.Name)
		}
	})

	t.Run("CreateRejectsDuplicateID", func(t *testing.T) {
		store := mk(t)
		ctx := context.Background()
		s := newTestSession("s_dup", "alice", StatusActive, time.Now().UTC())
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create #1: %v", err)
		}
		err := store.Create(ctx, s)
		if !errors.Is(err, ErrAlreadyExists) {
			t.Errorf("expected ErrAlreadyExists on duplicate Create, got %v", err)
		}
	})

	t.Run("CreateRejectsEmptyID", func(t *testing.T) {
		store := mk(t)
		err := store.Create(context.Background(), Session{})
		if err == nil {
			t.Error("expected error on Create with empty ID, got nil")
		}
	})

	t.Run("GetReturnsNotFoundForMissingID", func(t *testing.T) {
		store := mk(t)
		_, err := store.Get(context.Background(), "s_missing")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("UpdateMutatesOnlyMutableFields", func(t *testing.T) {
		// The Session doc-comment pins which fields are mutable on
		// Update. This is the SHARED enforcement test — both impls
		// must reject mutation of immutable fields identically.
		store := mk(t)
		ctx := context.Background()
		created := time.Now().UTC().Add(-2 * time.Hour)
		orig := newTestSession("s_upd", "alice", StatusActive, created)
		orig.CreatedAt = created
		orig.TraceContext = "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"
		if err := store.Create(ctx, orig); err != nil {
			t.Fatalf("Create: %v", err)
		}

		later := time.Now().UTC()
		mutated := Session{
			ID:           "s_upd",
			AgentName:    "bob-the-impostor",                                                  // ignored
			RuntimeType:  "local-opencode",                                                    // ignored
			Status:       StatusEnded,                                                         // applied
			CreatedAt:    time.Time{},                                                         // ignored
			LastActiveAt: later,                                                               // applied
			Spec:         newTestSpec("bob-the-impostor"),                                     // ignored
			TraceContext: "00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-01",          // ignored
			AdapterState: map[string]any{"pi.dir": "/new/path"},                               // applied
		}
		if err := store.Update(ctx, mutated); err != nil {
			t.Fatalf("Update: %v", err)
		}

		got, err := store.Get(ctx, "s_upd")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.AgentName != "alice" {
			t.Errorf("Update mutated AgentName: got %q", got.AgentName)
		}
		if got.RuntimeType != "local" {
			t.Errorf("Update mutated RuntimeType: got %q", got.RuntimeType)
		}
		if got.Status != StatusEnded {
			t.Errorf("Update did not apply Status: got %q", got.Status)
		}
		if !got.LastActiveAt.Equal(later) {
			t.Errorf("Update did not apply LastActiveAt: got %v want %v", got.LastActiveAt, later)
		}
		if !got.CreatedAt.Equal(created) {
			t.Errorf("Update mutated CreatedAt: got %v want %v", got.CreatedAt, created)
		}
		if got.Spec.Metadata.Name != "alice" {
			t.Errorf("Update mutated Spec: got %q", got.Spec.Metadata.Name)
		}
		if got.TraceContext != orig.TraceContext {
			t.Errorf("Update mutated TraceContext")
		}
		if got.AdapterState["pi.dir"] != "/new/path" {
			t.Errorf("Update did not apply AdapterState: got %+v", got.AdapterState)
		}
	})

	t.Run("UpdateReturnsNotFoundForMissing", func(t *testing.T) {
		store := mk(t)
		s := newTestSession("s_missing", "alice", StatusActive, time.Now().UTC())
		err := store.Update(context.Background(), s)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("DeleteIsIdempotent", func(t *testing.T) {
		store := mk(t)
		ctx := context.Background()
		s := newTestSession("s_del", "alice", StatusActive, time.Now().UTC())
		_ = store.Create(ctx, s)
		if err := store.Delete(ctx, "s_del"); err != nil {
			t.Fatalf("first Delete: %v", err)
		}
		if err := store.Delete(ctx, "s_del"); err != nil {
			t.Errorf("second Delete should be no-op, got %v", err)
		}
		if err := store.Delete(ctx, "never-existed"); err != nil {
			t.Errorf("delete of missing session should be no-op, got %v", err)
		}
	})

	t.Run("ListFiltersAndOrders", func(t *testing.T) {
		store := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		// Insert oldest first so a passing test proves List orders by
		// LastActiveAt, not insertion order.
		_ = store.Create(ctx, newTestSession("s_old", "alice", StatusActive, now.Add(-2*time.Hour)))
		_ = store.Create(ctx, newTestSession("s_mid", "alice", StatusEnded, now.Add(-1*time.Hour)))
		_ = store.Create(ctx, newTestSession("s_new", "bob", StatusActive, now))

		all, err := store.List(ctx, ListFilter{})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 3 {
			t.Fatalf("expected 3 sessions, got %d", len(all))
		}
		if all[0].ID != "s_new" || all[1].ID != "s_mid" || all[2].ID != "s_old" {
			t.Errorf("List order wrong: got %s, %s, %s", all[0].ID, all[1].ID, all[2].ID)
		}

		alice, _ := store.List(ctx, ListFilter{AgentName: "alice"})
		if len(alice) != 2 {
			t.Errorf("expected 2 alice sessions, got %d", len(alice))
		}

		active, _ := store.List(ctx, ListFilter{Status: StatusActive})
		if len(active) != 2 {
			t.Errorf("expected 2 active sessions, got %d", len(active))
		}

		aliceActive, _ := store.List(ctx, ListFilter{AgentName: "alice", Status: StatusActive})
		if len(aliceActive) != 1 || aliceActive[0].ID != "s_old" {
			t.Errorf("expected only s_old, got %+v", aliceActive)
		}

		limited, _ := store.List(ctx, ListFilter{Limit: 2})
		if len(limited) != 2 || limited[0].ID != "s_new" || limited[1].ID != "s_mid" {
			t.Errorf("Limit:2 wrong: got %+v", limited)
		}
	})

	t.Run("DeepCopiesNestedAdapterState", func(t *testing.T) {
		// Both impls must isolate nested AdapterState values. Memory
		// roundtrips via JSON in cloneMap; SQLite is automatically
		// isolated because writes go through the DB and reads
		// re-unmarshal.
		store := mk(t)
		ctx := context.Background()
		s := newTestSession("s_nested", "alice", StatusActive, time.Now().UTC())
		s.AdapterState = map[string]any{
			"pi.session.dir": "/orig/path",
			"pi.tools": map[string]any{
				"read": map[string]any{"calls": float64(3)},
			},
			"pi.queue": []any{"task1", "task2"},
		}
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
		s.AdapterState["pi.tools"].(map[string]any)["read"].(map[string]any)["calls"] = float64(999)
		s.AdapterState["pi.queue"].([]any)[0] = "tampered"

		got, _ := store.Get(ctx, "s_nested")
		gotRead := got.AdapterState["pi.tools"].(map[string]any)["read"].(map[string]any)
		if gotRead["calls"] != float64(3) {
			t.Errorf("Create leaked nested map mutation: got %v want 3", gotRead["calls"])
		}
		if got.AdapterState["pi.queue"].([]any)[0] != "task1" {
			t.Errorf("Create leaked nested slice mutation: got %v want \"task1\"",
				got.AdapterState["pi.queue"].([]any)[0])
		}

		// Mutate returned: must not leak back.
		gotRead["calls"] = float64(123456)
		got.AdapterState["pi.queue"].([]any)[0] = "tampered-via-get"
		again, _ := store.Get(ctx, "s_nested")
		if again.AdapterState["pi.tools"].(map[string]any)["read"].(map[string]any)["calls"] != float64(3) {
			t.Error("Get leaked nested map mutation")
		}
		if again.AdapterState["pi.queue"].([]any)[0] != "task1" {
			t.Error("Get leaked nested slice mutation")
		}
	})

	t.Run("DeepCopiesSpecReferenceFields", func(t *testing.T) {
		store := mk(t)
		ctx := context.Background()
		spec := newTestSpec("alice")
		spec.Tools = []adl.ResolvedRef{
			{Name: "read", Builtin: true, Config: map[string]any{"max_bytes": 1024}},
		}
		spec.MCPServers = []adl.MCPServer{
			{Name: "fs", Transport: "stdio", Command: "fs-server", Env: map[string]string{"FS_TOKEN": "orig"}},
		}
		s := Session{
			ID:           "s_isolate",
			AgentName:    "alice",
			RuntimeType:  "local",
			Status:       StatusActive,
			CreatedAt:    time.Now().UTC(),
			LastActiveAt: time.Now().UTC(),
			Spec:         spec,
		}
		if err := store.Create(ctx, s); err != nil {
			t.Fatalf("Create: %v", err)
		}
		s.Spec.Tools[0].Config["max_bytes"] = 4096
		s.Spec.MCPServers[0].Env["FS_TOKEN"] = "tampered-after-create"

		got, err := store.Get(ctx, "s_isolate")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		// JSON roundtrip turns int into float64 — that's fine, the
		// CompiledSpec.Config field is map[string]any and the wire
		// format already uses JSON numbers everywhere. Assert against
		// the float to match what callers actually see.
		if got.Spec.Tools[0].Config["max_bytes"] != float64(1024) {
			t.Errorf("Create leaked Tools[].Config mutation: got %v want 1024",
				got.Spec.Tools[0].Config["max_bytes"])
		}
		if got.Spec.MCPServers[0].Env["FS_TOKEN"] != "orig" {
			t.Errorf("Create leaked MCPServers[].Env mutation: got %q want %q",
				got.Spec.MCPServers[0].Env["FS_TOKEN"], "orig")
		}

		got.Spec.Tools[0].Config["max_bytes"] = 9999
		got.Spec.MCPServers[0].Env["FS_TOKEN"] = "tampered-after-get"
		again, _ := store.Get(ctx, "s_isolate")
		if again.Spec.Tools[0].Config["max_bytes"] != float64(1024) {
			t.Errorf("Get leaked Tools[].Config mutation: got %v",
				again.Spec.Tools[0].Config["max_bytes"])
		}
		if again.Spec.MCPServers[0].Env["FS_TOKEN"] != "orig" {
			t.Errorf("Get leaked MCPServers[].Env mutation: got %q",
				again.Spec.MCPServers[0].Env["FS_TOKEN"])
		}
	})

	t.Run("MarkExpiredTransitionsOnlyActiveBeforeCutoff", func(t *testing.T) {
		// Slice 6.4: MarkExpired is the bulk op the `sessions sweep`
		// command uses. Contract:
		//   - Only StatusActive sessions are touched (paused/ended
		//     stay where they are — the lifecycle history matters).
		//   - LastActiveAt strictly BEFORE cutoff is the cut.
		//   - Returns the IDs of transitioned sessions in stable
		//     (sorted) order for determinism.
		//   - Idempotent: a second sweep at the same cutoff is a
		//     no-op (the first sweep moved everything out of Active).
		store := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		_ = store.Create(ctx, newTestSession("a-stale", "x", StatusActive, now.Add(-2*time.Hour)))
		_ = store.Create(ctx, newTestSession("b-fresh", "x", StatusActive, now.Add(-1*time.Minute)))
		_ = store.Create(ctx, newTestSession("c-paused", "x", StatusPaused, now.Add(-3*time.Hour)))
		_ = store.Create(ctx, newTestSession("d-ended", "x", StatusEnded, now.Add(-3*time.Hour)))

		cutoff := now.Add(-30 * time.Minute)
		expired, err := store.MarkExpired(ctx, cutoff)
		if err != nil {
			t.Fatalf("MarkExpired: %v", err)
		}
		if len(expired) != 1 || expired[0] != "a-stale" {
			t.Errorf("expected [a-stale], got %v", expired)
		}
		// b-fresh stays Active (fresh).
		if got, _ := store.Get(ctx, "b-fresh"); got.Status != StatusActive {
			t.Errorf("b-fresh status = %q, want active", got.Status)
		}
		// c-paused stays Paused — even though its LastActiveAt is
		// past the cutoff, only Active sessions are swept.
		if got, _ := store.Get(ctx, "c-paused"); got.Status != StatusPaused {
			t.Errorf("c-paused status = %q, want paused", got.Status)
		}
		// d-ended stays Ended.
		if got, _ := store.Get(ctx, "d-ended"); got.Status != StatusEnded {
			t.Errorf("d-ended status = %q, want ended", got.Status)
		}
		// a-stale transitioned.
		if got, _ := store.Get(ctx, "a-stale"); got.Status != StatusExpired {
			t.Errorf("a-stale status = %q, want expired", got.Status)
		}

		// Second sweep at the same cutoff — no-op.
		again, err := store.MarkExpired(ctx, cutoff)
		if err != nil {
			t.Fatalf("second MarkExpired: %v", err)
		}
		if len(again) != 0 {
			t.Errorf("second sweep should be no-op, got %v", again)
		}
	})

	t.Run("UpdateRejectsTransitionOutOfExpired", func(t *testing.T) {
		// Slice 6.4 codex pass 2: Update must refuse to flip a
		// StatusExpired row to any other status. This is the
		// invariant that prevents `sessions sweep` from being undone
		// by a still-running chat process. Sweep is terminal; only
		// Expired→Expired (re-sweep, no-op) is allowed.
		store := mk(t)
		ctx := context.Background()
		now := time.Now().UTC()
		_ = store.Create(ctx, newTestSession("s_exp", "x", StatusActive, now.Add(-2*time.Hour)))

		if _, err := store.MarkExpired(ctx, now.Add(-time.Hour)); err != nil {
			t.Fatalf("MarkExpired: %v", err)
		}

		// Each non-Expired target status must be rejected with
		// ErrSessionExpired.
		for _, target := range []SessionStatus{
			StatusActive, StatusPaused, StatusEnded, StatusFailed,
		} {
			err := store.Update(ctx, Session{
				ID:           "s_exp",
				Status:       target,
				LastActiveAt: now,
			})
			if !errors.Is(err, ErrSessionExpired) {
				t.Errorf("Update to %q from Expired: expected ErrSessionExpired, got %v",
					target, err)
			}
		}

		got, err := store.Get(ctx, "s_exp")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != StatusExpired {
			t.Errorf("session was modified despite rejected Updates: %q", got.Status)
		}

		// Expired→Expired is idempotent and allowed.
		if err := store.Update(ctx, Session{
			ID:           "s_exp",
			Status:       StatusExpired,
			LastActiveAt: now,
		}); err != nil {
			t.Errorf("Update Expired→Expired should be allowed (idempotent), got %v", err)
		}
	})

	t.Run("HonorsContextCancellation", func(t *testing.T) {
		store := mk(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := store.Create(ctx, newTestSession("s_cancel", "alice", StatusActive, time.Now()))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Create should return Canceled, got %v", err)
		}
		_, err = store.Get(ctx, "any")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Get should return Canceled, got %v", err)
		}
	})
}
