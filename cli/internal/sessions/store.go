// Package sessions defines the durable session-store abstraction for
// long-running agentctl runs.
//
// Pre-v0.6 every `agentctl run` is a one-shot subprocess and the only
// resume surface is `--resume <id>` against Pi's on-disk session
// directory (`$HOME/.pi/agent/sessions/agentctl/<id>/`). Slice 6.1
// introduces the higher-level abstraction: an opaque "agentctl session"
// the runtime can look up by id, mutate as it runs, and persist across
// process restarts. v0.6.3 wires `agentctl chat` into it for the REPL
// surface; v0.7 workflow steps reuse it for multi-turn step state; v0.8
// HTTP/SSE servers swap the backing store for Postgres/Redis without
// touching callers.
//
// Two implementations ship in v0.6:
//
//   - MemoryStore (this slice, 6.1) — ephemeral, tests + REPL fallback.
//   - SQLiteStore (slice 6.2) — file-backed, single-host, zero config.
//
// The interface keeps a minimal surface so future stores (Postgres,
// Redis, in-cluster CRD) are also straightforward to drop in.

package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
)

// SessionStatus enumerates the lifecycle states a session passes through.
//
// Slice 6.1 only sets `active` and `ended`. The other three are
// reserved for slice 6.4 (lifecycle wire events) — defining them now
// keeps the type stable so 6.4 doesn't have to widen an in-use enum.
type SessionStatus string

const (
	// StatusActive is a session currently being driven by an adapter.
	StatusActive SessionStatus = "active"
	// StatusEnded is a normally-completed session. Terminal.
	StatusEnded SessionStatus = "ended"
	// StatusPaused (slice 6.4): a chat-mode session the user stepped
	// away from (Ctrl-C between turns) but did not end.
	StatusPaused SessionStatus = "paused"
	// StatusExpired (slice 6.4): a session whose TTL was hit before
	// completion. Terminal.
	StatusExpired SessionStatus = "expired"
	// StatusFailed is a session that exited with an error.
	StatusFailed SessionStatus = "failed"
)

// Session is the durable record of a long-running agent run.
//
// Field mutability split:
//   - Immutable after Create: ID, AgentName, RuntimeType, CreatedAt,
//     Spec, TraceContext. Store implementations IGNORE attempts to
//     change these on Update — the caller can't drift session identity.
//   - Mutable on Update: Status, LastActiveAt, AdapterState.
type Session struct {
	// ID is the primary key. Stable across resumes. Format is
	// `s_<base36-millis>` from the runtime adapter, BUT callers must
	// not parse it — the format is an opaque identifier.
	ID string

	// AgentName is `spec.metadata.name` at session start. Stored for
	// list-by-agent queries; the canonical source is the Spec field.
	AgentName string

	// RuntimeType is `spec.runtime.type` at start. Pi vs opencode dispatch
	// looks at this to know which adapter the session was bound to;
	// resuming under a mismatched runtime is rejected by the caller
	// (slice 6.3 wires the check).
	RuntimeType string

	// Status is the session's current lifecycle position. See the
	// StatusXxx constants. Slice 6.1 only sets active/ended; later
	// slices wire paused/expired/failed.
	Status SessionStatus

	// CreatedAt is the wall-clock time the session was first created.
	// Stable. UTC at write; callers may localize for display.
	CreatedAt time.Time

	// LastActiveAt is the wall-clock time of the last Update — i.e.
	// the last time an adapter reported progress for this session.
	// Used for List() ordering and for the expiry TTL check in 6.4.
	LastActiveAt time.Time

	// Spec is the CompiledSpec the session was started with. Snapshotted
	// at creation so resume can replay it. Storing the full spec (not
	// just a path) lets the store outlive the spec file on disk.
	Spec adl.CompiledSpec

	// TraceContext is the W3C `traceparent` header of the session's
	// root OTel span. Each resumed turn opens a child span under it
	// (slice 6.5 wires this). Empty when tracing was off at creation.
	TraceContext string

	// AdapterState is an opaque map the adapter (Pi or opencode) uses
	// to find its own session state on disk. Format is adapter-defined.
	// For the Pi adapter today this carries the path to the underlying
	// Pi session directory; opencode's state surfaces here in 6.3.
	AdapterState map[string]any
}

// Store is the abstraction every backing implementation satisfies.
// MemoryStore (6.1), SQLiteStore (6.2), and any future Postgres / Redis
// stores all expose this same surface.
//
// Concurrency contract:
//   - Implementations MUST be safe for concurrent use. The REPL (6.3)
//     and the future HTTP server (v0.8) both call from multiple
//     goroutines.
//   - All methods take a context.Context. Implementations honor
//     cancellation where the backing store supports it; in-memory ops
//     return immediately and ignore ctx beyond a check at entry.
type Store interface {
	// Create persists a new session. Returns ErrAlreadyExists if a
	// session with the same ID is already in the store.
	Create(ctx context.Context, s Session) error

	// Get returns the session by ID. Returns ErrNotFound when the
	// session doesn't exist.
	Get(ctx context.Context, id string) (Session, error)

	// Update overwrites the mutable fields (Status, LastActiveAt,
	// AdapterState) of an existing session. Returns ErrNotFound when
	// the session doesn't exist. Immutable fields on the input
	// (ID, AgentName, RuntimeType, CreatedAt, Spec, TraceContext) are
	// kept at their original values — see the Session doc-comment.
	//
	// Returns ErrSessionExpired if the existing row's Status is
	// already StatusExpired AND the incoming update would change
	// it to something other than Expired. The terminal-status
	// guard is enforced atomically by the impl so a chat process
	// can't undo a concurrent `sessions sweep` even mid-turn.
	// Slice 6.4 (codex pass 2).
	Update(ctx context.Context, s Session) error

	// Delete removes the session. Idempotent: deleting a missing
	// session is NOT an error (matches the best-effort-cleanup pattern
	// the K8s backend uses for Pods and Secrets).
	Delete(ctx context.Context, id string) error

	// List returns sessions matching the filter, ordered by
	// LastActiveAt descending (most-recently-active first).
	// Empty filter returns every session.
	List(ctx context.Context, filter ListFilter) ([]Session, error)

	// MarkExpired bulk-transitions every currently-Active session
	// whose LastActiveAt is strictly before `cutoff` to
	// StatusExpired. Returns the IDs of sessions that were
	// transitioned. Used by the `agentctl sessions sweep`
	// subcommand to retire idle sessions past their TTL.
	//
	// Idempotent: a second sweep with the same cutoff is a no-op
	// because the sessions transitioned by the first sweep are no
	// longer StatusActive. Only Active sessions are touched —
	// already-Paused or already-Ended sessions are left alone so
	// the lifecycle history remains intact. Added in slice 6.4.
	MarkExpired(ctx context.Context, cutoff time.Time) ([]string, error)

	// Close releases any resources the store holds (DB handles, file
	// locks, etc.). Idempotent — calling Close on an already-closed
	// store is a no-op. MemoryStore.Close drops all data; SQLiteStore
	// (6.2) closes the DB handle but keeps the file.
	Close() error
}

// ListFilter narrows List() results. All fields are optional and
// combine as AND. Limit <= 0 returns all matches.
type ListFilter struct {
	AgentName string
	Status    SessionStatus
	Limit     int
}

// ErrNotFound is returned by Get / Update when the session doesn't
// exist. Callers should use errors.Is(err, ErrNotFound) to match.
var ErrNotFound = errors.New("session not found")

// ErrAlreadyExists is returned by Create when a session with the same
// ID is already in the store. Callers should use errors.Is to match.
var ErrAlreadyExists = errors.New("session already exists")

// ErrSessionExpired is returned by Update when the target session's
// current Status is StatusExpired AND the update would transition it
// out of Expired. Slice 6.4 (codex pass 2) caught the race where
// `sessions sweep` retires an idle chat's row but the still-running
// REPL then writes StatusActive / StatusPaused / StatusEnded over
// it on its next turn or exit, "reviving" a session the operator
// meant to retire. Enforcing the terminal-status guard at the Store
// layer makes the check atomic (impl-side: SQL WHERE clause for
// SQLite, mutex-protected read-modify for Memory). Callers MUST
// match this sentinel with errors.Is and stop touching the session.
var ErrSessionExpired = errors.New("session is expired and cannot be updated")
