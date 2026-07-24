package main

// Slice 7.1 — `agentctl run --input k=v` parameterization.
//
// The Option-B v0.7 direction (see ROADMAP.md): agentctl is the
// agent runtime; external schedulers (Maestro, Airflow, Temporal)
// are the orchestrators. The scheduler dispatches an Agent run per
// task, parameterizing the task at the CLI boundary via repeated
// `--input k=v` flags.
//
// Interpolation surface (v0.7.1): `${inputs.<key>}` substitution in
// `spec.task` only. Other CompiledSpec fields are NOT interpolated
// in this slice — the most-common parameterization is "the same
// agent runs against different topics / inputs", which is
// task-shaped. Persona / tools / models change agent identity and
// should live in distinct Agent YAMLs.
//
// Semantics:
//   - Each `--input k=v` adds one key/value pair. The flag is
//     repeatable (`--input topic=AI --input persona=expert`).
//   - References are `${inputs.<key>}` where `<key>` matches
//     `[A-Za-z_][A-Za-z0-9_]*`. Keys outside this pattern are
//     rejected at flag-parse time so `${inputs.foo bar}` can't
//     accidentally match.
//   - A referenced key that has no `--input` is an error
//     (interpolation fails the run; better than silently sending
//     `${inputs.missing}` to the model as a literal).
//   - A provided `--input` key that doesn't appear in any
//     interpolated string is a warning to stderr (probably an
//     operator typo, but not fatal — the value might be reserved
//     for a future field).
//   - No escaping syntax. If an operator legitimately needs the
//     literal text `${inputs.X}` in their task they can split the
//     string. Adding escape rules now would lock us into a syntax
//     we may regret.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// maxInputFileBytes caps both the `--input KEY=@<path>` value form and
// the `--input-file <path>` JSON document (slice 7.4). 1 MiB is generous
// for a DAG handoff (a previous step's text/JSON result) while bounding
// the memory a single run reads from operator-controlled files. A handoff
// larger than this is a smell — pass a reference (path/URI) instead of
// inlining the payload into the prompt.
const maxInputFileBytes = 1 << 20 // 1 MiB

// inputKeyPattern restricts `--input` keys to identifier shape so a
// loose flag value like `--input "foo bar=baz"` doesn't introduce a
// key that can't be referenced via `${inputs.foo bar}` (regex would
// reject the reference but the flag would have been accepted —
// confusing).
var inputKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// inputsRefPattern matches `${inputs.<key>}` and captures <key>.
// Greedy by design — `${inputs.foo}` not `${inputs.foo.bar}` —
// because dotted-path access would require type semantics we're
// not committing to yet.
var inputsRefPattern = regexp.MustCompile(`\$\{inputs\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// inputsMalformedPattern catches text that LOOKS like an input
// reference but isn't well-formed enough to match inputsRefPattern
// — e.g. `${inputs.topic-name}` (dash isn't allowed in keys),
// `${inputs.}` (empty key), `${inputs.topic` (no closing brace).
// Codex pass 1 of slice 7.1 caught the silent-pass-through bug:
// without this guard, malformed placeholders slip through unchanged
// and reach the model as literal text — the LLM produces a
// confused response instead of the operator seeing a clear error.
// Run AFTER inputsRefPattern's substitution; any leftover match is
// by definition something we didn't (couldn't) recognize.
var inputsMalformedPattern = regexp.MustCompile(`\$\{inputs\.[^}]*\}?`)

// parseInputFlags converts repeated `--input k=v` flag values into a
// map. Validates key shape and duplicate-key rules. A duplicate key
// is an error (rather than last-wins) because last-wins is silent
// and easy to get wrong from a shell wrapper script.
//
// Slice 7.4: a value of the form `@<path>` reads the input value from a
// file (capped at maxInputFileBytes) instead of taking the literal text.
// This is the DAG file-handoff path — a previous step writes its result
// with `--output-file`, the next step consumes it with
// `--input text=@/shared/prev-result.json`. The file's exact bytes
// become the value (no trimming) so a round-trip through --output-file →
// --input is lossless.
func parseInputFlags(raw []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range raw {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return nil, fmt.Errorf(
				"--input must be KEY=VALUE with KEY non-empty (got %q)", kv)
		}
		key := kv[:eq]
		val := kv[eq+1:]
		if !inputKeyPattern.MatchString(key) {
			return nil, fmt.Errorf(
				"--input KEY must match [A-Za-z_][A-Za-z0-9_]* (got %q)", key)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf(
				"--input %s specified more than once; pass each key only once", key)
		}
		resolved, err := resolveInputValue(val)
		if err != nil {
			return nil, fmt.Errorf("--input %s: %w", key, err)
		}
		out[key] = resolved
	}
	return out, nil
}

// shouldInterpolateInputs reports whether `${inputs.<key>}` interpolation
// should run, based on caller INTENT (an input flag was supplied) rather
// than on whether the resulting inputs map is non-empty.
//
// This matters in two directions:
//   - An explicit `--input-file {}` (empty object) yields an empty map but
//     is still intent: a `${inputs.foo}` reference must then fail for the
//     missing key, not pass through to the model literally. Codex pass 1
//     of slice 7.4.
//   - The in-Pod child launched by KubernetesBackend passes NEITHER flag
//     (it runs the already-rendered spec), so it must stay off the
//     interpolation path to preserve opaque `${inputs.X}` values in the
//     rendered task. Codex pass 3 of slice 7.1.
func shouldInterpolateInputs(inputFlags []string, inputFile string) bool {
	return len(inputFlags) > 0 || inputFile != ""
}

// resolveInputValue returns the literal value, or — when val is `@<path>`
// — the contents of that file (slice 7.4). There is deliberately NO
// escaping syntax to pass a literal value beginning with `@` (consistent
// with slice 7.1's no-escapes-yet stance): if you need a literal leading
// `@`, supply the value through `--input-file` instead, whose JSON values
// are always literal. An empty value (`KEY=`) has no `@` prefix and stays
// a valid empty input.
func resolveInputValue(val string) (string, error) {
	if !strings.HasPrefix(val, "@") {
		return val, nil
	}
	path := val[1:]
	if path == "" {
		return "", fmt.Errorf(
			"value starts with @ but no file path follows (use KEY=@<path>)")
	}
	data, err := readCappedFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readCappedFile reads path, failing if it exceeds maxInputFileBytes.
// Uses an io.LimitReader of cap+1 so an oversized file is detected
// without reading the whole thing into memory first.
//
// Stats the path BEFORE opening (codex pass 2 of slice 7.4): opening a
// FIFO/socket for read can block indefinitely — a read-side FIFO open
// waits for a writer — so the LimitReader never gets a chance to bound
// anything and a scheduler task would hang. Rejecting non-regular files
// up front keeps these handoff flags fast-failing. os.Stat follows
// symlinks here, so a symlink TO a regular file remains a valid input
// (reading through a handoff symlink is legitimate); only the resolved
// target type matters.
func readCappedFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read input file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf(
			"input file %q is not a regular file (%s)", path, info.Mode().Type())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read input file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxInputFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read input file %q: %w", path, err)
	}
	if len(data) > maxInputFileBytes {
		return nil, fmt.Errorf(
			"input file %q exceeds the %d-byte cap; pass a reference instead of inlining",
			path, maxInputFileBytes)
	}
	return data, nil
}

// mergeInputFile reads a JSON object from path and merges its entries
// into inputs (slice 7.4). This is the multi-input handoff path — one
// upstream step emits a JSON object of parameters that a downstream
// `agentctl run --input-file params.json` consumes in one shot.
//
// Rules:
//   - The document must be a single JSON object. Scalar values (string,
//     number, bool) are accepted; a number keeps its exact textual form
//     (json.Number) so large ids / decimals survive losslessly. Arrays,
//     objects, and null are rejected — they can't interpolate into the
//     text of spec.task.
//   - Keys must match inputKeyPattern, same as `--input`.
//   - A key present in BOTH --input and --input-file is a hard error
//     (cross-channel duplicate) — silently letting one win is the kind
//     of ambiguity slice 7.1 already rejects for repeated --input keys.
//
// inputs is mutated in place (the --input map parsed first), so the
// duplicate check sees flag-supplied keys.
func mergeInputFile(inputs map[string]string, path string) error {
	data, err := readCappedFile(path)
	if err != nil {
		// Reword the generic readCappedFile prefix for the flag context.
		return fmt.Errorf("--input-file: %w", err)
	}
	obj, err := decodeInputObject(data)
	if err != nil {
		return fmt.Errorf("--input-file %q: %w", path, err)
	}
	// Sort keys so error messages and processing order are deterministic.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !inputKeyPattern.MatchString(k) {
			return fmt.Errorf(
				"--input-file %q key %q must match [A-Za-z_][A-Za-z0-9_]*", path, k)
		}
		if _, dup := inputs[k]; dup {
			return fmt.Errorf(
				"input %q provided via both --input and --input-file; pass it through only one", k)
		}
		s, err := coerceInputScalar(obj[k])
		if err != nil {
			return fmt.Errorf("--input-file %q key %q: %w", path, k, err)
		}
		inputs[k] = s
	}
	return nil
}

// decodeInputObject parses data as a single JSON object. It rejects, in
// addition to non-object documents:
//   - DUPLICATE member keys. json.Unmarshal into a map silently keeps the
//     last value, which would reintroduce the last-wins ambiguity slice
//     7.1 rejects for repeated --input flags and cross-channel
//     duplicates. We walk the object with a token stream so a repeated
//     key is a hard error.
//   - TRAILING content after the object (e.g. `{"a":1} junk`), the same
//     defense slice 7.2 applies to captured output.
//
// Numbers decode to json.Number (the decoder has UseNumber set) so large
// ids / high-precision decimals survive losslessly through to
// coerceInputScalar.
func decodeInputObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("must be a JSON object: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		// Describe the KIND, never the value: a mis-pointed --input-file
		// could be a JSON string holding a prior step's secret output, and
		// scheduler stderr is commonly logged. Codex pass 4 of slice 7.4.
		return nil, fmt.Errorf("must be a JSON object, not %s", describeJSONToken(tok))
	}

	obj := map[string]any{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			// Unreachable for well-formed JSON (object keys are always
			// strings), but guard rather than type-assert blindly. Don't
			// print the token value (defense against content leaks).
			return nil, fmt.Errorf("expected a string object key")
		}
		if _, dup := obj[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		// Decode the value (respects UseNumber; consumes nested
		// arrays/objects whole so the token cursor lands on the next key).
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		obj[key] = v
	}
	// Consume the closing '}'.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if rest := bytes.TrimSpace(data[dec.InputOffset():]); len(rest) > 0 {
		// Report only the LENGTH, never the bytes: trailing content after a
		// valid object could be a prior step's secret output, and scheduler
		// stderr is commonly logged. Codex pass 5 of slice 7.4 (consistent
		// with the pass-4 non-object fix).
		return nil, fmt.Errorf("%d bytes of trailing content after the JSON object", len(rest))
	}
	return obj, nil
}

// describeJSONToken names the KIND of a JSON token without revealing its
// value — used for error messages about a mis-pointed --input-file, whose
// contents may be sensitive and end up in scheduler logs.
func describeJSONToken(tok json.Token) string {
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			return "a JSON array"
		}
		return "a JSON value"
	case string:
		return "a JSON string"
	case json.Number:
		return "a JSON number"
	case bool:
		return "a JSON boolean"
	case nil:
		return "JSON null"
	default:
		return "a non-object JSON value"
	}
}

// coerceInputScalar converts a JSON scalar to its string form for the
// inputs map. Non-scalars (array/object/null) are rejected — inputs
// interpolate into the text of spec.task, which has no meaning for a
// structured value.
func coerceInputScalar(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "", fmt.Errorf("value is null (expected a string, number, or bool)")
	default:
		return "", fmt.Errorf("value must be a string, number, or bool, not %T", v)
	}
}

// interpolateInputs substitutes `${inputs.<key>}` references in s
// with values from the inputs map. Returns the interpolated string
// and a list of input keys that were NEVER referenced (caller
// surfaces these as warnings to stderr). Returns an error listing
// EVERY missing referenced key — the operator gets one message
// covering all typos instead of one-error-at-a-time iteration.
//
// Malformed placeholder validation runs against the INPUT TEMPLATE,
// not the rendered output. Codex pass 2 of slice 7.1 caught the
// inversion: input VALUES are opaque payloads; a scheduler
// legitimately passing `--input snippet='${inputs.foo}'` (a code
// fragment containing literal `${inputs.foo}` text) must not be
// rejected just because the substituted value happens to look like
// a placeholder. Single-pass substitution; no recursive
// interpolation of input values.
func interpolateInputs(s string, inputs map[string]string) (string, []string, error) {
	// Pass 0: scan the TEMPLATE (not the rendered output) for
	// malformed placeholders. Any `${inputs.` fragment that isn't
	// a well-formed `${inputs.<valid-key>}` is a structural error
	// in the task definition itself.
	if leftovers := findMalformedInputPlaceholders(s); len(leftovers) > 0 {
		return "", nil, fmt.Errorf(
			"spec.task contains malformed input placeholder(s): %s "+
				"(keys must match [A-Za-z_][A-Za-z0-9_]* and be wrapped in `${inputs.KEY}`)",
			strings.Join(leftovers, ", "))
	}

	used := map[string]struct{}{}
	var missing []string

	out := inputsRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		// inputsRefPattern guarantees the match has the shape
		// `${inputs.<key>}`. FindStringSubmatch re-runs the regex
		// on the match to extract <key> — small overhead, but
		// keeps the substitution callback readable.
		sub := inputsRefPattern.FindStringSubmatch(match)
		key := sub[1]
		val, ok := inputs[key]
		if !ok {
			missing = append(missing, key)
			// Leave the placeholder in place so the error message
			// can quote the rendered (still-broken) string for
			// debugging. ReplaceAllStringFunc requires a return.
			return match
		}
		used[key] = struct{}{}
		return val
	})

	if len(missing) > 0 {
		// Dedupe + sort for a stable error message. Operators who
		// see "missing inputs: [a b c]" should be able to fix all
		// three in one pass.
		sort.Strings(missing)
		uniq := missing[:0]
		var prev string
		for _, k := range missing {
			if k != prev {
				uniq = append(uniq, k)
				prev = k
			}
		}
		return out, nil, fmt.Errorf(
			"spec.task references unknown inputs: %s "+
				"(provide via --input KEY=VALUE)",
			strings.Join(uniq, ", "))
	}

	// Build the unused-keys list for the warning surface. Sorted
	// for determinism.
	var unused []string
	for k := range inputs {
		if _, hit := used[k]; !hit {
			unused = append(unused, k)
		}
	}
	sort.Strings(unused)
	return out, unused, nil
}

// findMalformedInputPlaceholders returns deduped + sorted occurrences
// of `${inputs.` text in s that DON'T match a well-formed
// `${inputs.<valid-key>}` reference. Used by interpolateInputs to
// validate the template BEFORE substitution (codex pass 2 of slice
// 7.1 — must not validate against the rendered output, which would
// reject input values that happen to contain `${inputs.` text).
func findMalformedInputPlaceholders(s string) []string {
	candidates := inputsMalformedPattern.FindAllString(s, -1)
	if len(candidates) == 0 {
		return nil
	}
	// Filter out the well-formed ones — those are NOT malformed.
	wellFormed := map[string]struct{}{}
	for _, m := range inputsRefPattern.FindAllString(s, -1) {
		wellFormed[m] = struct{}{}
	}
	seen := map[string]struct{}{}
	var bad []string
	for _, c := range candidates {
		if _, ok := wellFormed[c]; ok {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		bad = append(bad, c)
	}
	sort.Strings(bad)
	return bad
}
