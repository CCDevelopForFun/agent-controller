package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Slice 7.2 of v0.7 (Option-B pivot): scheduler-friendly result capture.
//
// `agentctl run --output-file <path>` writes the agent's last assistant
// message to <path> on a successful run. When the spec also declares
// `outputSchema`, the captured text is parsed as JSON, validated against
// the schema, and the (re-marshaled, validated) JSON is written instead
// — failing the run if extraction or validation fails.
//
// Design choices:
//
//   - We capture the LAST assistant message, not all of them. Single-turn
//     runs have exactly one; multi-turn runs (rare for `agentctl run`,
//     which is one-shot) write the final reply. Chat mode uses
//     `agentctl chat`, not `run`, so this isn't surprising.
//
//   - We strip a single ```json ... ``` fence if the assistant wrapped
//     the JSON in one. We do NOT try to parse partial JSON, look at
//     other fences, or extract from prose. The agent task is
//     responsible for asking the model to reply with JSON; light
//     fence-stripping is the only accommodation.
//
//   - Atomic writes: write to <path>.tmp + rename. Schedulers that
//     concurrently read the path see either the old contents or the
//     full new contents, never a partial write.

// jsonFencePattern matches an opening ```json (or ```JSON) line + content
// + a closing ``` line. Tolerates trailing whitespace on the fence lines.
// Multiline match required; the `(?s)` flag lets `.` cross newlines.
var jsonFencePattern = regexp.MustCompile("(?si)^\\s*```(?:json)?\\s*\\n(.*?)\\n?```\\s*$")

// extractJSONPayload returns the JSON portion of an assistant message.
// If the entire message is fenced as ```json … ``` (with optional whitespace
// padding), returns the unfenced content. Otherwise returns the original
// text unchanged — the caller will hand it to json.Unmarshal which
// surfaces a clear error if the content isn't JSON.
func extractJSONPayload(msg string) string {
	trimmed := strings.TrimSpace(msg)
	if m := jsonFencePattern.FindStringSubmatch(trimmed); m != nil {
		return strings.TrimSpace(m[1])
	}
	return trimmed
}

// writeOutputFile writes content to path atomically (write tmp + rename).
// Creates parent directories if missing — schedulers commonly point at
// a path under a fresh workspace dir that doesn't exist yet, and forcing
// the operator to pre-create it just shifts the problem.
//
// Permissions: 0600 on the file (the output may contain sensitive task
// results), 0700 on any created parent dirs.
func writeOutputFile(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create parent dir for --output-file: %w", err)
	}
	tmp, err := os.CreateTemp(parent, ".agentctl-output-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp output: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before rename.
	cleanup := func() {
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp output: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp output: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp output: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp output to %s: %w", path, err)
	}
	return nil
}

// outputAlreadyExists reports whether path is an existing regular file,
// for the `--skip-if-output-exists` idempotency flag (slice 7.4). A
// scheduler re-running a DAG step that already produced its output wants
// a fast no-op rather than re-spending tokens.
//
// Returns:
//   - (true, nil)  when path is an existing regular file. Because slice
//     7.2's finalizeOutput writes atomically (tmp+rename) and only on a
//     clean run with a non-empty message, a present regular file means a
//     prior run succeeded — exactly the "already done" signal we skip on.
//   - (false, nil) when path does not exist (the run should proceed).
//   - (_, error)   when path exists but is NOT a regular file (a
//     directory, FIFO, socket, device, or a SYMLINK) — none of those can
//     be the output a prior successful run wrote, so treating them as
//     "already done" would silently skip real work; surface a
//     configuration error instead. Also errors when Lstat fails for a
//     reason other than not-exist (e.g. a permission error worth
//     surfacing rather than silently treating as "proceed").
//
// Uses os.Lstat (NOT os.Stat) so a SYMLINK is not followed (codex pass 2
// of slice 7.4). os.Stat would follow a symlink to any regular file and
// report "already done", but writeOutputFile renames its temp file over
// the path — replacing the symlink itself — so a pre-existing symlink is
// not evidence THIS step succeeded.
func outputAlreadyExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("lstat --output-file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf(
			"--output-file %q exists but is not a regular file (%s); "+
				"--skip-if-output-exists only recognizes regular files written by a prior run",
			path, info.Mode().Type())
	}
	return true, nil
}

// prepareOutputCapture validates the --output-file / spec.outputSchema
// configuration and pre-compiles the schema BEFORE the backend runs.
// Returning a compiled schema up-front catches deterministic
// configuration errors (malformed JSON Schema, unsupported dialect)
// before a single token is spent — codex pass 3 of slice 7.2 caught
// the original ordering that compiled the schema only after the run
// completed, so a typo in `spec.outputSchema` would still execute the
// full agent loop before failing.
//
// Returns nil schema when no validation is required (outputFile or
// outputSchema absent). The caller passes the returned *jsonschema.Schema
// to finalizeOutput after the run.
func prepareOutputCapture(outputFile string, outputSchema *map[string]any) (*jsonschema.Schema, error) {
	if outputFile == "" || outputSchema == nil {
		return nil, nil
	}
	schema, err := compileOutputSchema(*outputSchema)
	if err != nil {
		return nil, fmt.Errorf("compile spec.outputSchema: %w", err)
	}
	return schema, nil
}

// finalizeOutput captures the assistant's last text, optionally validates
// it against outputSchema, and writes it to outputFilePath.
//
// Behavior matrix:
//
//   - lastAssistant == "" : an error unless outputSchema is nil and the
//     caller is OK with an empty file. We treat empty-final as an error
//     in BOTH cases — if the operator asked for --output-file but the
//     run produced no assistant message, that's a programming mistake
//     they want to know about (silent zero-byte files become diagnosis
//     puzzles later).
//
//   - outputSchema == nil : write lastAssistant text verbatim.
//
//   - outputSchema != nil : extract JSON payload (fence-strip), parse,
//     validate against the schema, write the re-marshaled JSON (pretty,
//     2-space indent, trailing newline — matches what most consumers
//     and humans expect from a JSON file).
//
// The caller is responsible for ONLY invoking this when the run
// completed successfully. Failed runs leave outputFilePath untouched.
//
// `schema` is the pre-compiled validator from prepareOutputCapture, or
// nil to write the raw text verbatim.
func finalizeOutput(outputFilePath, lastAssistant string, schema *jsonschema.Schema) error {
	if outputFilePath == "" {
		return nil
	}
	if strings.TrimSpace(lastAssistant) == "" {
		return errors.New(
			"--output-file requested but the agent produced no assistant message; " +
				"check that spec.task asks for a reply and that the run reached completion")
	}

	if schema == nil {
		// Plain text capture. Include a trailing newline so the file
		// ends cleanly (POSIX text-file convention).
		body := []byte(lastAssistant)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			body = append(body, '\n')
		}
		return writeOutputFile(outputFilePath, body)
	}

	// Schema-validated capture. Extract → parse → validate → write.
	//
	// Codex pass 1 of slice 7.2: use Decoder.UseNumber() so JSON
	// numbers parse to json.Number (a string under the hood) instead
	// of float64. Otherwise integers above 2^53 (e.g. Snowflake-style
	// 64-bit ids like 9007199254740993) round to the nearest
	// representable double on parse, and the re-marshal writes the
	// rounded value to the output file — silently corrupting the
	// scheduler payload even though schema validation succeeded.
	// santhosh-tekuri/jsonschema/v6 accepts json.Number for numeric
	// constraints, so validation continues to work unchanged.
	payload := extractJSONPayload(lastAssistant)
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return fmt.Errorf(
			"outputSchema set but the agent's last message is not valid JSON: %w "+
				"(first 200 bytes: %q)",
			err, truncateForError(payload, 200))
	}
	// Reject trailing content after the JSON value.
	//
	// Codex pass 2 of slice 7.2: `dec.More()` is the wrong check —
	// it's meant for array/object element iteration and returns
	// false when the next byte is a closing delimiter like `]` or
	// `}`. A malformed reply like `{"a":1}]` would slip past
	// More() and get re-marshaled as clean JSON, hiding the bug
	// from the scheduler.
	//
	// Inspect the raw payload past the decoder's input offset
	// instead. After Decode succeeds, InputOffset is the byte
	// position immediately after the decoded value; anything
	// remaining (minus whitespace) is genuine trailing content
	// — including stray delimiters, additional values, or prose.
	if tail := strings.TrimSpace(payload[dec.InputOffset():]); tail != "" {
		return fmt.Errorf(
			"outputSchema set but the agent's last message has trailing content after the JSON value "+
				"(reply must contain a single JSON document; trailing bytes: %q)",
			truncateForError(tail, 80))
	}

	if err := schema.Validate(parsed); err != nil {
		// jsonschema.ValidationError prints a multi-line tree; that's
		// what we want — the operator needs to know which field failed.
		return fmt.Errorf("agent output failed outputSchema validation:\n%s", err.Error())
	}

	// Re-marshal so the on-disk file is canonically formatted, not the
	// agent's original fenced-or-not raw text. 2-space indent matches the
	// near-universal default for hand-readable JSON.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(parsed); err != nil {
		// Unreachable: parsed is the output of json.Unmarshal so it's
		// re-marshalable by construction. Defensive guard only.
		return fmt.Errorf("re-marshal validated output: %w", err)
	}
	return writeOutputFile(outputFilePath, buf.Bytes())
}

// compileOutputSchema converts spec.outputSchema (parsed YAML/JSON as
// map[string]any) into a compiled jsonschema validator. We use the same
// santhosh-tekuri/jsonschema/v6 library the ADL validator uses, so a
// spec.outputSchema document targeting the same JSON Schema dialect as
// the ADL itself behaves identically.
func compileOutputSchema(raw map[string]any) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Resource id is arbitrary — we never reference it from elsewhere.
	const id = "agentctl://output-schema"
	if err := c.AddResource(id, raw); err != nil {
		return nil, fmt.Errorf("add output schema resource: %w", err)
	}
	schema, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile output schema: %w", err)
	}
	return schema, nil
}

// truncateForError returns s if len <= max, otherwise the first max bytes
// followed by `…`. Used to bound error-message size when the agent's
// reply is large.
func truncateForError(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
