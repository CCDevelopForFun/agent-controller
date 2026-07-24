package serve

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// fakeBackend is a test double for backend.Backend. It returns a
// scripted sequence of events on Events() and records calls.
type fakeBackend struct {
	events []wire.Event
}

func (f *fakeBackend) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}

func (f *fakeBackend) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	return "fake-handle", nil
}

func (f *fakeBackend) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event, len(f.events))
	for _, ev := range f.events {
		ch <- ev
	}
	close(ch)
	return ch
}

func (f *fakeBackend) Stop(_ backend.SessionHandle) error { return nil }

func (f *fakeBackend) Capabilities() backend.Caps { return backend.Caps{} }

// newTestManagerWithBackend returns a Manager wired with the provided fake backend.
func newTestManagerWithBackend(t *testing.T, fb *fakeBackend) *Manager {
	t.Helper()
	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 4,
		MaxSessions:        100,
		Backend:            fb,
	}
	return NewManager(cfg)
}

func TestRunTurn_HappyPath(t *testing.T) {
	endedData, _ := json.Marshal(map[string]string{"reason": "success", "message": ""})
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionStarted, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventMessage, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		},
	}
	m := newTestManagerWithBackend(t, fb)

	// Create a session to run the turn against.
	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var received []wire.Event
	runErr := m.RunTurn(context.Background(), sess.ID, "hello", func(ev wire.Event) error {
		received = append(received, ev)
		return nil
	})
	if runErr != nil {
		t.Fatalf("RunTurn returned unexpected error: %v", runErr)
	}
	if len(received) != 3 {
		t.Fatalf("got %d events, want 3", len(received))
	}
	if received[0].Type != wire.EventSessionStarted {
		t.Errorf("event[0].Type = %q, want %q", received[0].Type, wire.EventSessionStarted)
	}
	if received[1].Type != wire.EventMessage {
		t.Errorf("event[1].Type = %q, want %q", received[1].Type, wire.EventMessage)
	}
	if received[2].Type != wire.EventSessionEnded {
		t.Errorf("event[2].Type = %q, want %q", received[2].Type, wire.EventSessionEnded)
	}
}

func TestRunTurn_ErrorEvent(t *testing.T) {
	fb := &fakeBackend{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventError, Ts: time.Now().UTC()},
		},
	}
	m := newTestManagerWithBackend(t, fb)

	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	runErr := m.RunTurn(context.Background(), sess.ID, "hello", func(ev wire.Event) error { return nil })
	if runErr == nil {
		t.Fatal("RunTurn returned nil, want non-nil error for error event")
	}
}

func TestRunTurn_NotFound(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestManagerWithBackend(t, fb)
	err := m.RunTurn(context.Background(), "nonexistent-id", "hi", func(ev wire.Event) error { return nil })
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("got %v, want sessions.ErrNotFound", err)
	}
}

func TestRunTurn_TerminalSessionNotFound(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestManagerWithBackend(t, fb)

	// Create a session and transition it to terminal status.
	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch, set status to ended, and update in the store.
	sess, err = m.cfg.Store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess.Status = sessions.StatusEnded
	if err := m.cfg.Store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	// RunTurn should return ErrNotFound for terminal sessions.
	err = m.RunTurn(context.Background(), sess.ID, "hello", func(ev wire.Event) error { return nil })
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Errorf("got %v, want sessions.ErrNotFound", err)
	}
}

func TestRunTurn_EmitErrorCallsStop(t *testing.T) {
	// Create a fake backend that tracks Stop calls.
	fb := &fakeBackendWithStopTracking{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionStarted, Ts: time.Now().UTC()},
		},
		stopped: make(chan struct{}, 1),
	}

	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 4,
		MaxSessions:        100,
		Backend:            fb,
	}
	m := NewManager(cfg)

	// Create a session.
	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// RunTurn with an emit function that returns an error.
	runErr := m.RunTurn(context.Background(), sess.ID, "hello", func(ev wire.Event) error {
		return errors.New("emit failed")
	})
	if runErr == nil {
		t.Fatal("RunTurn returned nil, want non-nil error")
	}

	// Wait for Stop to be called on the backend with a timeout.
	select {
	case <-fb.stopped:
		// Stop was called — pass
	case <-time.After(2 * time.Second):
		t.Fatal("Backend.Stop was not called after emit error")
	}
}

// fakeBackendWithStopTracking is a test double that tracks Stop calls.
type fakeBackendWithStopTracking struct {
	events  []wire.Event
	stopped chan struct{}
}

func (f *fakeBackendWithStopTracking) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}

func (f *fakeBackendWithStopTracking) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	return "fake-handle", nil
}

func (f *fakeBackendWithStopTracking) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event, len(f.events))
	for _, ev := range f.events {
		ch <- ev
	}
	close(ch)
	return ch
}

func (f *fakeBackendWithStopTracking) Stop(_ backend.SessionHandle) error {
	select {
	case f.stopped <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeBackendWithStopTracking) Capabilities() backend.Caps {
	return backend.Caps{}
}

func TestCreateSession_PersistsActive(t *testing.T) {
	m := newTestManager(t)
	s, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != sessions.StatusActive {
		t.Errorf("status = %q", s.Status)
	}
	got, err := m.GetSession(context.Background(), s.ID)
	if err != nil || got.ID != s.ID {
		t.Fatalf("get: %v id=%q", err, got.ID)
	}
}

func TestCreateSession_MaxSessions429Path(t *testing.T) {
	m := NewManager(Config{Store: sessions.NewMemoryStore(), Spec: testSpec(), RuntimeCommand: []string{"true"}, MaxSessions: 1})
	if _, err := m.CreateSession(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateSession(context.Background(), nil); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("second create err = %v, want ErrTooManySessions", err)
	}
}

func TestCreateSession_UniqueIDsConcurrent(t *testing.T) {
	m := NewManager(Config{Store: sessions.NewMemoryStore(), Spec: testSpec(), RuntimeCommand: []string{"true"}})
	ctx := context.Background()

	const n = 2000
	ids := make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errChan := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.CreateSession(ctx, nil)
			if err != nil {
				errChan <- err
				return
			}
			mu.Lock()
			if ids[s.ID] {
				errChan <- errors.New("duplicate session ID: " + s.ID)
			}
			ids[s.ID] = true
			mu.Unlock()
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		if err != nil {
			t.Errorf("CreateSession error: %v", err)
		}
	}

	if len(ids) != n {
		t.Errorf("got %d unique IDs, want %d", len(ids), n)
	}
}

// fakeBackendRecordingSessionID records the SessionID from each Resolve call.
type fakeBackendRecordingSessionID struct {
	events      []wire.Event
	recordedIDs []string
}

func (f *fakeBackendRecordingSessionID) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	// Record the SessionID (or empty string if nil).
	var id string
	if spec.SessionID != nil {
		id = *spec.SessionID
	}
	f.recordedIDs = append(f.recordedIDs, id)
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}

func (f *fakeBackendRecordingSessionID) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	return "fake-handle", nil
}

func (f *fakeBackendRecordingSessionID) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event, len(f.events))
	for _, ev := range f.events {
		ch <- ev
	}
	close(ch)
	return ch
}

func (f *fakeBackendRecordingSessionID) Stop(_ backend.SessionHandle) error { return nil }

func (f *fakeBackendRecordingSessionID) Capabilities() backend.Caps { return backend.Caps{} }

func TestRunTurn_ResumeContinuity(t *testing.T) {
	endedData, _ := json.Marshal(map[string]string{"reason": "success", "message": ""})
	fb := &fakeBackendRecordingSessionID{
		events: []wire.Event{
			{V: wire.ProtocolVersion, Type: wire.EventSessionStarted, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventMessage, Ts: time.Now().UTC()},
			{V: wire.ProtocolVersion, Type: wire.EventSessionEnded, Ts: time.Now().UTC(), Data: endedData},
		},
	}
	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 4,
		MaxSessions:        100,
		Backend:            fb,
	}
	m := NewManager(cfg)

	// Create a session to run turns against.
	sess, err := m.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Run first turn.
	runErr := m.RunTurn(context.Background(), sess.ID, "hello turn 1", func(ev wire.Event) error {
		return nil
	})
	if runErr != nil {
		t.Fatalf("first RunTurn returned unexpected error: %v", runErr)
	}

	// Run second turn on the same session.
	runErr = m.RunTurn(context.Background(), sess.ID, "hello turn 2", func(ev wire.Event) error {
		return nil
	})
	if runErr != nil {
		t.Fatalf("second RunTurn returned unexpected error: %v", runErr)
	}

	// Verify that both Resolve calls received the same session ID.
	if len(fb.recordedIDs) != 2 {
		t.Fatalf("got %d recorded IDs, want 2", len(fb.recordedIDs))
	}

	if fb.recordedIDs[0] == "" {
		t.Error("first Resolve received nil/empty SessionID, want session.ID")
	}
	if fb.recordedIDs[1] == "" {
		t.Error("second Resolve received nil/empty SessionID, want session.ID")
	}

	if fb.recordedIDs[0] != sess.ID {
		t.Errorf("first Resolve SessionID = %q, want %q", fb.recordedIDs[0], sess.ID)
	}
	if fb.recordedIDs[1] != sess.ID {
		t.Errorf("second Resolve SessionID = %q, want %q", fb.recordedIDs[1], sess.ID)
	}

	// Prove continuity: both turns must have the same SessionID.
	if fb.recordedIDs[0] != fb.recordedIDs[1] {
		t.Errorf("SessionID mismatch across turns: turn1=%q, turn2=%q", fb.recordedIDs[0], fb.recordedIDs[1])
	}
}

// gatedBackend is a fake backend whose Events channel stays open until the
// caller closes the gate channel. This lets a test hold a turn in-flight
// while concurrently attempting a second turn on the same session.
type gatedBackend struct {
	gate chan struct{} // close to unblock Events drain
}

func (g *gatedBackend) Resolve(_ context.Context, spec adl.CompiledSpec, _ *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	return adl.ResolvedRunSpec{Spec: spec}, nil, nil
}

func (g *gatedBackend) Submit(_ context.Context, _ adl.ResolvedRunSpec) (backend.SessionHandle, error) {
	return "gate-handle", nil
}

func (g *gatedBackend) Events(_ backend.SessionHandle) <-chan wire.Event {
	ch := make(chan wire.Event)
	go func() {
		<-g.gate // block until gate is closed
		close(ch)
	}()
	return ch
}

func (g *gatedBackend) Stop(_ backend.SessionHandle) error { return nil }

func (g *gatedBackend) Capabilities() backend.Caps { return backend.Caps{} }

// TestTryLockSession_SingleFlight verifies per-session single-flight behaviour:
//  1. A second turn on the SAME session while one is in-flight is rejected as busy.
//  2. A turn on a DIFFERENT session is NOT rejected.
//
// Run with -race to confirm no data races.
func TestTryLockSession_SingleFlight(t *testing.T) {
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
	ctx := context.Background()

	// Create two separate sessions.
	sess1, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// started signals that the first turn has acquired the lock and is
	// blocked inside the backend Events drain.
	started := make(chan struct{})

	// Wrap the handler-level lock explicitly to simulate what handleRunTurn does.
	// Because RunTurn is lock-free (Approach A), we call tryLockSession directly.
	unlock1, ok := m.tryLockSession(sess1.ID)
	if !ok {
		t.Fatal("tryLockSession: expected ok=true for first acquire")
	}

	// Launch the first turn in a goroutine, holding the lock.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer unlock1()
		close(started) // signal that we've acquired the lock
		// Block until gate is released (simulating a long-running turn).
		_ = m.RunTurn(ctx, sess1.ID, "turn1", func(wire.Event) error { return nil })
	}()

	// Wait for the first turn to have acquired the lock.
	<-started

	// --- same session: must be rejected ---
	_, busy := m.tryLockSession(sess1.ID)
	if busy {
		t.Error("tryLockSession on busy session: got ok=true, want false (busy)")
	}

	// --- different session: must succeed ---
	unlock2, notBusy := m.tryLockSession(sess2.ID)
	if !notBusy {
		t.Error("tryLockSession on different session: got ok=false, want true (not busy)")
	} else {
		unlock2()
	}

	// Release the gate so the first turn can finish.
	close(gate)
	wg.Wait()

	// After the first turn completes, the lock must be released — a new turn
	// on sess1 must now succeed.
	unlock3, free := m.tryLockSession(sess1.ID)
	if !free {
		t.Error("tryLockSession after turn completed: got ok=false, want true (lock released)")
	} else {
		unlock3()
	}
}

// TestAcquireTurnSlot_GlobalCap verifies the global turn semaphore:
//  1. With MaxConcurrentTurns=1, a second turn on a DIFFERENT session is
//     rejected with ErrTooManyTurns while one slot is held.
//  2. After releasing the first slot, a subsequent acquire succeeds.
//
// Run with -race to confirm no data races.
func TestAcquireTurnSlot_GlobalCap(t *testing.T) {
	gate := make(chan struct{})
	gb := &gatedBackend{gate: gate}

	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 1, // cap at 1 global turn
		MaxSessions:        100,
		Backend:            gb,
	}
	m := NewManager(cfg)
	ctx := context.Background()

	// Create two separate sessions.
	sess1, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Acquire session lock for sess1 and the global turn slot — simulating
	// what handleRunTurn does before streaming.
	unlock1, lockOK := m.tryLockSession(sess1.ID)
	if !lockOK {
		t.Fatal("tryLockSession sess1: expected ok=true")
	}
	release1, slotOK := m.acquireTurnSlot()
	if !slotOK {
		unlock1()
		t.Fatal("acquireTurnSlot: expected ok=true for first acquire")
	}

	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer unlock1()
		defer release1()
		close(started)
		// Block until gate is released (holding the global slot).
		_ = m.RunTurn(ctx, sess1.ID, "turn1", func(wire.Event) error { return nil })
	}()

	<-started

	// --- different session: global cap must be enforced ---
	// The per-session lock for sess2 is free; only the global slot is exhausted.
	unlock2, lock2OK := m.tryLockSession(sess2.ID)
	if !lock2OK {
		t.Fatal("tryLockSession sess2: expected ok=true (different session)")
	}
	_, slotOK2 := m.acquireTurnSlot()
	if slotOK2 {
		unlock2()
		close(gate)
		wg.Wait()
		t.Error("acquireTurnSlot on second session: got ok=true, want false (global cap reached)")
		return
	}
	// Correct: slot was denied; release the session lock we acquired.
	unlock2()

	// Release gate → first turn finishes → slot freed.
	close(gate)
	wg.Wait()

	// After the first turn completes, the global slot must be free again.
	_, slotOK3 := m.acquireTurnSlot()
	if !slotOK3 {
		t.Error("acquireTurnSlot after turn completed: got ok=false, want true (slot released)")
	}
	// Note: release is already called by the goroutine's defer, so we need to
	// drain the semaphore manually for the slot we just acquired.
	<-m.turnSem
}

// TestAcquireTurnSlot_Unlimited verifies that MaxConcurrentTurns=0 means
// no cap: acquireTurnSlot always succeeds regardless of how many slots are held.
func TestAcquireTurnSlot_Unlimited(t *testing.T) {
	cfg := Config{
		Store:              sessions.NewMemoryStore(),
		Spec:               testSpec(),
		RuntimeCommand:     []string{"true"},
		MaxConcurrentTurns: 0, // unlimited
		MaxSessions:        100,
	}
	m := NewManager(cfg)

	// With unlimited concurrency, turnSem must be nil and every acquire succeeds.
	if m.turnSem != nil {
		t.Error("turnSem should be nil when MaxConcurrentTurns=0")
	}

	const n = 10
	releases := make([]func(), n)
	for i := 0; i < n; i++ {
		release, ok := m.acquireTurnSlot()
		if !ok {
			t.Fatalf("acquireTurnSlot #%d: got ok=false, want true (unlimited)", i)
		}
		releases[i] = release
	}
	// Clean up: call all release functions (they are no-ops for unlimited).
	for _, r := range releases {
		r()
	}
}

// TestSweepOnce_ExpiresIdleSession verifies that sweepOnce transitions an
// inactive session (LastActiveAt well past the TTL cutoff) to StatusExpired.
func TestSweepOnce_ExpiresIdleSession(t *testing.T) {
	ctx := context.Background()
	m := NewManager(Config{
		Store:      sessions.NewMemoryStore(),
		Spec:       testSpec(),
		SessionTTL: time.Minute,
	})

	// Create a session.
	sess, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Force LastActiveAt into the past (well beyond the 1-minute TTL).
	sess, err = m.cfg.Store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess.LastActiveAt = time.Now().UTC().Add(-2 * time.Hour)
	if err := m.cfg.Store.Update(ctx, sess); err != nil {
		t.Fatal(err)
	}

	// Call sweepOnce directly — no ticker involved.
	expired, err := m.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}

	// The session ID must appear in the returned list.
	found := false
	for _, id := range expired {
		if id == sess.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sweepOnce returned %v, want %q in the list", expired, sess.ID)
	}

	// The store must now show the session as StatusExpired.
	got, err := m.cfg.Store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after sweepOnce: %v", err)
	}
	if got.Status != sessions.StatusExpired {
		t.Errorf("session.Status = %q after sweepOnce, want %q", got.Status, sessions.StatusExpired)
	}
}

// TestSweepOnce_SparesFreshSession verifies that a recently-active session
// is NOT expired by sweepOnce.
func TestSweepOnce_SparesFreshSession(t *testing.T) {
	ctx := context.Background()
	m := NewManager(Config{
		Store:      sessions.NewMemoryStore(),
		Spec:       testSpec(),
		SessionTTL: time.Hour, // generous TTL
	})

	// Create a session with a current LastActiveAt (just created).
	sess, err := m.CreateSession(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	expired, err := m.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce returned error: %v", err)
	}
	for _, id := range expired {
		if id == sess.ID {
			t.Errorf("sweepOnce expired fresh session %q; should have been spared", sess.ID)
		}
	}

	// Confirm the session is still Active in the store.
	got, err := m.cfg.Store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after sweepOnce: %v", err)
	}
	if got.Status != sessions.StatusActive {
		t.Errorf("session.Status = %q after sweepOnce, want %q", got.Status, sessions.StatusActive)
	}
}

// TestStartSweeper_DisabledWhenTTLZero verifies that startSweeper is a no-op
// (no goroutine, no panic) when SessionTTL <= 0.
func TestStartSweeper_DisabledWhenTTLZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := NewManager(Config{
		Store:      sessions.NewMemoryStore(),
		Spec:       testSpec(),
		SessionTTL: 0, // disabled
	})

	// Must not panic; goroutine must not be launched (returns immediately).
	m.startSweeper(ctx)
	// No assertion needed beyond no-panic; the function is a no-op for TTL=0.
}
