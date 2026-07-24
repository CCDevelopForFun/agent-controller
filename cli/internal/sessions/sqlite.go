package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// Pure-Go SQLite driver — keeps `agentctl` cross-compilable to
	// any platform without CGO. The runtime cost vs. the C-based
	// mattn/go-sqlite3 is negligible for the session-store workload
	// (single-digit writes per chat session); the build-tooling
	// simplification is worth far more.
	_ "modernc.org/sqlite"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
)

// schemaVersion is the current sessions-table schema. Bump and add a
// migration when the column layout changes; slice 6.2 ships v1.
const schemaVersion = 1

// schemaSQL is the embedded DDL applied at NewSQLiteStore. SQLite is
// happy to run multiple `CREATE TABLE IF NOT EXISTS` statements at
// open time — no separate migration file needed at this stage.
//
// Design notes:
//   - id, agent_name, runtime_type are TEXT NOT NULL — the immutable
//     identity that Update can't change.
//   - status is TEXT for forward compat (new statuses in 6.4 land as
//     more enum string values; no schema migration needed).
//   - created_at / last_active_at as INTEGER (unix nanos UTC) so
//     ORDER BY is a plain integer comparison and timezone is implicit.
//   - spec_json and adapter_state_json as TEXT — opaque JSON the
//     application layer marshals. JSON1 extension is NOT relied on;
//     the store reads the blob and unmarshals in Go to keep the
//     impl portable to any SQLite build.
//   - trace_context as TEXT (W3C traceparent header, 55 chars).
//   - Index on last_active_at DESC — List() orders by this and slice
//     6.4 expiry sweep filters on it. The id PK auto-indexes Get.
//   - Index on (agent_name, status) — List filters combine these.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id                  TEXT PRIMARY KEY,
    agent_name          TEXT NOT NULL,
    runtime_type        TEXT NOT NULL,
    status              TEXT NOT NULL,
    created_at_unix_ns  INTEGER NOT NULL,
    last_active_unix_ns INTEGER NOT NULL,
    spec_json           TEXT NOT NULL,
    trace_context       TEXT NOT NULL DEFAULT '',
    adapter_state_json  TEXT NOT NULL DEFAULT '{}',
    schema_version      INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_sessions_last_active
    ON sessions (last_active_unix_ns DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_agent_status
    ON sessions (agent_name, status);
`

// busyTimeoutPragma is the FIRST pragma we apply on open — before
// journal_mode, before schema. journal_mode=WAL takes an EXCLUSIVE
// lock briefly during its switch, and if a concurrent process holds
// a lock at that instant, the open fails with SQLITE_BUSY even though
// we INTEND a 5s retry. Codex pass 4 of slice 6.2 caught the ordering:
// busy_timeout must be installed before the next lock-taking pragma
// runs. The retry budget then covers the WAL switch itself.
const busyTimeoutPragma = `PRAGMA busy_timeout=5000;`

// pragmaSQL applies SQLite tuning that matters for the session-store
// workload. Applied AFTER busyTimeoutPragma:
//
//   - journal_mode=WAL: concurrent readers don't block a writer; the
//     default DELETE journal serializes everything. Critical for the
//     REPL where one process writes (the chat) while another lists
//     (`agentctl sessions ls`).
//   - synchronous=NORMAL: WAL mode pairs with NORMAL for the standard
//     speed/durability tradeoff. FULL would force fsync on every
//     commit (overkill for session metadata); OFF would risk corrupt
//     state on crash (unacceptable for resume semantics).
//   - foreign_keys=ON: harmless today (no FKs declared), forward-compat
//     for future tables that might reference sessions.
const pragmaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
PRAGMA foreign_keys=ON;
`

// SQLiteStore is the file-backed Store implementation. Default ships
// with slice 6.2; v0.8 adds Postgres/Redis impls of the same interface.
//
// File path: callers pass an explicit path. For the default location
// (slice 6.3 will wire this), use DefaultSQLiteStorePath() which
// resolves to $XDG_DATA_HOME/agent-controller/sessions.db (or the
// platform-appropriate fallback).
type SQLiteStore struct {
	db        *sql.DB
	path      string
	closeOnce sync.Once
	closeErr  error
}

// DefaultSQLiteStorePath returns the conventional location for the
// session store database. Follows XDG when $XDG_DATA_HOME is set
// (Linux convention, also honored on macOS); otherwise falls back to
// $HOME/.local/share/agent-controller/sessions.db.
//
// Callers should not assume the parent directory exists —
// NewSQLiteStore will mkdir it.
func DefaultSQLiteStorePath() (string, error) {
	var base string
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		base = v
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir for default session store path: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agent-controller", "sessions.db"), nil
}

// NewSQLiteStore opens (or creates) the SQLite database at path,
// applies the schema + pragmas, and returns a ready-to-use Store.
// The parent directory is created with 0o700 if missing (session
// data is per-user — no group/other access).
//
// Accepted path forms:
//   - ":memory:" — ephemeral in-process database (tests, REPL fallback)
//   - A plain filesystem path (relative or absolute)
//
// NOT accepted: SQLite `file:` URIs (`file:/path/to/db?cache=shared`).
// They're rejected because our filesystem prep + permission hardening
// resolve the parent dir via filepath.Dir, which mangles URIs. Codex
// pass 4 of slice 6.2 caught the mismatch between an aspirational
// doc-comment and the actual behavior.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("NewSQLiteStore: path is required (pass \":memory:\" for in-memory)")
	}
	if strings.HasPrefix(path, "file:") {
		return nil, fmt.Errorf("NewSQLiteStore: SQLite `file:` URIs are not supported; pass a plain filesystem path (got %q)", path)
	}
	// Codex pass 6 of slice 6.2: also reject DSN query-string forms
	// like `sessions.db?_pragma=...`. modernc/sqlite strips the query
	// at open time, but our directory prep + permission hardening
	// uses the literal string — `filepath.Dir` returns the directory
	// of `sessions.db?_pragma=...` (which "exists" as the parent of
	// a nonexistent-but-not-an-error path), and the chmod loop then
	// hardens the wrong files (the literal `sessions.db?_pragma=...`
	// paths, which don't exist) while the REAL DB / WAL / SHM stay
	// at the process umask. `?` in a real Unix filename is
	// pathological for a CLI surface; rejecting it is far simpler
	// than dual-tracking the literal-vs-stripped path.
	if strings.Contains(path, "?") {
		return nil, fmt.Errorf("NewSQLiteStore: SQLite DSN query strings are not supported; pass a plain filesystem path without `?` (got %q)", path)
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		// Determine whether the parent dir exists BEFORE MkdirAll so
		// we can chmod safely only when WE created it. Codex pass 3
		// of slice 6.2 caught the original unconditional chmod: if a
		// caller passes a bare filename (e.g. `sessions.db`),
		// filepath.Dir returns "." and the chmod would clobber the
		// perms of the caller's CWD — never our prerogative to do.
		//
		// MkdirAll semantics: doesn't error if the path already
		// exists, doesn't apply mode to pre-existing components.
		// So "stat-then-mkdir-then-chmod-if-we-created" is the
		// exact-ownership pattern; relative-path callers + caller-
		// owned dirs are left untouched.
		_, statErr := os.Stat(dir)
		dirPreExisted := statErr == nil
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create session-store dir %s: %w", dir, err)
		}
		if !dirPreExisted {
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, fmt.Errorf("tighten session-store dir perms %s: %w", dir, err)
			}
		}
	}

	// modernc.org/sqlite registers under the driver name "sqlite"
	// (not "sqlite3" like mattn). DSN-style URIs are accepted but
	// the bare path works for the simple case.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	// Single connection — SQLite serializes writes regardless of how
	// many connections we open, and our access pattern is low-volume.
	// Multiple connections would mean multiple WAL writers competing
	// for the same lock, no speedup. Keeping this at 1 also avoids a
	// class of "different connection sees stale data" bugs that
	// emerged when modernc's WAL handling was rougher.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Codex pass 4 of slice 6.2: busy_timeout MUST be the first
	// pragma we apply. WAL-mode setup briefly takes an EXCLUSIVE
	// lock, and if a concurrent process holds a lock at that
	// instant, the SQLITE_BUSY return is immediate without the
	// retry. Setting busy_timeout first means the retry budget
	// covers the WAL switch + schema creation too.
	if _, err := db.Exec(busyTimeoutPragma); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite busy_timeout: %w", err)
	}
	if _, err := db.Exec(pragmaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite pragmas: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply sqlite schema: %w", err)
	}

	// Codex pass 5 of slice 6.2: force WAL/SHM file creation BEFORE
	// chmod-ing them. Under WAL mode, SQLite creates the `-wal` and
	// `-shm` files lazily on first write — and on reopen of an
	// existing DB where the schema is already present (idempotent
	// CREATE TABLE), no write occurs at init, so the files don't
	// exist when the chmod loop runs and get skipped. Subsequent
	// writes then create them with the process umask (typically
	// 0o644) and `spec_json` containing MCP env / headers leaks into
	// a world-readable WAL.
	//
	// `BEGIN IMMEDIATE; COMMIT;` is the canonical SQLite idiom for
	// "force a no-op write": IMMEDIATE acquires a RESERVED lock,
	// which under WAL mode requires the SHM file to exist; COMMIT
	// writes a single (empty) WAL frame, ensuring the WAL file
	// exists too. No table data is modified.
	if path != ":memory:" {
		if _, err := db.Exec("BEGIN IMMEDIATE; COMMIT;"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("force WAL/SHM file creation: %w", err)
		}
	}

	// Chmod the DB files AFTER the pragmas + schema + forced-write
	// — those are what actually create / touch the files on disk
	// (under the process umask, typically 0o022 ⇒ 0o644). Without
	// this, spec_json (which can carry MCPServers[].Env / .Headers —
	// API tokens, OAuth creds) would be world-readable on a
	// multi-user box. Codex pass 2 of slice 6.2 caught the missing
	// chmod entirely; codex pass 5 caught the timing hole on reopen.
	//
	// SQLite preserves file metadata (perms, inode) through WAL
	// truncation / checkpoint, so a once-at-open chmod sticks.
	if path != ":memory:" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			p := path + suffix
			if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
				_ = db.Close()
				return nil, fmt.Errorf("tighten session-store file perms %s: %w", p, err)
			}
		}
	}

	return &SQLiteStore{db: db, path: path}, nil
}

// Path returns the file path the store was opened against.
// Exposed for tests + future `agentctl sessions ls` output.
func (s *SQLiteStore) Path() string { return s.path }

func (s *SQLiteStore) Create(ctx context.Context, sess Session) error {
	if sess.ID == "" {
		return fmt.Errorf("SQLiteStore.Create: session ID is required")
	}
	specJSON, adapterJSON, err := marshalSessionPayloads(sess)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO sessions (
            id, agent_name, runtime_type, status,
            created_at_unix_ns, last_active_unix_ns,
            spec_json, trace_context, adapter_state_json,
            schema_version
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		sess.ID, sess.AgentName, sess.RuntimeType, string(sess.Status),
		sess.CreatedAt.UTC().UnixNano(), sess.LastActiveAt.UTC().UnixNano(),
		specJSON, sess.TraceContext, adapterJSON,
		schemaVersion,
	)
	if err != nil {
		// SQLite returns a constraint-violation error when the PK
		// collides. modernc surfaces this as an error whose string
		// contains "constraint" / "UNIQUE". We don't sniff the
		// driver-specific code — the message check is the same one
		// mattn-based code uses, and the test pins the behavior.
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, sess.ID)
		}
		return fmt.Errorf("SQLiteStore.Create insert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, agent_name, runtime_type, status,
               created_at_unix_ns, last_active_unix_ns,
               spec_json, trace_context, adapter_state_json
        FROM sessions WHERE id = ?
    `, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("SQLiteStore.Get: %w", err)
	}
	return sess, nil
}

func (s *SQLiteStore) Update(ctx context.Context, sess Session) error {
	if sess.ID == "" {
		return fmt.Errorf("SQLiteStore.Update: session ID is required")
	}
	adapterJSON, err := json.Marshal(sess.AdapterState)
	if err != nil {
		return fmt.Errorf("SQLiteStore.Update marshal AdapterState: %w", err)
	}
	// Update ONLY the mutable fields (Status, LastActiveAt,
	// AdapterState). The immutable-on-Update contract documented on
	// Session in store.go is enforced here at the SQL level — no
	// columns for ID/AgentName/RuntimeType/CreatedAt/Spec/TraceContext
	// appear in the SET clause, so even if a caller passes mutated
	// values they can't reach the row.
	//
	// Slice 6.4 codex pass 2: WHERE-clause terminal-Expired guard.
	// Refuse to transition a row OUT of StatusExpired so a chat
	// process can't revive a sweep-retired session (whether on a
	// mid-turn LastActiveAt bump or an exit-status write). The
	// "or already expired and staying expired" branch keeps the
	// transition idempotent — a second sweep at the same cutoff
	// is a no-op rather than an error.
	expiredLit := string(StatusExpired)
	result, err := s.db.ExecContext(ctx, `
        UPDATE sessions
        SET status = ?, last_active_unix_ns = ?, adapter_state_json = ?
        WHERE id = ?
          AND (status != ? OR ? = ?)
    `,
		string(sess.Status), sess.LastActiveAt.UTC().UnixNano(), string(adapterJSON),
		sess.ID,
		expiredLit, string(sess.Status), expiredLit,
	)
	if err != nil {
		return fmt.Errorf("SQLiteStore.Update exec: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("SQLiteStore.Update rows-affected: %w", err)
	}
	if affected == 0 {
		// Either NotFound or the row exists and is Expired. Distinguish
		// via a follow-up Get under the same conn. The race window
		// between the UPDATE and the Get is tiny but real: a concurrent
		// Delete could remove the row in between. Either way the
		// session is gone from the caller's POV — return the more
		// specific error when we can.
		row := s.db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, sess.ID)
		var statusStr string
		switch err := row.Scan(&statusStr); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrNotFound, sess.ID)
		case err != nil:
			return fmt.Errorf("SQLiteStore.Update follow-up scan: %w", err)
		}
		if SessionStatus(statusStr) == StatusExpired {
			return fmt.Errorf("%w: %s", ErrSessionExpired, sess.ID)
		}
		// Status was something else and 0 rows affected? Shouldn't
		// happen — the WHERE only excludes Expired-being-changed.
		// Fall through to NotFound for safety.
		return fmt.Errorf("%w: %s", ErrNotFound, sess.ID)
	}
	return nil
}

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	// Idempotent — Delete of a missing row is not an error
	// (matches MemoryStore + the K8s-backend best-effort-cleanup
	// convention). We don't check RowsAffected.
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("SQLiteStore.Delete: %w", err)
	}
	return nil
}

func (s *SQLiteStore) List(ctx context.Context, filter ListFilter) ([]Session, error) {
	// Build the query dynamically based on filter fields. Parameterized
	// throughout — no string concatenation of user input into SQL.
	query := `
        SELECT id, agent_name, runtime_type, status,
               created_at_unix_ns, last_active_unix_ns,
               spec_json, trace_context, adapter_state_json
        FROM sessions
    `
	var args []any
	var where []string
	if filter.AgentName != "" {
		where = append(where, "agent_name = ?")
		args = append(args, filter.AgentName)
	}
	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, string(filter.Status))
	}
	if len(where) > 0 {
		query += " WHERE " + joinWithAnd(where)
	}
	// Match MemoryStore ordering exactly: LastActiveAt DESC, id ASC tiebreak.
	// Determinism matters — both impls back the same contract.
	query += " ORDER BY last_active_unix_ns DESC, id ASC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("SQLiteStore.List query: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, scanErr := scanSession(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("SQLiteStore.List scan: %w", scanErr)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SQLiteStore.List iterate: %w", err)
	}
	return out, nil
}

func (s *SQLiteStore) MarkExpired(ctx context.Context, cutoff time.Time) ([]string, error) {
	// Codex slice 6.4 pass 4: single `UPDATE ... RETURNING id`
	// statement instead of the original SELECT + UPDATE inside a
	// deferred transaction. Why this matters:
	//
	//   - A deferred BEGIN takes a READ snapshot up front. SQLite's
	//     WAL mode cannot promote a stale read transaction into a
	//     write transaction if another writer has committed in the
	//     meantime — the sweep fails with SQLITE_BUSY.
	//   - A chat process calling SQLiteStore.Update between our
	//     SELECT and our UPDATE was exactly that scenario: a stale
	//     read snapshot, then a write-promotion failure.
	//   - `UPDATE ... RETURNING` (SQLite 3.35+, included in
	//     modernc.org/sqlite v1.52) gives us atomic select-and-update
	//     in one statement. No transaction needed; no race window.
	//
	// Sort the returned IDs in Go for determinism — RETURNING's
	// output order is unspecified and MemoryStore guarantees sorted
	// output, so we sort here too to keep the contract identical.
	cutoffNs := cutoff.UTC().UnixNano()
	rows, err := s.db.QueryContext(ctx, `
        UPDATE sessions
        SET status = ?
        WHERE status = ? AND last_active_unix_ns < ?
        RETURNING id
    `, string(StatusExpired), string(StatusActive), cutoffNs)
	if err != nil {
		return nil, fmt.Errorf("SQLiteStore.MarkExpired update-returning: %w", err)
	}
	defer rows.Close()

	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("SQLiteStore.MarkExpired scan: %w", err)
		}
		expired = append(expired, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SQLiteStore.MarkExpired iterate: %w", err)
	}
	sort.Strings(expired)
	return expired, nil
}

// Close releases the SQLite connection pool. Idempotent and safe for
// concurrent use:
//
//   - sync.Once guarantees the underlying db.Close runs exactly once
//     even under racing Close calls from multiple goroutines.
//   - The *sql.DB handle is NOT nil'd. After Close, database/sql
//     returns sql.ErrConnDone from any subsequent operation, so a
//     racing Create/Get/List/Update/Delete fails cleanly with an
//     error instead of panicking on a nil-pointer dereference.
//
// Codex pass 1 of slice 6.2 caught the original implementation's
// unsynchronized read/write of s.db.
func (s *SQLiteStore) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// ── internal helpers ──────────────────────────────────────────────────────

// scanner is the minimal interface that both *sql.Row and *sql.Rows
// satisfy. Lets scanSession serve both Get (single row) and List (many).
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(s scanner) (Session, error) {
	var (
		sess         Session
		statusStr    string
		createdNs    int64
		lastActiveNs int64
		specJSON     string
		adapterJSON  string
	)
	if err := s.Scan(
		&sess.ID, &sess.AgentName, &sess.RuntimeType, &statusStr,
		&createdNs, &lastActiveNs,
		&specJSON, &sess.TraceContext, &adapterJSON,
	); err != nil {
		return Session{}, err
	}
	sess.Status = SessionStatus(statusStr)
	sess.CreatedAt = time.Unix(0, createdNs).UTC()
	sess.LastActiveAt = time.Unix(0, lastActiveNs).UTC()
	if err := json.Unmarshal([]byte(specJSON), &sess.Spec); err != nil {
		return Session{}, fmt.Errorf("unmarshal spec_json for session %s: %w", sess.ID, err)
	}
	// Adapter state: tolerate empty/missing as nil-equivalent
	// (Create defaults to `{}` so this is just defense-in-depth for
	// rows written by a future hand-edited DB).
	if adapterJSON == "" || adapterJSON == "null" {
		sess.AdapterState = nil
	} else if err := json.Unmarshal([]byte(adapterJSON), &sess.AdapterState); err != nil {
		return Session{}, fmt.Errorf("unmarshal adapter_state_json for session %s: %w", sess.ID, err)
	}
	return sess, nil
}

func marshalSessionPayloads(sess Session) (specJSON string, adapterJSON string, err error) {
	specBytes, err := json.Marshal(sess.Spec)
	if err != nil {
		return "", "", fmt.Errorf("marshal Spec: %w", err)
	}
	// Normalize nil → {} on the wire so a future hand-edited DB
	// doesn't have to special-case `null` vs `{}`.
	adapter := sess.AdapterState
	if adapter == nil {
		adapter = map[string]any{}
	}
	adapterBytes, err := json.Marshal(adapter)
	if err != nil {
		return "", "", fmt.Errorf("marshal AdapterState: %w", err)
	}
	// Sanity: ensure the Spec roundtrips. If it doesn't, fail fast at
	// Create rather than at Get — easier to debug "Create rejected my
	// spec" than "Get returned mangled data".
	var roundTrip adl.CompiledSpec
	if err := json.Unmarshal(specBytes, &roundTrip); err != nil {
		return "", "", fmt.Errorf("Spec is not JSON-roundtrippable: %w", err)
	}
	return string(specBytes), string(adapterBytes), nil
}

func joinWithAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// isUniqueConstraintErr sniffs the modernc/sqlite driver's error message
// for a UNIQUE constraint failure (SQLite error code 2067 /
// SQLITE_CONSTRAINT_PRIMARYKEY). The driver doesn't expose typed
// errors, so a substring match is the cleanest portable check.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// modernc surfaces both phrases depending on build; check both.
	return containsAny(msg, "UNIQUE constraint failed", "SQLITE_CONSTRAINT_PRIMARYKEY", "constraint failed: sessions.id")
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(s) >= len(n) {
			for i := 0; i+len(n) <= len(s); i++ {
				if s[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}
