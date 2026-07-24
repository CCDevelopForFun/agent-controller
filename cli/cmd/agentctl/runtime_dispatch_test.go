package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRuntimeCommand_DispatchesByRuntimeType pins the dispatch
// table for spec.runtime.type. Slice 2.1 added local-opencode alongside
// the legacy local / local-pi values. Each case sets up a fake working
// directory containing the expected dist file and asserts that the
// chosen argv points at it.
func TestResolveRuntimeCommand_DispatchesByRuntimeType(t *testing.T) {
	cases := []struct {
		name            string
		runtimeType     string
		expectedDistRel string
	}{
		{"empty (legacy default)", "", filepath.Join("runtime", "dist", "index.js")},
		{"local (legacy v0.1.x alias)", "local", filepath.Join("runtime", "dist", "index.js")},
		{"local-pi", "local-pi", filepath.Join("runtime", "dist", "index.js")},
		{"local-opencode", "local-opencode", filepath.Join("runtime-opencode", "dist", "index.js")},
	}

	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)

	originalRuntimeEnv := os.Getenv("AGENT_CONTROLLER_RUNTIME")
	defer os.Setenv("AGENT_CONTROLLER_RUNTIME", originalRuntimeEnv)
	os.Unsetenv("AGENT_CONTROLLER_RUNTIME")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Resolve symlinks so the comparison matches on macOS where
			// /tmp → /private/tmp. os.Getwd inside the SUT returns the
			// canonical form; t.TempDir returns the symlinked form.
			tmp, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatalf("evalsymlinks: %v", err)
			}
			// Create both candidate dist files so the test doesn't accidentally
			// pass by virtue of the wrong one being absent.
			for _, rel := range []string{
				filepath.Join("runtime", "dist", "index.js"),
				filepath.Join("runtime-opencode", "dist", "index.js"),
			} {
				full := filepath.Join(tmp, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(full, []byte("// stub"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := os.Chdir(tmp); err != nil {
				t.Fatalf("chdir: %v", err)
			}

			argv, err := resolveRuntimeCommand(tc.runtimeType)
			if err != nil {
				t.Fatalf("resolveRuntimeCommand(%q): %v", tc.runtimeType, err)
			}
			if len(argv) != 2 || argv[0] != "node" {
				t.Errorf("expected [node, <path>], got %v", argv)
			}
			expected := filepath.Join(tmp, tc.expectedDistRel)
			if argv[1] != expected {
				t.Errorf("runtime type %q dispatched to %s, want %s", tc.runtimeType, argv[1], expected)
			}
		})
	}
}

func TestResolveRuntimeCommand_RejectsUnknownType(t *testing.T) {
	tmp := t.TempDir()
	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmp)

	originalRuntimeEnv := os.Getenv("AGENT_CONTROLLER_RUNTIME")
	defer os.Setenv("AGENT_CONTROLLER_RUNTIME", originalRuntimeEnv)
	os.Unsetenv("AGENT_CONTROLLER_RUNTIME")

	_, err := resolveRuntimeCommand("local-bogus")
	if err == nil {
		t.Fatal("expected error for unknown runtime type")
	}
	if !strings.Contains(err.Error(), "local-bogus") {
		t.Errorf("error %q should mention the bogus type", err)
	}
	if !strings.Contains(err.Error(), "local-opencode") {
		t.Errorf("error %q should list the supported values", err)
	}
}

func TestResolveRuntimeCommand_HonorsRuntimeEnvOverride(t *testing.T) {
	originalRuntimeEnv := os.Getenv("AGENT_CONTROLLER_RUNTIME")
	defer os.Setenv("AGENT_CONTROLLER_RUNTIME", originalRuntimeEnv)

	override := "/tmp/some/custom/runtime.js"
	os.Setenv("AGENT_CONTROLLER_RUNTIME", override)

	// Any runtime type — env var trumps.
	for _, rt := range []string{"local", "local-pi", "local-opencode", "anything-else"} {
		argv, err := resolveRuntimeCommand(rt)
		if err != nil {
			t.Errorf("resolveRuntimeCommand(%q) with env override: %v", rt, err)
			continue
		}
		if argv[1] != override {
			t.Errorf("env override should win for runtime type %q; got %s", rt, argv[1])
		}
	}
}
