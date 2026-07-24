package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/observability"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/serve"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
)

func newServeCmd() *cobra.Command {
	var port int
	var inMemory bool
	var maxTurns, maxSessions int
	var sessionTTL, shutdownGrace time.Duration
	c := &cobra.Command{
		Use:   "serve <spec.yaml>",
		Short: "Serve an ADL agent over HTTP/SSE (v0.8)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseValidateCompile(args[0])
			if err != nil {
				return err
			}
			runtimeCmd, err := resolveRuntimeCommand(spec.Runtime.Type)
			if err != nil {
				return err
			}

			// Init OTel once for the lifetime of the server. When tracing
			// is disabled (OTEL_EXPORTER_OTLP_* unset), this is a no-op.
			// Per-turn root spans are opened inside RunTurn.
			otelShutdown, err := observability.InitTracerProvider(cmd.Context(), version)
			if err != nil {
				return fmt.Errorf("init OTel: %w", err)
			}
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = otelShutdown(sctx)
			}()

			var store sessions.Store
			if inMemory {
				store = sessions.NewMemoryStore()
			} else {
				path, perr := sessions.DefaultSQLiteStorePath()
				if perr != nil {
					return perr
				}
				s, serr := sessions.NewSQLiteStore(path)
				if serr != nil {
					return serr
				}
				store = s
			}
			defer store.Close()

			be := backend.NewLocalBackend(backend.LocalConfig{RuntimeCommand: runtimeCmd})
			m := serve.NewManager(serve.Config{
				Store: store, Spec: spec, RuntimeCommand: runtimeCmd,
				MaxConcurrentTurns: maxTurns, MaxSessions: maxSessions,
				SessionTTL: sessionTTL, ShutdownGrace: shutdownGrace,
				Backend: be,
			})
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			addr := fmt.Sprintf(":%d", port)
			fmt.Fprintf(cmd.ErrOrStderr(), "agentctl serve: listening on %s (agent %q, runtime %s)\n", addr, spec.Metadata.Name, spec.Runtime.Type)
			return m.Serve(ctx, addr)
		},
	}
	c.Flags().IntVar(&port, "port", 8080, "TCP port to listen on")
	c.Flags().BoolVar(&inMemory, "in-memory", false, "use an in-memory session store (default: SQLite)")
	c.Flags().IntVar(&maxTurns, "max-concurrent-turns", 8, "max concurrent in-flight turns before returning 429")
	c.Flags().IntVar(&maxSessions, "max-sessions", 1000, "max active sessions before create returns 429")
	c.Flags().DurationVar(&sessionTTL, "session-ttl", 168*time.Hour, "idle session TTL swept in the background")
	c.Flags().DurationVar(&shutdownGrace, "shutdown-grace", 25*time.Second, "max time to drain in-flight turns on shutdown")
	return c
}
