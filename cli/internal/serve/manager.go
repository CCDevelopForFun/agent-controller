package serve

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
)

// Config holds the knobs newServeCmd wires from flags.
type Config struct {
	Store              sessions.Store
	Spec               adl.CompiledSpec
	RuntimeCommand     []string
	MaxConcurrentTurns int
	MaxSessions        int
	SessionTTL         time.Duration
	ShutdownGrace      time.Duration
	Backend            backend.Backend
}

// Manager owns all server-side session state: the store, adapter dispatch,
// concurrency controls, and the draining flag. Safe for concurrent use.
type Manager struct {
	cfg      Config
	draining struct {
		sync.RWMutex
		v bool
	}
	// sessionLocks maps session id → *sync.Mutex for per-session single-flight.
	// Entries are created lazily via LoadOrStore and never deleted (session IDs
	// are short-lived strings; the memory cost is negligible).
	sessionLocks sync.Map
	// turnSem is a buffered channel used as a counting semaphore for the global
	// concurrent-turn cap (MaxConcurrentTurns). nil means unlimited (cap == 0).
	turnSem chan struct{}
}

func NewManager(cfg Config) *Manager {
	m := &Manager{cfg: cfg}
	if cfg.MaxConcurrentTurns > 0 {
		m.turnSem = make(chan struct{}, cfg.MaxConcurrentTurns)
	}
	return m
}

func (m *Manager) SetDraining(v bool) {
	m.draining.Lock()
	m.draining.v = v
	m.draining.Unlock()
}

func (m *Manager) Draining() bool {
	m.draining.RLock()
	defer m.draining.RUnlock()
	return m.draining.v
}

// ErrTooManySessions is returned by CreateSession when the active session
// count has reached the configured MaxSessions limit.
var ErrTooManySessions = errors.New("serve: max sessions reached")

// ErrSessionBusy is returned by tryLockSession when another turn is already
// in-flight for the requested session id.
var ErrSessionBusy = errors.New("serve: session busy")

// ErrTooManyTurns is returned by acquireTurnSlot when the global concurrent
// turn cap (MaxConcurrentTurns) has been reached.
var ErrTooManyTurns = errors.New("serve: too many concurrent turns")

// acquireTurnSlot attempts to acquire a global turn semaphore slot.
// On success it returns (release, true); the caller MUST call release() when
// the turn completes (typically via defer). On failure it returns (nil, false),
// meaning MaxConcurrentTurns in-flight turns are already active.
// When MaxConcurrentTurns == 0 (turnSem is nil), the slot is always granted
// with a no-op release function (unlimited concurrency).
func (m *Manager) acquireTurnSlot() (release func(), ok bool) {
	if m.turnSem == nil {
		return func() {}, true
	}
	select {
	case m.turnSem <- struct{}{}:
		return func() { <-m.turnSem }, true
	default:
		return nil, false
	}
}

// tryLockSession attempts to acquire the per-session mutex for id.
// On success it returns (unlock, true); the caller MUST call unlock() when the
// turn completes (typically via defer). On failure it returns (nil, false),
// meaning another turn is already in-flight for that session.
//
// Design: Approach A — the lock lives in the HTTP handler, so RunTurn itself
// is lock-free. This guarantees the 409 is detected and returned as a clean
// JSON response BEFORE any SSE headers are flushed.
func (m *Manager) tryLockSession(id string) (unlock func(), ok bool) {
	mu := &sync.Mutex{}
	actual, _ := m.sessionLocks.LoadOrStore(id, mu)
	mu = actual.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// CreateSession creates a new active session. It generates a unique session
// id, applies ${inputs.*} interpolation to a copy of the spec's task, and
// persists the session via the store. Returns ErrTooManySessions when the
// active count is at or above MaxSessions (0 means unlimited).
func (m *Manager) CreateSession(ctx context.Context, inputs map[string]string) (sessions.Session, error) {
	if m.cfg.MaxSessions > 0 {
		active, err := m.cfg.Store.List(ctx, sessions.ListFilter{Status: sessions.StatusActive})
		if err != nil {
			return sessions.Session{}, err
		}
		if len(active) >= m.cfg.MaxSessions {
			return sessions.Session{}, ErrTooManySessions
		}
	}
	spec := m.cfg.Spec // struct copy
	for k, v := range inputs {
		spec.Task = strings.ReplaceAll(spec.Task, "${inputs."+k+"}", v)
	}
	now := time.Now().UTC()
	s := sessions.Session{
		ID:           fmt.Sprintf("s_%s%08x", strconv.FormatInt(now.UnixMilli(), 36), rand.Uint32()),
		AgentName:    spec.Metadata.Name,
		RuntimeType:  spec.Runtime.Type,
		Status:       sessions.StatusActive,
		CreatedAt:    now,
		LastActiveAt: now,
		Spec:         spec,
		AdapterState: map[string]any{},
	}
	if err := m.cfg.Store.Create(ctx, s); err != nil {
		return sessions.Session{}, err
	}
	return s, nil
}

// GetSession retrieves a session by id. Returns sessions.ErrNotFound when
// the session doesn't exist.
func (m *Manager) GetSession(ctx context.Context, id string) (sessions.Session, error) {
	return m.cfg.Store.Get(ctx, id)
}

// ListSessions returns sessions matching the given status. An empty status
// returns all sessions.
func (m *Manager) ListSessions(ctx context.Context, status sessions.SessionStatus) ([]sessions.Session, error) {
	return m.cfg.Store.List(ctx, sessions.ListFilter{Status: status})
}

// EndSession transitions a session to StatusEnded. Returns sessions.ErrNotFound
// when the session doesn't exist.
func (m *Manager) EndSession(ctx context.Context, id string) error {
	s, err := m.cfg.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	s.Status = sessions.StatusEnded
	return m.cfg.Store.Update(ctx, s)
}

// sweepOnce computes the cutoff time from the configured SessionTTL and
// delegates to the store's MarkExpired to bulk-expire idle sessions.
// Extracted as its own method so tests can call it directly without a ticker.
func (m *Manager) sweepOnce(ctx context.Context) ([]string, error) {
	cutoff := time.Now().UTC().Add(-m.cfg.SessionTTL)
	return m.cfg.Store.MarkExpired(ctx, cutoff)
}

// startSweeper launches a background goroutine that periodically calls
// sweepOnce. If SessionTTL <= 0, sweeping is disabled and this is a no-op.
// The goroutine exits when ctx is cancelled.
func (m *Manager) startSweeper(ctx context.Context) {
	if m.cfg.SessionTTL <= 0 {
		return
	}
	interval := m.cfg.SessionTTL / 4
	if interval <= 0 {
		interval = time.Second
	}
	if interval > time.Hour {
		interval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = m.sweepOnce(ctx)
			}
		}
	}()
}
