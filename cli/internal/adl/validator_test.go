package adl

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, name string) map[string]any {
	t.Helper()
	doc, err := Parse(readFixture(t, name))
	if err != nil {
		t.Fatalf("Parse %s: %v", name, err)
	}
	return doc
}

func TestValidateAcceptsHello(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(mustParse(t, "valid_hello.yaml")); err != nil {
		t.Fatalf("Validate: unexpected error %v", err)
	}
}

func TestValidateRejectsMissingTask(t *testing.T) {
	v, _ := NewValidator()
	err := v.Validate(mustParse(t, "missing_task.yaml"))
	if err == nil {
		t.Fatalf("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("error %q should mention 'task'", err)
	}
}

func TestValidateRejectsUnknownField(t *testing.T) {
	v, _ := NewValidator()
	err := v.Validate(mustParse(t, "unknown_field.yaml"))
	if err == nil {
		t.Fatalf("expected error for unknown field 'policies'")
	}
	if !strings.Contains(err.Error(), "policies") && !strings.Contains(err.Error(), "additionalProperties") {
		t.Errorf("error %q should mention 'policies' or additionalProperties", err)
	}
}

func TestValidateAcceptsMCPStdio(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(mustParse(t, "valid_mcp_stdio.yaml")); err != nil {
		t.Fatalf("Validate: unexpected error for valid stdio MCP server: %v", err)
	}
}

func TestValidateAcceptsMCPHttp(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(mustParse(t, "valid_mcp_http.yaml")); err != nil {
		t.Fatalf("Validate: unexpected error for valid http MCP server: %v", err)
	}
}

func TestValidateRejectsMCPStdioMissingCommand(t *testing.T) {
	v, _ := NewValidator()
	err := v.Validate(mustParse(t, "invalid_mcp_stdio_no_command.yaml"))
	if err == nil {
		t.Fatalf("expected error for stdio MCP server missing command")
	}
}

func TestValidateAcceptsInstalls(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(mustParse(t, "valid_installs.yaml")); err != nil {
		t.Fatalf("Validate: unexpected error for valid installs: %v", err)
	}
}

func TestValidateRejectsInstallsWithEmptyString(t *testing.T) {
	v, _ := NewValidator()
	// Build a doc with an empty-string install entry.
	doc := mustParse(t, "valid_installs.yaml")
	spec := doc["spec"].(map[string]any)
	spec["installs"] = []any{""}
	err := v.Validate(doc)
	if err == nil {
		t.Fatalf("expected error for empty string in installs")
	}
}

// v0.1.6: spec.extensions[].source field

func TestValidateAcceptsExtensionWithSourceField(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	// Add an extension entry with a source field.
	spec["extensions"] = []any{
		map[string]any{
			"name":   "pi-mcp-extension",
			"source": "npm:pi-mcp-extension",
		},
	}
	if err := v.Validate(doc); err != nil {
		t.Fatalf("Validate: unexpected error for extension with source: %v", err)
	}
}

func TestValidateAcceptsExtensionWithBothNameAndSource(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	// Both name and source are allowed together.
	spec["extensions"] = []any{
		map[string]any{
			"name":   "pi-mcp-extension",
			"source": "npm:pi-mcp-extension",
			"config": map[string]any{"key": "value"},
		},
	}
	if err := v.Validate(doc); err != nil {
		t.Fatalf("Validate: unexpected error for extension with both name and source: %v", err)
	}
}

func TestValidateRejectsExtensionSourceWithEmptyString(t *testing.T) {
	v, _ := NewValidator()
	doc := mustParse(t, "valid_hello.yaml")
	spec := doc["spec"].(map[string]any)
	spec["extensions"] = []any{
		map[string]any{
			"name":   "pi-mcp-extension",
			"source": "",
		},
	}
	err := v.Validate(doc)
	if err == nil {
		t.Fatalf("expected error for empty string in extension source")
	}
}

func TestValidateAcceptsGuardrailsValidModes(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	for _, mode := range []string{"warn", "block", "correct"} {
		doc := mustParse(t, "valid_hello.yaml")
		doc["spec"].(map[string]any)["guardrails"] = map[string]any{
			"hallucinationDetector": mode,
		}
		if err := v.Validate(doc); err != nil {
			t.Errorf("Validate hallucinationDetector=%q: unexpected error %v", mode, err)
		}
	}
}

func TestValidateRejectsGuardrailsUnknownMode(t *testing.T) {
	v, _ := NewValidator()
	err := v.Validate(mustParse(t, "invalid_guardrails_mode.yaml"))
	if err == nil {
		t.Fatalf("expected error for unknown hallucinationDetector mode")
	}
	if !strings.Contains(err.Error(), "hallucinationDetector") &&
		!strings.Contains(err.Error(), "enum") {
		t.Errorf("error %q should mention hallucinationDetector or enum", err)
	}
}

func TestValidateRejectsGuardrailsUnknownField(t *testing.T) {
	v, _ := NewValidator()
	doc := mustParse(t, "valid_hello.yaml")
	doc["spec"].(map[string]any)["guardrails"] = map[string]any{
		"hallucinationDetector": "warn",
		"unknownField":          true,
	}
	err := v.Validate(doc)
	if err == nil {
		t.Fatalf("expected error for unknown guardrails field")
	}
	if !strings.Contains(err.Error(), "unknownField") &&
		!strings.Contains(err.Error(), "additionalProperties") {
		t.Errorf("error %q should mention unknownField or additionalProperties", err)
	}
}
