package serve

import (
	"context"
	"net/http"
	"time"
)

func buildMux(m *Manager) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", m.handleHealthz)
	mux.HandleFunc("GET /readyz", m.handleReadyz)
	mux.HandleFunc("POST /v1/sessions", m.handleCreateSession)
	mux.HandleFunc("GET /v1/sessions", m.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", m.handleGetSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", m.handleDeleteSession)
	mux.HandleFunc("POST /v1/sessions/{id}/turns", m.handleRunTurn)
	return mux
}

// Serve starts the HTTP server on addr and blocks until ctx is cancelled,
// then drains in-flight requests up to cfg.ShutdownGrace. Returns nil on a
// clean drain.
func (m *Manager) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           buildMux(m),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE turn responses are long-lived.
	}
	m.startSweeper(ctx)
	go func() {
		<-ctx.Done()
		m.SetDraining(true)
		grace := m.cfg.ShutdownGrace
		if grace <= 0 {
			grace = 25 * time.Second
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
