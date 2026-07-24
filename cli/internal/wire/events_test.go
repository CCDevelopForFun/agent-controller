package wire

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeKnownEvent(t *testing.T) {
	line := `{"v":1,"type":"tool.call","ts":"2026-05-25T17:23:04.182Z","sessionId":"s_abc","data":{"toolName":"get_time","callId":"c1"}}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.Type != "tool.call" {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.SessionID != "s_abc" {
		t.Errorf("SessionID = %q", ev.SessionID)
	}
	if got, want := ev.Ts, time.Date(2026, 5, 25, 17, 23, 4, 182_000_000, time.UTC); !got.Equal(want) {
		t.Errorf("Ts = %v, want %v", got, want)
	}
}

func TestDecodeWrongVersionRejected(t *testing.T) {
	line := `{"v":2,"type":"session.started","ts":"2026-05-25T00:00:00Z","sessionId":"s","data":{}}`
	_, err := Decode([]byte(line))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("expected version error, got %v", err)
	}
}

// Slice 5.2: optional apiVersion + traceparent fields. Legacy events
// without them must continue to decode, and events with them must
// round-trip.

func TestDecodeLegacyEventWithoutApiVersionOrTraceparent(t *testing.T) {
	line := `{"v":1,"type":"message","ts":"2026-06-10T00:00:00Z","sessionId":"s","data":{"role":"user","text":"hi"}}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.ApiVersion != "" {
		t.Errorf("ApiVersion should be empty for legacy events, got %q", ev.ApiVersion)
	}
	if ev.Traceparent != "" {
		t.Errorf("Traceparent should be empty for legacy events, got %q", ev.Traceparent)
	}
}

func TestDecodeV1alpha1EventCarriesNewFields(t *testing.T) {
	line := `{"v":1,"apiVersion":"agent-controller.dev/events/v1alpha1","type":"tool.started","ts":"2026-06-10T00:00:00Z","sessionId":"s","traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","data":{"toolName":"get_time","callId":"c1","arguments":{}}}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ev.ApiVersion != EventsAPIVersionV1alpha1 {
		t.Errorf("ApiVersion = %q, want %q", ev.ApiVersion, EventsAPIVersionV1alpha1)
	}
	if ev.Traceparent != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Errorf("Traceparent round-trip wrong: %q", ev.Traceparent)
	}
	if ev.Type != EventToolStarted {
		t.Errorf("Type = %q, want tool.started", ev.Type)
	}
}

func TestDecodeAcceptsAllReservedEventTypes(t *testing.T) {
	// Smoke-test that every reserved type can round-trip through Decode
	// — catches typos in the constants and confirms the type-set is open
	// for adapter emission in slices 5.3+.
	types := []EventType{
		EventToolStarted, EventToolCompleted, EventToolFailed,
		EventArtifactCreated, EventAuditEvent,
		// Slice 6.4: long-running-session lifecycle additions.
		EventSessionResumed, EventSessionPaused, EventSessionExpired,
	}
	for _, ty := range types {
		line := `{"v":1,"apiVersion":"agent-controller.dev/events/v1alpha1","type":"` + string(ty) + `","ts":"2026-06-10T00:00:00Z","sessionId":"s","data":{}}`
		ev, err := Decode([]byte(line))
		if err != nil {
			t.Errorf("Decode %s: %v", ty, err)
			continue
		}
		if ev.Type != ty {
			t.Errorf("type round-trip: got %q want %q", ev.Type, ty)
		}
	}
}
