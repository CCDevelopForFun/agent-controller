package serve

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// writeJSON encodes v as JSON and sends it with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr sends a JSON error envelope: {"error":{"code":<status>,"message":<msg>}}.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    status,
			"message": msg,
		},
	})
}

func (m *Manager) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (m *Manager) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if m.Draining() {
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

// sessionResponse is the JSON shape returned for a single session.
type sessionResponse struct {
	ID          string    `json:"id"`
	AgentName   string    `json:"agentName"`
	RuntimeType string    `json:"runtimeType"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
}

func sessionToResponse(s sessions.Session) sessionResponse {
	return sessionResponse{
		ID:          s.ID,
		AgentName:   s.AgentName,
		RuntimeType: s.RuntimeType,
		Status:      string(s.Status),
		CreatedAt:   s.CreatedAt,
	}
}

// handleCreateSession handles POST /v1/sessions.
// Body: optional — omitted entirely or {} creates a session with no inputs;
// {"inputs":{"k":"v"}} supplies interpolation values.
// Malformed non-empty JSON returns 400.
// Returns 201 + session JSON on success.
// Returns 503 if draining, 429 if MaxSessions reached, 400 on malformed JSON.
func (m *Manager) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if m.Draining() {
		writeErr(w, http.StatusServiceUnavailable, "server is draining")
		return
	}

	var body struct {
		Inputs map[string]string `json:"inputs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	s, err := m.CreateSession(r.Context(), body.Inputs)
	if err != nil {
		if errors.Is(err, ErrTooManySessions) {
			writeErr(w, http.StatusTooManyRequests, "max sessions reached")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, sessionToResponse(s))
}

// handleListSessions handles GET /v1/sessions.
// Optional query param: ?status=active|ended|...
func (m *Manager) handleListSessions(w http.ResponseWriter, r *http.Request) {
	statusFilter := sessions.SessionStatus(r.URL.Query().Get("status"))

	list, err := m.ListSessions(r.Context(), statusFilter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := make([]sessionResponse, len(list))
	for i, s := range list {
		resp[i] = sessionToResponse(s)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetSession handles GET /v1/sessions/{id}.
// Returns 200 + session JSON, or 404 if not found.
func (m *Manager) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s, err := m.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, sessionToResponse(s))
}

// handleDeleteSession handles DELETE /v1/sessions/{id}.
// Returns 204 on success, or 404 if not found.
func (m *Manager) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := m.EndSession(r.Context(), id); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRunTurn handles POST /v1/sessions/{id}/turns.
// The response body is an SSE stream of wire events for the turn.
func (m *Manager) handleRunTurn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Refuse new turns during graceful drain — match handleCreateSession behavior.
	if m.Draining() {
		writeErr(w, http.StatusServiceUnavailable, "server is draining")
		return
	}

	// Decode body: {"input": "..."} — EOF treated as empty input.
	var body struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Pre-flight existence check: return a clean pre-stream 404 for missing or terminal sessions.
	s, err := m.GetSession(r.Context(), id)
	if errors.Is(err, sessions.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isTerminalStatus(s.Status) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}

	// Per-session single-flight: at most one turn in-flight per session id.
	// tryLockSession uses TryLock so this is a non-blocking check. The lock is
	// released via defer AFTER the SSE stream completes. This check MUST happen
	// before SSE headers are flushed so a busy response arrives as a clean 409.
	unlock, ok := m.tryLockSession(id)
	if !ok {
		writeErr(w, http.StatusConflict, "session busy: a turn is already in progress")
		return
	}
	defer unlock()

	// Global turn semaphore: at most MaxConcurrentTurns turns active across all
	// sessions. This is a non-blocking acquire — if the semaphore is full, we
	// return a clean pre-stream 429. The slot is released via defer AFTER the
	// SSE stream completes (slot freed on all exit paths including RunTurn errors).
	release, slotOK := m.acquireTurnSlot()
	if !slotOK {
		writeErr(w, http.StatusTooManyRequests, "too many concurrent turns")
		return
	}
	defer release()

	// Assert the ResponseWriter supports streaming.
	flusher, ok2 := w.(http.Flusher)
	if !ok2 {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Set SSE headers and flush so the client sees 200 immediately.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// emitted tracks whether at least one SSE frame has been sent.
	// RunTurn calls emit synchronously in its own drain loop (no separate
	// goroutine), so a plain bool is safe here — no race.
	emitted := false
	emit := func(ev wire.Event) error {
		emitted = true
		if err := WriteSSE(w, ev); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := m.RunTurn(r.Context(), id, body.Input, emit); err != nil && !emitted {
		// Only synthesize an error frame when nothing was streamed yet.
		// If emit was already called, the adapter already sent an error
		// event (EventError or session.ended{reason=error}) — a second
		// frame would duplicate the signal for the client.
		errData, _ := json.Marshal(map[string]string{"message": err.Error()})
		ev := wire.Event{
			V:         wire.ProtocolVersion,
			Type:      wire.EventError,
			SessionID: id,
			Ts:        time.Now().UTC(),
			Data:      json.RawMessage(errData),
		}
		_ = WriteSSE(w, ev)
		flusher.Flush()
	}
}
