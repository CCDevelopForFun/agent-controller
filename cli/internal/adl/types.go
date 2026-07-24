package adl

// CompiledSpec is the wire-format payload the CLI sends to the runtime on
// stdin. The wire-protocol contract is summarized in docs/architecture/overview.md.
type CompiledSpec struct {
	V          int           `json:"v"`
	Metadata   SpecMetadata  `json:"metadata"`
	Model      Model         `json:"model"`
	Persona    *Persona      `json:"persona,omitempty"`
	Task       string        `json:"task"`
	Tools      []ResolvedRef `json:"tools"`
	Extensions []ResolvedRef `json:"extensions"`
	Skills     []ResolvedRef `json:"skills"`
	Subagents  []ResolvedRef `json:"subagents,omitempty"`
	MCPServers []MCPServer   `json:"mcpServers,omitempty"`
	Installs   []string      `json:"installs,omitempty"`
	Runtime    RuntimeConfig `json:"runtime"`
	Guardrails    *Guardrails    `json:"guardrails,omitempty"`
	Observability *Observability `json:"observability,omitempty"`
	// OutputSchema (slice 7.2 of v0.7) is an optional JSON Schema the agent's
	// final assistant message must conform to. When set AND `--output-file
	// <path>` is passed, the CLI extracts JSON from the last assistant message
	// (stripping a single ```json fenced block if present), validates against
	// this schema, and writes the validated JSON to <path>. Without
	// `--output-file` the field has no runtime effect; we gate validation on a
	// consumer being present. The compiler passes the schema through verbatim
	// — we don't constrain its meta-schema.
	//
	// Pointer-to-map (not bare map) is intentional: the empty schema
	// `outputSchema: {}` is meaningful (it requires JSON-syntax output with no
	// further constraints). A `map[string]any` field with `omitempty` would
	// drop a zero-length map at JSON marshal time and the compiled-spec stream
	// (agentctl compile, K8s in-Pod handoff) would lose the distinction
	// between "no schema" and "JSON-only schema." Pointer omitempty only drops
	// nil pointers, so empty-map values survive. Codex pass 3 of slice 7.2.
	OutputSchema *map[string]any `json:"outputSchema,omitempty"`
	// SessionID is set by the CLI when the user passes --resume <id>.
	// When present, the runtime opens/continues the named persistent session
	// instead of creating a throw-away in-memory session.
	SessionID *string `json:"sessionId,omitempty"`
}

// Guardrails configures runtime safety behaviors. Each field has a sensible
// default applied at runtime when absent. The compiler passes user-specified
// values through verbatim; defaults are *not* materialized here so that
// callers downstream (runtime) can distinguish "user said X" from "user said
// nothing" if that ever matters.
type Guardrails struct {
	// HallucinationDetector controls how the runtime reacts to fabricated
	// tool-call XML in assistant message text. Valid values are "warn",
	// "block" (default at runtime), and "correct". See the schema description
	// of spec.guardrails.hallucinationDetector for behavior details.
	HallucinationDetector string `json:"hallucinationDetector,omitempty"`
}

// Observability controls signal emission for a run.
//
// Two fields today (both opt-in via spec):
//
//   - Tracing (slice 5.1+): gates OTel SDK init on the agentctl host and,
//     starting with slice 5.3, also inside the Pi adapter (slice 5.4 for
//     opencode). Absence of the parent object — or `tracing: false` —
//     means the SDK is never initialized and the default no-op
//     propagator/provider apply.
//
//   - CaptureContent (slice 5.3+): when true, the runtime adapter
//     attaches the actual prompt messages, completion text, tool-call
//     arguments and tool-call results as `gen_ai.*` span attributes
//     (per OTel GenAI semconv). Off by default — only enable on runs
//     shipping to a backend approved to store conversation content. A
//     future slice will gate this behind workspace policy.
//
// The OTLP endpoint comes from the standard OTEL_EXPORTER_OTLP_ENDPOINT
// env var; we don't ADL-encode it because spec authors don't always
// know the operator's collector address.
type Observability struct {
	Tracing        bool `json:"tracing,omitempty"`
	CaptureContent bool `json:"captureContent,omitempty"`
}

// MCPServer describes one MCP server declared in spec.mcpServers[].
// The runtime writes these into <cwd>/.pi/mcp.json (keyed by name) so that
// pi-mcp-extension can connect to them.
type MCPServer struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Lifecycle string            `json:"lifecycle,omitempty"`
	// stdio fields
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// http/sse fields
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type SpecMetadata struct {
	Name        string `json:"name"`
	Owner       string `json:"owner,omitempty"`
	Description string `json:"description,omitempty"`
}

type Model struct {
	Provider    string   `json:"provider"`
	Name        string   `json:"name"`
	Temperature *float64 `json:"temperature,omitempty"`
}

type Persona struct {
	Role         string `json:"role,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// ResolvedRef pairs a tool/extension name with the absolute path to its
// Pi extension entrypoint and any config the user provided.
//
// When Source is set (e.g. "npm:pi-mcp-extension"), the extension is
// self-contained: the runtime will install it if needed and resolve the
// entrypoint at session-start time. Entrypoint is left blank in that case.
type ResolvedRef struct {
	Name string `json:"name"`
	// Entrypoint is the absolute path to a Pi extension module that
	// implements this ref. Empty for Pi-builtin tools (Builtin=true) and
	// for source-bound extensions (Source set; resolved at runtime).
	Entrypoint string `json:"entrypoint,omitempty"`
	// Source declares an npm/etc package; the runtime installs and
	// resolves it (see spec.extensions[].source in adl.v1alpha1).
	Source string `json:"source,omitempty"`
	// Builtin marks a Pi-shipped tool (bash, read, edit, write). The
	// runtime adds the name to Pi's tool allowlist without loading any
	// entrypoint. See compiler.go for the list.
	Builtin bool           `json:"builtin,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

type RuntimeConfig struct {
	Type string `json:"type"`
	// Requirements is a free-form capability map (added in v0.3.1).
	// Boolean flags advertising what the runtime must provide. Consumed in
	// two steps: v0.3.2 adds the RuntimeBinding schema (the resource that
	// advertises which capabilities a target provides), and v0.3.3 wires
	// the Backend.Resolve() matcher that compares them. Today (v0.3.1)
	// it's advisory metadata and passes through CompiledSpec unchanged.
	// v0.3.1 does NOT distinguish "absent" from "explicit empty map {}"
	// — both compile to a nil map here and are dropped from the JSON
	// envelope by `omitempty`. If a future slice needs to distinguish
	// (e.g. to mean "this spec verified it needs nothing"), promote the
	// field to *map[string]bool then.
	Requirements map[string]bool `json:"requirements,omitempty"`
}

// ResolvedRunSpec is the post-resolution shape a Backend.Submit consumes.
// It wraps the original CompiledSpec plus the RuntimeBinding (if any) the
// resolver matched. Backends use the binding to decide HOW to spawn (e.g.
// override the runtime adapter path via target.runtimeCommand) and what
// extra warnings/diagnostics to emit before the run begins.
//
// v0.3.3a (this slice): ResolvedRunSpec is the new typed boundary between
// the CLI and the backend. The resolver is a no-op for now — it wraps
// the CompiledSpec with a nil Binding. Slice 3.3b adds:
//   - --binding <path> CLI flag to load a RuntimeBinding YAML
//   - capability matcher (compares spec.runtime.requirements vs
//     binding.spec.selector.capabilities) with warn-but-proceed policy
//   - spec.target.strict opt-in to flip warn→fail per Binding
//
// Why a wrapper rather than embedding Binding into CompiledSpec: the
// CompiledSpec is what flows to the runtime adapter over stdin and
// represents agent INTENT (model, persona, tools, etc.). The Binding
// is a deployment decision that lives outside the agent's spec and
// can change without recompiling the agent. Keeping them in separate
// fields preserves that split.
type ResolvedRunSpec struct {
	// Spec is the CompiledSpec the resolver received. Backends ship this
	// to the runtime adapter over stdin unchanged.
	Spec CompiledSpec `json:"spec"`
	// Binding is the RuntimeBinding the resolver matched, or nil when
	// the caller did not supply a Binding (the v0.2.x default flow).
	// Backends consult target.runtimeCommand and other target fields
	// from this binding when spawning the runtime adapter.
	Binding *RuntimeBinding `json:"binding,omitempty"`
}
