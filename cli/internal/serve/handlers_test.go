package serve

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

func TestSessionsCRUD_HTTP(t *testing.T) {
	m := newTestManager(t)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// create
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("create response missing id")
	}

	// list
	if r, _ := http.Get(srv.URL + "/v1/sessions"); r.StatusCode != 200 {
		t.Errorf("list = %d, want 200", r.StatusCode)
	}

	// get
	if r, _ := http.Get(srv.URL + "/v1/sessions/" + created.ID); r.StatusCode != 200 {
		t.Errorf("get = %d, want 200", r.StatusCode)
	}

	// get missing
	if r, _ := http.Get(srv.URL + "/v1/sessions/nope"); r.StatusCode != 404 {
		t.Errorf("get missing = %d, want 404", r.StatusCode)
	}

	// delete
	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/sessions/"+created.ID, nil)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 204 {
		t.Errorf("delete = %d, want 204", r.StatusCode)
	}
}

func TestCreateSession_Draining503(t *testing.T) {
	m := newTestManager(t)
	m.SetDraining(true)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Errorf("create while draining = %d, want 503", resp.StatusCode)
	}
}

// TestRunTurnHTTP_Draining503 verifies that POST /v1/sessions/{id}/turns
// returns a pre-stream JSON 503 (not an open SSE stream) when the server is
// draining. A new turn must be refused the same way a new session is refused.
func TestRunTurnHTTP_Draining503(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestManagerWithBackend(t, fb)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Create a session while not yet draining.
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create session = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Start draining, then attempt a new turn.
	m.SetDraining(true)

	turnResp, err := http.Post(
		srv.URL+"/v1/sessions/"+created.ID+"/turns",
		"application/json",
		strings.NewReader(`{"input":"hello"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer turnResp.Body.Close()

	if turnResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("turn while draining = %d, want 503", turnResp.StatusCode)
	}
	// Must be JSON (pre-stream), not text/event-stream.
	if ct := turnResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestCreateSession_TooMany429(t *testing.T) {
	m := newTestManager(t)
	// fill up sessions by creating MaxSessions (100) sessions — too many; instead set MaxSessions=1
	m2 := NewManager(Config{
		Store:          m.cfg.Store,
		Spec:           testSpec(),
		RuntimeCommand: []string{"true"},
		MaxSessions:    1,
	})
	srv := httptest.NewServer(buildMux(m2))
	defer srv.Close()

	// first create should succeed
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("first create = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// second create should be 429
	resp2, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != 429 {
		t.Errorf("second create = %d, want 429", resp2.StatusCode)
	}
}

func TestCreateSession_MalformedJSON400(t *testing.T) {
	m := newTestManager(t)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{bad json`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("malformed JSON = %d, want 400", resp.StatusCode)
	}
}

// TestCreateSession_EmptyBody201 verifies that POST /v1/sessions with a truly
// empty body (no JSON at all) is accepted and returns 201 with a non-empty id.
// This covers the curl -X POST /v1/sessions use case where the body is omitted.
func TestCreateSession_EmptyBody201(t *testing.T) {
	m := newTestManager(t)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("empty-body create = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" {
		t.Fatal("empty-body create response missing id")
	}
}

// TestRunTurnHTTP_HappyPath posts a turn to a created session and asserts
// the SSE response body contains the expected event frames.
func TestRunTurnHTTP_HappyPath(t *testing.T) {
	endedData, _ := json.Marshal(map[string]string{"reason": "success", "message": ""})
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionStarted, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventMessage, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		},
	}
	m := newTestManagerWithBackend(t, fb)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Create a session first.
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create session = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// POST a turn — response body is the SSE stream.
	turnResp, err := http.Post(
		srv.URL+"/v1/sessions/"+created.ID+"/turns",
		"application/json",
		strings.NewReader(`{"input":"hello"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer turnResp.Body.Close()

	if turnResp.StatusCode != 200 {
		t.Fatalf("turn status = %d, want 200", turnResp.StatusCode)
	}
	if ct := turnResp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body, err := io.ReadAll(turnResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "event: session.started") {
		t.Errorf("SSE body missing 'event: session.started'; got:\n%s", bodyStr)
	}
	if !strings.Contains(bodyStr, "event: session.ended") {
		t.Errorf("SSE body missing 'event: session.ended'; got:\n%s", bodyStr)
	}
}

// TestRunTurnHTTP_NotFound asserts that POST /v1/sessions/{id}/turns for a
// missing session id returns a pre-stream JSON 404.
func TestRunTurnHTTP_NotFound(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestManagerWithBackend(t, fb)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	resp, err := http.Post(
		srv.URL+"/v1/sessions/does-not-exist/turns",
		"application/json",
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("missing session turn = %d, want 404", resp.StatusCode)
	}
	// Must be JSON (pre-stream), not SSE.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestRunTurnHTTP_ErrorEvent_ExactlyOneErrorFrame verifies Fix 1: when the
// backend scripts an EventError event, the SSE body contains EXACTLY ONE
// "event: error" frame — not two (the adapter-streamed one plus a synthetic
// one from handleRunTurn).
func TestRunTurnHTTP_ErrorEvent_ExactlyOneErrorFrame(t *testing.T) {
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventError, Ts: time.Now().UTC()},
		},
	}
	m := newTestManagerWithBackend(t, fb)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Create a session first.
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("create session = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// POST a turn that will produce an error event from the backend.
	turnResp, err := http.Post(
		srv.URL+"/v1/sessions/"+created.ID+"/turns",
		"application/json",
		strings.NewReader(`{"input":"hello"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer turnResp.Body.Close()

	if turnResp.StatusCode != 200 {
		t.Fatalf("turn status = %d, want 200", turnResp.StatusCode)
	}

	body, err := io.ReadAll(turnResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	bodyStr := string(body)

	// Count occurrences of "event: error" — must be exactly 1.
	count := strings.Count(bodyStr, "event: error")
	if count != 1 {
		t.Errorf("SSE body contains %d 'event: error' frame(s), want exactly 1; body:\n%s", count, bodyStr)
	}
}

// TestRunTurnHTTP_TerminalSession_PreStream404 verifies Fix 2: POSTing a turn
// to a TERMINAL session returns a pre-stream JSON 404 (not an SSE 200 that
// 404s inside the stream).
func TestRunTurnHTTP_TerminalSession_PreStream404(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestManagerWithBackend(t, fb)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Create a session, then transition it to a terminal state via the store.
	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err = m.cfg.Store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Status = sessions.StatusEnded
	if err := m.cfg.Store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	// POST a turn to the now-terminal session.
	resp, err := http.Post(
		srv.URL+"/v1/sessions/"+sess.ID+"/turns",
		"application/json",
		strings.NewReader(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Must be pre-stream 404, not SSE 200.
	if resp.StatusCode != 404 {
		t.Fatalf("terminal session turn = %d, want 404", resp.StatusCode)
	}
	// Must be JSON (pre-stream), not text/event-stream.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestRunTurnHTTP_ConcurrentTurns_409 verifies per-session single-flight at
// the HTTP layer: posting two concurrent turns to the same session returns
// 200 (SSE) for the first and 409 (JSON) for the second. A turn on a
// DIFFERENT session must not be rejected. Run with -race.
func TestRunTurnHTTP_ConcurrentTurns_409(t *testing.T) {
	gate := make(chan struct{})
	gb := &gatedBackend{gate: gate}

	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 4,
		MaxSessions:        100,
		Backend:            gb,
	}
	m := NewManager(cfg)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Helper: create a session via HTTP.
	createSession := func(t *testing.T) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("create session = %d, want 201", resp.StatusCode)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}

	sessID := createSession(t)
	otherSessID := createSession(t)

	// firstGotHeaders is closed once the server sends the first turn's response
	// headers (i.e. the lock is held and the SSE 200 has been written).
	firstGotHeaders := make(chan struct{})

	// Launch first turn in the background — it will block in gatedBackend.Events
	// until we close gate.
	var wg sync.WaitGroup
	wg.Add(1)
	firstStatusCh := make(chan int, 1)
	go func() {
		defer wg.Done()
		// Use a transport that notifies us as soon as response headers arrive,
		// without needing to read the body first.
		client := &http.Client{
			Transport: &notifyOnHeadersTransport{
				wrapped: http.DefaultTransport,
				notify:  firstGotHeaders,
			},
		}
		r, err := client.Post(srv.URL+"/v1/sessions/"+sessID+"/turns",
			"application/json", strings.NewReader(`{"input":"turn1"}`))
		if err != nil {
			t.Errorf("first turn request: %v", err)
			firstStatusCh <- 0
			return
		}
		firstStatusCh <- r.StatusCode
		r.Body.Close()
	}()

	// Wait until the first turn has sent its response headers (lock acquired,
	// SSE 200 written).
	select {
	case <-firstGotHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not start within 5 s")
	}

	// Second turn on the SAME session must get 409 with JSON body.
	resp2, err := http.Post(srv.URL+"/v1/sessions/"+sessID+"/turns",
		"application/json", strings.NewReader(`{"input":"turn2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second turn (same session) status = %d, want 409", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("second turn Content-Type = %q, want application/json (pre-stream)", ct)
	}

	// Turn on DIFFERENT session must NOT be rejected (different lock slot).
	// Use a regular (non-gated) backend for this check by calling tryLockSession
	// directly — the gated backend would also block. We verify at the Manager
	// level: tryLockSession on a distinct id must succeed.
	unlock, okOther := m.tryLockSession(otherSessID)
	if !okOther {
		t.Error("tryLockSession on different session: got busy, want available")
	} else {
		unlock()
	}

	// Release gate so the first turn can finish.
	close(gate)
	wg.Wait()

	firstStatus := <-firstStatusCh
	if firstStatus != http.StatusOK {
		t.Errorf("first turn status = %d, want 200", firstStatus)
	}
}

// notifyOnHeadersTransport wraps an http.RoundTripper and closes the notify
// channel as soon as response headers are available (before the body is read).
type notifyOnHeadersTransport struct {
	wrapped http.RoundTripper
	notify  chan struct{}
	once    sync.Once
}

func (t *notifyOnHeadersTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.wrapped.RoundTrip(req)
	if err == nil {
		t.once.Do(func() { close(t.notify) })
	}
	return resp, err
}

// TestRunTurnHTTP_GlobalCap429 verifies the global concurrent-turn cap at the
// HTTP layer: with MaxConcurrentTurns=1, posting a second turn to a DIFFERENT
// session while the first is in-flight returns 429 with application/json.
// Run with -race.
func TestRunTurnHTTP_GlobalCap429(t *testing.T) {
	gate := make(chan struct{})
	gb := &gatedBackend{gate: gate}

	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 1, // global cap = 1
		MaxSessions:        100,
		Backend:            gb,
	}
	m := NewManager(cfg)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Helper: create a session via HTTP.
	createSession := func(t *testing.T) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("create session = %d, want 201", resp.StatusCode)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}

	sessID1 := createSession(t)
	sessID2 := createSession(t)

	// firstGotHeaders is closed once the first turn's SSE 200 has been written.
	firstGotHeaders := make(chan struct{})

	// Launch first turn against sess1 — it will block in gatedBackend.Events.
	var wg sync.WaitGroup
	wg.Add(1)
	firstStatusCh := make(chan int, 1)
	go func() {
		defer wg.Done()
		client := &http.Client{
			Transport: &notifyOnHeadersTransport{
				wrapped: http.DefaultTransport,
				notify:  firstGotHeaders,
			},
		}
		r, err := client.Post(srv.URL+"/v1/sessions/"+sessID1+"/turns",
			"application/json", strings.NewReader(`{"input":"turn1"}`))
		if err != nil {
			t.Errorf("first turn request: %v", err)
			firstStatusCh <- 0
			return
		}
		firstStatusCh <- r.StatusCode
		r.Body.Close()
	}()

	// Wait until the first turn has written its SSE 200 (slot is now held).
	select {
	case <-firstGotHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not start within 5 s")
	}

	// Second turn on a DIFFERENT session — must hit the global cap → 429 JSON.
	resp2, err := http.Post(srv.URL+"/v1/sessions/"+sessID2+"/turns",
		"application/json", strings.NewReader(`{"input":"turn2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second turn (different session) status = %d, want 429", resp2.StatusCode)
	}
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("second turn Content-Type = %q, want application/json (pre-stream)", ct)
	}

	// Release gate → first turn finishes → slot freed.
	close(gate)
	wg.Wait()

	if firstStatus := <-firstStatusCh; firstStatus != http.StatusOK {
		t.Errorf("first turn status = %d, want 200", firstStatus)
	}
}

// TestRunTurnHTTP_GlobalCap_Unlimited verifies that MaxConcurrentTurns=0 does
// not cap concurrency: multiple concurrent turns all proceed (no 429).
func TestRunTurnHTTP_GlobalCap_Unlimited(t *testing.T) {
	gate := make(chan struct{})
	gb := &gatedBackend{gate: gate}

	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 0, // unlimited
		MaxSessions:        100,
		Backend:            gb,
	}
	m := NewManager(cfg)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()

	// Create two sessions.
	createSession := func(t *testing.T) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/sessions", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("create session = %d, want 201", resp.StatusCode)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		return created.ID
	}

	sessID1 := createSession(t)
	sessID2 := createSession(t)

	firstGotHeaders := make(chan struct{})
	secondGotHeaders := make(chan struct{})

	startTurn := func(sessID string, notify chan struct{}) chan int {
		statusCh := make(chan int, 1)
		go func() {
			client := &http.Client{
				Transport: &notifyOnHeadersTransport{
					wrapped: http.DefaultTransport,
					notify:  notify,
				},
			}
			r, err := client.Post(srv.URL+"/v1/sessions/"+sessID+"/turns",
				"application/json", strings.NewReader(`{"input":"hi"}`))
			if err != nil {
				statusCh <- 0
				return
			}
			statusCh <- r.StatusCode
			r.Body.Close()
		}()
		return statusCh
	}

	ch1 := startTurn(sessID1, firstGotHeaders)
	select {
	case <-firstGotHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn did not start within 5 s")
	}

	// With unlimited cap, second turn on a different session must also get 200.
	ch2 := startTurn(sessID2, secondGotHeaders)
	select {
	case <-secondGotHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("second turn did not start within 5 s")
	}

	// Both turns are in-flight — release gate and collect statuses.
	close(gate)

	if s := <-ch1; s != http.StatusOK {
		t.Errorf("turn1 status = %d, want 200", s)
	}
	if s := <-ch2; s != http.StatusOK {
		t.Errorf("turn2 status = %d, want 200", s)
	}
}
