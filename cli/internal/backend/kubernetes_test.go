package backend

import (
	"context"
	"strings"
	"testing"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestKubernetesBackend returns a KubernetesBackend wired to a
// fake.Clientset so unit tests can exercise Resolve + Submit + Stop
// without a real cluster. NewKubernetesBackend itself requires a
// loadable kubeconfig (so it's not usable in CI without setup); these
// tests construct the struct directly with a pre-built fake client.
func newTestKubernetesBackend(t *testing.T) *KubernetesBackend {
	t.Helper()
	return &KubernetesBackend{
		client:   fake.NewSimpleClientset(),
		sessions: map[SessionHandle]*k8sSession{},
	}
}

func makeK8sSpec() adl.CompiledSpec {
	return adl.CompiledSpec{
		V:        1,
		Metadata: adl.SpecMetadata{Name: "test-agent"},
		Model:    adl.Model{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
		Task:     "Say hello.",
		Tools:    []adl.ResolvedRef{},
		Runtime:  adl.RuntimeConfig{Type: "local"},
	}
}

func makeK8sBinding(strict bool) *adl.RuntimeBinding {
	return &adl.RuntimeBinding{
		APIVersion: "agent-controller.dev/v1alpha1",
		Kind:       "RuntimeBinding",
		Metadata:   adl.RuntimeBindingMeta{Name: "k8s-test"},
		Spec: adl.RuntimeBindingSpec{
			Selector: adl.RuntimeBindingSelector{
				RuntimeType: "local",
			},
			Target: adl.RuntimeBindingTarget{
				Type:   "kubernetes",
				Strict: strict,
				Kubernetes: &adl.KubernetesTarget{
					Namespace: "test-ns",
					Image:     "ghcr.io/test/agent-runtime:latest",
					SecretRef: &adl.KubernetesSecretRef{
						Name: "anthropic-creds",
					},
				},
			},
		},
	}
}

func TestKubernetesBackendResolveNilBindingIsNoOp(t *testing.T) {
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	resolved, warnings, err := be.Resolve(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Resolve(nil) returned err: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for nil binding, got %d", len(warnings))
	}
	if resolved.Spec.Metadata.Name != spec.Metadata.Name {
		t.Errorf("expected spec passed through verbatim")
	}
}

func TestKubernetesBackendResolveRejectsLocalTargetType(t *testing.T) {
	be := newTestKubernetesBackend(t)
	binding := makeK8sBinding(false)
	binding.Spec.Target.Type = "local" // wrong backend
	_, _, err := be.Resolve(context.Background(), makeK8sSpec(), binding)
	if err == nil {
		t.Fatalf("expected error for local-target binding passed to KubernetesBackend")
	}
	if !strings.Contains(err.Error(), "target.type=") {
		t.Errorf("error should mention target.type mismatch, got: %v", err)
	}
}

func TestKubernetesBackendResolveRejectsMissingKubernetesBlock(t *testing.T) {
	be := newTestKubernetesBackend(t)
	binding := makeK8sBinding(false)
	binding.Spec.Target.Kubernetes = nil // dropped the config block
	_, _, err := be.Resolve(context.Background(), makeK8sSpec(), binding)
	if err == nil {
		t.Fatalf("expected error for binding with kubernetes target but no kubernetes block")
	}
	if !strings.Contains(err.Error(), "secretRef") {
		t.Errorf("error should mention the missing secretRef field, got: %v", err)
	}
}

func TestKubernetesBackendResolveDelegatesToSharedMatcher(t *testing.T) {
	// Selector mismatch — same hard-error path as LocalBackend, proves
	// matchBinding is wired in.
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Runtime.Type = "local-opencode" // doesn't match selector runtimeType
	binding := makeK8sBinding(false)
	_, _, err := be.Resolve(context.Background(), spec, binding)
	if err == nil {
		t.Fatalf("expected selector-mismatch error from shared matcher")
	}
	if !strings.Contains(err.Error(), "selector does not match") {
		t.Errorf("error should be the shared-matcher selector-mismatch message, got: %v", err)
	}
}

func TestKubernetesBackendSubmitCreatesPodAndSpecSecret(t *testing.T) {
	be := newTestKubernetesBackend(t)
	resolved := adl.ResolvedRunSpec{
		Spec:    makeK8sSpec(),
		Binding: makeK8sBinding(false),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := be.Submit(ctx, resolved)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if h == "" {
		t.Fatalf("expected non-empty session handle")
	}

	// Confirm a Pod and the spec Secret landed in the test namespace.
	pods, err := be.client.CoreV1().Pods("test-ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 Pod, got %d", len(pods.Items))
	}
	pod := pods.Items[0]
	if !strings.HasPrefix(pod.Name, "agentctl-") {
		t.Errorf("Pod name should start with agentctl-, got %q", pod.Name)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("expected RestartPolicy=Never, got %q", pod.Spec.RestartPolicy)
	}
	if got := pod.Spec.Containers[0].Image; got != "ghcr.io/test/agent-runtime:latest" {
		t.Errorf("expected image from binding, got %q", got)
	}
	if got := pod.Spec.Containers[0].Args; len(got) != 3 || got[0] != "run" || got[1] != "--ndjson-stdout" || got[2] != "/workspace/spec.yaml" {
		t.Errorf("expected Args [run --ndjson-stdout /workspace/spec.yaml], got %v", got)
	}
	// Pod's spec volume must mount the agentctl-spec-* Secret (NOT a ConfigMap).
	if pod.Spec.Volumes[0].Secret == nil {
		t.Errorf("expected spec Volume to be a Secret source (slice 4.3 codex pass 2 — credentials)")
	}
	if pod.Spec.Volumes[0].ConfigMap != nil {
		t.Errorf("spec Volume should NOT be a ConfigMap")
	}

	secrets, err := be.client.CoreV1().Secrets("test-ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Secrets: %v", err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("expected 1 spec Secret, got %d", len(secrets.Items))
	}
	sec := secrets.Items[0]
	if !strings.HasPrefix(sec.Name, "agentctl-spec-") {
		t.Errorf("Secret name should start with agentctl-spec-, got %q", sec.Name)
	}
	body, ok := sec.StringData["spec.yaml"]
	if !ok {
		// StringData is only set on the way IN to the API; the fake client
		// echoes whatever was sent, so we read from there.
		t.Errorf("Secret missing spec.yaml StringData key")
	}
	if !strings.Contains(body, "kind: Agent") {
		t.Errorf("Secret spec.yaml should contain `kind: Agent`, got: %s", body)
	}
	// No ConfigMap should be created.
	cms, _ := be.client.CoreV1().ConfigMaps("test-ns").List(ctx, metav1.ListOptions{})
	if len(cms.Items) != 0 {
		t.Errorf("expected 0 ConfigMaps (Secrets-only spec storage), got %d", len(cms.Items))
	}
}

func TestKubernetesBackendSubmitRejectsMissingSecretRef(t *testing.T) {
	be := newTestKubernetesBackend(t)
	binding := makeK8sBinding(false)
	binding.Spec.Target.Kubernetes.SecretRef = nil
	resolved := adl.ResolvedRunSpec{Spec: makeK8sSpec(), Binding: binding}
	_, err := be.Submit(context.Background(), resolved)
	if err == nil {
		t.Fatalf("expected error for binding with no secretRef")
	}
	if !strings.Contains(err.Error(), "secretRef.name is required") {
		t.Errorf("error should mention secretRef.name requirement, got: %v", err)
	}
}

func TestKubernetesBackendStopDeletesPodAndSpecSecret(t *testing.T) {
	be := newTestKubernetesBackend(t)
	resolved := adl.ResolvedRunSpec{Spec: makeK8sSpec(), Binding: makeK8sBinding(false)}
	h, err := be.Submit(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := be.Stop(h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	pods, _ := be.client.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("expected Pod deleted, got %d remaining", len(pods.Items))
	}
	secrets, _ := be.client.CoreV1().Secrets("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(secrets.Items) != 0 {
		t.Errorf("expected spec Secret deleted, got %d remaining", len(secrets.Items))
	}
}

func TestKubernetesBackendSubmitAppliesDefaults(t *testing.T) {
	be := newTestKubernetesBackend(t)
	binding := makeK8sBinding(false)
	binding.Spec.Target.Kubernetes.Namespace = "" // exercise namespace default
	binding.Spec.Target.Kubernetes.Image = ""     // exercise image default
	resolved := adl.ResolvedRunSpec{Spec: makeK8sSpec(), Binding: binding}
	_, err := be.Submit(context.Background(), resolved)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	pods, err := be.client.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("expected Pod in default namespace, got err=%v len=%d", err, len(pods.Items))
	}
	if got := pods.Items[0].Spec.Containers[0].Image; got != defaultKubernetesImage {
		t.Errorf("expected default image %q, got %q", defaultKubernetesImage, got)
	}
}

func TestKubernetesBackendStopUnknownHandle(t *testing.T) {
	be := newTestKubernetesBackend(t)
	err := be.Stop("nope")
	if err == nil {
		t.Fatalf("expected error for unknown session handle")
	}
	if !strings.Contains(err.Error(), "unknown session") {
		t.Errorf("error should mention unknown session, got: %v", err)
	}
}

// Slice 4.3 codex pass 1: validate rejects CompiledSpec fields the Pod
// can't handle (registry-resolved ResolvedRefs, installs[], --resume).
func TestKubernetesBackendSubmitRejectsCustomPiTool(t *testing.T) {
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Tools = []adl.ResolvedRef{{Name: "get_time", Entrypoint: "/abs/host/path/tools/get_time/entrypoint.ts"}}
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: makeK8sBinding(false)})
	if err == nil || !strings.Contains(err.Error(), "custom Pi-extension tools") {
		t.Fatalf("expected error about custom tools, got: %v", err)
	}
}

func TestKubernetesBackendSubmitRejectsExtensions(t *testing.T) {
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Extensions = []adl.ResolvedRef{{Name: "audit-log", Entrypoint: "/abs/extensions/audit-log/entrypoint.ts"}}
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: makeK8sBinding(false)})
	if err == nil || !strings.Contains(err.Error(), "spec.extensions") {
		t.Fatalf("expected error about extensions, got: %v", err)
	}
}

// Slice 5.1 codex pass 3: spec.observability.tracing must survive the
// CompiledSpec → Agent YAML round-trip so the in-Pod agentctl honors
// the opt-in.
func TestKubernetesBackendSubmitPreservesObservability(t *testing.T) {
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Observability = &adl.Observability{Tracing: true}
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: makeK8sBinding(false)})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	secrets, _ := be.client.CoreV1().Secrets("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(secrets.Items) != 1 {
		t.Fatalf("expected 1 spec Secret, got %d", len(secrets.Items))
	}
	body := secrets.Items[0].StringData["spec.yaml"]
	if !strings.Contains(body, "observability:") || !strings.Contains(body, "tracing: true") {
		t.Errorf("Secret spec.yaml should preserve observability.tracing=true; got:\n%s", body)
	}
}

func TestKubernetesBackendSubmitAcceptsBuiltinTools(t *testing.T) {
	// Built-in tools (no Entrypoint) should pass — they're recognized by
	// name by the in-Pod adapter.
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Tools = []adl.ResolvedRef{{Name: "bash"}, {Name: "read"}}
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: makeK8sBinding(false)})
	if err != nil {
		t.Fatalf("Submit with built-in tools failed: %v", err)
	}
	secrets, _ := be.client.CoreV1().Secrets("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(secrets.Items) != 1 {
		t.Fatalf("expected 1 spec Secret, got %d", len(secrets.Items))
	}
	body := secrets.Items[0].StringData["spec.yaml"]
	if !strings.Contains(body, "bash") || !strings.Contains(body, "read") {
		t.Errorf("Secret should preserve built-in tool names: %s", body)
	}
}

func TestKubernetesBackendSubmitOverridesRuntimeForOpencode(t *testing.T) {
	// When spec.runtime.type is local-opencode, the Pod env must point
	// AGENT_CONTROLLER_RUNTIME at the opencode adapter (the image's
	// default is the Pi adapter).
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Runtime.Type = "local-opencode"
	binding := makeK8sBinding(false)
	binding.Spec.Selector.RuntimeType = "local-opencode"
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: binding})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	pods, _ := be.client.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 Pod, got %d", len(pods.Items))
	}
	var found bool
	for _, e := range pods.Items[0].Spec.Containers[0].Env {
		if e.Name == "AGENT_CONTROLLER_RUNTIME" && strings.Contains(e.Value, "runtime-opencode") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected AGENT_CONTROLLER_RUNTIME env pointing at the opencode adapter; got envs: %+v", pods.Items[0].Spec.Containers[0].Env)
	}
}

func TestKubernetesBackendSubmitRejectsUnknownRuntime(t *testing.T) {
	be := newTestKubernetesBackend(t)
	spec := makeK8sSpec()
	spec.Runtime.Type = "wasm-edge" // not a real runtime
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: spec, Binding: makeK8sBinding(false)})
	if err == nil || !strings.Contains(err.Error(), "unsupported spec.runtime.type") {
		t.Fatalf("expected unsupported-runtime error, got: %v", err)
	}
}

// Slice 4.3 codex pass 11: Pods must NOT mount the namespace's default
// ServiceAccount token. Model-driven shells could otherwise exfiltrate
// cluster credentials.
func TestKubernetesBackendSubmitDisablesServiceAccountTokenAutomount(t *testing.T) {
	be := newTestKubernetesBackend(t)
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: makeK8sSpec(), Binding: makeK8sBinding(false)})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	pods, _ := be.client.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 Pod, got %d", len(pods.Items))
	}
	auto := pods.Items[0].Spec.AutomountServiceAccountToken
	if auto == nil || *auto {
		t.Errorf("expected AutomountServiceAccountToken=false, got %v", auto)
	}
}

// Slice 4.3 codex pass 6: surface terminal Waiting reasons rather than
// the generic 60s timeout when the Pod is stuck in Pending due to a bad
// image ref or missing Secret.
func TestTerminalWaitingReasonDetectsFatal(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CreateContainerConfigError",
						Message: `couldn't find key ANTHROPIC_API_KEY in Secret default/anthropic-creds`,
					},
				},
			}},
		},
	}
	reason, msg, fatal := terminalWaitingReason(pod)
	if !fatal {
		t.Fatalf("expected fatal=true for CreateContainerConfigError")
	}
	if reason != "CreateContainerConfigError" {
		t.Errorf("reason: got %q", reason)
	}
	if !strings.Contains(msg, "ANTHROPIC_API_KEY") {
		t.Errorf("message should propagate the kubelet detail; got %q", msg)
	}
}

func TestTerminalWaitingReasonIgnoresTransient(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "agent",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason: "ContainerCreating",
					},
				},
			}},
		},
	}
	if _, _, fatal := terminalWaitingReason(pod); fatal {
		t.Errorf("ContainerCreating must NOT be treated as terminal")
	}
}

func TestKubernetesBackendStopIsIdempotent(t *testing.T) {
	be := newTestKubernetesBackend(t)
	h, err := be.Submit(context.Background(), adl.ResolvedRunSpec{Spec: makeK8sSpec(), Binding: makeK8sBinding(false)})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := be.Stop(h); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	// Second Stop should be a no-op (sync.Once guards the delete).
	if err := be.Stop(h); err != nil {
		t.Errorf("second Stop should be idempotent, got: %v", err)
	}
}

// withK8sTestPropagator installs the W3C TraceContext propagator + a
// recording tracer provider just for the K8s trace-chain tests, and
// restores the previous globals on teardown. Mirrors the helper in
// trace_propagation_test.go — the K8s tests run in the same package
// so we can't import its withTestPropagator without renaming; this
// duplication is small and self-explanatory.
func withK8sTestPropagator(t *testing.T) {
	t.Helper()
	prevProp := otel.GetTextMapPropagator()
	prevTP := otel.GetTracerProvider()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevProp)
		otel.SetTracerProvider(prevTP)
	})
}

// findEnvByName scans a container's envVars for an entry by Name. Used
// by the trace-chain tests below; returns nil if absent.
func findEnvByName(envVars []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range envVars {
		if envVars[i].Name == name {
			return &envVars[i]
		}
	}
	return nil
}

// Slice 5.5: prove the K8s host-side trace injection chain (built in
// slice 5.2) is reachable end-to-end. The standalone injectTraceparent
// helper has its own tests in trace_propagation_test.go; what's new
// here is verifying that KubernetesBackend.Submit's Pod-spec assembly
// path actually wires the helper's output into the Pod container's
// envVars in the expected shape.
func TestKubernetesBackendSubmitPropagatesActiveTraceparentToPodEnv(t *testing.T) {
	withK8sTestPropagator(t)
	tracer := otel.Tracer("k8s-trace-chain-test")
	ctx, span := tracer.Start(context.Background(), "host.agentctl.run")
	defer span.End()

	expectedTraceIDHex := span.SpanContext().TraceID().String()

	be := newTestKubernetesBackend(t)
	h, err := be.Submit(ctx, adl.ResolvedRunSpec{
		Spec:    makeK8sSpec(),
		Binding: makeK8sBinding(false),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if h == "" {
		t.Fatal("expected non-empty session handle")
	}

	pods, err := be.client.CoreV1().Pods("test-ns").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 Pod, got %d", len(pods.Items))
	}
	pod := pods.Items[0]
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	envVars := pod.Spec.Containers[0].Env

	tp := findEnvByName(envVars, "TRACEPARENT")
	if tp == nil {
		t.Fatalf("Pod container env missing TRACEPARENT; got %d entries: %+v", len(envVars), envVars)
	}
	// W3C TraceContext: `00-<32-hex-trace-id>-<16-hex-span-id>-<2-hex-flags>`.
	// The trace-id segment must match the active host span's trace id —
	// that's the whole point of slice 5.2's propagation chain. Without
	// this check we'd verify the envVar exists but not that it actually
	// carries the active context.
	parts := strings.Split(tp.Value, "-")
	if len(parts) != 4 || parts[0] != "00" {
		t.Errorf("TRACEPARENT not in W3C format: %q", tp.Value)
	}
	if parts[1] != expectedTraceIDHex {
		t.Errorf("TRACEPARENT trace id mismatch: got %q, want %q", parts[1], expectedTraceIDHex)
	}
	// Slice 5.2 contract: TRACEPARENT comes via Value, not ValueFrom.
	// SecretRef-injected env vars use ValueFrom; mixing the two on the
	// SAME envVar entry is a K8s validation error. This guard catches a
	// hypothetical regression where someone wires trace propagation
	// through a different mechanism that conflicts with the secret env.
	if tp.ValueFrom != nil {
		t.Errorf("TRACEPARENT envVar must use Value, not ValueFrom")
	}
}

// Slice 5.5: when no host-side OTel span is active, the Pod's env must
// NOT carry a TRACEPARENT — otherwise the in-Pod agentctl would extract
// a stale/empty parent and produce a detached root. This pins the
// no-op contract of injectTraceparent at the KubernetesBackend layer.
func TestKubernetesBackendSubmitOmitsTraceparentWithoutActiveSpan(t *testing.T) {
	withK8sTestPropagator(t)
	// Plain context, no active span — propagator should yield nothing
	// and injectTraceparent should return env unchanged.
	be := newTestKubernetesBackend(t)
	_, err := be.Submit(context.Background(), adl.ResolvedRunSpec{
		Spec:    makeK8sSpec(),
		Binding: makeK8sBinding(false),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	pods, _ := be.client.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 Pod, got %d", len(pods.Items))
	}
	envVars := pods.Items[0].Spec.Containers[0].Env
	if findEnvByName(envVars, "TRACEPARENT") != nil {
		t.Errorf("Pod env should not contain TRACEPARENT when no span is active")
	}
	if findEnvByName(envVars, "TRACESTATE") != nil {
		t.Errorf("Pod env should not contain TRACESTATE when no span is active")
	}
}

// Slice 5.5: secret-injected env vars and trace-injected env vars must
// coexist without collision. SecretRef envs come from `secretKeys`
// (ValueFrom: SecretKeyRef), trace envs are appended later as plain
// Value entries — they should sit side-by-side, not clobber each other.
func TestKubernetesBackendSubmitTraceEnvCoexistsWithSecretRefEnv(t *testing.T) {
	withK8sTestPropagator(t)
	tracer := otel.Tracer("k8s-coexist-test")
	ctx, span := tracer.Start(context.Background(), "host.run")
	defer span.End()

	be := newTestKubernetesBackend(t)
	_, err := be.Submit(ctx, adl.ResolvedRunSpec{
		Spec:    makeK8sSpec(),
		Binding: makeK8sBinding(false),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	pods, _ := be.client.CoreV1().Pods("test-ns").List(ctx, metav1.ListOptions{})
	envVars := pods.Items[0].Spec.Containers[0].Env

	// Both should be present. The default binding fixture declares one
	// secretKey (ANTHROPIC_API_KEY); slice 5.2's trace injection adds
	// TRACEPARENT alongside it.
	secretEnv := findEnvByName(envVars, "ANTHROPIC_API_KEY")
	if secretEnv == nil {
		t.Fatalf("Pod env missing ANTHROPIC_API_KEY from SecretRef")
	}
	if secretEnv.ValueFrom == nil || secretEnv.ValueFrom.SecretKeyRef == nil {
		t.Errorf("ANTHROPIC_API_KEY should reference the SecretKeyRef")
	}
	if secretEnv.Value != "" {
		t.Errorf("ANTHROPIC_API_KEY must use ValueFrom, not Value: %q", secretEnv.Value)
	}

	traceEnv := findEnvByName(envVars, "TRACEPARENT")
	if traceEnv == nil {
		t.Fatalf("Pod env missing TRACEPARENT (trace injection skipped?)")
	}
	if traceEnv.ValueFrom != nil {
		t.Errorf("TRACEPARENT must use Value, not ValueFrom")
	}
}
