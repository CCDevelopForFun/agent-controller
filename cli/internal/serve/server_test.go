package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
)

func testSpec() adl.CompiledSpec {
	return adl.CompiledSpec{
		V:        1,
		Metadata: adl.SpecMetadata{Name: "t"},
		Model:    adl.Model{Provider: "anthropic", Name: "x"},
		Runtime:  adl.RuntimeConfig{Type: "local"},
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(Config{Store: sessions.NewMemoryStore(), Spec: testSpec(), RuntimeCommand: []string{"true"}, MaxConcurrentTurns: 4, MaxSessions: 100})
}

func TestHealthz_OK(t *testing.T) {
	srv := httptest.NewServer(buildMux(newTestManager(t)))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("healthz = %d, want 200", resp.StatusCode)
	}
}

func TestReadyz_503WhenDraining(t *testing.T) {
	m := newTestManager(t)
	srv := httptest.NewServer(buildMux(m))
	defer srv.Close()
	if r, _ := http.Get(srv.URL + "/readyz"); r.StatusCode != 200 {
		t.Errorf("readyz = %d, want 200", r.StatusCode)
	}
	m.SetDraining(true)
	if r, _ := http.Get(srv.URL + "/readyz"); r.StatusCode != 503 {
		t.Errorf("readyz(draining) = %d, want 503", r.StatusCode)
	}
}

func TestServe_ShutsDownOnContextCancel(t *testing.T) {
	m := NewManager(Config{Store: sessions.NewMemoryStore(), RuntimeCommand: []string{"true"}, MaxConcurrentTurns: 4, MaxSessions: 100, ShutdownGrace: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- m.Serve(ctx, "127.0.0.1:0") }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
