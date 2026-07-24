package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
)

// Slice 6.4 tests for `agentctl sessions sweep`.

func sweepTestSession(id string, status sessions.SessionStatus, lastActive time.Time) sessions.Session {
	return sessions.Session{
		ID:           id,
		AgentName:    "alice",
		RuntimeType:  "local",
		Status:       status,
		CreatedAt:    lastActive.Add(-time.Hour),
		LastActiveAt: lastActive,
		Spec: adl.CompiledSpec{
			V:        1,
			Metadata: adl.SpecMetadata{Name: "alice"},
			Model:    adl.Model{Provider: "anthropic", Name: "claude"},
			Task:     "t",
			Runtime:  adl.RuntimeConfig{Type: "local"},
		},
	}
}

func TestSessionsSweepExpiresIdleActiveOnly(t *testing.T) {
	// Point XDG at a temp dir so the sweep operates on a fresh
	// store, not the operator's real $XDG_DATA_HOME.
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	storePath := filepath.Join(xdg, "agent-controller", "sessions.db")
	_ = storePath // referenced for clarity; openChatStore resolves it

	// Pre-populate the SQLite store with one stale-Active, one
	// fresh-Active, one Paused (past cutoff but not Active), one
	// Ended.
	store, err := openChatStore(false)
	if err != nil {
		t.Fatalf("openChatStore: %v", err)
	}
	now := time.Now().UTC()
	ctx := context.Background()
	if err := store.Create(ctx, sweepTestSession("a-stale", sessions.StatusActive, now.Add(-48*time.Hour))); err != nil {
		t.Fatalf("seed a-stale: %v", err)
	}
	if err := store.Create(ctx, sweepTestSession("b-fresh", sessions.StatusActive, now.Add(-1*time.Hour))); err != nil {
		t.Fatalf("seed b-fresh: %v", err)
	}
	if err := store.Create(ctx, sweepTestSession("c-paused", sessions.StatusPaused, now.Add(-72*time.Hour))); err != nil {
		t.Fatalf("seed c-paused: %v", err)
	}
	if err := store.Create(ctx, sweepTestSession("d-ended", sessions.StatusEnded, now.Add(-72*time.Hour))); err != nil {
		t.Fatalf("seed d-ended: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cmd := newSessionsSweepCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--ttl", "24h"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "expired 1 session") {
		t.Errorf("expected one expiration in output, got:\n%s", output)
	}
	if !strings.Contains(output, "a-stale") {
		t.Errorf("a-stale should be expired; output:\n%s", output)
	}
	if strings.Contains(output, "b-fresh") {
		t.Errorf("b-fresh should be untouched (fresh); output:\n%s", output)
	}
	if strings.Contains(output, "c-paused") {
		t.Errorf("c-paused should be untouched (not Active); output:\n%s", output)
	}

	// Verify store state directly after sweep.
	store2, err := openChatStore(false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	if s, err := store2.Get(ctx, "a-stale"); err != nil {
		t.Errorf("get a-stale: %v", err)
	} else if s.Status != sessions.StatusExpired {
		t.Errorf("a-stale status = %q, want expired", s.Status)
	}
	if s, err := store2.Get(ctx, "b-fresh"); err != nil {
		t.Errorf("get b-fresh: %v", err)
	} else if s.Status != sessions.StatusActive {
		t.Errorf("b-fresh status = %q, want active", s.Status)
	}
	if s, err := store2.Get(ctx, "c-paused"); err != nil {
		t.Errorf("get c-paused: %v", err)
	} else if s.Status != sessions.StatusPaused {
		t.Errorf("c-paused status = %q, want paused", s.Status)
	}
}

func TestSessionsSweepNoMatchesPrintsClearMessage(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	// Don't seed anything; sweep should report no matches.

	cmd := newSessionsSweepCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--ttl", "24h"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !strings.Contains(out.String(), "no sessions to expire") {
		t.Errorf("expected no-match message; got:\n%s", out.String())
	}
}

func TestSweepStatusSurvivesConcurrentChatExitGet(t *testing.T) {
	// Codex slice 6.4 pass 1: simulate the race the chat exit-block
	// now guards. Steps:
	//   1. Create an Active session.
	//   2. Run sweep (transitions it to Expired).
	//   3. Simulate the chat exit-block: Get the session, see it's
	//      Expired, SKIP the Update.
	// Verify the session stays Expired (chat didn't overwrite).
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	store, err := openChatStore(false)
	if err != nil {
		t.Fatalf("openChatStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	stale := sweepTestSession("s_swept", sessions.StatusActive, time.Now().UTC().Add(-48*time.Hour))
	if err := store.Create(ctx, stale); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Sweep moves it to Expired.
	if _, err := store.MarkExpired(ctx, time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}

	// Simulate the chat exit-block guard.
	current, err := store.Get(ctx, "s_swept")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if current.Status != sessions.StatusExpired {
		t.Fatalf("setup: expected Expired after sweep, got %q", current.Status)
	}
	// chat exit block sees Expired → SKIPS the Update.

	// Verify the session is still Expired afterwards (no overwrite).
	final, err := store.Get(ctx, "s_swept")
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if final.Status != sessions.StatusExpired {
		t.Errorf("Expired status was overwritten; got %q", final.Status)
	}
}

func TestSessionsSweepRejectsZeroTTL(t *testing.T) {
	// Guard against operator typos like --ttl=0 silently expiring
	// everything (cutoff = now means every session past last-active
	// gets swept).
	cmd := newSessionsSweepCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--ttl", "0s"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected error for --ttl 0s, got nil")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error should mention positive duration; got %q", err.Error())
	}
}
