// Mirror of cli/internal/adl/types.go and the spec §6 stdin payload.

export interface CompiledSpec {
  v: 1;
  metadata: SpecMetadata;
  model: Model;
  persona?: Persona;
  task: string;
  tools: ResolvedRef[];
  extensions: ResolvedRef[];
  skills: ResolvedRef[];
  mcpServers?: MCPServer[];
  subagents?: ResolvedRef[];
  /**
   * Deprecated: use spec.extensions[].source instead.
   * When non-empty the runtime emits a deprecation warning to stderr.
   * Still passed through unchanged; `agentctl install --from` uses it.
   */
  installs?: string[];
  runtime: RuntimeConfig;
  guardrails?: Guardrails;
  /** Observability opts (slice 5.1+). Today only `tracing` exists. */
  observability?: Observability;
  /** Set by CLI when user passes --resume <id>. Runtime opens/continues the named session. */
  sessionId?: string;
}

/**
 * Mirrors cli/internal/adl/types.go::Observability.
 *
 * Adapter-side OTel emission lands in slice 5.4 for opencode (flips
 * opencode's `experimental.openTelemetry: true` flag and threads the
 * TRACEPARENT delivered by slice 5.2). The Pi adapter's slice 5.3
 * implementation is the reference for `captureContent` semantics:
 * when true, the adapter attaches prompts/completions/tool args/results
 * as gen_ai.* span attributes; off by default for privacy.
 */
export interface Observability {
  tracing?: boolean;
  captureContent?: boolean;
}

/**
 * Per-session safety guardrail configuration. Defaults are applied at use
 * site in adapter.ts so this object can be `undefined` (no guardrails block
 * in the spec) without forcing the compiler to materialize defaults.
 */
export interface Guardrails {
  /**
   * How the runtime reacts when the assistant fabricates tool-call XML in
   * its message body. Defaults to "block" when absent. See honesty.ts for
   * the behavior of each mode.
   */
  hallucinationDetector?: HallucinationMode;
}

export type HallucinationMode = "warn" | "block" | "correct";

/** One MCP server declared in spec.mcpServers[]. Mirrors MCPServer in types.go. */
export interface MCPServer {
  name: string;
  transport: "stdio" | "streamable-http" | "sse";
  lifecycle?: "eager" | "lazy";
  // stdio fields
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  // http/sse fields
  url?: string;
  headers?: Record<string, string>;
}

export interface SpecMetadata {
  name: string;
  owner?: string;
  description?: string;
}

export interface Model {
  provider: "anthropic" | "openai" | "google";
  name: string;
  temperature?: number;
}

export interface Persona {
  role?: string;
  instructions?: string;
}

export interface ResolvedRef {
  name: string;
  /**
   * Absolute path to the Pi extension entrypoint. Blank when source is
   * set OR when builtin is true (Pi ships the implementation).
   */
  entrypoint?: string;
  /**
   * Pi-builtin tool (bash, read, edit, write). The runtime adds the name
   * to Pi's tool allowlist without loading any entrypoint.
   */
  builtin?: boolean;
  /**
   * Self-install source, e.g. "npm:pi-mcp-extension".
   * When set the runtime installs the package if missing and resolves
   * the entrypoint from the package's pi.extensions manifest field.
   * Only "npm:" prefix is supported at v0.1.6.
   */
  source?: string;
  config?: Record<string, unknown>;
}

export interface RuntimeConfig {
  /**
   * Which adapter the CLI dispatches the CompiledSpec to. `local` is the
   * v0.1.x legacy alias for `local-pi` (this Pi adapter) and remains
   * accepted by the schema for backwards compatibility. `local-opencode`
   * routes to the opencode adapter (runtime-opencode/), added in v0.2
   * slice 2.1. Mirror of the enum in schemas/adl.v1alpha1.json.
   */
  type: "local" | "local-pi" | "local-opencode";
  /**
   * v0.3.1 additive field: free-form capability requirements the runtime
   * must satisfy. Boolean flags consumed in two steps: v0.3.2 adds the
   * RuntimeBinding schema (resource advertising what capabilities a
   * target provides), and v0.3.3 wires Backend.Resolve() to compare the
   * two. Today (v0.3.1) it passes through CompiledSpec unchanged and
   * the opencode adapter does not act on it. Reserved well-known keys:
   * streaming, sandbox, gpu, restrictedNetwork, ephemeralFilesystem.
   * Arbitrary keys are accepted so capability bundles can advertise
   * their own flags (e.g. spark, notebookContext).
   */
  requirements?: Record<string, boolean>;
}

// Wire-protocol envelope and event types. Mirror of cli/internal/wire/events.go.

/** Stamped on every event agentctl emits after slice 5.2. */
export const EventsAPIVersionV1alpha1 = "agent-controller.dev/events/v1alpha1";

export type WireEventType =
  // Lifecycle (stable since v0.1.x).
  | "session.started"
  | "session.ended"
  | "message"
  | "model.request"
  | "model.response"
  // Long-running-session lifecycle (slice 6.4 of v0.6.0).
  | "session.resumed"
  | "session.paused"
  | "session.expired"
  // Tool execution (legacy — Pi + opencode adapters emit these through v0.5.x).
  | "tool.call"
  | "tool.result"
  // Tool execution (slice 5.2: reserved; 5.3 / 5.4 start emitting).
  | "tool.started"
  | "tool.completed"
  | "tool.failed"
  // Artifact lifecycle + audit (slice 5.2: reserved).
  | "artifact.created"
  | "audit.event"
  // Non-fatal guardrails (emitted today).
  | "warning"
  | "error";

/**
 * Slice 5.2 added two optional envelope fields:
 *
 *   - `apiVersion`: `agent-controller.dev/events/v1alpha1` once an
 *     adapter emits with the new shape. Absent on legacy v0.4.x
 *     events; consumers treat absence as "implicit v0 namespace".
 *
 *   - `traceparent`: W3C TraceContext header value
 *     (https://www.w3.org/TR/trace-context/) so adapter-side spans
 *     (slices 5.3 / 5.4) can be stitched as children of the host
 *     `agentctl.run` span. Absent when tracing is off.
 */
export interface WireEvent<T = unknown> {
  v: 1;
  apiVersion?: string;
  type: WireEventType;
  ts: string;
  sessionId: string;
  traceparent?: string;
  data: T;
}
