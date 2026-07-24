package main

import (
	"strings"
	"testing"
)

// Slice 7.1 — `--input k=v` parsing + `${inputs.<key>}` interpolation.

func TestParseInputFlagsBasic(t *testing.T) {
	got, err := parseInputFlags([]string{"topic=AI", "persona=expert"})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if got["topic"] != "AI" || got["persona"] != "expert" {
		t.Errorf("got %+v", got)
	}
}

func TestParseInputFlagsAcceptsEmptyValue(t *testing.T) {
	// `KEY=` is valid — empty string is a real input. Useful for
	// flags that toggle behavior in the task template.
	got, err := parseInputFlags([]string{"flag="})
	if err != nil {
		t.Fatalf("empty value should be allowed: %v", err)
	}
	if got["flag"] != "" {
		t.Errorf("expected empty value, got %q", got["flag"])
	}
}

func TestParseInputFlagsAcceptsEqualsInValue(t *testing.T) {
	// Values like base64 or URLs commonly contain `=`. The parser
	// splits on the FIRST `=` only; everything after is part of
	// the value.
	got, err := parseInputFlags([]string{"token=abc=def=="})
	if err != nil {
		t.Fatalf("parseInputFlags: %v", err)
	}
	if got["token"] != "abc=def==" {
		t.Errorf("value lost equals signs: %q", got["token"])
	}
}

func TestParseInputFlagsRejectsMissingEquals(t *testing.T) {
	_, err := parseInputFlags([]string{"topic"})
	if err == nil {
		t.Fatal("expected error for missing =, got nil")
	}
	if !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("error should mention KEY=VALUE; got %q", err.Error())
	}
}

func TestParseInputFlagsRejectsEmptyKey(t *testing.T) {
	_, err := parseInputFlags([]string{"=value"})
	if err == nil {
		t.Fatal("expected error for empty key, got nil")
	}
}

func TestParseInputFlagsRejectsInvalidKey(t *testing.T) {
	// Keys must match [A-Za-z_][A-Za-z0-9_]* — operators trying to
	// use dotted/dashed keys learn early that those won't work in
	// the `${inputs.X}` reference.
	for _, bad := range []string{
		"foo.bar=v",
		"foo-bar=v",
		"123=v",      // starts with digit
		"foo bar=v",  // space
	} {
		_, err := parseInputFlags([]string{bad})
		if err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}

func TestParseInputFlagsRejectsDuplicateKey(t *testing.T) {
	// Last-wins would be silent and easy to get wrong from a shell
	// wrapper. Explicit error pushes the bug back onto the caller.
	_, err := parseInputFlags([]string{"topic=A", "topic=B"})
	if err == nil {
		t.Fatal("expected error on duplicate key, got nil")
	}
	if !strings.Contains(err.Error(), "topic") {
		t.Errorf("error should name the duplicate key; got %q", err.Error())
	}
}

func TestInterpolateInputsBasicSubstitution(t *testing.T) {
	got, unused, err := interpolateInputs(
		"Research \"${inputs.topic}\" for the ${inputs.audience} audience.",
		map[string]string{"topic": "AI safety", "audience": "engineering"},
	)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	want := `Research "AI safety" for the engineering audience.`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if len(unused) != 0 {
		t.Errorf("unexpected unused keys: %v", unused)
	}
}

func TestInterpolateInputsRepeatedReferenceWorks(t *testing.T) {
	// Same key referenced twice should substitute twice (not be
	// flagged unused on the second pass).
	got, unused, err := interpolateInputs(
		"${inputs.topic} is the topic; remember: ${inputs.topic}.",
		map[string]string{"topic": "X"},
	)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	if got != "X is the topic; remember: X." {
		t.Errorf("got %q", got)
	}
	if len(unused) != 0 {
		t.Errorf("unexpected unused: %v", unused)
	}
}

func TestInterpolateInputsMissingKeyIsError(t *testing.T) {
	_, _, err := interpolateInputs(
		"Research ${inputs.topic} for ${inputs.audience}.",
		map[string]string{"topic": "AI"}, // missing "audience"
	)
	if err == nil {
		t.Fatal("expected error for missing input key")
	}
	if !strings.Contains(err.Error(), "audience") {
		t.Errorf("error should name the missing key; got %q", err.Error())
	}
}

func TestInterpolateInputsAllMissingKeysReported(t *testing.T) {
	// Operator should see EVERY missing key in one pass — fix all,
	// rerun. One-at-a-time error iteration is the wrong UX.
	_, _, err := interpolateInputs(
		"${inputs.a} ${inputs.b} ${inputs.c}",
		map[string]string{}, // all missing
	)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing key %q; got %q", want, err.Error())
		}
	}
}

func TestInterpolateInputsUnusedKeysReturned(t *testing.T) {
	// Extra `--input` keys not referenced in the task surface as
	// `unused` for the caller to warn about. Sorted for stable
	// output. Not an error — the operator might intend the key as
	// reserved for a future task version.
	_, unused, err := interpolateInputs(
		"Research ${inputs.topic}",
		map[string]string{"topic": "AI", "zzz": "last", "aaa": "first"},
	)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	if len(unused) != 2 || unused[0] != "aaa" || unused[1] != "zzz" {
		t.Errorf("expected sorted [aaa zzz], got %v", unused)
	}
}

func TestInterpolateInputsNoReferencesReturnsInputUnchanged(t *testing.T) {
	got, unused, err := interpolateInputs("just a literal task", nil)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	if got != "just a literal task" {
		t.Errorf("got %q", got)
	}
	if len(unused) != 0 {
		t.Errorf("unused: %v", unused)
	}
}

func TestInterpolateInputsRejectsMalformedPlaceholders(t *testing.T) {
	// Codex pass 1 of slice 7.1: anything looking like an input
	// reference but structurally invalid must fail FAST, not slip
	// through to the model as literal text.
	cases := []struct {
		name string
		task string
	}{
		{"dash in key", "Research ${inputs.topic-name}."},
		{"dot in key", "Research ${inputs.topic.name}."},
		{"space in key", "Research ${inputs.topic name}."},
		{"empty key", "Research ${inputs.}."},
		{"no closing brace", "Research ${inputs.topic without close"},
		{"digit-leading key", "Research ${inputs.123topic}."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := interpolateInputs(tc.task, map[string]string{"topic": "AI"})
			if err == nil {
				t.Fatalf("expected error for malformed placeholder; task=%q", tc.task)
			}
			if !strings.Contains(err.Error(), "malformed input placeholder") {
				t.Errorf("error should mention malformed input placeholder; got %q", err.Error())
			}
		})
	}
}

func TestInterpolateInputsTreatsValuesAsOpaque(t *testing.T) {
	// Codex pass 2 of slice 7.1: input VALUES that legitimately
	// contain `${inputs.foo}` text (e.g. a code snippet the
	// scheduler wants to pass through verbatim) must NOT be
	// rejected by the malformed-placeholder check. Single-pass
	// substitution; values are NOT recursively interpolated.
	got, unused, err := interpolateInputs(
		"Render this snippet: ${inputs.snippet}",
		map[string]string{"snippet": "before ${inputs.foo} after"},
	)
	if err != nil {
		t.Fatalf("opaque value rejected: %v", err)
	}
	want := "Render this snippet: before ${inputs.foo} after"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if len(unused) != 0 {
		t.Errorf("unexpected unused: %v", unused)
	}
}

func TestInterpolateInputsResolvesEmptyValueToEmpty(t *testing.T) {
	// Codex pass 2 of slice 7.1: this is the precondition the
	// newRunCmd empty-task guard relies on. A template that is
	// JUST `${inputs.prompt}` with `--input prompt=` resolves to
	// "" — and the RunE guard then re-enforces the schema's
	// `task` minLength=1 invariant (which ran BEFORE
	// interpolation). Whitespace-around-empty also collapses to
	// trimmed-empty, hence TrimSpace in the guard.
	got, _, err := interpolateInputs("${inputs.prompt}", map[string]string{"prompt": ""})
	if err != nil {
		t.Fatalf("empty value should interpolate, not error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
	got, _, err = interpolateInputs(" ${inputs.x} ", map[string]string{"x": ""})
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Errorf("trimmed got %q, want empty", got)
	}
}

func TestInterpolateInputsLeavesUnrelatedDollarBracesAlone(t *testing.T) {
	// Only `${inputs.<key>}` is interpolated. Other shell-like
	// syntax (`$HOME`, `${OTHER_VAR}`, `$1`) is left literal so we
	// don't accidentally substitute outside our namespace.
	got, _, err := interpolateInputs(
		"Read $HOME or ${OTHER_VAR} and use ${inputs.topic}.",
		map[string]string{"topic": "X"},
	)
	if err != nil {
		t.Fatalf("interpolateInputs: %v", err)
	}
	want := "Read $HOME or ${OTHER_VAR} and use X."
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
