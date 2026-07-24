package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// checkRuntimeStaleness compares the newest mtime under srcDir against the
// newest mtime under distDir. Returns nil when dist is fresh, a descriptive
// error otherwise.
//
// Closes Round-2 finding #4: for several v0.1.x slices we tested against
// a stale runtime/dist/ without realizing it, then chased phantom bugs in
// "fixed" code that wasn't actually being run. This check refuses to launch
// when source is newer than the built artifact, so the mistake fails loudly.
//
// Behavior contract:
//   - srcDir missing/unreadable → return nil (likely an install where only
//     dist/ ships; no staleness to detect).
//   - distDir missing/unreadable → return an error pointing to the build.
//   - Both present, src newest > dist newest → return a "stale dist" error.
//   - Both present, dist >= src → return nil.
//
// Bypass paths handled by the caller:
//   - AGENT_CONTROLLER_RUNTIME env var set (user explicitly chose a binary).
//   - --no-staleness-check CLI flag (hot-reload workflows; intentionally
//     awkward name to discourage casual use).
//
// adapterName is the package directory name (e.g. "runtime" or
// "runtime-opencode") and is interpolated into the error messages so a
// user following the suggestion rebuilds the correct adapter. Slice 2.1
// added the multi-adapter dispatch; before that this helper hardcoded
// "runtime" in its diagnostics and would have misled users targeting
// runtime-opencode.
func checkRuntimeStaleness(srcDir, distDir, adapterName string) error {
	// Match the .ts scan to the build inputs. tsconfig.json under each
	// adapter excludes src/**/*.test.ts from emit, so test-only edits
	// never invalidate dist/ and shouldn't trigger staleness.
	srcMtime, err := newestMtime(srcDir, ".ts", []string{".test.ts"})
	if err != nil {
		// srcDir missing or unreadable — likely an install where only dist/
		// ships. Skip the check rather than false-alarm.
		return nil
	}
	distMtime, err := newestMtime(distDir, ".js", nil)
	if err != nil {
		return fmt.Errorf(
			"%s/dist/ missing or unreadable (%s); run `npm --prefix %s run build` and retry",
			adapterName, err, adapterName,
		)
	}
	if srcMtime.After(distMtime) {
		return fmt.Errorf(
			"%s/dist/ is stale: newest source file mtime %s is more recent than newest dist file mtime %s.\n"+
				"Run `npm --prefix %s run build` and retry.\n"+
				"Pass --no-staleness-check to bypass (only useful for hot-reload workflows; the check exists because v0.1.x stress-testing wasted time on stale-dist debugging).",
			adapterName, srcMtime.Format(time.RFC3339), distMtime.Format(time.RFC3339), adapterName,
		)
	}
	return nil
}

// newestMtime walks dir for files matching ext (e.g. ".ts" or ".js") and
// returns the newest mtime found. Skips node_modules directories so package
// installs don't dominate the result. Returns an error when dir doesn't
// exist or contains no matching files.
//
// Files whose names match any of the excludeSuffixes are skipped. Used by
// the .ts scan to skip *.test.ts because runtime/tsconfig.json explicitly
// excludes test files from the dist build — editing only a test file
// shouldn't trigger staleness. Pass nil to scan every file with the
// extension (used by the .js scan).
func newestMtime(dir, ext string, excludeSuffixes []string) (time.Time, error) {
	var newest time.Time
	found := false
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// node_modules can contain millions of files and their mtimes
			// reflect install time, not our codebase. Skip them.
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ext) {
			return nil
		}
		for _, ex := range excludeSuffixes {
			if strings.HasSuffix(path, ex) {
				return nil
			}
		}
		info, ferr := d.Info()
		if ferr != nil {
			return ferr
		}
		m := info.ModTime()
		if !found || m.After(newest) {
			newest = m
			found = true
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return time.Time{}, fmt.Errorf("no %s files found under %s", ext, dir)
	}
	return newest, nil
}
