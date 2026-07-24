package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Slice 7.4 — file-handoff input primitives: `--input KEY=@<path>` and
// `--input-file <path>`.

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp %s: %v", name, err)
	}
	return p
}

func TestParseInputFlagsReadsAtFile(t *testing.T) {
	p := writeTemp(t, "snippet.txt", "the previous step's result")
	got, err := parseInputFlags([]string{"text=@" + p})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if got["text"] != "the previous step's result" {
		t.Errorf("got %q", got["text"])
	}
}

func TestParseInputFlagsAtFilePreservesExactBytes(t *testing.T) {
	// No trimming — a round-trip through --output-file → --input must be
	// lossless, including a trailing newline.
	p := writeTemp(t, "out.json", "{\"label\":\"positive\"}\n")
	got, err := parseInputFlags([]string{"prev=@" + p})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if got["prev"] != "{\"label\":\"positive\"}\n" {
		t.Errorf("trailing newline not preserved: %q", got["prev"])
	}
}

func TestParseInputFlagsAtFileEmptyPathErrors(t *testing.T) {
	_, err := parseInputFlags([]string{"text=@"})
	if err == nil {
		t.Fatal("expected error for KEY=@ with no path, got nil")
	}
	if !strings.Contains(err.Error(), "no file path") {
		t.Errorf("error should explain the missing path; got %q", err.Error())
	}
}

func TestParseInputFlagsAtFileMissingErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")
	_, err := parseInputFlags([]string{"text=@" + missing})
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestParseInputFlagsAtFileExceedsCapErrors(t *testing.T) {
	big := strings.Repeat("a", maxInputFileBytes+1)
	p := writeTemp(t, "big.txt", big)
	_, err := parseInputFlags([]string{"text=@" + p})
	if err == nil {
		t.Fatal("expected error for oversized file, got nil")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error should mention the cap; got %q", err.Error())
	}
}

func TestParseInputFlagsAtFileAtCapBoundarySucceeds(t *testing.T) {
	// Exactly maxInputFileBytes must be accepted (off-by-one guard).
	atCap := strings.Repeat("a", maxInputFileBytes)
	p := writeTemp(t, "atcap.txt", atCap)
	got, err := parseInputFlags([]string{"text=@" + p})
	if err != nil {
		t.Fatalf("file exactly at cap should be accepted: %v", err)
	}
	if len(got["text"]) != maxInputFileBytes {
		t.Errorf("got %d bytes, want %d", len(got["text"]), maxInputFileBytes)
	}
}

func TestParseInputFlagsAtOnlyTriggersOnLeadingAt(t *testing.T) {
	// A value containing `@` not at the start is a literal, not a file
	// reference (e.g. an email address).
	got, err := parseInputFlags([]string{"email=user@example.com"})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if got["email"] != "user@example.com" {
		t.Errorf("got %q", got["email"])
	}
}

func TestMergeInputFileBasic(t *testing.T) {
	p := writeTemp(t, "in.json", `{"topic":"AI","persona":"expert"}`)
	inputs := map[string]string{}
	if err := mergeInputFile(inputs, p); err != nil {
		t.Fatalf("mergeInputFile: %v", err)
	}
	if inputs["topic"] != "AI" || inputs["persona"] != "expert" {
		t.Errorf("got %+v", inputs)
	}
}

func TestMergeInputFileCoercesScalars(t *testing.T) {
	p := writeTemp(t, "in.json", `{"count":42,"ratio":0.5,"enabled":true,"disabled":false}`)
	inputs := map[string]string{}
	if err := mergeInputFile(inputs, p); err != nil {
		t.Fatalf("mergeInputFile: %v", err)
	}
	if inputs["count"] != "42" {
		t.Errorf("count: got %q want 42", inputs["count"])
	}
	if inputs["ratio"] != "0.5" {
		t.Errorf("ratio: got %q want 0.5", inputs["ratio"])
	}
	if inputs["enabled"] != "true" || inputs["disabled"] != "false" {
		t.Errorf("bool coercion: %+v", inputs)
	}
}

func TestMergeInputFilePreservesLargeIntegerPrecision(t *testing.T) {
	// json.Number keeps the exact textual form; without it a Snowflake-
	// style id would round through float64.
	p := writeTemp(t, "in.json", `{"id":9007199254740993}`)
	inputs := map[string]string{}
	if err := mergeInputFile(inputs, p); err != nil {
		t.Fatalf("mergeInputFile: %v", err)
	}
	if inputs["id"] != "9007199254740993" {
		t.Errorf("large int lost precision: %q", inputs["id"])
	}
}

func TestMergeInputFileRejectsCrossChannelDuplicate(t *testing.T) {
	p := writeTemp(t, "in.json", `{"topic":"fromfile"}`)
	inputs := map[string]string{"topic": "fromflag"} // already set via --input
	err := mergeInputFile(inputs, p)
	if err == nil {
		t.Fatal("expected cross-channel duplicate error, got nil")
	}
	if !strings.Contains(err.Error(), "both --input and --input-file") {
		t.Errorf("error should name the conflict; got %q", err.Error())
	}
	if inputs["topic"] != "fromflag" {
		t.Errorf("flag value should be untouched on error; got %q", inputs["topic"])
	}
}

func TestMergeInputFileRejectsInvalidKey(t *testing.T) {
	p := writeTemp(t, "in.json", `{"bad-key":"v"}`)
	if err := mergeInputFile(map[string]string{}, p); err == nil {
		t.Fatal("expected error for invalid key, got nil")
	}
}

func TestMergeInputFileRejectsNonObject(t *testing.T) {
	for _, body := range []string{`[1,2,3]`, `42`, `"a string"`} {
		p := writeTemp(t, "in.json", body)
		if err := mergeInputFile(map[string]string{}, p); err == nil {
			t.Errorf("expected error for non-object %q, got nil", body)
		}
	}
}

func TestMergeInputFileErrorDoesNotLeakScalarContents(t *testing.T) {
	// A mis-pointed --input-file holding a secret as a top-level JSON
	// string must NOT have that secret echoed into the error (scheduler
	// stderr is logged). Codex pass 4 of slice 7.4.
	secret := "sk-super-secret-token-value-1234567890"
	p := writeTemp(t, "in.json", `"`+secret+`"`)
	err := mergeInputFile(map[string]string{}, p)
	if err == nil {
		t.Fatal("expected error for JSON string (non-object), got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked file contents: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "JSON string") {
		t.Errorf("error should name the kind; got %q", err.Error())
	}
}

func TestDescribeJSONToken(t *testing.T) {
	cases := map[string]string{
		`[1]`:  "a JSON array",
		`"x"`:  "a JSON string",
		`42`:   "a JSON number",
		`true`: "a JSON boolean",
		`null`: "JSON null",
	}
	for body, want := range cases {
		_, err := decodeInputObject([]byte(body))
		if err == nil {
			t.Errorf("%s: expected non-object error", body)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q should contain %q", body, err.Error(), want)
		}
	}
}

func TestMergeInputFileRejectsNull(t *testing.T) {
	p := writeTemp(t, "in.json", `null`)
	if err := mergeInputFile(map[string]string{}, p); err == nil {
		t.Fatal("expected error for top-level null, got nil")
	}
}

func TestMergeInputFileRejectsNestedNonScalar(t *testing.T) {
	for _, body := range []string{
		`{"k":[1,2]}`,
		`{"k":{"nested":1}}`,
		`{"k":null}`,
	} {
		p := writeTemp(t, "in.json", body)
		if err := mergeInputFile(map[string]string{}, p); err == nil {
			t.Errorf("expected error for non-scalar value %q, got nil", body)
		}
	}
}

func TestMergeInputFileRejectsTrailingContent(t *testing.T) {
	// The trailing bytes could be a prior step's secret; the error must
	// report only that trailing content exists, not echo it. Codex pass 5
	// of slice 7.4.
	secret := "sk-trailing-secret-9876543210"
	p := writeTemp(t, "in.json", `{"a":"1"} `+secret)
	err := mergeInputFile(map[string]string{}, p)
	if err == nil {
		t.Fatal("expected error for trailing content, got nil")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Errorf("error should mention trailing content; got %q", err.Error())
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked trailing bytes: %q", err.Error())
	}
}

func TestMergeInputFileExceedsCapErrors(t *testing.T) {
	big := `{"k":"` + strings.Repeat("a", maxInputFileBytes) + `"}`
	p := writeTemp(t, "big.json", big)
	if err := mergeInputFile(map[string]string{}, p); err == nil {
		t.Fatal("expected cap error, got nil")
	}
}

func TestMergeInputFileEmptyObjectIsNoOp(t *testing.T) {
	p := writeTemp(t, "in.json", `{}`)
	inputs := map[string]string{"existing": "v"}
	if err := mergeInputFile(inputs, p); err != nil {
		t.Fatalf("empty object should be a no-op: %v", err)
	}
	if len(inputs) != 1 || inputs["existing"] != "v" {
		t.Errorf("empty object changed the map: %+v", inputs)
	}
}

// End-to-end: a file value + a JSON file together feed interpolation.
func TestFileInputsFlowIntoInterpolation(t *testing.T) {
	snippet := writeTemp(t, "snippet.txt", "hello world")
	jsonFile := writeTemp(t, "in.json", `{"topic":"AI"}`)

	inputs, err := parseInputFlags([]string{"text=@" + snippet})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if err := mergeInputFile(inputs, jsonFile); err != nil {
		t.Fatalf("mergeInputFile: %v", err)
	}
	out, unused, err := interpolateInputs("Topic ${inputs.topic}: ${inputs.text}", inputs)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	if out != "Topic AI: hello world" {
		t.Errorf("got %q", out)
	}
	if len(unused) != 0 {
		t.Errorf("unexpected unused keys: %v", unused)
	}
}

func TestMergeInputFileRejectsDuplicateKeys(t *testing.T) {
	// json.Unmarshal into a map would silently keep "b"; we reject it to
	// match the no-last-wins rule for repeated --input flags. Codex pass 1
	// of slice 7.4.
	p := writeTemp(t, "dup.json", `{"topic":"a","topic":"b"}`)
	err := mergeInputFile(map[string]string{}, p)
	if err == nil {
		t.Fatal("expected duplicate-key error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate key") || !strings.Contains(err.Error(), "topic") {
		t.Errorf("error should name the duplicate key; got %q", err.Error())
	}
}

func TestDecodeInputObjectRejectsDuplicateKeys(t *testing.T) {
	if _, err := decodeInputObject([]byte(`{"a":1,"b":2,"a":3}`)); err == nil {
		t.Fatal("expected duplicate-key error, got nil")
	}
}

func TestDecodeInputObjectAcceptsDistinctKeys(t *testing.T) {
	obj, err := decodeInputObject([]byte(`{"a":1,"b":"two","c":true}`))
	if err != nil {
		t.Fatalf("decodeInputObject: %v", err)
	}
	if len(obj) != 3 {
		t.Errorf("got %d keys, want 3: %+v", len(obj), obj)
	}
}

func TestShouldInterpolateInputs(t *testing.T) {
	// Intent comes from the flags, not the resulting map.
	if shouldInterpolateInputs(nil, "") {
		t.Error("no flags → should NOT interpolate (in-Pod child path)")
	}
	if !shouldInterpolateInputs([]string{"topic=AI"}, "") {
		t.Error("--input given → should interpolate")
	}
	if !shouldInterpolateInputs(nil, "params.json") {
		t.Error("--input-file given (even if it yields {}) → should interpolate")
	}
}

func TestEmptyInputFileStillTriggersInterpolationValidation(t *testing.T) {
	// The empty-object case from codex pass 1: --input-file {} yields no
	// keys, but a ${inputs.foo} reference must still fail rather than be
	// sent literally. Simulate the run-command flow: merge {} then gate on
	// intent.
	p := writeTemp(t, "empty.json", `{}`)
	inputs := map[string]string{}
	if err := mergeInputFile(inputs, p); err != nil {
		t.Fatalf("mergeInputFile: %v", err)
	}
	if !shouldInterpolateInputs(nil, p) {
		t.Fatal("an explicit --input-file must request interpolation")
	}
	if _, _, err := interpolateInputs("Use ${inputs.foo}", inputs); err == nil {
		t.Fatal("expected missing-input error for ${inputs.foo} with empty --input-file, got nil")
	}
}

// minimalAgentSpec is a valid kind:Agent with no tools/extensions/skills,
// so parseValidateCompile resolves it against an empty registry without
// needing project fixtures.
const minimalAgentSpec = `apiVersion: agent-controller.dev/v1alpha1
kind: Agent
metadata:
  name: t
spec:
  model:
    provider: anthropic
    name: claude-sonnet-4-6
  task: "do ${inputs.foo}"
  tools: []
  runtime:
    type: local
`

func TestRunRejectsEmptyInputFileFlag(t *testing.T) {
	// An explicit `--input-file ""` (e.g. a wrapper expanding an unset
	// var) must error rather than silently skip the file + interpolation
	// path. Exercises the cobra Changed("input-file") guard in RunE.
	// Codex pass 4 of slice 7.4.
	spec := writeTemp(t, "agent.yaml", minimalAgentSpec)
	cmd := newRunCmd()
	cmd.SetArgs([]string{spec, "--input-file", ""})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for empty --input-file, got nil")
	}
	if !strings.Contains(err.Error(), "empty path") {
		t.Errorf("error should mention the empty path; got %q", err.Error())
	}
}

func TestOutputAlreadyExists(t *testing.T) {
	dir := t.TempDir()

	// Existing regular file → true.
	f := filepath.Join(dir, "result.json")
	if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := outputAlreadyExists(f); err != nil || !ok {
		t.Errorf("existing file: got (%v, %v), want (true, nil)", ok, err)
	}

	// Non-existent path → false, no error.
	if ok, err := outputAlreadyExists(filepath.Join(dir, "missing.json")); err != nil || ok {
		t.Errorf("missing file: got (%v, %v), want (false, nil)", ok, err)
	}

	// Directory → error (misconfiguration).
	if _, err := outputAlreadyExists(dir); err == nil {
		t.Error("directory should be an error, got nil")
	}
}

func TestReadCappedFileFollowsSymlinkToRegular(t *testing.T) {
	// A symlink to a regular file is a legitimate input handoff target —
	// only the resolved type matters for reading.
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	data, err := readCappedFile(link)
	if err != nil {
		t.Fatalf("symlink-to-regular should be readable: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("got %q", data)
	}
}

func TestOutputAlreadyExistsRejectsSymlink(t *testing.T) {
	// os.Lstat (not Stat): a symlink at --output-file is NOT evidence the
	// step succeeded — writeOutputFile would replace the symlink. Codex
	// pass 2 of slice 7.4.
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	ok, err := outputAlreadyExists(link)
	if err == nil {
		t.Fatal("symlink output path should be a configuration error, got nil")
	}
	if ok {
		t.Error("symlink must not report as an existing regular output")
	}
}

// Guard: readCappedFile uses cap+1 LimitReader; make sure a file one byte
// over the cap is rejected and the boundary math is exact.
func TestReadCappedFileBoundary(t *testing.T) {
	over := writeTemp(t, "over.bin", strings.Repeat("x", maxInputFileBytes+1))
	if _, err := readCappedFile(over); err == nil {
		t.Error("cap+1 bytes should be rejected")
	}
	exact := writeTemp(t, "exact.bin", strings.Repeat("x", maxInputFileBytes))
	data, err := readCappedFile(exact)
	if err != nil {
		t.Fatalf("exact-cap file should be accepted: %v", err)
	}
	if !bytes.Equal(data, []byte(strings.Repeat("x", maxInputFileBytes))) {
		t.Error("exact-cap content mismatch")
	}
}
