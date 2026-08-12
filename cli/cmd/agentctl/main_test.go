package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// TestPrintEventIncludesTimestamp guards against the bug where printEvent
// wrote only "[type] {data}" and silently dropped ev.Ts, even though every
// wire.Event carries a populated Ts. Downstream consumers of agentctl's
// stdout (e.g. assay) rely on that timestamp to compute tool-call
// duration from the tool.call/tool.result pair, so it must survive the
// print.
func TestPrintEventIncludesTimestamp(t *testing.T) {
	ts := time.Date(2026, 5, 25, 17, 23, 4, 182_000_000, time.UTC)
	ev := wire.Event{
		V:         wire.ProtocolVersion,
		Type:      wire.EventToolCall,
		Ts:        ts,
		SessionID: "s_abc",
		Data:      json.RawMessage(`{"toolName":"get_time","callId":"c1"}`),
	}

	var buf bytes.Buffer
	printEvent(&buf, ev)

	line := buf.String()
	if !strings.HasPrefix(line, "[tool.call] ") {
		t.Fatalf("printEvent line = %q, want prefix %q", line, "[tool.call] ")
	}

	jsonPart := strings.TrimSpace(strings.TrimPrefix(line, "[tool.call] "))
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(jsonPart), &data); err != nil {
		t.Fatalf("printed data is not valid JSON: %v (line: %q)", err, line)
	}

	gotTs, ok := data["ts"].(string)
	if !ok {
		t.Fatalf("printed data missing string \"ts\" field: %v", data)
	}
	parsed, err := time.Parse(time.RFC3339Nano, gotTs)
	if err != nil {
		t.Fatalf("ts %q did not parse as RFC3339: %v", gotTs, err)
	}
	if !parsed.Equal(ts) {
		t.Errorf("ts = %v, want %v", parsed, ts)
	}

	// Pre-existing data fields must survive the merge untouched.
	if data["toolName"] != "get_time" {
		t.Errorf("toolName = %v, want get_time", data["toolName"])
	}
	if data["callId"] != "c1" {
		t.Errorf("callId = %v, want c1", data["callId"])
	}
}
