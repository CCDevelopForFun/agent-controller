package main

import (
	"strings"
	"testing"
)

func TestNewServeCmd_FlagsRegistered(t *testing.T) {
	cmd := newServeCmd()
	if cmd.Use != "serve <spec.yaml>" {
		t.Errorf("Use = %q, want %q", cmd.Use, "serve <spec.yaml>")
	}
	for _, name := range []string{"port", "in-memory", "max-concurrent-turns", "max-sessions", "session-ttl", "shutdown-grace"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	if !strings.Contains(cmd.Short, "HTTP/SSE") {
		t.Errorf("Short does not mention HTTP/SSE: %q", cmd.Short)
	}
}
