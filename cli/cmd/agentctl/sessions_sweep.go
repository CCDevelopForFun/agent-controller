package main

// Slice 6.4: `agentctl sessions sweep` — bulk-transitions every
// currently-Active session whose LastActiveAt is older than --ttl
// into StatusExpired. Used by operators to retire idle sessions
// without manually opening each one. Intended to be safe to run
// from cron / systemd timers; idempotent (a second sweep at the
// same wall time is a no-op because the first sweep moved the
// matched sessions out of Active).
//
// Wire emission: sweep does NOT emit `session.expired` on the wire
// because it doesn't have an active wire-stream consumer (no
// adapter, no REPL stdin). The wire event for expired sessions is
// emitted by `agentctl chat --resume <id>` when it encounters an
// already-expired record in the store.

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newSessionsSweepCmd() *cobra.Command {
	var ttl time.Duration
	var inMemory bool

	c := &cobra.Command{
		Use:   "sweep",
		Short: "Mark idle Active sessions as Expired (TTL sweep)",
		Long: `Bulk-transition Active sessions whose last-active timestamp ` +
			`is older than --ttl to Expired. Safe to run from a cron / ` +
			`systemd timer; idempotent (a second sweep is a no-op).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ttl <= 0 {
				return fmt.Errorf("--ttl must be a positive duration (got %s)", ttl)
			}
			store, err := openChatStore(inMemory)
			if err != nil {
				return err
			}
			defer store.Close()

			cutoff := time.Now().UTC().Add(-ttl)
			expired, err := store.MarkExpired(cmd.Context(), cutoff)
			if err != nil {
				return fmt.Errorf("sweep: %w", err)
			}
			if len(expired) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"no sessions to expire (cutoff %s)\n",
					cutoff.Format(time.RFC3339))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"expired %d session(s) with last-active before %s:\n",
				len(expired), cutoff.Format(time.RFC3339))
			for _, id := range expired {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", id)
			}
			return nil
		},
	}

	// 24h default: a chat that hasn't been touched in a day is the
	// classic "user abandoned it" threshold. Operators with shorter
	// retention requirements override per-invocation.
	c.Flags().DurationVar(&ttl, "ttl", 24*time.Hour,
		"sessions Active longer than this without activity are expired (e.g. 24h, 168h, 30m — Go time.ParseDuration syntax; days/weeks must be expressed in hours)")
	c.Flags().BoolVar(&inMemory, "in-memory", false,
		"sweep the in-memory store instead of the default SQLite store (mostly useful for tests)")
	return c
}
