package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setMTime writes a file (creating it if needed) and sets its mtime.
func setMTime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

func TestCheckRuntimeStaleness_DistNewer_NoError(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dist := filepath.Join(root, "dist")
	t0 := time.Now().Add(-2 * time.Hour)
	t1 := time.Now().Add(-1 * time.Hour)
	setMTime(t, filepath.Join(src, "a.ts"), t0)
	setMTime(t, filepath.Join(dist, "a.js"), t1)

	if err := checkRuntimeStaleness(src, dist, "runtime"); err != nil {
		t.Errorf("expected nil error when dist is newer, got: %v", err)
	}
}

func TestCheckRuntimeStaleness_SrcNewer_Errors(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dist := filepath.Join(root, "dist")
	tOld := time.Now().Add(-2 * time.Hour)
	tNew := time.Now().Add(-30 * time.Minute)
	setMTime(t, filepath.Join(dist, "a.js"), tOld)
	setMTime(t, filepath.Join(src, "a.ts"), tNew)

	err := checkRuntimeStaleness(src, dist, "runtime")
	if err == nil {
		t.Fatalf("expected staleness error when src is newer")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error %q should mention 'stale'", err)
	}
	if !strings.Contains(err.Error(), "npm --prefix runtime run build") {
		t.Errorf("error %q should suggest the build command", err)
	}
	if !strings.Contains(err.Error(), "--no-staleness-check") {
		t.Errorf("error %q should mention the bypass flag", err)
	}
}

func TestCheckRuntimeStaleness_DistMissing_Errors(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dist := filepath.Join(root, "missing-dist")
	setMTime(t, filepath.Join(src, "a.ts"), time.Now())

	err := checkRuntimeStaleness(src, dist, "runtime")
	if err == nil {
		t.Fatalf("expected error when dist directory is missing")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q should mention 'missing'", err)
	}
}

func TestCheckRuntimeStaleness_SrcMissing_NoError(t *testing.T) {
	// Install scenario: only dist/ ships. We should NOT false-alarm when
	// src/ doesn't exist — there is nothing to be stale relative to.
	root := t.TempDir()
	src := filepath.Join(root, "missing-src")
	dist := filepath.Join(root, "dist")
	setMTime(t, filepath.Join(dist, "a.js"), time.Now())

	if err := checkRuntimeStaleness(src, dist, "runtime"); err != nil {
		t.Errorf("expected nil error when src directory is missing (install scenario), got: %v", err)
	}
}

func TestCheckRuntimeStaleness_SkipsNodeModules(t *testing.T) {
	// A very-new file under src/node_modules/ should not trigger staleness
	// (those mtimes reflect install time, not our code).
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dist := filepath.Join(root, "dist")
	tOld := time.Now().Add(-2 * time.Hour)
	tFuture := time.Now().Add(24 * time.Hour)
	setMTime(t, filepath.Join(src, "a.ts"), tOld)
	setMTime(t, filepath.Join(src, "node_modules", "pkg", "very-new.ts"), tFuture)
	setMTime(t, filepath.Join(dist, "a.js"), tOld.Add(time.Minute))

	if err := checkRuntimeStaleness(src, dist, "runtime"); err != nil {
		t.Errorf("expected nil error — newest non-node_modules src is older than dist; got: %v", err)
	}
}

func TestNewestMtime_NoMatchingFiles_Errors(t *testing.T) {
	root := t.TempDir()
	setMTime(t, filepath.Join(root, "irrelevant.txt"), time.Now())
	_, err := newestMtime(root, ".ts", nil)
	if err == nil {
		t.Errorf("expected error when no matching files exist")
	}
}

func TestCheckRuntimeStaleness_TestOnlyEdit_NoError(t *testing.T) {
	// Codex pass-1 finding (slice 0.3): editing only a *.test.ts file
	// must NOT trigger staleness — tsconfig excludes those from the
	// dist build, so they are not build inputs.
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dist := filepath.Join(root, "dist")
	tOld := time.Now().Add(-2 * time.Hour)
	tNew := time.Now().Add(-1 * time.Minute)
	setMTime(t, filepath.Join(src, "adapter.ts"), tOld)
	setMTime(t, filepath.Join(dist, "adapter.js"), tOld.Add(time.Minute))
	// Touch a test file with a very-new mtime. Should NOT count as
	// staleness because it's not a build input.
	setMTime(t, filepath.Join(src, "adapter.test.ts"), tNew)

	if err := checkRuntimeStaleness(src, dist, "runtime"); err != nil {
		t.Errorf("expected nil error — test-only edit should not be a build input; got: %v", err)
	}
}

func TestNewestMtime_ExcludeSuffixes(t *testing.T) {
	root := t.TempDir()
	tOld := time.Now().Add(-1 * time.Hour)
	tNew := time.Now()
	setMTime(t, filepath.Join(root, "a.ts"), tOld)
	setMTime(t, filepath.Join(root, "b.test.ts"), tNew)

	got, err := newestMtime(root, ".ts", []string{".test.ts"})
	if err != nil {
		t.Fatalf("newestMtime: %v", err)
	}
	// With .test.ts excluded, the newest .ts file is a.ts (tOld).
	if !got.Equal(tOld) {
		t.Errorf("got mtime %s, want %s (a.ts, with b.test.ts excluded)", got, tOld)
	}
}
