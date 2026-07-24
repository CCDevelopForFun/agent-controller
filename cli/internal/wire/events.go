// Package wire defines the CLI<->runtime stdio protocol.
//
// One JSON document in (the CompiledSpec, written to runtime stdin and
// closed); newline-delimited JSON events out (one envelope per line on
// runtime stdout). See spec §6 for the canonical definition.
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// ProtocolVersion is the integer wire-version field every event carries.
// Stays at 1 through v0.5.x; bumps to 2 only on a wire-incompatible
// change. Slice 5.2 introduces a parallel `apiVersion` namespace
// (EventsAPIVersionV1alpha1) for additive shape evolution that doesn't
// need a v-bump.
const ProtocolVersion = 1

// EventsAPIVersionV1alpha1 is the apiVersion stamped on every event
// agentctl emits after slice 5.2. The string deliberately mirrors the
// ADL schema namespace (`agent-controller.dev/v1alpha1`) so consumers
// can reuse the same versioning intuition across resources, schemas,
// and the wire protocol. Events without an `apiVersion` field at all
// are accepted (legacy v0.4.x and earlier emitted no namespace).
const EventsAPIVersionV1alpha1 = "agent-controller.dev/events/v1alpha1"

type EventType string

const (
	// Lifecycle (stable since v0.1.x).
	EventSessionStarted EventType = "session.started"
	EventSessionEnded   EventType = "session.ended"
	EventMessage        EventType = "message"
	EventModelRequest   EventType = "model.request"
	EventModelResponse  EventType = "model.response"

	// Long-running-session lifecycle (slice 6.4 of v0.6.0). These
	// supplement the started/ended pair to describe sessions that
	// outlive a single run:
	//   - resumed: a new chat turn is reusing a stored session id.
	//     `data.previousLastActiveAt` carries the prior timestamp so
	//     observability dashboards can compute time-since-last-touch.
	//   - paused: the user stepped away without a clean /exit
	//     (EOF or SIGTERM). The session record stays paused in the
	//     store; a future chat --resume continues it. Distinct from
	//     `ended` (explicit /exit) so resume-fitness logic in slice
	//     6.6 can pick paused over ended.
	//   - expired: the TTL sweep moved this session past its
	//     idle-time limit. Resume attempts return this on the wire
	//     before bailing.
	EventSessionResumed EventType = "session.resumed"
	EventSessionPaused  EventType = "session.paused"
	EventSessionExpired EventType = "session.expired"

	// Tool execution (legacy — emitted by the Pi and opencode adapters
	// through v0.5.x). Slice 5.3 layers the started/completed/failed
	// triplet over these for richer span boundaries.
	EventToolCall   EventType = "tool.call"
	EventToolResult EventType = "tool.result"

	// Tool execution (slice 5.2: reserved; slice 5.3 starts emitting).
	// The split lets observability tooling open a span at `started`,
	// attach the tool arguments, and close it at `completed`/`failed`
	// with the result + status. Maps cleanly onto OTel semconv
	// (`gen_ai.tool.call.*`).
	EventToolStarted   EventType = "tool.started"
	EventToolCompleted EventType = "tool.completed"
	EventToolFailed    EventType = "tool.failed"

	// Artifact lifecycle (slice 5.2: reserved; emission lands when an
	// adapter actually produces durable artifacts — file writes, MCP
	// objects, etc.).
	EventArtifactCreated EventType = "artifact.created"

	// Audit / governance events (slice 5.2: reserved; v0.5+ governance
	// fields will emit these).
	EventAuditEvent EventType = "audit.event"

	// Non-fatal guardrails (emitted today).
	EventWarning EventType = "warning"
	EventError   EventType = "error"
)

// Event is the decoded form of one NDJSON line from the runtime.
//
// Slice 5.2 added two optional fields:
//
//   - ApiVersion stamps the event with `agent-controller.dev/events/v1alpha1`
//     so future protocol additions can introduce new shapes without
//     forcing a `v:` bump. Absent on legacy v0.4.x and earlier events;
//     callers should treat absence as "implicit v0 namespace, legacy
//     shape" (i.e. only the event types in this file that pre-date 5.2
//     can appear).
//
//   - Traceparent carries the W3C TraceContext header value
//     (https://www.w3.org/TR/trace-context/) so adapter-side spans
//     (slices 5.3 / 5.4) can be stitched as children of the host
//     `agentctl.run` span. Absent when tracing is off.
type Event struct {
	V           int             `json:"v"`
	ApiVersion  string          `json:"apiVersion,omitempty"`
	Type        EventType       `json:"type"`
	Ts          time.Time       `json:"ts"`
	SessionID   string          `json:"sessionId"`
	Traceparent string          `json:"traceparent,omitempty"`
	Data        json.RawMessage `json:"data"`
}

// Decode parses one NDJSON line into an Event.
//
// Backwards compatibility: events without an `apiVersion` field are
// accepted verbatim — legacy producers (v0.4.x and earlier) emit
// envelopes with just `v: 1`. Decode does NOT enforce that
// apiVersion-tagged events only carry types declared in v1alpha1; the
// type set is a forward-compatible enum, so older consumers gracefully
// ignore unknown types.
func Decode(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, fmt.Errorf("decode wire event: %w", err)
	}
	if ev.V != ProtocolVersion {
		return ev, fmt.Errorf("unsupported wire protocol version %d (expected %d)", ev.V, ProtocolVersion)
	}
	return ev, nil
}
