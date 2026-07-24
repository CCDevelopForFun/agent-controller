package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func runtimeIsWindows() bool { return runtime.GOOS == "windows" }

// SQLiteStore conformance tests. The shared Store contract lives in
// contract_test.go; this file adds SQLite-specific assertions around
// disk persistence, schema initialization, and path resolution.

func TestSQLiteStoreContract_InMemory(t *testing.T) {
	runStoreContract(t, func(t *testing.T) Store {
		// Each subtest gets a fresh in-memory DB so state doesn't
		// leak between tests. modernc.org/sqlite supports `:memory:`
		// the same way mattn does — no file is created.
		s, err := NewSQLiteStore(":memory:")
		if err != nil {
			t.Fatalf("NewSQLiteStore(:memory:): %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestSQLiteStoreContract_FileBacked(t *testing.T) {
	// Also run the full contract against a file-backed DB. This is
	// slower (~10ms per subtest vs <1ms in-memory) but catches any
	// behavior that diverges between memory and disk modes — e.g.
	// WAL-mode semantics, fsync timing, journal-file lifecycle.
	runStoreContract(t, func(t *testing.T) Store {
		dir := t.TempDir()
		s, err := NewSQLiteStore(filepath.Join(dir, "sessions.db"))
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestSQLiteStorePersistsAcrossReopen(t *testing.T) {
	// The whole point of the SQLite store: data survives process exit.
	// Open, write, close, reopen, read. If this fails, the abstraction
	// is broken at the layer that matters most.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	ctx := context.Background()

	s1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	sess := newTestSession("s_persist", "alice", StatusActive, time.Now().UTC())
	sess.AdapterState = map[string]any{"pi.dir": "/persisted/path"}
	if err := s1.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.Get(ctx, "s_persist")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.ID != "s_persist" {
		t.Errorf("ID lost across reopen: got %q", got.ID)
	}
	if got.AdapterState["pi.dir"] != "/persisted/path" {
		t.Errorf("AdapterState lost across reopen: got %+v", got.AdapterState)
	}
	if got.Spec.Metadata.Name != "alice" {
		t.Errorf("Spec lost across reopen: got %q", got.Spec.Metadata.Name)
	}
}

func TestSQLiteStoreCreatesParentDirectory(t *testing.T) {
	// Operators don't pre-create $XDG_DATA_HOME/agent-controller/.
	// NewSQLiteStore must mkdir -p the parent and pick a reasonable
	// 0o700 mode (session metadata is per-user).
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c", "sessions.db")
	s, err := NewSQLiteStore(nested)
	if err != nil {
		t.Fatalf("NewSQLiteStore through missing parents: %v", err)
	}
	defer s.Close()

	// Verify the parent dir was created and is mode 0o700.
	parent := filepath.Dir(nested)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("parent is not a directory")
	}
	// Mode check is best-effort — on Windows the perms don't translate
	// cleanly. Skip the assertion on non-Unix file modes.
	if mode := info.Mode().Perm(); mode != 0 && mode != 0o700 {
		t.Logf("parent dir mode is %o (informational — expected 0o700 on Unix)", mode)
	}
}

func TestSQLiteStoreTightensFilePermissions(t *testing.T) {
	// Codex pass 2 of slice 6.2: spec_json can persist MCP env /
	// headers (potential API keys, OAuth creds). On a typical 0o022
	// umask the SQLite files default to 0o644 — readable by other
	// local users. NewSQLiteStore must chmod the DB + WAL + SHM to
	// 0o600. The parent dir is chmodded to 0o700 ONLY when we
	// created it (codex pass 3 narrowed the contract — caller-owned
	// pre-existing dirs are left untouched).
	//
	// Skipped on Windows where the Unix perm model doesn't apply
	// cleanly; the chmod calls are still issued (no-op on Windows)
	// but mode-bit assertions would be brittle.
	if runtimeIsWindows() {
		t.Skip("Unix permission semantics not applicable on Windows")
	}

	dir := t.TempDir()
	// Pass a path under a NEW subdirectory we don't pre-create —
	// NewSQLiteStore mkdir's it and (since it didn't exist before)
	// chmods it to 0o700. This is the default-installation flow.
	storeDir := filepath.Join(dir, "agent-controller")
	dbPath := filepath.Join(storeDir, "sessions.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Force a write so WAL + SHM files exist (WAL mode lazy-creates
	// them on first INSERT/UPDATE).
	if err := s.Create(context.Background(), newTestSession("s_perm", "alice", StatusActive, time.Now())); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if info, err := os.Stat(storeDir); err != nil {
		t.Fatalf("stat parent: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700 (NewSQLiteStore created it)", mode)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Errorf("stat %s: %v", p, err)
			continue
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 0600", p, mode)
		}
	}
}

func TestSQLiteStoreTightensWALOnReopen(t *testing.T) {
	// Codex pass 5 of slice 6.2: a reopen of an existing DB doesn't
	// naturally write to the WAL during init (schema is idempotent,
	// no-op), so the WAL/SHM files may not exist at chmod time and
	// get skipped. Subsequent writes then create them with the
	// process umask (typically 0o644). The forced
	// `BEGIN IMMEDIATE; COMMIT;` in NewSQLiteStore must close that
	// gap so WAL+SHM are always 0o600 by the time the store returns.
	if runtimeIsWindows() {
		t.Skip("Unix permission semantics not applicable on Windows")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")

	// First open creates the DB; close cleanly so WAL+SHM may or
	// may not persist depending on SQLite checkpoint policy.
	s1, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("first NewSQLiteStore: %v", err)
	}
	if err := s1.Create(context.Background(), newTestSession("s1", "alice", StatusActive, time.Now())); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// REOPEN — the timing-hole case the codex finding describes.
	// On Linux+macOS, modernc.org/sqlite checkpoints on Close,
	// which may delete the WAL/SHM. The forced-write at open must
	// recreate them so the chmod loop sees real files.
	s2, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			// Acceptable for SHM on some platforms; main + WAL
			// should always exist by now.
			if suffix == "" || suffix == "-wal" {
				t.Errorf("expected %s to exist after reopen", p)
			}
			continue
		}
		if err != nil {
			t.Errorf("stat %s: %v", p, err)
			continue
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode after reopen = %o, want 0600", p, mode)
		}
	}
}

func TestSQLiteStorePreservesPreExistingDirPermissions(t *testing.T) {
	// Codex pass 3 of slice 6.2: when the caller passes a path whose
	// parent dir ALREADY EXISTS, NewSQLiteStore must NOT chmod it —
	// that would silently strip group/other permissions from a
	// caller-owned directory (e.g. `./sessions.db` resolves dir=".",
	// chmodding the project root). The narrower contract: WE created
	// it → WE chmod it. WE didn't → leave it alone.
	if runtimeIsWindows() {
		t.Skip("Unix permission semantics not applicable on Windows")
	}

	dir := t.TempDir()
	// Pre-create at a permissive mode and verify the mode survives.
	// 0o755 is the canonical "world-readable, owner-writable" perm
	// most operators expect on a shared project directory.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-chmod: %v", err)
	}
	dbPath := filepath.Join(dir, "sessions.db")
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Errorf("pre-existing dir mode changed: got %o want 0755 (NewSQLiteStore should not chmod dirs it didn't create)", mode)
	}
	// Files themselves still get tightened to 0o600 (those ARE ours).
	if info, err := os.Stat(dbPath); err == nil {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("DB file mode = %o, want 0600", mode)
		}
	}
}

func TestSQLiteStoreRejectsEmptyPath(t *testing.T) {
	_, err := NewSQLiteStore("")
	if err == nil {
		t.Error("expected error for empty path, got nil")
	}
}

func TestSQLiteStoreRejectsFileURIs(t *testing.T) {
	// Codex pass 4 of slice 6.2: filesystem prep + permission
	// hardening use filepath.Dir, which mangles SQLite `file:` URIs.
	// Reject them at the entry point with a clear error rather than
	// silently misbehaving.
	for _, badPath := range []string{
		"file:/tmp/sessions.db",
		"file:./sessions.db",
		"file:sessions.db?cache=shared",
	} {
		_, err := NewSQLiteStore(badPath)
		if err == nil {
			t.Errorf("expected error for URI %q, got nil", badPath)
		}
	}
}

func TestSQLiteStoreRejectsDSNQueryStrings(t *testing.T) {
	// Codex pass 6 of slice 6.2: paths containing `?` are modernc DSN
	// query strings (driver strips them at open), but our chmod uses
	// the literal path — so the real DB/WAL/SHM stay at the process
	// umask while we harden nonexistent literal-string paths. Reject
	// `?` with a clear error.
	for _, badPath := range []string{
		"sessions.db?_pragma=busy_timeout(5000)",
		"/tmp/sessions.db?cache=shared",
		"./sessions.db?mode=ro",
	} {
		_, err := NewSQLiteStore(badPath)
		if err == nil {
			t.Errorf("expected error for DSN %q, got nil", badPath)
		}
	}
}

func TestSQLiteStoreCloseIsIdempotent(t *testing.T) {
	s, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close should be no-op, got %v", err)
	}
}

func TestSQLiteStoreConcurrentCloseAndOps(t *testing.T) {
	// Codex pass 1 of slice 6.2 caught that the original Close
	// nil'd s.db without synchronization — racing Close + op would
	// have either (a) crashed on nil-deref, or (b) double-closed.
	// Now Close uses sync.Once and leaves the handle in place;
	// post-close ops return sql.ErrConnDone cleanly.
	//
	// This test runs the race detector against that contract. Run with
	// `go test -race` to validate.
	s, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}

	var wg sync.WaitGroup
	// 8 goroutines each calling Close once + a mix of ops.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Close()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// These will mostly fail (closed DB), but must NOT panic.
			_, _ = s.Get(context.Background(), "any")
			_ = s.Delete(context.Background(), "any")
		}()
	}
	wg.Wait()

	// One more Close — still must not error.
	if err := s.Close(); err != nil {
		t.Errorf("final Close after race should be no-op, got %v", err)
	}
}

func TestDefaultSQLiteStorePathHonorsXDG(t *testing.T) {
	// When XDG_DATA_HOME is set, it wins. The Linux convention; macOS
	// users who set XDG envs explicitly opt-in to the Linux layout.
	t.Setenv("XDG_DATA_HOME", "/some/xdg/data")
	got, err := DefaultSQLiteStorePath()
	if err != nil {
		t.Fatalf("DefaultSQLiteStorePath: %v", err)
	}
	want := filepath.Join("/some/xdg/data", "agent-controller", "sessions.db")
	if got != want {
		t.Errorf("XDG path: got %q want %q", got, want)
	}
}

func TestDefaultSQLiteStorePathFallsBackToHome(t *testing.T) {
	// No XDG_DATA_HOME — fall back to $HOME/.local/share/...
	// t.Setenv with empty string + unset would normally be tricky;
	// the resolver explicitly checks `!= ""`, so empty works.
	t.Setenv("XDG_DATA_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no HOME available: %v", err)
	}
	got, err := DefaultSQLiteStorePath()
	if err != nil {
		t.Fatalf("DefaultSQLiteStorePath: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "agent-controller", "sessions.db")
	if got != want {
		t.Errorf("fallback path: got %q want %q", got, want)
	}
}
