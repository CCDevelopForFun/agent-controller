// Package serve implements the agentctl HTTP/SSE agent server (v0.8).
package serve

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// WriteSSE writes one Server-Sent-Events frame for a wire event: an
// `event:` line carrying the wire type (so clients can switch on it) and a
// `data:` line carrying the full event JSON, terminated by a blank line.
// The caller is responsible for flushing.
func WriteSSE(w io.Writer, ev wire.Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("serve: marshal wire event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
		return fmt.Errorf("serve: write sse frame: %w", err)
	}
	return nil
}
