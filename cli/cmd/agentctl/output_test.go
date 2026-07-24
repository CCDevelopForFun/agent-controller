package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Slice 7.2 — `--output-file` capture + outputSchema extraction.

// mustCompileSchema is a test helper that compiles raw a JSON Schema map
// to the *jsonschema.Schema finalizeOutput now expects. Codex pass 3 of
// slice 7.2 moved compilation upstream so config errors fail before the
// run; tests still compile here for ergonomics.
func mustCompileSchema(t *testing.T, raw map[string]any) *jsonschema.Schema {
	t.Helper()
	s, err := compileOutputSchema(raw)
	if err != nil {
		t.Fatalf("compileOutputSchema: %v", err)
	}
	return s
}

func TestExtractJSONPayloadUnfenced(t *testing.T) {
	// Bare JSON should pass through unchanged after trimming.
	got := extractJSONPayload(`  {"a":1}  `)
	if got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
}

func TestExtractJSONPayloadStripsJsonFence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"lowercase json", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"uppercase JSON", "```JSON\n{\"a\":1}\n```", `{"a":1}`},
		{"no language tag", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"surrounding whitespace", "\n  ```json\n{\"a\":1}\n```\n  ", `{"a":1}`},
		{"multi-line body", "```json\n{\n  \"a\": 1\n}\n```", "{\n  \"a\": 1\n}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSONPayload(tc.in)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestExtractJSONPayloadLeavesProseAlone(t *testing.T) {
	// We do NOT try to extract JSON from prose. The agent task is
	// responsible for asking the model to reply with JSON; if the
	// model returns prose, the downstream json.Unmarshal will fail
	// with a clear error.
	in := "Here is the result: {\"a\":1}. Hope that helps!"
	got := extractJSONPayload(in)
	if got != in {
		t.Errorf("extractJSONPayload mangled prose: got %q", got)
	}
}

func TestFinalizeOutputNoOpWhenPathEmpty(t *testing.T) {
	if err := finalizeOutput("", "hello", nil); err != nil {
		t.Errorf("empty path should be a no-op, got %v", err)
	}
}

func TestFinalizeOutputErrorsOnEmptyAssistantMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	cases := []string{"", "   ", "\n\n"}
	for _, msg := range cases {
		err := finalizeOutput(path, msg, nil)
		if err == nil {
			t.Errorf("empty assistant message %q should error", msg)
		}
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("output file should not exist after empty-assistant error; stat err=%v", statErr)
		}
	}
}

func TestFinalizeOutputWritesPlainTextWhenNoSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := finalizeOutput(path, "the answer is 42", nil); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Trailing newline appended per POSIX text-file convention.
	want := "the answer is 42\n"
	if string(got) != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestFinalizeOutputPreservesExistingTrailingNewline(t *testing.T) {
	// If the assistant already ended with a newline, don't double up.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := finalizeOutput(path, "line1\nline2\n", nil); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "line1\nline2\n" {
		t.Errorf("expected single trailing newline, got %q", got)
	}
}

func TestFinalizeOutputCreatesParentDirectories(t *testing.T) {
	// Schedulers commonly point at a fresh-workspace path that doesn't
	// exist yet — `mkdir -p`-style behavior keeps that ergonomic.
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeply", "out.txt")
	if err := finalizeOutput(path, "hi", nil); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestFinalizeOutputFileIs0600(t *testing.T) {
	// Output may contain sensitive task results; tighten perms so a
	// shared host doesn't expose another user to whatever the agent
	// returned.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := finalizeOutput(path, "secret", nil); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("got perm %o want 0600", mode)
	}
}

func TestFinalizeOutputValidatesAgainstSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{
		"type":     "object",
		"required": []any{"label", "confidence"},
		"properties": map[string]any{
			"label":      map[string]any{"type": "string", "enum": []any{"positive", "negative"}},
			"confidence": map[string]any{"type": "number", "minimum": float64(0), "maximum": float64(1)},
		},
	}
	msg := "```json\n{\"label\":\"positive\",\"confidence\":0.87}\n```"
	if err := finalizeOutput(path, msg, mustCompileSchema(t, schema)); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, _ := os.ReadFile(path)
	// Re-marshaled pretty JSON; just assert the parse round-trips.
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if parsed["label"] != "positive" {
		t.Errorf("got label=%v want positive", parsed["label"])
	}
	if parsed["confidence"] != 0.87 {
		t.Errorf("got confidence=%v want 0.87", parsed["confidence"])
	}
	// File should end with a trailing newline (json.Encoder.Encode does this).
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("file should end with newline; got %q", got)
	}
}

func TestFinalizeOutputRejectsInvalidJSONWhenSchemaSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{"type": "object"}
	err := finalizeOutput(path, "this is not JSON at all", mustCompileSchema(t, schema))
	if err == nil {
		t.Fatal("expected error for non-JSON output")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error should mention non-JSON; got %q", err.Error())
	}
	// File MUST NOT be written when validation fails — the scheduler
	// would otherwise consume a half-baked or empty payload.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("output file should not exist after validation failure; stat err=%v", statErr)
	}
}

func TestFinalizeOutputRejectsSchemaViolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{
		"type":     "object",
		"required": []any{"label"},
		"properties": map[string]any{
			"label": map[string]any{"type": "string", "enum": []any{"positive", "negative"}},
		},
	}
	// JSON is well-formed but `label` is not in the enum.
	err := finalizeOutput(path, `{"label":"WAT"}`, mustCompileSchema(t, schema))
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "outputSchema validation") {
		t.Errorf("error should mention outputSchema validation; got %q", err.Error())
	}
}

func TestFinalizeOutputAtomicWriteCleansUpTempOnFailure(t *testing.T) {
	// When validation fails, the temp file MUST be removed. Otherwise
	// repeated failures pile .agentctl-output-*.tmp into the dir.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{
		"type":     "object",
		"required": []any{"x"},
	}
	_ = finalizeOutput(path, "not json", mustCompileSchema(t, schema))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentctl-output-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestFinalizeOutputAtomicWriteIsAtomic(t *testing.T) {
	// Sanity check that writeOutputFile uses rename (no intermediate
	// partial-content state visible at <path>). We simulate by first
	// writing an existing file, then succeeding a second write, and
	// asserting the final content matches.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("OLD CONTENT"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := finalizeOutput(path, "new", nil); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("got %q want %q", got, "new\n")
	}
}

func TestFinalizeOutputEmptySchemaStillForcesJSONParse(t *testing.T) {
	// Codex pass 1 of slice 7.2: `outputSchema: {}` is valid JSON
	// Schema (accepts any JSON value). The compiler must NOT drop it
	// (`ok` only, no `len > 0` gate), AND finalizeOutput must treat
	// a non-nil empty map as "schema mode" so non-JSON output is
	// rejected. Without this, a user who wrote `outputSchema: {}` to
	// require JSON syntax would get raw-text capture and never know
	// the model failed to produce JSON.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	emptySchema := map[string]any{} // valid JSON Schema, no constraints
	err := finalizeOutput(path, "definitely not JSON", mustCompileSchema(t, emptySchema))
	if err == nil {
		t.Fatal("expected error: empty schema should still force JSON parse")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("error should mention non-JSON; got %q", err.Error())
	}
}

func TestFinalizeOutputPreservesLargeIntegerPrecision(t *testing.T) {
	// Codex pass 1 of slice 7.2: integers above 2^53 (e.g. Snowflake
	// IDs, blockchain values, GH issue ids on huge repos) must
	// survive the parse → validate → re-marshal round trip
	// LOSSLESSLY. With json.Unmarshal into `any`, 9007199254740993
	// rounds to 9007199254740992 (the nearest float64), silently
	// corrupting the scheduler payload. UseNumber prevents this.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{"type": "object"}
	const bigInt = "9007199254740993" // 2^53 + 1; not representable as float64
	msg := `{"id":` + bigInt + `}`
	if err := finalizeOutput(path, msg, mustCompileSchema(t, schema)); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, _ := os.ReadFile(path)
	// Read the file as raw bytes; if precision was lost the file
	// would contain `9007199254740992` instead of `9007199254740993`.
	if !strings.Contains(string(got), bigInt) {
		t.Errorf("large integer precision lost; file contents: %s", got)
	}
}

func TestFinalizeOutputPreservesHighPrecisionDecimal(t *testing.T) {
	// Same precision concern as the large-int test, for decimals
	// that can't round-trip through float64 (e.g. financial amounts
	// formatted to many significant digits).
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{"type": "object"}
	const highPrec = "0.1234567890123456789"
	msg := `{"price":` + highPrec + `}`
	if err := finalizeOutput(path, msg, mustCompileSchema(t, schema)); err != nil {
		t.Fatalf("finalizeOutput: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), highPrec) {
		t.Errorf("decimal precision lost; file contents: %s", got)
	}
}

func TestFinalizeOutputRejectsTrailingContentAfterJSON(t *testing.T) {
	// Codex pass 2 of slice 7.2 expanded the cases here. `dec.More()`
	// is array/object-iteration-only and returns false when the next
	// byte is `]` or `}`; we need to inspect bytes past
	// InputOffset() to catch stray closing delimiters too.
	dir := t.TempDir()
	schema := map[string]any{"type": "object"}
	cases := []struct {
		name string
		msg  string
	}{
		{"prose tail", `{"a":1} trailing garbage`},
		{"stray closing bracket", `{"a":1}]`},
		{"stray closing brace", `{"a":1}}`},
		{"second value", `{"a":1} {"b":2}`},
		{"stray comma", `{"a":1},`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			err := finalizeOutput(path, tc.msg, mustCompileSchema(t, schema))
			if err == nil {
				t.Fatalf("expected error; msg=%q", tc.msg)
			}
			if !strings.Contains(err.Error(), "trailing content") {
				t.Errorf("error should mention trailing content; got %q", err.Error())
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("output file should not exist; stat err=%v", statErr)
			}
		})
	}
}

func TestFinalizeOutputAllowsTrailingWhitespaceAfterJSON(t *testing.T) {
	// Whitespace after the JSON value is normal (a trailing newline
	// from the model is common). It must NOT count as trailing
	// content.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	schema := map[string]any{"type": "object"}
	for _, msg := range []string{
		`{"a":1}`,
		"{\"a\":1}\n",
		"{\"a\":1}\n\n  \t",
		"   {\"a\":1}   ",
	} {
		if err := finalizeOutput(path, msg, mustCompileSchema(t, schema)); err != nil {
			t.Errorf("whitespace tail rejected: msg=%q err=%v", msg, err)
		}
	}
}

func TestPrepareOutputCaptureNoOpWhenOutputFileMissing(t *testing.T) {
	// No output file → no schema compilation, no error even if a
	// schema is set. Validation is gated on a consumer being present.
	schema := map[string]any{"type": "object"}
	got, err := prepareOutputCapture("", &schema)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil compiled schema, got %v", got)
	}
}

func TestPrepareOutputCaptureNoOpWhenSchemaMissing(t *testing.T) {
	// Output file but no schema → plain-text capture mode; no
	// compiled schema needed.
	got, err := prepareOutputCapture("/tmp/out.txt", nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil compiled schema, got %v", got)
	}
}

func TestPrepareOutputCaptureCompilesValidSchema(t *testing.T) {
	schema := map[string]any{"type": "object"}
	got, err := prepareOutputCapture("/tmp/out.json", &schema)
	if err != nil {
		t.Fatalf("prepareOutputCapture: %v", err)
	}
	if got == nil {
		t.Fatal("expected compiled schema, got nil")
	}
}

func TestPrepareOutputCaptureFailsFastOnInvalidSchema(t *testing.T) {
	// Codex pass 3 of slice 7.2: malformed `spec.outputSchema` must
	// fail BEFORE the backend runs so a typo doesn't spend tokens
	// and execute tools before the deterministic error surfaces.
	bad := map[string]any{"type": 12345} // type must be a string
	_, err := prepareOutputCapture("/tmp/out.json", &bad)
	if err == nil {
		t.Fatal("expected pre-run compile error for invalid schema")
	}
	if !strings.Contains(err.Error(), "compile spec.outputSchema") {
		t.Errorf("error should mention compile spec.outputSchema; got %q", err.Error())
	}
}

func TestPrepareOutputCaptureAcceptsEmptySchema(t *testing.T) {
	// Codex pass 1 of slice 7.2: empty schema is meaningful (any
	// JSON). Make sure preflight doesn't reject it.
	empty := map[string]any{}
	got, err := prepareOutputCapture("/tmp/out.json", &empty)
	if err != nil {
		t.Fatalf("empty schema rejected: %v", err)
	}
	if got == nil {
		t.Fatal("expected compiled schema for empty schema, got nil")
	}
}

func TestCompiledSpecJSONMarshalPreservesEmptyOutputSchema(t *testing.T) {
	// Codex pass 3 of slice 7.2: `omitempty` on a bare
	// `map[string]any` would drop a zero-length map at JSON
	// marshal time, so `agentctl compile` (which marshals
	// CompiledSpec to JSON) would lose the empty-schema
	// distinction. The field is now `*map[string]any` so
	// omitempty only fires on nil pointer.
	empty := map[string]any{}
	spec := struct {
		OutputSchema *map[string]any `json:"outputSchema,omitempty"`
	}{
		OutputSchema: &empty,
	}
	got, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"outputSchema":{}}`
	if string(got) != want {
		t.Errorf("got %s want %s", got, want)
	}

	// Conversely, a nil pointer DOES still get omitted.
	specNil := struct {
		OutputSchema *map[string]any `json:"outputSchema,omitempty"`
	}{}
	got, _ = json.Marshal(specNil)
	if string(got) != "{}" {
		t.Errorf("nil pointer not omitted: got %s", got)
	}
}

func TestCompileOutputSchemaRejectsInvalidSchema(t *testing.T) {
	// A schema with a malformed `type` value should fail compilation
	// rather than silently accept-all.
	_, err := compileOutputSchema(map[string]any{"type": 12345})
	if err == nil {
		t.Fatal("expected compilation error for invalid schema")
	}
}
