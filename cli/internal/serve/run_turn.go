package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/observability"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// isTerminalStatus reports whether s is a session status from which no
// further turns can be dispatched.
func isTerminalStatus(s sessions.SessionStatus) bool {
	switch s {
	case sessions.StatusEnded, sessions.StatusExpired, sessions.StatusFailed:
		return true
	default:
		return false
	}
}

// RunTurn drives a single adapter turn for the session identified by id.
// It mirrors the runChatTurn logic in cli/cmd/agentctl/chat.go:
//
//  1. Load the session; return ErrNotFound if missing or in a terminal state.
//  2. Build a per-turn spec (Task = input, SessionID = session id).
//  3. Resolve → emit warnings via emit.
//  4. Submit → drain Events, forwarding each to emit.
//  5. Map EventError / session.ended{reason=error|cancelled} to a returned error.
//  6. Best-effort: update LastActiveAt in the store before returning.
func (m *Manager) RunTurn(ctx context.Context, id, input string, emit func(wire.Event) error) error {
	s, err := m.cfg.Store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return sessions.ErrNotFound
		}
		return fmt.Errorf("serve: load session: %w", err)
	}
	if isTerminalStatus(s.Status) {
		return sessions.ErrNotFound
	}

	// Open one OTel root span per turn. When tracing is disabled
	// (InitTracerProvider not called / OTEL_EXPORTER_OTLP_* unset),
	// StartRootSpan returns a no-op span — safe to call unconditionally.
	ctx, span := observability.StartRootSpan(ctx, observability.RunAttributes{
		AgentName:     s.Spec.Metadata.Name,
		ModelProvider: s.Spec.Model.Provider,
		ModelName:     s.Spec.Model.Name,
		RuntimeType:   s.Spec.Runtime.Type,
		BackendType:   "local",
		SessionID:     s.ID,
	})
	defer span.End()

	// Build per-turn spec: set Task and SessionID but leave the rest
	// unchanged (persona, model, tools etc. come from the stored session).
	spec := s.Spec
	spec.Task = input
	spec.SessionID = &s.ID

	run, warnings, err := m.cfg.Backend.Resolve(ctx, spec, nil)
	if err != nil {
		return fmt.Errorf("serve: resolve: %w", err)
	}
	for _, w := range warnings {
		if emitErr := emit(w); emitErr != nil {
			return fmt.Errorf("serve: emit warning: %w", emitErr)
		}
	}

	h, err := m.cfg.Backend.Submit(ctx, run)
	if err != nil {
		return fmt.Errorf("serve: submit: %w", err)
	}

	var turnErr error
	for ev := range m.cfg.Backend.Events(h) {
		if emitErr := emit(ev); emitErr != nil {
			go m.cfg.Backend.Stop(h)
			return fmt.Errorf("serve: emit event: %w", emitErr)
		}
		if ev.Type == wire.EventError {
			turnErr = errors.New("turn ended with error event")
		}
		if ev.Type == wire.EventSessionEnded {
			var ended struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(ev.Data, &ended)
			switch ended.Reason {
			case "error":
				if ended.Message != "" {
					turnErr = fmt.Errorf("turn ended with error: %s", ended.Message)
				} else {
					turnErr = errors.New("turn ended with reason=error")
				}
			case "cancelled":
				turnErr = errors.New("turn was cancelled")
			}
		}
	}

	// Best-effort store update: touch LastActiveAt so TTL sweeps see
	// activity. Ignore the error — if the store rejects it (expired
	// session, network blip) the turn result still propagates normally.
	s.LastActiveAt = time.Now().UTC()
	_ = m.cfg.Store.Update(ctx, s)

	return turnErr
}
