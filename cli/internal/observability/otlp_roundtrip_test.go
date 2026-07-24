package observability

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// unsetenv removes `name` from the process env for the duration of the
// test, capturing any prior value and restoring it on cleanup.
// t.Setenv(name, "") doesn't work for OTel scrubbing because the SDK
// parses empty-string sampler/limit values rather than treating them
// as "absent" — codex pass 3 of slice 5.6 surfaced an "unsupported
// sampler: " warning from an OTEL_TRACES_SAMPLER="" scrub.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	orig, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetenv %s: %v", name, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, orig)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

// Slice 5.6 — OTLP-HTTP roundtrip acceptance test.
//
// Stands up an httptest.Server that emulates an OTLP-HTTP collector
// (BrainTrust, MLflow, Langfuse, Jaeger, otel-collector — they all
// implement the same OTLP-HTTP/protobuf surface). Runs the host-side
// tracing path against it, then decodes the received payload and
// asserts the wire shape that downstream backends rely on:
//
//   - Span name + the GenAI semconv attributes the schema documents
//     (gen_ai.system, gen_ai.request.model, gen_ai.operation.name)
//   - service.name + service.version resource attributes match the
//     identity we publish (operators filter dashboards by these)
//   - Default Content-Type is application/x-protobuf; the handler
//     also accepts gzip-compressed bodies in case the SDK enables
//     compression in a future release
//
// This is the test the v0.5 tracing track was waiting on — it proves
// that what we emit is what a generic OTLP collector will ingest,
// without needing a real one in CI.

// otlpCapture is a thread-safe holder for the decoded exports the
// fake collector received. The OTel batcher fires from a background
// goroutine, so writes need a mutex.
type otlpCapture struct {
	mu              sync.Mutex
	receivedExports []*coltracepb.ExportTraceServiceRequest
	requestPaths    []string
	contentTypes    []string
}

func (c *otlpCapture) add(path, ct string, req *coltracepb.ExportTraceServiceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestPaths = append(c.requestPaths, path)
	c.contentTypes = append(c.contentTypes, ct)
	c.receivedExports = append(c.receivedExports, req)
}

func (c *otlpCapture) snapshot() ([]string, []string, []*coltracepb.ExportTraceServiceRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requestPaths...),
		append([]string(nil), c.contentTypes...),
		append([]*coltracepb.ExportTraceServiceRequest(nil), c.receivedExports...)
}

// newFakeOtlpHttpCollector returns an httptest.Server that accepts
// POST /v1/traces, decodes the body as an ExportTraceServiceRequest
// (transparently handling gzip), and appends each export to capture.
// Returns the standard OTLP-HTTP success response: empty
// ExportTraceServiceResponse marshaled into protobuf with HTTP 200.
func newFakeOtlpHttpCollector(t *testing.T, capture *otlpCapture) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expect POST", http.StatusMethodNotAllowed)
			return
		}
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "gzip decode: "+err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			reader = gz
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &req); err != nil {
			http.Error(w, "proto unmarshal: "+err.Error(), http.StatusBadRequest)
			return
		}
		capture.add(r.URL.Path, r.Header.Get("Content-Type"), &req)

		resp := &coltracepb.ExportTraceServiceResponse{}
		respBytes, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(respBytes)
	})
	return httptest.NewServer(mux)
}

// findAttr returns the string value of the named attribute, or "" if
// the attribute is missing or holds a non-string value. We only need
// strings for assertion purposes — agentctl's attributes are all
// string-typed except for resource defaults (process.pid etc.) which
// the test ignores.
func findAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func TestObservabilityOtlpRoundtripEmitsExpectedSpan(t *testing.T) {
	capture := &otlpCapture{}
	server := newFakeOtlpHttpCollector(t, capture)
	defer server.Close()

	// Slice 5.1's InitTracerProvider reads OTEL_EXPORTER_OTLP_ENDPOINT
	// at SDK init. Set it BEFORE the call. The SDK auto-appends
	// /v1/traces to bare endpoints, so we pass just the server URL
	// and the handler at /v1/traces fires.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	// httptest.Server is plain HTTP — disable TLS so the SDK doesn't
	// try to negotiate one. Insulates the test against ambient env
	// from the developer's machine.
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	// Codex passes 1–3 of slice 5.6: scrub every ambient OTel env
	// var that would otherwise distort the test. Listed exhaustively
	// rather than dynamically discovered so the isolation surface is
	// auditable. We use a true unset (with cleanup-restore) rather
	// than t.Setenv("", "") because the SDK parses empty-string
	// values for sampler/limit envs — "" reads as "invalid sampler"
	// (codex pass 3 surfaced the warning). Five categories matter:
	//
	//   * Endpoint redirects — TRACE_ENDPOINT wins over the generic
	//     one we set above, so a developer with their own collector
	//     in env would silently send our spans to it.
	//   * Exporter behavior — TLS / compression / protocol flags can
	//     turn the otlptracehttp.New call into something other than
	//     plain HTTP-protobuf against httptest.Server (which is plain
	//     HTTP only).
	//   * Resource overrides — OTEL_SERVICE_NAME and
	//     OTEL_RESOURCE_ATTRIBUTES inject themselves into the
	//     resource attribute set that our service.name + service.version
	//     assertions depend on.
	for _, leakyVar := range []string{
		// Endpoint precedence
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		// Header injection
		"OTEL_EXPORTER_OTLP_HEADERS",
		"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		// Protocol switching (we want HTTP/proto, not gRPC)
		"OTEL_EXPORTER_OTLP_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		// Compression — would flip our Content-Type assertion path
		"OTEL_EXPORTER_OTLP_COMPRESSION",
		"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		// Timeouts — a low ambient value (e.g. CI env exporting
		// OTEL_EXPORTER_OTLP_TIMEOUT=1ms) makes the exporter give up
		// before the httptest collector responds, making shutdown()
		// fail. Codex pass 4 of slice 5.6 flagged this.
		"OTEL_EXPORTER_OTLP_TIMEOUT",
		"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		// TLS / certificates — httptest.Server is plain HTTP, any of
		// these forcing TLS would fail the dial
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
		// Exporter selection — "none" would disable our exporter
		"OTEL_TRACES_EXPORTER",
		// Resource attribute overrides that would invalidate our
		// service.name / service.version assertions
		"OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES",
		// Codex pass 3 of slice 5.6: SDK-level config envs the
		// TracerProvider reads at construction. A `OTEL_TRACES_SAMPLER=
		// always_off` exported in the dev's shell would drop every
		// span before it reached the exporter (test would see zero
		// requests at the fake collector). Span-limit envs would
		// truncate the asserted attributes. BSP envs aren't strictly
		// necessary to scrub — we explicitly shutdown() before the
		// assertions — but defensive isolation is cheaper than
		// debugging an intermittent CI failure later.
		"OTEL_TRACES_SAMPLER",
		"OTEL_TRACES_SAMPLER_ARG",
		"OTEL_ATTRIBUTE_COUNT_LIMIT",
		"OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT",
		"OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT",
		"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT",
		"OTEL_SPAN_EVENT_COUNT_LIMIT",
		"OTEL_SPAN_LINK_COUNT_LIMIT",
		"OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT",
		"OTEL_LINK_ATTRIBUTE_COUNT_LIMIT",
		"OTEL_BSP_SCHEDULE_DELAY",
		"OTEL_BSP_EXPORT_TIMEOUT",
		"OTEL_BSP_MAX_QUEUE_SIZE",
		"OTEL_BSP_MAX_EXPORT_BATCH_SIZE",
	} {
		unsetenv(t, leakyVar)
	}

	ctx := context.Background()
	shutdown, err := InitTracerProvider(ctx, "0.5.1-test")
	if err != nil {
		t.Fatalf("InitTracerProvider: %v", err)
	}

	// Open the root span the host would emit for a real run. Keep the
	// attribute set aligned with cli/cmd/agentctl/main.go in production
	// — drift here is what breaks downstream dashboards.
	_, span := StartRootSpan(ctx, RunAttributes{
		AgentName:     "acceptance-agent",
		ModelProvider: "anthropic",
		ModelName:     "claude-sonnet-4-6",
		RuntimeType:   "local-pi",
	})
	span.End()

	// Force flush. The batcher fires on its own 2s schedule;
	// without an explicit shutdown the test races the timer and
	// intermittently sees zero exports.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	paths, contentTypes, exports := capture.snapshot()
	if len(exports) == 0 {
		t.Fatalf("expected at least one OTLP export; received zero requests at the fake collector. paths=%v", paths)
	}
	for _, p := range paths {
		if p != "/v1/traces" {
			t.Errorf("OTLP-HTTP requests must POST to /v1/traces, got %q", p)
		}
	}
	// Default Content-Type is protobuf. JSON is opt-in via env we
	// don't set here.
	for _, ct := range contentTypes {
		if !strings.Contains(ct, "application/x-protobuf") {
			t.Errorf("expected Content-Type application/x-protobuf, got %q", ct)
		}
	}

	// Locate the agentctl.run span across all received exports. The
	// SDK may emit one or more batches; we walk every ResourceSpans /
	// ScopeSpans / Spans tuple and find our span by name.
	var foundSpan *tracepb.Span
	var foundResource []*commonpb.KeyValue
	for _, exp := range exports {
		for _, rs := range exp.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, sp := range ss.GetSpans() {
					if sp.GetName() == "agentctl.run" {
						foundSpan = sp
						foundResource = rs.GetResource().GetAttributes()
					}
				}
			}
		}
	}
	if foundSpan == nil {
		t.Fatalf("agentctl.run span not found in any export; got %d exports", len(exports))
	}

	// Span attribute contract — per slice 5.1's schema. Backends like
	// BrainTrust filter their UI by these keys.
	for _, expect := range []struct{ key, want string }{
		{"gen_ai.system", "anthropic"},
		{"gen_ai.request.model", "claude-sonnet-4-6"},
		{"gen_ai.operation.name", "agent.run"},
		{"agent_controller.agent.name", "acceptance-agent"},
		{"agent_controller.runtime.type", "local-pi"},
	} {
		if got := findAttr(foundSpan.GetAttributes(), expect.key); got != expect.want {
			t.Errorf("span attr %s = %q; want %q", expect.key, got, expect.want)
		}
	}

	// Resource attribute contract — service.name is the discriminator
	// backends use to group spans by emitter. If this drifts, the
	// operator's saved dashboards break silently.
	if name := findAttr(foundResource, "service.name"); name != "agentctl" {
		t.Errorf("resource service.name = %q; want %q", name, "agentctl")
	}
	if version := findAttr(foundResource, "service.version"); version != "0.5.1-test" {
		t.Errorf("resource service.version = %q; want %q", version, "0.5.1-test")
	}
}
