package serve

import (
	"bytes"
	"strings"
	"testing"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

func TestWriteSSE_FramesEventAndData(t *testing.T) {
	var buf bytes.Buffer
	ev := wire.Event{V: 1, Type: wire.EventMessage, SessionID: "s_1"}
	if err := WriteSSE(&buf, ev); err != nil {
		t.Fatalf("WriteSSE: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "event: message\n") {
		t.Errorf("missing event line: %q", out)
	}
	if !strings.Contains(out, `data: {"v":1,`) {
		t.Errorf("missing data line: %q", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("frame must end with blank line: %q", out)
	}
}
