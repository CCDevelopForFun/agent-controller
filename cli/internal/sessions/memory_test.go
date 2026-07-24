package sessions

import (
	"context"
	"testing"
	"time"
)

// MemoryStore conformance tests. The shared Store contract lives in
// contract_test.go; this file holds only memory-specific assertions
// (Close drops data + post-close error semantics, copy-isolation at
// the top-level map layer that doesn't apply to SQLite where the DB
// inherently isolates rows).

func TestMemoryStoreContract(t *testing.T) {
	runStoreContract(t, func(t *testing.T) Store {
		s := NewMemoryStore()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestMemoryStoreCloseDropsAllData(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	_ = store.Create(ctx, newTestSession("s_x", "alice", StatusActive, time.Now()))

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("second Close should be no-op, got %v", err)
	}
	if _, err := store.Get(ctx, "s_x"); err == nil {
		t.Error("Get on closed store should error")
	}
	if err := store.Create(ctx, newTestSession("s_y", "alice", StatusActive, time.Now())); err == nil {
		t.Error("Create on closed store should error")
	}
}

func TestMemoryStoreReturnsCopiesNotInteriorPointers(t *testing.T) {
	// Top-level AdapterState map isolation. The shared contract
	// (DeepCopiesNestedAdapterState) covers nested isolation across
	// both impls; this test pins the surface-level map clone that's
	// specific to the MemoryStore in-process path.
	store := NewMemoryStore()
	defer store.Close()
	ctx := context.Background()

	s := newTestSession("s_iso", "alice", StatusActive, time.Now())
	s.AdapterState = map[string]any{"pi.dir": "/orig/path"}
	_ = store.Create(ctx, s)

	s.AdapterState["pi.dir"] = "/should-not-leak"
	got, _ := store.Get(ctx, "s_iso")
	if got.AdapterState["pi.dir"] != "/orig/path" {
		t.Errorf("Create did not isolate AdapterState; got %q", got.AdapterState["pi.dir"])
	}

	got.AdapterState["pi.dir"] = "/also-should-not-leak"
	again, _ := store.Get(ctx, "s_iso")
	if again.AdapterState["pi.dir"] != "/orig/path" {
		t.Errorf("Get did not isolate AdapterState; got %q", again.AdapterState["pi.dir"])
	}
}
