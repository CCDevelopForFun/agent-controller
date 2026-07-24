package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/workspace"
)

// v0.7 acceptance: prove the slices compose into the end-to-end DAG story
// the release is built around — one step's OUTPUT becomes the next step's
// INPUT, and durable MEMORY survives across runs. These drive the real
// shipped helpers (finalizeOutput / parseInputFlags / interpolateInputs /
// the workspace store), so a regression in any one slice fails the
// capstone, not just a unit test. Mirrors slice 6.6's v06 acceptance.

// Step 1 captures a result with --output-file; step 2 consumes it with
// --input KEY=@<path> and interpolates it into spec.task. (slices 7.2 + 7.4 + 7.1)
func TestV07AcceptanceOutputToInputHandoff(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "step1-result.txt")

	// Step 1: the agent's final message is written verbatim (no schema).
	if err := finalizeOutput(resultPath, "sentiment: positive", nil); err != nil {
		t.Fatalf("step 1 finalizeOutput: %v", err)
	}

	// Step 2: a scheduler wires step 1's output file in as an input value.
	inputs, err := parseInputFlags([]string{"prev=@" + resultPath})
	if err != nil {
		t.Fatalf("step 2 parseInputFlags: %v", err)
	}
	out, _, err := interpolateInputs("Given the previous result: ${inputs.prev}", inputs)
	if err != nil {
		t.Fatalf("step 2 interpolateInputs: %v", err)
	}
	// finalizeOutput appends a trailing newline (POSIX text convention);
	// the @-file value is the exact bytes, so it round-trips with it.
	want := "Given the previous result: sentiment: positive\n"
	if out != want {
		t.Errorf("handoff mismatch:\n got %q\nwant %q", out, want)
	}
}

// Step 1 remembers structured state in the workspace; step 2 (a separate
// run pointing --workspace at the same dir) recalls it. (slice 7.5)
func TestV07AcceptanceWorkspaceMemoryAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Run 1.
	w1, err := workspace.Open(dir)
	if err != nil {
		t.Fatalf("run 1 open: %v", err)
	}
	if err := w1.Remember(ctx, "label", "positive"); err != nil {
		t.Fatalf("run 1 remember: %v", err)
	}
	if err := w1.AppendNote("classified the input as positive"); err != nil {
		t.Fatalf("run 1 note: %v", err)
	}
	_ = w1.Close()

	// Run 2: a later step reuses the same workspace dir.
	w2, err := workspace.Open(dir)
	if err != nil {
		t.Fatalf("run 2 open: %v", err)
	}
	defer w2.Close()
	val, found, err := w2.Recall(ctx, "label")
	if err != nil || !found || val != "positive" {
		t.Fatalf("run 2 recall: val=%q found=%v err=%v", val, found, err)
	}
	// The journal from run 1 is visible as a durable output to run 2.
	outs, err := w2.ListOutputs()
	if err != nil {
		t.Fatalf("run 2 list outputs: %v", err)
	}
	var sawNotes bool
	for _, o := range outs {
		if o.Name == workspace.NotesFileName {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Errorf("run 2 should see run 1's notes.md in outputs; got %+v", outs)
	}
}

// Idempotency: --skip-if-output-exists treats an existing result as done. (slice 7.4)
func TestV07AcceptanceSkipIfOutputExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")
	if err := os.WriteFile(path, []byte(`{"done":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	exists, err := outputAlreadyExists(path)
	if err != nil || !exists {
		t.Fatalf("a prior successful output should be detected: exists=%v err=%v", exists, err)
	}
}
