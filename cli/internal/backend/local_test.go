package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// fakeRuntime is a tiny shell script the test uses in place of the real
// Node runtime: it reads the CompiledSpec from stdin and emits a fixed
// sequence of wire events.
const fakeRuntime = `#!/usr/bin/env bash
read -r FIRST < /dev/stdin
cat <<'EOF'
{"v":1,"type":"session.started","ts":"2026-05-25T00:00:00Z","sessionId":"s1","data":{}}
{"v":1,"type":"session.ended","ts":"2026-05-25T00:00:01Z","sessionId":"s1","data":{"reason":"completed"}}
EOF
`

func TestLocalBackendStreamsEvents(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "fake-runtime.sh")
	if err := os.WriteFile(runtimePath, []byte(fakeRuntime), 0o755); err != nil {
		t.Fatal(err)
	}

	be := NewLocalBackend(LocalConfig{RuntimeCommand: []string{"bash", runtimePath}})

	spec := adl.CompiledSpec{V: 1, Metadata: adl.SpecMetadata{Name: "test"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := be.Submit(ctx, adl.ResolvedRunSpec{Spec: spec})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	events := []wire.Event{}
	for ev := range be.Events(h) {
		events = append(events, ev)
	}

	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(events), events)
	}
	if events[0].Type != wire.EventSessionStarted {
		t.Errorf("event[0].Type = %q", events[0].Type)
	}
	if events[1].Type != wire.EventSessionEnded {
		t.Errorf("event[1].Type = %q", events[1].Type)
	}

	var ended struct{ Reason string }
	_ = json.Unmarshal(events[1].Data, &ended)
	if ended.Reason != "completed" {
		t.Errorf("ended.Reason = %q", ended.Reason)
	}
}

// crashRuntime exits non-zero without emitting any wire events. The
// LocalBackend should synthesize an error event so the CLI never reports
// a failed run as successful.
const crashRuntime = `#!/usr/bin/env bash
read -r FIRST < /dev/stdin
echo "import error: missing module" >&2
exit 7
`

// oversizeRuntime emits a single line longer than the scanner's 1 MiB buffer
// limit, which causes bufio.Scanner.Scan() to return false with a non-nil
// scanner.Err(). Without the fix the events channel would close silently;
// with the fix a synthetic error event should be emitted.
const oversizeRuntime = `#!/usr/bin/env bash
read -r FIRST < /dev/stdin
# Emit one line larger than 1 MiB so the scanner overflows and errors.
head -c 2000000 /dev/zero | tr '\0' 'x'
echo ""
`

func TestLocalBackendEmitsErrorOnRuntimeCrash(t *testing.T) {
	dir := t.TempDir()
	rPath := filepath.Join(dir, "crash.sh")
	if err := os.WriteFile(rPath, []byte(crashRuntime), 0o755); err != nil {
		t.Fatal(err)
	}
	be := NewLocalBackend(LocalConfig{RuntimeCommand: []string{"bash", rPath}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := be.Submit(ctx, adl.ResolvedRunSpec{Spec: adl.CompiledSpec{V: 1}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var sawError bool
	for ev := range be.Events(h) {
		if ev.Type == wire.EventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected a synthetic error event on crashing runtime")
	}
}

// TestLocalBackendEmitsErrorOnScannerOverflow verifies that when the scanner
// fails due to an oversized line (bufio.ErrTooLong) and session.ended was never
// emitted, a synthetic error event is surfaced rather than silently closing the
// events channel. Without the scanner.Err() check the CLI would exit 0 on a
// truncated run.
func TestLocalBackendEmitsErrorOnScannerOverflow(t *testing.T) {
	dir := t.TempDir()
	rPath := filepath.Join(dir, "oversize.sh")
	if err := os.WriteFile(rPath, []byte(oversizeRuntime), 0o755); err != nil {
		t.Fatal(err)
	}
	be := NewLocalBackend(LocalConfig{RuntimeCommand: []string{"bash", rPath}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := be.Submit(ctx, adl.ResolvedRunSpec{Spec: adl.CompiledSpec{V: 1}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var sawError bool
	for ev := range be.Events(h) {
		if ev.Type == wire.EventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected a synthetic error event when scanner overflows on oversized line")
	}
}

// v0.3.3a established the no-op pass-through for nil binding.
// v0.3.3b activates the matcher: selector check + capability check +
// warn-but-proceed or strict-fail. These tests cover the resolver
// contract end-to-end.

func TestLocalBackendResolveNilBindingIsNoOp(t *testing.T) {
	be := NewLocalBackend(LocalConfig{RuntimeCommand: []string{"bash", "-c", "true"}})
	ctx := context.Background()

	spec := adl.CompiledSpec{V: 1, Metadata: adl.SpecMetadata{Name: "test"},
		Runtime: adl.RuntimeConfig{Type: "local"}}

	run, warnings, err := be.Resolve(ctx, spec, nil)
	if err != nil {
		t.Fatalf("Resolve(nil binding): %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("Resolve(nil binding): expected 0 warnings, got %d", len(warnings))
	}
	if run.Binding != nil {
		t.Errorf("Resolve(nil binding): expected nil binding, got %+v", run.Binding)
	}
	if run.Spec.Metadata.Name != "test" {
		t.Errorf("Resolve(nil binding): spec round-trip mismatch: %+v", run.Spec)
	}
}

// helper: builds a matching spec + binding pair so individual tests can
// vary requirements/capabilities/strict without retyping the boilerplate.
func makeSpecAndBinding(reqs map[string]bool, caps map[string]bool, strict bool) (adl.CompiledSpec, *adl.RuntimeBinding) {
	spec := adl.CompiledSpec{
		V: 1, Metadata: adl.SpecMetadata{Name: "test"},
		Runtime: adl.RuntimeConfig{Type: "local-pi", Requirements: reqs},
	}
	binding := &adl.RuntimeBinding{
		APIVersion: "agent-controller.dev/v1alpha1",
		Kind:       "RuntimeBinding",
		Metadata:   adl.RuntimeBindingMeta{Name: "test-binding"},
		Spec: adl.RuntimeBindingSpec{
			Selector: adl.RuntimeBindingSelector{
				RuntimeType:  "local-pi",
				Capabilities: caps,
			},
			Target: adl.RuntimeBindingTarget{Type: "local", Strict: strict},
		},
	}
	return spec, binding
}

func TestLocalBackendResolveAllRequirementsMet(t *testing.T) {
	be := NewLocalBackend(LocalConfig{})
	spec, binding := makeSpecAndBinding(
		map[string]bool{"streaming": true, "sandbox": true},
		map[string]bool{"streaming": true, "sandbox": true, "gpu": false},
		false,
	)
	run, warnings, err := be.Resolve(context.Background(), spec, binding)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings when all requirements met, got %d", len(warnings))
	}
	if run.Binding == nil {
		t.Errorf("expected binding to pass through")
	}
}

func TestLocalBackendResolveWarnsOnUnmetRequirement(t *testing.T) {
	be := NewLocalBackend(LocalConfig{})
	spec, binding := makeSpecAndBinding(
		map[string]bool{"streaming": true, "sandbox": true, "gpu": true},
		map[string]bool{"streaming": true}, // missing sandbox + gpu
		false,                              // warn-but-proceed
	)
	_, warnings, err := be.Resolve(context.Background(), spec, binding)
	if err != nil {
		t.Fatalf("warn-but-proceed should not return an error, got %v", err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings (gpu, sandbox), got %d", len(warnings))
	}
	// Warnings must be sorted alphabetically for stable output (gpu < sandbox).
	for i, want := range []string{"gpu", "sandbox"} {
		var payload struct {
			Kind        string `json:"kind"`
			Requirement string `json:"requirement"`
		}
		if err := json.Unmarshal(warnings[i].Data, &payload); err != nil {
			t.Fatalf("unmarshal warning[%d]: %v", i, err)
		}
		if payload.Kind != "unmet_runtime_requirement" {
			t.Errorf("warning[%d].Kind = %q", i, payload.Kind)
		}
		if payload.Requirement != want {
			t.Errorf("warning[%d].Requirement = %q, want %q", i, payload.Requirement, want)
		}
	}
}

func TestLocalBackendResolveIgnoresFalseRequirements(t *testing.T) {
	be := NewLocalBackend(LocalConfig{})
	spec, binding := makeSpecAndBinding(
		map[string]bool{"sandbox": false, "gpu": false}, // explicitly false
		map[string]bool{},                               // binding advertises nothing
		false,
	)
	_, warnings, err := be.Resolve(context.Background(), spec, binding)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings — requirements: false means \"do NOT require it\", got %d warnings", len(warnings))
	}
}

func TestLocalBackendResolveStrictModeFailsClosed(t *testing.T) {
	be := NewLocalBackend(LocalConfig{})
	spec, binding := makeSpecAndBinding(
		map[string]bool{"sandbox": true},
		map[string]bool{}, // doesn't advertise sandbox
		true,              // strict mode
	)
	_, warnings, err := be.Resolve(context.Background(), spec, binding)
	if err == nil {
		t.Fatalf("strict mode + unmet requirement should return an error")
	}
	if len(warnings) != 0 {
		t.Errorf("strict mode should not also emit warnings (errors are the loud channel), got %d", len(warnings))
	}
	if got := err.Error(); got == "" || !contains(got, "sandbox") || !contains(got, "strict") {
		t.Errorf("error message should name the unmet capability and mention strict mode: %q", got)
	}
}

func TestLocalBackendResolveSelectorMismatchIsHardError(t *testing.T) {
	be := NewLocalBackend(LocalConfig{})
	spec, binding := makeSpecAndBinding(nil, nil, false)
	// Override binding to declare a different runtime type than the spec.
	binding.Spec.Selector.RuntimeType = "local-opencode"
	// spec.Runtime.Type stays "local-pi" from the helper.

	_, _, err := be.Resolve(context.Background(), spec, binding)
	if err == nil {
		t.Fatalf("selector mismatch should be a hard error regardless of strict mode")
	}
	if got := err.Error(); !contains(got, "selector does not match") {
		t.Errorf("error should mention selector mismatch: %q", got)
	}
}

// tiny helper since strings.Contains isn't imported here yet
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
