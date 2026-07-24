package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func openTemp(t *testing.T) *Workspace {
	t.Helper()
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func TestRememberRecallRoundTrip(t *testing.T) {
	w := openTemp(t)
	ctx := context.Background()
	if err := w.Remember(ctx, "topic", "AI safety"); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	val, found, err := w.Recall(ctx, "topic")
	if err != nil || !found || val != "AI safety" {
		t.Fatalf("Recall: val=%q found=%v err=%v", val, found, err)
	}
}

func TestRecallMissingKey(t *testing.T) {
	w := openTemp(t)
	_, found, err := w.Recall(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Recall err: %v", err)
	}
	if found {
		t.Error("missing key should report found=false")
	}
}

func TestRememberUpsertsAndEmptyValueIsDistinct(t *testing.T) {
	w := openTemp(t)
	ctx := context.Background()
	_ = w.Remember(ctx, "k", "first")
	if err := w.Remember(ctx, "k", "second"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	val, _, _ := w.Recall(ctx, "k")
	if val != "second" {
		t.Errorf("upsert should overwrite; got %q", val)
	}
	// Empty value is a real "set to empty", distinct from not-found.
	if err := w.Remember(ctx, "blank", ""); err != nil {
		t.Fatalf("remember empty: %v", err)
	}
	val, found, _ := w.Recall(ctx, "blank")
	if !found || val != "" {
		t.Errorf("empty value should be found and empty; got val=%q found=%v", val, found)
	}
}

func TestRememberRejectsEmptyKey(t *testing.T) {
	if err := openTemp(t).Remember(context.Background(), "", "v"); err == nil {
		t.Fatal("empty key should be rejected")
	}
}

func TestRecallAllSortedByKey(t *testing.T) {
	w := openTemp(t)
	ctx := context.Background()
	_ = w.Remember(ctx, "zebra", "1")
	_ = w.Remember(ctx, "alpha", "2")
	_ = w.Remember(ctx, "mango", "3")
	all, err := w.RecallAll(ctx)
	if err != nil {
		t.Fatalf("RecallAll: %v", err)
	}
	if len(all) != 3 || all[0].Key != "alpha" || all[1].Key != "mango" || all[2].Key != "zebra" {
		t.Errorf("expected key-sorted order, got %+v", all)
	}
}

func TestAppendNoteWritesTimestampedJournal(t *testing.T) {
	w := openTemp(t)
	if err := w.AppendNote("first thing"); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	if err := w.AppendNote("second thing"); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(w.Dir(), NotesFileName))
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	got := string(data)
	// Two appended lines, both present, order preserved.
	if !contains(got, "first thing") || !contains(got, "second thing") {
		t.Errorf("notes missing content: %q", got)
	}
	if idxOf(got, "first thing") > idxOf(got, "second thing") {
		t.Errorf("append order not preserved: %q", got)
	}
}

func TestAppendNoteTightensExistingNotesPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes aren't exposed on Windows")
	}
	w := openTemp(t)
	notes := filepath.Join(w.Dir(), NotesFileName)
	// Pre-existing journal with loose perms (e.g. a prior run under a
	// permissive umask). AppendNote must tighten it to 0600. Codex pass 6.
	if err := os.WriteFile(notes, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.AppendNote("new"); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	info, err := os.Stat(notes)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("notes perms = %o, want 600", perm)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	w1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	_ = w1.Remember(ctx, "carry", "over")
	_ = w1.Close()

	// A second run pointing --workspace at the same dir sees prior memory.
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer w2.Close()
	val, found, _ := w2.Recall(ctx, "carry")
	if !found || val != "over" {
		t.Errorf("memory did not persist across reopen: val=%q found=%v", val, found)
	}
}

func TestListOutputsExcludesInternalDBAndSortsByName(t *testing.T) {
	w := openTemp(t)
	dir := w.Dir()
	// Step artifacts a prior --output-file run would have written.
	for _, name := range []string{"b-result.json", "a-result.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.AppendNote("note") // creates notes.md
	// An in-flight --output-file temp file (slice 7.2 writeOutputFile
	// pattern) must NOT be listed as a durable output. Codex pass 4.
	if err := os.WriteFile(filepath.Join(dir, ".agentctl-output-123.tmp"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	outs, err := w.ListOutputs()
	if err != nil {
		t.Fatalf("ListOutputs: %v", err)
	}
	names := make([]string, len(outs))
	for i, o := range outs {
		names[i] = o.Name
		if o.Name == DBFileName || o.Name == DBFileName+"-wal" || o.Name == DBFileName+"-shm" {
			t.Errorf("internal DB file leaked into outputs: %q", o.Name)
		}
		if o.Name == ".agentctl-output-123.tmp" {
			t.Errorf("output temp file leaked into outputs: %q", o.Name)
		}
	}
	// Sorted: a-result.json, b-result.json, notes.md
	if len(names) != 3 || names[0] != "a-result.json" || names[1] != "b-result.json" || names[2] != "notes.md" {
		t.Errorf("unexpected outputs listing: %v", names)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second close should be a no-op, got: %v", err)
	}
}

func TestOpenRejectsEmptyAndDSNPaths(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("empty dir should be rejected")
	}
	if _, err := Open("file:/tmp/ws"); err == nil {
		t.Error("file: URI should be rejected")
	}
	if _, err := Open("/tmp/ws?cache=shared"); err == nil {
		t.Error("DSN query path should be rejected")
	}
}

func TestDBFileIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes aren't exposed on Windows (matches the session-store tests)")
	}
	w := openTemp(t)
	info, err := os.Stat(filepath.Join(w.Dir(), DBFileName))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("db perms = %o, want 600", perm)
	}
}

// tiny helpers to avoid importing strings just for two calls
func contains(s, sub string) bool { return idxOf(s, sub) >= 0 }
func idxOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
