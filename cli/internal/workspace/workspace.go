// Package workspace implements the durable, harness-agnostic agent
// workspace introduced in slice 7.5 of v0.7.
//
// A workspace is a DIRECTORY shared across the steps of a scheduler DAG.
// It holds:
//   - .agentctl-workspace.db — a SQLite key/value store for structured
//     memory (the workspace_remember / workspace_recall tools).
//   - notes.md — an append-only journal (the workspace_note_append tool).
//   - the step output files written by `agentctl run --output-file`
//     (surfaced by the workspace_list_outputs tool).
//
// The memory tools are exposed to the AGENT over MCP, not as
// harness-specific native tools, so the same workspace works whether the
// run uses the Pi adapter or the opencode adapter. agentctl itself is the
// MCP server (see the `__workspace-mcp` hidden subcommand); this package
// is the storage layer behind it.
//
// SQLite choice mirrors the session store (slice 6.2): modernc.org/sqlite
// (pure Go, no CGO) so agentctl stays cross-compilable, WAL + a busy
// timeout for safe concurrent access, and 0600 perms because remembered
// values can be sensitive.
package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DBFileName is the SQLite database basename inside a workspace dir. It is
// hidden (leading dot) and excluded from workspace_list_outputs — it is
// agentctl's bookkeeping, not a step artifact.
const DBFileName = ".agentctl-workspace.db"

// NotesFileName is the append-only journal basename inside a workspace
// dir. Unlike the DB it IS a regular handoff artifact: it shows up in
// list_outputs and a downstream step can read it with --input note=@notes.md.
const NotesFileName = "notes.md"

const schemaSQL = `
CREATE TABLE IF NOT EXISTS kv (
    key                TEXT PRIMARY KEY,
    value              TEXT NOT NULL,
    updated_at_unix_ns INTEGER NOT NULL
);
`

// busy_timeout must be applied before the WAL switch (which briefly takes
// an EXCLUSIVE lock); otherwise a concurrent open races to SQLITE_BUSY
// without the retry. Same ordering lesson as sessions/sqlite.go.
const busyTimeoutPragma = `PRAGMA busy_timeout=5000;`

const pragmaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
`

// Workspace is a handle to one workspace directory + its SQLite KV store.
// Safe for concurrent use within a process (SQLite serializes writes);
// across processes the WAL + busy timeout cover concurrent DAG steps that
// point --workspace at the same dir.
type Workspace struct {
	dir       string
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

// KV is one remembered key/value pair (used by RecallAll for a stable,
// sorted listing).
type KV struct {
	Key   string
	Value string
}

// Output describes a regular file in the workspace dir for
// workspace_list_outputs.
type Output struct {
	Name  string
	Size  int64
	IsDir bool
}

// Open opens (creating if needed) the workspace rooted at dir. The dir is
// created 0o700 when absent; the DB + its WAL/SHM sidecars are chmod'd
// 0o600 because remembered values can be secrets. dir must be a real
// filesystem path (no `file:` URI / `?` DSN forms — same restriction the
// session store documents).
func Open(dir string) (*Workspace, error) {
	if dir == "" {
		return nil, fmt.Errorf("workspace.Open: dir is required")
	}
	// Reject DB-path footguns that would mis-target the chmod hardening.
	if hasPathDSNQuirk(dir) {
		return nil, fmt.Errorf("workspace.Open: dir %q contains an unsupported character ('?' or a 'file:' prefix)", dir)
	}

	dirPreExisted := dirExists(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace dir %s: %w", dir, err)
	}
	if !dirPreExisted {
		// Only tighten perms on a dir WE created — never clobber a
		// caller-owned dir (same rule as the session store).
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("tighten workspace dir perms %s: %w", dir, err)
		}
	}

	dbPath := filepath.Join(dir, DBFileName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace db %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, step := range []struct {
		what string
		sql  string
	}{
		{"busy_timeout", busyTimeoutPragma},
		{"pragmas", pragmaSQL},
		{"schema", schemaSQL},
		// Force WAL/SHM creation before the chmod loop so they don't get
		// created later under the process umask (0644) and leak values.
		{"force WAL/SHM", "BEGIN IMMEDIATE; COMMIT;"},
	} {
		if _, err := db.Exec(step.sql); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("workspace db %s: %w", step.what, err)
		}
	}

	// Harden perms on the DB and its sidecars (created above).
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		if _, statErr := os.Stat(p); statErr == nil {
			if err := os.Chmod(p, 0o600); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("tighten workspace db perms %s: %w", p, err)
			}
		}
	}

	return &Workspace{dir: dir, db: db}, nil
}

// Dir returns the workspace directory path.
func (w *Workspace) Dir() string { return w.dir }

// Close closes the underlying database. Idempotent.
func (w *Workspace) Close() error {
	w.closeOnce.Do(func() {
		w.closeErr = w.db.Close()
	})
	return w.closeErr
}

// Remember upserts a key/value pair. An empty key is rejected (it could
// never be recalled meaningfully); an empty value is allowed (a real
// "set to empty" signal).
func (w *Workspace) Remember(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("remember: key is required")
	}
	_, err := w.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at_unix_ns) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at_unix_ns=excluded.updated_at_unix_ns`,
		key, value, time.Now().UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("remember %q: %w", key, err)
	}
	return nil
}

// Recall returns the value for key. found is false when the key was never
// remembered (distinct from a remembered empty string).
func (w *Workspace) Recall(ctx context.Context, key string) (value string, found bool, err error) {
	row := w.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key)
	switch err := row.Scan(&value); err {
	case nil:
		return value, true, nil
	case sql.ErrNoRows:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("recall %q: %w", key, err)
	}
}

// RecallAll returns every remembered pair, sorted by key for determinism.
func (w *Workspace) RecallAll(ctx context.Context) ([]KV, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT key, value FROM kv`)
	if err != nil {
		return nil, fmt.Errorf("recall all: %w", err)
	}
	defer rows.Close()
	var out []KV
	for rows.Next() {
		var kv KV
		if err := rows.Scan(&kv.Key, &kv.Value); err != nil {
			return nil, fmt.Errorf("recall all scan: %w", err)
		}
		out = append(out, kv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recall all rows: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// AppendNote appends a timestamped line to notes.md in the workspace dir.
//
// Notes live in a FILE (not the DB) on purpose: the journal is then a
// first-class handoff artifact — it appears in ListOutputs and a
// downstream step can consume it with `--input note=@<dir>/notes.md`.
// The write uses O_APPEND so concurrent small appends from sequential DAG
// steps don't truncate each other; the workspace model assumes steps
// sharing a dir run sequentially, which is the common scheduler pattern.
func (w *Workspace) AppendNote(note string) error {
	f, err := os.OpenFile(filepath.Join(w.dir, NotesFileName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open notes file: %w", err)
	}
	defer f.Close()
	// The 0o600 in OpenFile only applies when CREATING the file; an
	// existing notes.md keeps its old (possibly looser) mode. Re-assert
	// 0o600 since notes can be sensitive — matches the DB hardening.
	// Codex pass 6 of slice 7.5. (Chmod is a no-op on platforms without
	// POSIX modes; harmless there.)
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("tighten notes file perms: %w", err)
	}
	line := fmt.Sprintf("[%s] %s\n", time.Now().UTC().Format(time.RFC3339), note)
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("append note: %w", err)
	}
	return nil
}

// ListOutputs returns the regular files in the workspace dir — the step
// artifacts written by `--output-file`, plus notes.md. The internal
// SQLite files (.agentctl-workspace.db + WAL/SHM sidecars) are excluded:
// they are agentctl bookkeeping, not step outputs. Results are sorted by
// name for determinism.
func (w *Workspace) ListOutputs() ([]Output, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	var out []Output
	for _, e := range entries {
		name := e.Name()
		if isInternalFile(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Entry vanished between ReadDir and Info (concurrent step):
			// skip rather than fail the whole listing.
			continue
		}
		// Only surface regular files + dirs; skip sockets/FIFOs/devices.
		if !info.Mode().IsRegular() && !info.IsDir() {
			continue
		}
		out = append(out, Output{Name: name, Size: info.Size(), IsDir: info.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// outputTmpPrefix matches the temp files agentctl's `--output-file`
// writer creates during its atomic write (slice 7.2 writeOutputFile uses
// os.CreateTemp with the pattern ".agentctl-output-*.tmp"). When
// --output-file targets a path inside the workspace, a concurrent run or
// a crash can leave one of these mid-write; they must NOT be listed as
// durable outputs. Keep in sync with writeOutputFile's CreateTemp prefix
// in cli/cmd/agentctl/output.go.
const outputTmpPrefix = ".agentctl-output-"

// isInternalFile reports whether name is agentctl bookkeeping that must
// not appear in the user-facing outputs listing: the SQLite DB + its
// WAL/SHM sidecars, and any in-flight --output-file temp file.
func isInternalFile(name string) bool {
	return name == DBFileName ||
		name == DBFileName+"-wal" ||
		name == DBFileName+"-shm" ||
		strings.HasPrefix(name, outputTmpPrefix)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// hasPathDSNQuirk rejects path forms that would make our chmod hardening
// target the wrong files (a `?` DSN query or a `file:` URI), mirroring the
// session store's restriction.
func hasPathDSNQuirk(path string) bool {
	if len(path) >= 5 && path[:5] == "file:" {
		return true
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '?' {
			return true
		}
	}
	return false
}
