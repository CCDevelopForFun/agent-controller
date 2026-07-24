package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
)

// MemoryStore is the in-memory Store implementation. Suitable for
// tests and ephemeral REPL sessions where persistence across process
// exit is explicitly not wanted. Sessions die with the process; the
// SQLite store (slice 6.2) is the default for actual persistence.
//
// Concurrency: a sync.RWMutex guards the map. Reads (Get, List) take
// the read lock; writes (Create, Update, Delete) take the write lock.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	closed   bool
}

// NewMemoryStore returns an empty in-memory store ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: map[string]Session{}}
}

func (m *MemoryStore) Create(ctx context.Context, s Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ID == "" {
		return fmt.Errorf("MemoryStore.Create: session ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("MemoryStore.Create: store is closed")
	}
	if _, ok := m.sessions[s.ID]; ok {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, s.ID)
	}
	// Defensive copy of AdapterState so the caller can't mutate the
	// stored map after Create returns. Other fields are values; only
	// the map needs cloning.
	m.sessions[s.ID] = copySession(s)
	return nil
}

func (m *MemoryStore) Get(ctx context.Context, id string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Session{}, fmt.Errorf("MemoryStore.Get: store is closed")
	}
	s, ok := m.sessions[id]
	if !ok {
		return Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	// Return a copy so the caller can't mutate the stored entry.
	return copySession(s), nil
}

func (m *MemoryStore) Update(ctx context.Context, s Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ID == "" {
		return fmt.Errorf("MemoryStore.Update: session ID is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("MemoryStore.Update: store is closed")
	}
	existing, ok := m.sessions[s.ID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, s.ID)
	}
	// Slice 6.4 codex pass 2: terminal-status guard. Once a sweep
	// has transitioned the row to StatusExpired, no caller can
	// flip it back to Active/Paused/Ended/Failed. Same Expired→Expired
	// is allowed (idempotent re-sweep). This guard runs UNDER the
	// write lock so the read of existing.Status and the conditional
	// commit are atomic with respect to concurrent MarkExpired.
	if existing.Status == StatusExpired && s.Status != StatusExpired {
		return fmt.Errorf("%w: %s", ErrSessionExpired, s.ID)
	}
	// Apply ONLY the mutable fields. The immutable-on-update contract
	// (see Session doc-comment in store.go) is enforced here so an
	// SQLite or Postgres impl doesn't have to duplicate the rule.
	existing.Status = s.Status
	existing.LastActiveAt = s.LastActiveAt
	existing.AdapterState = cloneMap(s.AdapterState)
	m.sessions[s.ID] = existing
	return nil
}

func (m *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("MemoryStore.Delete: store is closed")
	}
	// Idempotent: deleting a missing session is not an error.
	delete(m.sessions, id)
	return nil
}

func (m *MemoryStore) List(ctx context.Context, filter ListFilter) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("MemoryStore.List: store is closed")
	}
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if filter.AgentName != "" && s.AgentName != filter.AgentName {
			continue
		}
		if filter.Status != "" && s.Status != filter.Status {
			continue
		}
		out = append(out, copySession(s))
	}
	// Order: most-recently-active first. Ties broken by ID for
	// determinism (tests + UI both prefer stable ordering when
	// LastActiveAt collides — e.g. tests that create sessions in a
	// tight loop).
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActiveAt.Equal(out[j].LastActiveAt) {
			return out[i].LastActiveAt.After(out[j].LastActiveAt)
		}
		return out[i].ID < out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (m *MemoryStore) MarkExpired(ctx context.Context, cutoff time.Time) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("MemoryStore.MarkExpired: store is closed")
	}
	var expired []string
	for id, s := range m.sessions {
		if s.Status == StatusActive && s.LastActiveAt.Before(cutoff) {
			s.Status = StatusExpired
			m.sessions[id] = s
			expired = append(expired, id)
		}
	}
	// Sort for determinism — callers (sweep subcommand, tests)
	// expect a stable order even though map iteration is random.
	sort.Strings(expired)
	return expired, nil
}

func (m *MemoryStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Idempotent.
	m.closed = true
	m.sessions = nil
	return nil
}

// copySession returns a deep copy of s so the caller can't mutate the
// stored session's contents through the returned value (and vice versa
// on Create). Two reference-typed surfaces matter:
//
//   - AdapterState — a string→any map; cloned shallow (values are
//     stored as-is; the convention is they're JSON-encodable scalars).
//
//   - Spec — a CompiledSpec with nested slices (Tools, MCPServers,
//     Skills, Subagents, Extensions) and maps (Tools[].Config,
//     MCPServers[].Env / .Headers). Deep-copied via JSON roundtrip.
//     The spec is already designed to be JSON-serializable as the
//     wire-format payload to runtime adapters, so the roundtrip is
//     guaranteed lossless and matches the existing serialization
//     contract. Codex pass 1 of slice 6.1 caught the aliasing.
//
// Cost: one JSON marshal + unmarshal per Create / Get / List entry.
// Acceptable — sessions are created at chat start and read at resume,
// neither is a hot path. If the future Postgres store wants to skip
// the in-memory roundtrip on Get (because the row was deserialized
// from storage anyway, so it's already isolated), it can.
func copySession(s Session) Session {
	s.AdapterState = cloneMap(s.AdapterState)
	s.Spec = deepCopySpec(s.Spec)
	return s
}

func deepCopySpec(spec adl.CompiledSpec) adl.CompiledSpec {
	// JSON roundtrip — see copySession doc-comment for rationale.
	// Marshal failure here means the spec contains a non-encodable
	// value (channel, func, etc.) that should never have entered the
	// store. Panic rather than silently aliasing — a corrupt-store
	// invariant violation is a programmer bug, not a runtime
	// condition to recover from.
	encoded, err := json.Marshal(spec)
	if err != nil {
		panic(fmt.Sprintf("sessions: failed to marshal CompiledSpec for deep copy: %v", err))
	}
	var out adl.CompiledSpec
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(fmt.Sprintf("sessions: failed to unmarshal CompiledSpec for deep copy: %v", err))
	}
	return out
}

// cloneMap returns a deep copy of m via JSON roundtrip — nested maps
// and slices are also copied, not aliased. Adapter state is documented
// as an opaque JSON-encodable map (see Session.AdapterState in
// store.go), so the roundtrip matches both the contract and how
// SQLite (slice 6.2) will serialize/deserialize the column.
//
// Note: JSON roundtrip transforms `int` into `float64` in a
// `map[string]any`. Callers writing typed values into AdapterState
// (e.g. `1024`) read them back as `float64(1024)`. This matches the
// same tradeoff `CompiledSpec.Tools[].Config` already imposes — the
// wire format is JSON throughout, so callers already operate in
// "JSON-typed" terms.
//
// Codex pass 2 of slice 6.1 caught that the original shallow clone
// left nested AdapterState values aliased through Create/Get/List.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if len(m) == 0 {
		// Tiny optimization: avoid a marshal/unmarshal pair for the
		// common empty-but-non-nil case (callers idiomatically init
		// AdapterState to `map[string]any{}` to avoid nil-deref).
		return map[string]any{}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("sessions: failed to marshal AdapterState for deep copy: %v", err))
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(fmt.Sprintf("sessions: failed to unmarshal AdapterState for deep copy: %v", err))
	}
	return out
}
