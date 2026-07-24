package adl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/registry"
)

func TestCompileGolden(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")

	// Use the project's actual tools/extensions trees as the registry root.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := registry.Scan(root)
	if err != nil {
		t.Fatalf("registry.Scan: %v", err)
	}

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	gotJSON, _ := json.MarshalIndent(got, "", "  ")

	// Normalize absolute paths in the actual output so the golden file is
	// stable across machines.
	normalized := strings.ReplaceAll(string(gotJSON), root, "<REGISTRY>")

	wantBytes, err := os.ReadFile(filepath.Join("testdata", "valid_hello.compiled.json"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.TrimSpace(normalized) != strings.TrimSpace(string(wantBytes)) {
		t.Errorf("compiled spec mismatch.\n--- got ---\n%s\n--- want ---\n%s\n", normalized, string(wantBytes))
	}
}

func TestCompileInstallsPassthrough(t *testing.T) {
	doc := mustParse(t, "valid_installs.yaml")

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := registry.Scan(root)
	if err != nil {
		t.Fatalf("registry.Scan: %v", err)
	}

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	want := []string{"npm:pi-mcp-extension", "npm:some-other-package"}
	if len(got.Installs) != len(want) {
		t.Fatalf("Installs: got %v, want %v", got.Installs, want)
	}
	for i, pkg := range want {
		if got.Installs[i] != pkg {
			t.Errorf("Installs[%d]: got %q, want %q", i, got.Installs[i], pkg)
		}
	}
}

func TestCompileFailsOnUnknownTool(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	// Mangle the doc to reference an unknown tool.
	spec := doc["spec"].(map[string]any)
	spec["tools"] = []any{map[string]any{"name": "nonexistent_tool"}}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "nonexistent_tool") {
		t.Errorf("error %q should mention the missing tool name", err)
	}
}

// v0.1.6: spec.extensions[].source — self-contained extension compilation

func TestCompileExtensionWithSourceSkipsRegistry(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	// Replace extensions with a source-bound entry that is NOT in the registry.
	spec["extensions"] = []any{
		map[string]any{
			"name":   "nonexistent-extension",
			"source": "npm:nonexistent-extension",
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		// Should NOT fail — source-bound extensions bypass registry lookup.
		t.Fatalf("Compile: unexpected error for source-bound extension: %v", err)
	}
	if len(got.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(got.Extensions))
	}
	ext := got.Extensions[0]
	if ext.Name != "nonexistent-extension" {
		t.Errorf("Name: got %q, want %q", ext.Name, "nonexistent-extension")
	}
	if ext.Source != "npm:nonexistent-extension" {
		t.Errorf("Source: got %q, want %q", ext.Source, "npm:nonexistent-extension")
	}
	if ext.Entrypoint != "" {
		t.Errorf("Entrypoint should be empty for source-bound extension, got %q", ext.Entrypoint)
	}
}

func TestCompileExtensionWithSourcePreservesConfig(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["extensions"] = []any{
		map[string]any{
			"name":   "my-ext",
			"source": "npm:my-ext",
			"config": map[string]any{"key": "val"},
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(got.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(got.Extensions))
	}
	ext := got.Extensions[0]
	if ext.Source != "npm:my-ext" {
		t.Errorf("Source: got %q", ext.Source)
	}
	if ext.Config["key"] != "val" {
		t.Errorf("Config: got %v", ext.Config)
	}
}

func TestCompileExtensionWithoutSourceStillRequiresRegistry(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	// Extension without source must still resolve from registry.
	spec["extensions"] = []any{
		map[string]any{"name": "nonexistent-registry-ext"},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error for extension without source not in registry")
	}
	if !strings.Contains(err.Error(), "nonexistent-registry-ext") {
		t.Errorf("error %q should mention the missing extension name", err)
	}
}

func TestCompileGuardrailsHallucinationDetectorPassthrough(t *testing.T) {
	doc := mustParse(t, "valid_guardrails_warn.yaml")

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got.Guardrails == nil {
		t.Fatalf("Guardrails: nil; expected non-nil with HallucinationDetector=\"warn\"")
	}
	if got.Guardrails.HallucinationDetector != "warn" {
		t.Errorf("Guardrails.HallucinationDetector: got %q, want %q",
			got.Guardrails.HallucinationDetector, "warn")
	}
}

// v0.1.11: spec.tools[].config on a Pi built-in is now rejected at compile
// time. Prior to v0.1.11 the field was accepted and silently dropped at the
// runtime (Pi built-ins don't read AGENT_CONTROLLER_EXT_CONFIG), so a user
// who declared `bash` with `config: { allowedCommands: [...] }` got the
// illusion of governance without enforcement. The fix surfaces the bug
// loudly and points users at @gotgenes/pi-permission-system.
func TestCompileBuiltinToolWithConfigRejected(t *testing.T) {
	for _, builtin := range []string{"bash", "read", "edit", "write"} {
		t.Run(builtin, func(t *testing.T) {
			doc := mustParse(t, "valid_hello.yaml")
			spec := doc["spec"].(map[string]any)
			spec["tools"] = []any{
				map[string]any{
					"name":   builtin,
					"config": map[string]any{"allowedCommands": []any{"ls"}},
				},
			}

			root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
			idx, _ := registry.Scan(root)

			_, err := Compile(doc, idx)
			if err == nil {
				t.Fatalf("expected error for built-in %q with config", builtin)
			}
			if !strings.Contains(err.Error(), builtin) {
				t.Errorf("error %q should name the offending built-in", err)
			}
			if !strings.Contains(err.Error(), "pi-permission-system") {
				t.Errorf("error %q should point at @gotgenes/pi-permission-system", err)
			}
		})
	}
}

// Empty config map on a built-in is accepted (the user wrote `config: {}`
// or omitted the field entirely). Only NON-EMPTY config is rejected.
func TestCompileBuiltinToolWithEmptyConfigAccepted(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["tools"] = []any{
		map[string]any{
			"name":   "bash",
			"config": map[string]any{},
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("expected empty config to pass, got %v", err)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "bash" || !got.Tools[0].Builtin {
		t.Errorf("Tools: got %+v", got.Tools)
	}
}

// Custom tools (registry-resolved, not built-in) keep their config — the
// runtime DOES surface config to custom extensions via the
// AGENT_CONTROLLER_EXT_CONFIG env. Only built-ins are now rejected.
func TestCompileCustomToolWithConfigStillAccepted(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["tools"] = []any{
		map[string]any{
			"name":   "get_time", // registry entry under tools/get_time/
			"config": map[string]any{"format": "iso8601"},
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got.Tools[0].Config["format"] != "iso8601" {
		t.Errorf("Custom-tool config dropped: %+v", got.Tools[0])
	}
}

// v0.3.1: spec.runtime.requirements is an additive free-form boolean map
// that passes through CompiledSpec unchanged. v0.3.2 (RuntimeBinding) will
// consume it; for now we just verify the parse path.
func TestCompileRuntimeRequirementsPassthrough(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{
		"type": "local",
		"requirements": map[string]any{
			"streaming":           true,
			"sandbox":             true,
			"gpu":                 false,
			"spark":               true, // arbitrary capability bundle flag
			"restrictedNetwork":   true,
			"ephemeralFilesystem": true,
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := map[string]bool{
		"streaming":           true,
		"sandbox":             true,
		"gpu":                 false,
		"spark":               true,
		"restrictedNetwork":   true,
		"ephemeralFilesystem": true,
	}
	if len(got.Runtime.Requirements) != len(want) {
		t.Fatalf("Requirements: got %d keys, want %d (got=%v)",
			len(got.Runtime.Requirements), len(want), got.Runtime.Requirements)
	}
	for k, v := range want {
		if got.Runtime.Requirements[k] != v {
			t.Errorf("Requirements[%q]: got %v, want %v", k, got.Runtime.Requirements[k], v)
		}
	}
}

// v0.3.1: both "field absent" and "field declared as {}" produce a nil
// map here. The omitempty JSON tag drops the field in both cases. v0.3.2
// can promote to *map[string]bool if it needs to distinguish; documenting
// the current behavior so the contract is testable and stable.
func TestCompileRuntimeRequirementsAbsentOrEmptyBothNil(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	// (a) field omitted entirely (the existing valid_hello fixture)
	doc1 := mustParse(t, "valid_hello.yaml")
	got1, err := Compile(doc1, idx)
	if err != nil {
		t.Fatalf("Compile (omitted): %v", err)
	}
	if got1.Runtime.Requirements != nil {
		t.Errorf("absent: Requirements should be nil, got %v", got1.Runtime.Requirements)
	}

	// (b) field explicitly empty {}
	doc2 := mustParse(t, "valid_hello.yaml")
	doc2["spec"].(map[string]any)["runtime"] = map[string]any{
		"type":         "local",
		"requirements": map[string]any{},
	}
	got2, err := Compile(doc2, idx)
	if err != nil {
		t.Fatalf("Compile (empty): %v", err)
	}
	if got2.Runtime.Requirements != nil {
		t.Errorf("empty {}: Requirements should also be nil, got %v", got2.Runtime.Requirements)
	}
}

func TestCompileOutputSchemaOmittedRemainsNil(t *testing.T) {
	// Slice 7.2: spec omits outputSchema → OutputSchema stays nil so
	// the CLI keeps raw-text capture mode for --output-file.
	doc := mustParse(t, "valid_hello.yaml")

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got.OutputSchema != nil {
		t.Errorf("OutputSchema: got %+v, want nil", got.OutputSchema)
	}
}

func TestCompileOutputSchemaEmptyMapPreserved(t *testing.T) {
	// Codex pass 1 of slice 7.2: `outputSchema: {}` is valid JSON
	// Schema (accepts any JSON). Compile must NOT drop it — the CLI
	// uses non-nil-vs-nil to distinguish "validate as JSON, no
	// constraints" from "plain text capture." A `len > 0` guard would
	// silently downgrade an empty-schema run to raw-text mode and the
	// operator would never know the model failed to produce JSON.
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["outputSchema"] = map[string]any{}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got.OutputSchema == nil {
		t.Errorf("OutputSchema: nil; expected non-nil pointer")
	}
	if len(*got.OutputSchema) != 0 {
		t.Errorf("OutputSchema: got %d entries, want empty map", len(*got.OutputSchema))
	}
}

func TestCompileOutputSchemaPassthrough(t *testing.T) {
	// Slice 7.2: a non-empty outputSchema flows through verbatim.
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["outputSchema"] = map[string]any{
		"type":     "object",
		"required": []any{"label"},
		"properties": map[string]any{
			"label": map[string]any{"type": "string"},
		},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got.OutputSchema == nil {
		t.Fatal("OutputSchema: nil; expected non-nil pointer")
	}
	if (*got.OutputSchema)["type"] != "object" {
		t.Errorf("type: got %v want object", (*got.OutputSchema)["type"])
	}
}

func TestCompileOutputSchemaRejectsBooleanShorthand(t *testing.T) {
	// Codex pass 4 of slice 7.2: JSON Schema draft 2020-12 allows
	// the boolean shorthand `true` (accept anything) and `false`
	// (reject anything). We intentionally restrict outputSchema to
	// object form because:
	//   (a) `true` is already expressible as `outputSchema: {}`,
	//   (b) `false` rejects all output and has no real use,
	//   (c) supporting both would widen the field's type from
	//       `*map[string]any` to `any` across compiler / K8s
	//       marshaller / pre-compile path for negligible value.
	// This test pins the rejection at the ADL validator layer so a
	// later widening is an explicit, observable change.
	for _, val := range []any{true, false} {
		doc := mustParse(t, "valid_hello.yaml")
		spec := doc["spec"].(map[string]any)
		spec["outputSchema"] = val
		// Validate against the embedded schema, not Compile,
		// because Validate is what catches shape errors.
		v, err := NewValidator()
		if err != nil {
			t.Fatalf("NewValidator: %v", err)
		}
		if err := v.Validate(doc); err == nil {
			t.Errorf("expected validation error for outputSchema: %v", val)
		}
	}
}

func TestCompileGuardrailsOmittedRemainsNil(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Spec omits guardrails entirely — Compile must NOT materialize a
	// default object. The runtime applies the "block" default downstream;
	// keeping nil here means we can distinguish "user said nothing" from
	// "user explicitly chose block" if that ever matters.
	if got.Guardrails != nil {
		t.Errorf("Guardrails: got %+v, want nil", got.Guardrails)
	}
}

// v0.3.4: adapter-compatibility checks fire at compile time. The opencode
// adapter rejects four kinds of ADL surface; this slice moves three of
// them into the compiler (the fourth, --resume/spec.sessionId, lives in
// the CLI because the resume flag is applied after Compile).

func TestCompileOpencodeRejectsExtensions(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-opencode"}
	spec["tools"] = []any{}
	// hello.yaml's audit-log extension survives by default; ensure we have
	// at least one extension to trigger the rejection.

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: spec.extensions on local-opencode")
	}
	if !strings.Contains(err.Error(), "spec.extensions") {
		t.Errorf("error %q should name spec.extensions", err)
	}
	if !strings.Contains(err.Error(), "local-opencode") {
		t.Errorf("error %q should mention the local-opencode runtime type", err)
	}
}

func TestCompileOpencodeRejectsCustomPiTools(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-opencode"}
	spec["extensions"] = []any{}
	// spec.tools still contains get_time from valid_hello.yaml — a custom
	// Pi-extension tool. That must be rejected on local-opencode.

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: custom Pi tool on local-opencode")
	}
	if !strings.Contains(err.Error(), "get_time") {
		t.Errorf("error %q should name the offending tool", err)
	}
}

func TestCompileOpencodeRejectsInstalls(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-opencode"}
	spec["tools"] = []any{}
	spec["extensions"] = []any{}
	spec["installs"] = []any{"npm:something"}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: spec.installs on local-opencode")
	}
	if !strings.Contains(err.Error(), "spec.installs") {
		t.Errorf("error %q should name spec.installs", err)
	}
}

func TestCompileOpencodeBuiltinToolsAccepted(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-opencode"}
	spec["extensions"] = []any{}
	spec["tools"] = []any{
		map[string]any{"name": "bash"},
		map[string]any{"name": "read"},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	got, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("Built-in tools on local-opencode should pass: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Errorf("Tools: got %d, want 2", len(got.Tools))
	}
}

func TestCompileOpencodeMultipleProblemsAllListed(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-opencode"}
	// hello.yaml already has get_time (custom Pi tool) + audit-log (extension).
	// Add installs to make three concurrent problems.
	spec["installs"] = []any{"npm:something"}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: multiple unsupported fields on local-opencode")
	}
	// All three problems should be listed (not just the first one).
	for _, want := range []string{"spec.extensions", "spec.installs", "get_time"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestCompileLocalPiAcceptsAllFieldsThatOpencodeRejects(t *testing.T) {
	// Same spec, but runtime.type: local-pi → compile succeeds.
	// Locks the rule that the rejection is opencode-specific, not global.
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-pi"}
	spec["installs"] = []any{"npm:something"}
	// keep get_time + audit-log from valid_hello.yaml

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("local-pi should accept Pi-flavored fields, got: %v", err)
	}
}

// v0.4+: codex adapter compile-time rejection checks.

func TestCompileCodexRejectsNonOpenAIProvider(t *testing.T) {
	// local-codex only supports provider: openai; anthropic must be rejected.
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-codex"}
	spec["extensions"] = []any{}
	spec["tools"] = []any{}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: non-openai provider on local-codex")
	}
	if !strings.Contains(err.Error(), "local-codex") {
		t.Errorf("error %q should mention local-codex", err)
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("error %q should mention provider", err)
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("error %q should mention openai", err)
	}
}

func TestCompileCodexRejectsExtensions(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-codex"}
	spec["model"] = map[string]any{"provider": "openai", "name": "codex-mini-latest"}
	spec["tools"] = []any{}
	// valid_hello.yaml has audit-log extension; keep it to trigger rejection.

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: spec.extensions on local-codex")
	}
	if !strings.Contains(err.Error(), "spec.extensions") {
		t.Errorf("error %q should name spec.extensions", err)
	}
	if !strings.Contains(err.Error(), "local-codex") {
		t.Errorf("error %q should mention local-codex", err)
	}
}

func TestCompileCodexRejectsInstalls(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-codex"}
	spec["model"] = map[string]any{"provider": "openai", "name": "codex-mini-latest"}
	spec["extensions"] = []any{}
	spec["tools"] = []any{}
	spec["installs"] = []any{"npm:something"}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: spec.installs on local-codex")
	}
	if !strings.Contains(err.Error(), "spec.installs") {
		t.Errorf("error %q should name spec.installs", err)
	}
}

func TestCompileCodexRejectsCustomPiTools(t *testing.T) {
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-codex"}
	spec["model"] = map[string]any{"provider": "openai", "name": "codex-mini-latest"}
	spec["extensions"] = []any{}
	// valid_hello.yaml has get_time (custom Pi tool) — must be rejected.

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err == nil {
		t.Fatalf("expected error: custom Pi tool on local-codex")
	}
	if !strings.Contains(err.Error(), "get_time") {
		t.Errorf("error %q should name the offending tool", err)
	}
}

func TestCompileCodexRejectsSubagents(t *testing.T) {
	// Test checkCodexIncompatibilities directly since subagent lookup
	// requires a registry entry and would fail before reaching the compat check.
	spec := CompiledSpec{
		Runtime: RuntimeConfig{Type: "local-codex"},
		Model:   Model{Provider: "openai", Name: "codex-mini-latest"},
		Subagents: []ResolvedRef{
			{Name: "some-subagent", Entrypoint: "agents/some-subagent"},
		},
	}
	err := checkCodexIncompatibilities(spec)
	if err == nil {
		t.Fatalf("expected error: subagents on local-codex")
	}
	if !strings.Contains(err.Error(), "subagent") {
		t.Errorf("error %q should mention subagents", err)
	}
	if !strings.Contains(err.Error(), "local-codex") {
		t.Errorf("error %q should mention local-codex", err)
	}
}

func TestCompileCodexCleanSpecPasses(t *testing.T) {
	// A clean local-codex spec with provider: openai, no extensions/subagents/
	// installs, and only built-in tools (bash, read) must compile without error.
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["runtime"] = map[string]any{"type": "local-codex"}
	spec["model"] = map[string]any{"provider": "openai", "name": "codex-mini-latest"}
	spec["extensions"] = []any{}
	spec["tools"] = []any{
		map[string]any{"name": "bash"},
		map[string]any{"name": "read"},
	}

	root, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	idx, _ := registry.Scan(root)

	_, err := Compile(doc, idx)
	if err != nil {
		t.Fatalf("clean local-codex spec should pass: %v", err)
	}
}
