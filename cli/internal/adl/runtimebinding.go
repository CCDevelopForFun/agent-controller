package adl

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// RuntimeBinding is the typed form of a YAML RuntimeBinding resource. It
// mirrors schemas/runtimebinding.v1alpha1.json field-for-field and is the
// shape v0.3.3 (Backend.Resolve) will consume to translate an Agent spec
// into a concrete execution plan.
//
// v0.3.2 ships this type + the schema + the parser. No code in the run
// path consumes it yet; that's slice 3.3.
type RuntimeBinding struct {
	APIVersion string             `json:"apiVersion" yaml:"apiVersion"`
	Kind       string             `json:"kind" yaml:"kind"`
	Metadata   RuntimeBindingMeta `json:"metadata" yaml:"metadata"`
	Spec       RuntimeBindingSpec `json:"spec" yaml:"spec"`
}

type RuntimeBindingMeta struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type RuntimeBindingSpec struct {
	Selector RuntimeBindingSelector `json:"selector" yaml:"selector"`
	Target   RuntimeBindingTarget   `json:"target" yaml:"target"`
}

// RuntimeBindingSelector decides which Agents this Binding is willing to
// host. v0.3.3 will use these fields to filter candidate Bindings during
// Backend.Resolve().
type RuntimeBindingSelector struct {
	// RuntimeType must equal the Agent's spec.runtime.type. Allowed values
	// match the schema enum:
	// local | local-pi | local-opencode | local-codex | local-claude.
	RuntimeType string `json:"runtimeType" yaml:"runtimeType"`
	// Capabilities this Binding's target provides. The matcher (slice 3.3)
	// will require every `true` requirement in the Agent's
	// spec.runtime.requirements to also be `true` here. Missing keys mean
	// the Binding does not advertise that capability.
	Capabilities map[string]bool `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// RuntimeBindingTarget says where the agent runs once a Binding is
// selected. v0.3.x ships `type: local`. v0.4 slice 4.3 adds `kubernetes`.
type RuntimeBindingTarget struct {
	// Type is the backend implementation. v0.4: "local" | "kubernetes".
	Type string `json:"type" yaml:"type"`
	// RuntimeCommand overrides the runtime-adapter binary path for the
	// local backend only. Equivalent to setting AGENT_CONTROLLER_RUNTIME
	// at run time. Ignored when Type is "kubernetes".
	RuntimeCommand string `json:"runtimeCommand,omitempty" yaml:"runtimeCommand,omitempty"`
	// Strict (added v0.3.3b): when true, the capability matcher promotes
	// unmet spec.runtime.requirements from warn-but-proceed to a hard error.
	// agentctl run exits non-zero before any session.started event is
	// emitted. Default (false) preserves the warn-but-proceed policy
	// recorded in ROADMAP.md "Recorded design decisions".
	Strict bool `json:"strict,omitempty" yaml:"strict,omitempty"`
	// Kubernetes carries target-specific config when Type is "kubernetes".
	// Ignored otherwise. Slice 4.3 ships the v0.1 of this surface;
	// kubeconfig path / context / serviceAccount land in slice 4.4.
	Kubernetes *KubernetesTarget `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
}

// KubernetesTarget configures the KubernetesBackend. v0.4 slice 4.3.
type KubernetesTarget struct {
	// Namespace in which the agent Pod + its ConfigMap are created.
	// Default applied at runtime: "default".
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	// Image to launch. Default applied at runtime:
	// "ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.1".
	Image string `json:"image,omitempty" yaml:"image,omitempty"`
	// SecretRef points at a Kubernetes Secret in the same namespace that
	// holds adapter credentials. Required when the spec needs API keys
	// (Anthropic etc.); each declared Keys entry is mapped to an env var
	// of the same name on the Pod.
	SecretRef *KubernetesSecretRef `json:"secretRef,omitempty" yaml:"secretRef,omitempty"`
}

// KubernetesSecretRef is a name + key list pointing at a Secret in the
// Pod's namespace. Default Keys: ["ANTHROPIC_API_KEY"].
type KubernetesSecretRef struct {
	Name string   `json:"name" yaml:"name"`
	Keys []string `json:"keys,omitempty" yaml:"keys,omitempty"`
}

// ParseBinding reads YAML bytes into a typed RuntimeBinding. The caller
// should run Validate against the schema FIRST — ParseBinding only enforces
// YAML well-formedness and the JSON struct tags. Schema-level invariants
// (required fields, enum values, additionalProperties:false) come from the
// JSON Schema validator, not from this function.
func ParseBinding(data []byte) (*RuntimeBinding, error) {
	var b RuntimeBinding
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse binding YAML: %w", err)
	}
	return &b, nil
}
