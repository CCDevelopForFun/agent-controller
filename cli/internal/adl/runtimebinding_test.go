package adl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v0.3.2: Validator must dispatch to the right schema based on `kind`.
// This test exercises both the Agent path (existing behavior) and the
// new RuntimeBinding path together to lock the dispatch contract.

func TestValidatorDispatchesByKind(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Valid Agent — must validate.
	agentDoc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "hello"},
		"spec": map[string]any{
			"model": map[string]any{"provider": "anthropic", "name": "claude-sonnet-4-20250514"},
			"task":  "say hi",
			"tools": []any{},
			"runtime": map[string]any{"type": "local"},
		},
	}
	if err := v.Validate(agentDoc); err != nil {
		t.Errorf("valid Agent rejected: %v", err)
	}

	// Valid RuntimeBinding — must validate.
	bindingDoc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "RuntimeBinding",
		"metadata":   map[string]any{"name": "local-default"},
		"spec": map[string]any{
			"selector": map[string]any{
				"runtimeType":  "local-pi",
				"capabilities": map[string]any{"streaming": true},
			},
			"target": map[string]any{
				"type": "local",
			},
		},
	}
	if err := v.Validate(bindingDoc); err != nil {
		t.Errorf("valid RuntimeBinding rejected: %v", err)
	}

	// Missing kind — clear error.
	if err := v.Validate(map[string]any{"apiVersion": "agent-controller.dev/v1alpha1"}); err == nil {
		t.Errorf("expected error for missing kind")
	} else if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error %q should mention `kind`", err)
	}

	// Unknown kind — clear error listing supported kinds.
	bad := map[string]any{"apiVersion": "agent-controller.dev/v1alpha1", "kind": "Workflow"}
	if err := v.Validate(bad); err == nil {
		t.Errorf("expected error for unknown kind Workflow")
	} else if !strings.Contains(err.Error(), "Workflow") || !strings.Contains(err.Error(), "Agent") {
		t.Errorf("error %q should name the unknown kind and the supported set", err)
	}
}

// TestValidatorAcceptsClaudeRuntimeBinding locks in the local-claude enum
// entry added to selector.runtimeType alongside local/local-pi/local-opencode.
// Task 6 fills in a claude column across the capability matrix, including
// this RuntimeBinding row, so a binding selecting local-claude must validate.
// Mirrors the local-pi bindingDoc in TestValidatorDispatchesByKind.
func TestValidatorAcceptsClaudeRuntimeBinding(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	bindingDoc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "RuntimeBinding",
		"metadata":   map[string]any{"name": "local-claude-default"},
		"spec": map[string]any{
			"selector": map[string]any{
				"runtimeType": "local-claude",
			},
			"target": map[string]any{
				"type": "local",
			},
		},
	}
	if err := v.Validate(bindingDoc); err != nil {
		t.Errorf("valid RuntimeBinding with runtimeType local-claude rejected: %v", err)
	}
}

// TestValidatorAcceptsCodexRuntimeBinding is the codex twin of the
// local-claude case above. `local-codex` was missing from the
// selector.runtimeType enum even though docs/architecture/harness-matrix.md
// asserts `agentctl run --binding` works against codex-typed Agents, so a
// perfectly valid codex binding failed validation.
func TestValidatorAcceptsCodexRuntimeBinding(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	bindingDoc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "RuntimeBinding",
		"metadata":   map[string]any{"name": "local-codex-default"},
		"spec": map[string]any{
			"selector": map[string]any{
				"runtimeType": "local-codex",
			},
			"target": map[string]any{
				"type": "local",
			},
		},
	}
	if err := v.Validate(bindingDoc); err != nil {
		t.Errorf("valid RuntimeBinding with runtimeType local-codex rejected: %v", err)
	}
}

// TestValidatorRejectsUnknownBindingRuntimeType guards the other direction:
// widening the enum must not turn it into a free-form string.
func TestValidatorRejectsUnknownBindingRuntimeType(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	bindingDoc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "RuntimeBinding",
		"metadata":   map[string]any{"name": "bogus"},
		"spec": map[string]any{
			"selector": map[string]any{"runtimeType": "local-hermes"},
			"target":   map[string]any{"type": "local"},
		},
	}
	if err := v.Validate(bindingDoc); err == nil {
		t.Errorf("expected error for unsupported selector.runtimeType=local-hermes")
	}
}

func TestValidatorRejectsBindingMissingFields(t *testing.T) {
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Missing target.
	doc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "RuntimeBinding",
		"metadata":   map[string]any{"name": "x"},
		"spec": map[string]any{
			"selector": map[string]any{"runtimeType": "local-pi"},
		},
	}
	if err := v.Validate(doc); err == nil {
		t.Errorf("expected error when target is missing")
	}

	// Invalid target.type. v0.4 slice 4.3 accepts "local" and "kubernetes";
	// anything else (e.g. "databricks", "agentcore" — still on the roadmap)
	// must fail at validate time, not silently dispatch to a missing backend.
	doc["spec"].(map[string]any)["target"] = map[string]any{"type": "databricks"}
	if err := v.Validate(doc); err == nil {
		t.Errorf("expected error for unsupported target.type=databricks (only local|kubernetes ship today)")
	}

	// Conditional schema (slice 4.3, codex pass 2): target.type=kubernetes
	// without a kubernetes block must fail validation (otherwise Resolve
	// rejects late). Same when the kubernetes block omits secretRef.
	doc["spec"].(map[string]any)["target"] = map[string]any{"type": "kubernetes"}
	if err := v.Validate(doc); err == nil {
		t.Errorf("expected error for target.type=kubernetes with no kubernetes block")
	}
	doc["spec"].(map[string]any)["target"] = map[string]any{
		"type":       "kubernetes",
		"kubernetes": map[string]any{}, // no secretRef
	}
	if err := v.Validate(doc); err == nil {
		t.Errorf("expected error for target.kubernetes with no secretRef")
	}
	// Empty secretRef.name must fail at validate (slice 4.3 codex pass 7).
	doc["spec"].(map[string]any)["target"] = map[string]any{
		"type": "kubernetes",
		"kubernetes": map[string]any{
			"secretRef": map[string]any{"name": ""},
		},
	}
	if err := v.Validate(doc); err == nil {
		t.Errorf("expected error for empty secretRef.name (minLength: 1)")
	}
}

func TestParseBindingExampleRoundtrips(t *testing.T) {
	// The checked-in example must parse + validate as a sanity check.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(root, "examples", "bindings", "local-default.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	b, err := ParseBinding(data)
	if err != nil {
		t.Fatalf("ParseBinding: %v", err)
	}
	if b.Metadata.Name != "local-default" {
		t.Errorf("Metadata.Name: got %q", b.Metadata.Name)
	}
	if b.Spec.Selector.RuntimeType != "local-pi" {
		t.Errorf("Selector.RuntimeType: got %q", b.Spec.Selector.RuntimeType)
	}
	if !b.Spec.Selector.Capabilities["streaming"] {
		t.Errorf("Selector.Capabilities[streaming]: expected true")
	}
	if b.Spec.Target.Type != "local" {
		t.Errorf("Target.Type: got %q", b.Spec.Target.Type)
	}

	// And it must pass schema validation through the same dispatch path
	// `agentctl validate <path>` uses.
	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(doc); err != nil {
		t.Errorf("example binding fails schema validation: %v", err)
	}
}

func TestParseKubernetesBindingExampleRoundtrips(t *testing.T) {
	// Slice 4.3: the kubernetes-kind.yaml example must parse + validate.
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(root, "examples", "bindings", "kubernetes-kind.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	b, err := ParseBinding(data)
	if err != nil {
		t.Fatalf("ParseBinding: %v", err)
	}
	if b.Spec.Target.Type != "kubernetes" {
		t.Errorf("Target.Type: got %q", b.Spec.Target.Type)
	}
	if b.Spec.Target.Kubernetes == nil {
		t.Fatalf("Target.Kubernetes: nil (expected populated config block)")
	}
	if got := b.Spec.Target.Kubernetes.Namespace; got != "default" {
		t.Errorf("Target.Kubernetes.Namespace: got %q", got)
	}
	if b.Spec.Target.Kubernetes.SecretRef == nil {
		t.Fatalf("Target.Kubernetes.SecretRef: nil")
	}
	if got := b.Spec.Target.Kubernetes.SecretRef.Name; got != "anthropic-creds" {
		t.Errorf("Target.Kubernetes.SecretRef.Name: got %q", got)
	}

	// Schema validation through the same dispatch path agentctl validate uses.
	doc, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v, err := NewValidator()
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := v.Validate(doc); err != nil {
		t.Errorf("kubernetes example binding fails schema validation: %v", err)
	}
}
