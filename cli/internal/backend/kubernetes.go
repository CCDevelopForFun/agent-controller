// KubernetesBackend launches the agent-runtime-base image as a Pod and
// streams its NDJSON wire-event stdout back to the CLI. Submission +
// log-streaming + cleanup all run on the host process; the cluster only
// sees a Pod and a single-key ConfigMap.
//
// Slice 4.3 (skeleton):
//   - Resolve() delegates to the shared matcher (matcher.go)
//   - Submit() materializes a ConfigMap with the spec YAML, creates a Pod
//     that mounts it at /workspace/spec.yaml, and starts a goroutine that
//     follows the Pod logs and forwards parsed wire events on Events(h).
//   - Stop() deletes the Pod (foreground propagation) which terminates
//     the running container; the ConfigMap is reaped in the same call.
//   - Capabilities() advertises streaming and a configurable concurrency
//     ceiling. The default ceiling (1) matches LocalBackend; the K8s
//     backend can be configured higher per Binding once batch scenarios
//     show up.
//
// Out of scope for slice 4.3 (tracked as later 4.x slices):
//   - SecurityContext / NetworkPolicy enforcement (slice 4.5)
//   - Job vs Pod toggle, replicas, retries
//   - serviceAccount / imagePullSecrets / kubeconfig context selection
//     (slice 4.4)
//   - Pre-existing Secret validation (we just reference it by name and
//     let the Pod admission webhook / kubelet surface "Secret not found"
//     when the Pod is scheduled)
package backend

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	// Register the legacy auth-provider plugins (gcp, azure, oidc, …)
	// so a kubeconfig that authenticates fine with kubectl also works
	// here. Without this blank import, NewForConfig returns
	// "no Auth Provider found for name 'gcp'" or similar. Codex pass
	// 11 of slice 4.3.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"sigs.k8s.io/yaml"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// Default values for KubernetesTarget when the Binding leaves a field empty.
// These are intentionally hard-coded rather than derived from build-time
// constants — the Backend defaults to whatever image is documented as
// "current" for the matching CLI release. Bump together with new image
// publishes.
const (
	defaultKubernetesNamespace = "default"
	// defaultKubernetesImage MUST carry an agentctl + ADL schema that
	// recognizes every field we round-trip into the Pod's spec YAML.
	// Slice 5.1 added spec.observability; v0.1.2's embedded schema
	// predates it and would reject the field, so the default bumps to
	// v0.1.3 in lockstep with the v0.5.0 release. Sequencing after slice
	// merges:
	//   1) tag v0.5.0 (umbrella) → npm publishes adapters at 0.5.0
	//   2) tag runtime-image/v0.1.3 → image republishes with the new
	//      agentctl; this constant becomes resolvable on GHCR.
	defaultKubernetesImage = "ghcr.io/ccdevelopforfun/agent-runtime-base:0.1.5"
	defaultSecretKey       = "ANTHROPIC_API_KEY"

	// Pod / ConfigMap polling. Kept tight because users see no output
	// until the Pod transitions to Running.
	podReadyPollInterval = 250 * time.Millisecond
	podReadyTimeout      = 60 * time.Second
)

// KubernetesConfig bundles the Backend's runtime options. The struct is
// intentionally narrow — per-Binding fields (namespace, image, secret)
// live on the Binding, not here. KubernetesConfig only carries connection
// settings that aren't part of the ADL surface.
type KubernetesConfig struct {
	// Kubeconfig is the path to a kubeconfig file. Empty means use
	// the standard precedence: in-cluster ServiceAccount → KUBECONFIG
	// env → ~/.kube/config.
	Kubeconfig string
	// Context selects a kubeconfig context. Empty uses the kubeconfig's
	// `current-context`. Ignored when running in-cluster.
	Context string
}

// KubernetesBackend implements Backend over the Kubernetes API.
type KubernetesBackend struct {
	cfg    KubernetesConfig
	client kubernetes.Interface

	mu       sync.Mutex
	sessions map[SessionHandle]*k8sSession
	counter  uint64
}

type k8sSession struct {
	namespace string
	podName   string
	// specSecretName is the Secret holding the rendered Agent YAML. We
	// use a Secret (not a ConfigMap) because the spec can transitively
	// carry sensitive material — e.g. spec.mcpServers[].env or .headers
	// may pass API tokens to MCP servers. ConfigMaps are typically
	// readable by broader namespace RBAC than Secrets, so leaning toward
	// Secret matches the local backend's stdin-only path. Codex pass 2
	// of slice 4.3 caught the credential-leak surface.
	specSecretName string
	events         chan wire.Event
	cancel         context.CancelFunc
	done           chan struct{}
	// cancelled is set by Stop before cancelling the streaming context,
	// so streamPodLogs can emit reason=cancelled instead of reason=error
	// in the synthetic session.ended. Without this, user Ctrl-C would
	// surface as a Pod-failure error event (codex pass 2 of slice 4.3).
	cancelled   bool
	cancelledMu sync.Mutex
	// cleanupOnce + cleanupErr coordinate Pod+Secret teardown between
	// the streaming goroutine (normal-completion path) and Stop (cancel
	// path). sync.Once ensures Delete is called exactly once even when
	// both fire.
	cleanupOnce sync.Once
	cleanupErr  error
}

func (s *k8sSession) markCancelled() {
	s.cancelledMu.Lock()
	s.cancelled = true
	s.cancelledMu.Unlock()
}

func (s *k8sSession) wasCancelled() bool {
	s.cancelledMu.Lock()
	defer s.cancelledMu.Unlock()
	return s.cancelled
}

// NewKubernetesBackend loads kubeconfig, builds a typed clientset, and
// returns the backend. Returns an error when the kubeconfig can't be
// loaded or the cluster isn't reachable — failing here is cheaper than
// surfacing a vague "session.ended reason=error" after Submit.
func NewKubernetesBackend(cfg KubernetesConfig) (*KubernetesBackend, error) {
	restCfg, err := resolveRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("load kube config: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return &KubernetesBackend{
		cfg:      cfg,
		client:   cs,
		sessions: map[SessionHandle]*k8sSession{},
	}, nil
}

// resolveRESTConfig implements the precedence documented on
// KubernetesConfig: explicit Kubeconfig path → in-cluster ServiceAccount
// (when no path/context override is set) → standard kubeconfig
// discovery (KUBECONFIG env, ~/.kube/config). client-go's
// NewNonInteractiveDeferredLoadingClientConfig does NOT auto-fall-back
// to in-cluster mode when no kubeconfig is found — codex pass 2 of
// slice 4.3 caught the resulting "agentctl-in-Pod" gap.
func resolveRESTConfig(cfg KubernetesConfig) (*rest.Config, error) {
	// In-cluster mode only when neither an explicit path nor a context
	// override was requested — the operator clearly wants kubeconfig
	// when they set either.
	if cfg.Kubeconfig == "" && cfg.Context == "" {
		if ic, err := rest.InClusterConfig(); err == nil {
			return ic, nil
		}
		// rest.ErrNotInCluster falls through to kubeconfig discovery.
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		loadingRules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func (b *KubernetesBackend) Capabilities() Caps {
	// Streaming: yes (we pump Pod logs as wire events).
	// MCP: not yet — would need stdio relay back into the Pod.
	// Concurrency: same as LocalBackend until batch scenarios land.
	return Caps{SupportsStreaming: true, SupportsMCP: false, MaxConcurrency: 1}
}

// Resolve uses the shared matcher. The KubernetesBackend additionally
// requires the binding's target.type to be "kubernetes" — running a
// local-target Binding through this backend is a wiring bug, not a
// capability gap.
func (b *KubernetesBackend) Resolve(ctx context.Context, spec adl.CompiledSpec, binding *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	_ = ctx
	resolved, warnings, err := matchBinding(spec, binding)
	if err != nil {
		return resolved, warnings, err
	}
	if binding != nil && binding.Spec.Target.Type != "kubernetes" {
		return adl.ResolvedRunSpec{}, nil, fmt.Errorf(
			"KubernetesBackend received a binding with target.type=%q (expected %q) — "+
				"select a LocalBackend or rebind to a kubernetes target.",
			binding.Spec.Target.Type, "kubernetes",
		)
	}
	if binding != nil && binding.Spec.Target.Kubernetes == nil {
		return adl.ResolvedRunSpec{}, nil, fmt.Errorf(
			"binding %q has target.type=kubernetes but no target.kubernetes config block. "+
				"At minimum, set target.kubernetes.secretRef.name to the Secret that holds "+
				"the adapter credentials (e.g. ANTHROPIC_API_KEY).",
			binding.Metadata.Name,
		)
	}
	return resolved, warnings, nil
}

// Submit creates the ConfigMap + Pod and starts a goroutine that streams
// Pod logs as wire events on the SessionHandle's channel. Returns once
// the resources are accepted by the API server; the caller drains
// Events(h) for the actual run progress.
//
// Naming: every Submit picks a fresh `agentctl-<counter>-<unix-millis>`
// suffix so multiple concurrent runs (or rapid retries after a failed
// Pod) don't collide. The counter is per-Backend-instance, so multiple
// agentctl processes hitting the same cluster also need timestamp
// disambiguation — hence the millis suffix.
func (b *KubernetesBackend) Submit(ctx context.Context, run adl.ResolvedRunSpec) (SessionHandle, error) {
	spec := run.Spec
	binding := run.Binding
	if binding == nil || binding.Spec.Target.Kubernetes == nil {
		return "", fmt.Errorf("KubernetesBackend.Submit: ResolvedRunSpec is missing target.kubernetes config")
	}
	// Fail fast for CompiledSpecs that carry fields the Pod can't satisfy.
	// Codex pass 1 of slice 4.3 caught the silent field-drop.
	if err := validateCompiledSpecForK8s(spec); err != nil {
		return "", err
	}
	// Validate runtime.type BEFORE any cluster mutation. Hand-crafted
	// CompiledSpecs that bypass the CLI/schema dispatch (e.g. unit-test
	// fixtures or library consumers) could otherwise leave a spec Secret
	// orphaned in the namespace. Codex pass 5 of slice 4.3 caught this.
	switch spec.Runtime.Type {
	case "", "local", "local-pi", "local-opencode":
		// ok
	default:
		return "", fmt.Errorf(
			"KubernetesBackend: unsupported spec.runtime.type %q — expected one of: local | local-pi | local-opencode",
			spec.Runtime.Type,
		)
	}
	k := binding.Spec.Target.Kubernetes
	namespace := k.Namespace
	if namespace == "" {
		namespace = defaultKubernetesNamespace
	}
	image := k.Image
	if image == "" {
		image = defaultKubernetesImage
	}
	if k.SecretRef == nil || k.SecretRef.Name == "" {
		return "", fmt.Errorf(
			"binding %q: target.kubernetes.secretRef.name is required (set it to a Secret "+
				"in the same namespace that holds ANTHROPIC_API_KEY)",
			binding.Metadata.Name,
		)
	}
	secretKeys := k.SecretRef.Keys
	if len(secretKeys) == 0 {
		secretKeys = []string{defaultSecretKey}
	}

	// Serialize the spec as YAML for the in-Pod agentctl to consume.
	// CompiledSpec is what Submit receives, but the Pod's agentctl wants
	// the original ADL Agent YAML shape so it can compile + validate
	// inside the container. We reconstruct an Agent YAML from the
	// CompiledSpec — slice 4.3 only supports self-contained specs (no
	// registry-backed tools/extensions/skills/subagents) so this
	// round-trip is lossless. Slice 4.4 will add registry handling.
	specYAML, err := marshalCompiledSpecAsAgentYAML(spec)
	if err != nil {
		return "", fmt.Errorf("serialize spec for Pod: %w", err)
	}

	b.mu.Lock()
	b.counter++
	id := b.counter
	b.mu.Unlock()
	// Per-instance counter + 5-byte hex random suffix. counter alone
	// disambiguates within ONE agentctl process; the random suffix
	// disambiguates across processes that submit to the same namespace
	// in the same millisecond. Codex pass 12 of slice 4.3 caught the
	// cross-process collision.
	suffix := fmt.Sprintf("%d-%s", id, randomSuffix(5))
	specSecretName := "agentctl-spec-" + suffix
	podName := "agentctl-" + suffix

	// 1) Create the Secret holding the spec. We use a Secret (not a
	//    ConfigMap) because spec.mcpServers[].env / .headers can pass
	//    API tokens to MCP servers, and ConfigMaps are typically
	//    readable by broader RBAC than Secrets.
	specSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      specSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agentctl",
				"agent-controller.dev/role":    "spec",
				"agent-controller.dev/pod":     podName,
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"spec.yaml": specYAML},
	}
	if _, err := b.client.CoreV1().Secrets(namespace).Create(ctx, specSecret, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create spec Secret %s/%s: %w", namespace, specSecretName, err)
	}

	// 2) Create the Pod that mounts the ConfigMap at /workspace/spec.yaml
	//    via subPath (so /workspace remains writable for the in-Pod
	//    adapter's .pi/ state) and reads creds from the named Secret.
	envVars := make([]corev1.EnvVar, 0, len(secretKeys)+1)
	for _, key := range secretKeys {
		envVars = append(envVars, corev1.EnvVar{
			Name: key,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: k.SecretRef.Name},
					Key:                  key,
				},
			},
		})
	}
	// AGENT_CONTROLLER_RUNTIME selects which adapter the in-Pod agentctl
	// spawns. The agent-runtime-base image defaults to the Pi adapter;
	// when the spec wants opencode we MUST override here, otherwise the
	// in-Pod agentctl silently launches Pi (which would then reject the
	// spec at runtime). Codex pass 1 of slice 4.3 caught this drop.
	// Runtime-type validity was checked above.
	if spec.Runtime.Type == "local-opencode" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_CONTROLLER_RUNTIME",
			Value: "/usr/local/lib/node_modules/@agent-controller/runtime-opencode/dist/index.js",
		})
	}
	// Slice 5.2: inject TRACEPARENT (+ TRACESTATE if present) so the
	// in-Pod agentctl can pick up the host-side trace context and
	// nest its spans under the host `agentctl.run` span. injectTraceparent
	// returns the input unchanged when tracing is off (the global
	// propagator is the SDK's no-op default), so this is safe to call
	// unconditionally.
	traceEnv := injectTraceparent(ctx, nil)
	for _, kv := range traceEnv {
		// kv is "TRACEPARENT=..." or "TRACESTATE=..."; split once.
		eq := -1
		for i, c := range kv {
			if c == '=' {
				eq = i
				break
			}
		}
		if eq <= 0 {
			continue
		}
		envVars = append(envVars, corev1.EnvVar{Name: kv[:eq], Value: kv[eq+1:]})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agentctl",
				"agent-controller.dev/role":    "agent-run",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// SECURITY: the in-Pod agentctl does NOT need to call the
			// Kubernetes API, but the namespace default ServiceAccount
			// would otherwise be mounted at
			// /var/run/secrets/kubernetes.io/serviceaccount/token. Model-
			// driven shells/MCP commands could then read it and act on
			// the cluster with the host operator's namespace permissions.
			// Slice 4.4 will let bindings opt back in via
			// target.kubernetes.serviceAccount. Codex pass 11 of slice 4.3.
			AutomountServiceAccountToken: ptr(false),
			Containers: []corev1.Container{{
				Name:  "agent",
				Image: image,
				// --ndjson-stdout flips the in-Pod agentctl's stdout from
				// human-formatted `[type] {...}` to raw wire NDJSON so
				// kubectl logs (which streamPodLogs reads from) yields
				// parseable wire events. Without it, every K8s run looks
				// like an error to the host. Codex pass 4 of slice 4.3.
				Args: []string{"run", "--ndjson-stdout", "/workspace/spec.yaml"},
				Env:  envVars,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "spec",
					MountPath: "/workspace/spec.yaml",
					SubPath:   "spec.yaml",
					ReadOnly:  true,
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "spec",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: specSecretName,
					},
				},
			}},
		},
	}
	if _, err := b.client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		// Best-effort spec-Secret cleanup before bubbling up. If the
		// delete ALSO fails (RBAC, transient API error), aggregate both
		// errors so the operator knows the Secret is orphaned and may
		// hold spec.mcpServers credentials. Codex pass 10 of slice 4.3.
		createErr := fmt.Errorf("create Pod %s/%s: %w", namespace, podName, err)
		if delErr := b.client.CoreV1().Secrets(namespace).Delete(context.Background(), specSecretName, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return "", errors.Join(createErr, fmt.Errorf("rollback delete of orphan spec Secret %s/%s: %w (delete it manually)", namespace, specSecretName, delErr))
		}
		return "", createErr
	}

	// 3) Spawn the log-streaming + lifecycle goroutine. The session
	//    handle is returned immediately so the caller can start draining
	//    Events(h) while the Pod is still scheduling.
	sessionCtx, cancel := context.WithCancel(context.Background())
	sess := &k8sSession{
		namespace:      namespace,
		podName:        podName,
		specSecretName: specSecretName,
		events:         make(chan wire.Event, 32),
		cancel:         cancel,
		done:           make(chan struct{}),
	}
	h := SessionHandle("k8s-" + suffix)
	b.mu.Lock()
	b.sessions[h] = sess
	b.mu.Unlock()
	go b.streamPodLogs(sessionCtx, sess)
	return h, nil
}

func (b *KubernetesBackend) Events(h SessionHandle) <-chan wire.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[h]
	if !ok {
		// Closed channel so callers can range over it without checking.
		c := make(chan wire.Event)
		close(c)
		return c
	}
	return s.events
}

// Stop tears down the running Pod and its spec Secret. Cancellation of
// the streaming goroutine is best-effort — the caller may already have
// observed a terminal wire event (session.ended) before calling Stop.
// Stop is idempotent: streamPodLogs also calls cleanup() on normal
// completion. We also mark the session as user-cancelled BEFORE
// cancelling the context, so the streaming goroutine emits the
// canonical reason=cancelled terminal event instead of synthesizing a
// reason=error (codex pass 2 of slice 4.3 caught the wrong-reason gap).
func (b *KubernetesBackend) Stop(h SessionHandle) error {
	b.mu.Lock()
	s, ok := b.sessions[h]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %s", h)
	}
	s.markCancelled()
	// Try cleanup FIRST so the Pod is gone before we cut visibility. If
	// cleanup fails (e.g. RBAC allows pod creation but not deletion, or
	// the API delete times out), the streaming goroutine will surface a
	// wire warning before closing — the user sees that their cancel
	// didn't actually reclaim the Pod. Codex pass 8 of slice 4.3 caught
	// the silent-Pod-leak gap.
	cleanupErr := b.cleanupSession(s)
	s.cancel()
	return cleanupErr
}

// cleanupSession deletes the Pod and spec Secret for a session. Guarded
// by sync.Once so the streaming goroutine and Stop can BOTH call it
// safely. Each delete is attempted independently — a Pod-delete failure
// (e.g. RBAC, transient API error) must NOT skip the spec Secret delete,
// because the spec Secret carries the rendered Agent YAML (potentially
// including spec.mcpServers credentials). Codex pass 9 of slice 4.3.
func (b *KubernetesBackend) cleanupSession(s *k8sSession) error {
	s.cleanupOnce.Do(func() {
		var errs []error
		// Separate per-call timeouts so a stalled Pod-delete cannot
		// starve the Secret-delete. Without this, an apiserver/admission
		// hang on the Pod call would expire the shared context and the
		// spec Secret (which can carry MCP credentials) would never even
		// be attempted. Codex pass 12 of slice 4.3.
		podCtx, podCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer podCancel()
		if err := b.client.CoreV1().Pods(s.namespace).Delete(podCtx, s.podName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete Pod %s/%s: %w", s.namespace, s.podName, err))
		}
		secCtx, secCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer secCancel()
		if err := b.client.CoreV1().Secrets(s.namespace).Delete(secCtx, s.specSecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete spec Secret %s/%s: %w", s.namespace, s.specSecretName, err))
		}
		s.cleanupErr = errors.Join(errs...)
	})
	return s.cleanupErr
}

// streamPodLogs waits for the Pod to reach a state where logs can be
// streamed, opens a log stream with follow=true, and forwards each
// NDJSON line as a wire.Event. On stream EOF or context cancellation:
//   - If a terminal wire event (session.ended) was already forwarded,
//     the channel just closes — the in-Pod agentctl said its piece.
//   - If NOT, we inspect the Pod's container exit status and synthesize
//     an error + session.ended(reason=error) so the caller sees a
//     terminal event (otherwise agentctl exits 0 silently — codex pass
//     1 of slice 4.3 caught this).
//   - Either way, we delete the Pod and ConfigMap via cleanupSession.
func (b *KubernetesBackend) streamPodLogs(ctx context.Context, sess *k8sSession) {
	defer close(sess.done)
	// Defers run LIFO. We want: cleanup → emit-cleanup-warning-if-needed →
	// close events. Codex pass 8 of slice 4.3 caught the silent-cleanup-
	// failure gap (Pod + spec Secret could remain on the cluster with
	// MCP env/header credentials).
	defer close(sess.events)
	defer func() {
		if err := b.cleanupSession(sess); err != nil {
			msg := fmt.Sprintf(
				"cleanup of Pod %s/%s and/or its spec Secret failed: %v — "+
					"the Pod may still be running on the cluster with this run's credentials, and the spec Secret may still hold any spec.mcpServers credentials it referenced. Manual cleanup: "+
					"kubectl -n %s delete pod %s && kubectl -n %s delete secret %s",
				sess.namespace, sess.podName, err,
				sess.namespace, sess.podName,
				sess.namespace, sess.specSecretName,
			)
			// Non-blocking send so a full / closed channel doesn't deadlock
			// us during shutdown.
			select {
			case sess.events <- syntheticError(msg):
			default:
			}
		}
	}()

	if err := waitForPodLogsReady(ctx, b.client, sess.namespace, sess.podName); err != nil {
		// If the user cancelled during the readiness wait (Ctrl-C while
		// Pod is Pending or image-pulling), surface reason=cancelled —
		// matches LocalBackend's exit-130 convention. Codex pass 3 of
		// slice 4.3.
		if sess.wasCancelled() || ctx.Err() != nil {
			sess.events <- syntheticSessionEnded("cancelled", "agent run cancelled by user before Pod became ready")
			return
		}
		sess.events <- syntheticError(fmt.Sprintf("Pod %s/%s never became ready for log streaming: %v", sess.namespace, sess.podName, err))
		sess.events <- syntheticSessionEnded("error", fmt.Sprintf("Pod readiness wait failed: %v", err))
		return
	}

	// Always specify Container in PodLogOptions. Without it, the logs
	// API errors when more than one container exists (sidecar injection
	// via mutating admission webhooks). Codex pass 3 of slice 4.3.
	req := b.client.CoreV1().Pods(sess.namespace).GetLogs(sess.podName, &corev1.PodLogOptions{
		Container: "agent",
		Follow:    true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		if sess.wasCancelled() || ctx.Err() != nil {
			sess.events <- syntheticSessionEnded("cancelled", "agent run cancelled by user before log stream opened")
			return
		}
		sess.events <- syntheticError(fmt.Sprintf("open Pod log stream: %v", err))
		sess.events <- syntheticSessionEnded("error", fmt.Sprintf("could not open Pod log stream: %v", err))
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	// Default Scanner buffer is 64KB which is plenty for wire events;
	// raise the cap so an unusually-large `message` event doesn't get
	// truncated.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	sawTerminal := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, err := wire.Decode(line)
		if err != nil {
			// Forward non-NDJSON lines as a synthetic warning so we don't
			// silently swallow stderr/diagnostic chatter from the Pod.
			sess.events <- syntheticWarning(string(line))
			continue
		}
		if ev.Type == wire.EventSessionEnded {
			sawTerminal = true
		}
		select {
		case sess.events <- ev:
		case <-ctx.Done():
			// Cancellation raced with a buffered-send (or user Ctrl-C
			// fired while we were blocked here). The CLI keys exit code
			// 130 off the LAST event being session.ended(cancelled), so
			// synthesize one before returning — non-blocking so a closed/
			// full channel doesn't deadlock during shutdown. Codex pass
			// 10 of slice 4.3.
			select {
			case sess.events <- syntheticSessionEnded("cancelled", "agent run cancelled by user during event delivery"):
			default:
			}
			return
		}
	}
	// Only surface scanner errors when no terminal event was already seen.
	// Post-terminal transport noise (Pod deleted, container exited
	// cleanly after session.ended) should NOT flip an otherwise-successful
	// run into exit 1. Codex pass 3 of slice 4.3 caught this.
	if err := scanner.Err(); err != nil && !isClosedConnError(err) && !sawTerminal {
		sess.events <- syntheticError(fmt.Sprintf("read Pod log stream: %v", err))
	}
	if !sawTerminal {
		// Stream ended without the in-Pod agentctl emitting session.ended.
		// If the user initiated Stop, surface reason=cancelled (matches
		// LocalBackend's exit-130 convention); otherwise inspect the
		// container's terminal state for a meaningful error event.
		if sess.wasCancelled() {
			sess.events <- syntheticSessionEnded("cancelled", "agent run cancelled by user (SIGINT/SIGTERM)")
			return
		}
		reason, msg := inspectPodTerminalState(context.Background(), b.client, sess.namespace, sess.podName)
		sess.events <- syntheticError(msg)
		sess.events <- syntheticSessionEnded(reason, msg)
	}
}

// inspectPodTerminalState reads the Pod's status and returns a
// (reason, message) pair suitable for a synthetic session.ended event.
// Distinguishes "container exited non-zero", "Pod failed for non-
// container reason", and "context cancelled before terminal state".
func inspectPodTerminalState(ctx context.Context, client kubernetes.Interface, namespace, podName string) (string, string) {
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "error", fmt.Sprintf("Pod %s/%s log stream ended before session.ended; could not inspect terminal state: %v", namespace, podName, err)
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		// Shouldn't happen in practice — if the container exited 0
		// without emitting session.ended the in-Pod agentctl is buggy.
		return "error", fmt.Sprintf("Pod %s/%s exited 0 but never emitted session.ended; the in-Pod agentctl may have crashed before producing wire events", namespace, podName)
	case corev1.PodFailed:
		for _, c := range pod.Status.ContainerStatuses {
			if c.State.Terminated != nil {
				return "error", fmt.Sprintf("Pod %s/%s container %q exited %d (%s) without emitting session.ended", namespace, podName, c.Name, c.State.Terminated.ExitCode, c.State.Terminated.Reason)
			}
		}
		return "error", fmt.Sprintf("Pod %s/%s failed without container terminal state", namespace, podName)
	default:
		return "error", fmt.Sprintf("Pod %s/%s log stream ended while Pod phase=%s; in-Pod agentctl never emitted session.ended", namespace, podName, pod.Status.Phase)
	}
}

func waitForPodLogsReady(ctx context.Context, client kubernetes.Interface, namespace, podName string) error {
	deadline, cancel := context.WithTimeout(ctx, podReadyTimeout)
	defer cancel()
	for {
		pod, err := client.CoreV1().Pods(namespace).Get(deadline, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return nil
		case corev1.PodPending, "":
			// While Pending, inspect the agent container's Waiting state.
			// Fast-fail on terminal Waiting reasons so the user sees the
			// actionable message ("Secret X not found", "ImagePullBackOff
			// for ghcr.io/.../agent-runtime-base:0.1.99") instead of the
			// generic 60s readiness timeout. Codex pass 6 of slice 4.3.
			if reason, msg, fatal := terminalWaitingReason(pod); fatal {
				return fmt.Errorf("Pod stuck in Pending: %s — %s", reason, msg)
			}
		default:
			return fmt.Errorf("unexpected Pod phase %q", pod.Status.Phase)
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("timed out waiting for Pod to reach Running phase")
		case <-time.After(podReadyPollInterval):
		}
	}
}

// terminalWaitingReason returns (reason, message, true) when the agent
// container is stuck in a Waiting state we know won't resolve on its own
// (bad image ref, missing referenced Secret, CreateContainerConfigError,
// etc.). Returns ("", "", false) for transient Waiting reasons like
// ContainerCreating, PodInitializing, or empty (just-scheduled).
func terminalWaitingReason(pod *corev1.Pod) (string, string, bool) {
	// Reasons listed in kubernetes/pkg/kubelet/events/event.go +
	// container/manager.go. "ContainerCreating" / "PodInitializing" are
	// transient; anything else under Waiting on a non-init container
	// usually means kubelet won't be able to fix it on its own.
	fatal := map[string]struct{}{
		"ErrImagePull":                 {},
		"ImagePullBackOff":             {},
		"InvalidImageName":             {},
		"ImageInspectError":            {},
		"ErrImageNeverPull":            {},
		"RegistryUnavailable":          {},
		"CreateContainerConfigError":   {},
		"CreateContainerError":         {},
		"PreCreateHookError":           {},
		"PreStartHookError":            {},
		"PostStartHookError":           {},
		"RunContainerError":            {},
		"KillContainerError":           {},
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" {
			continue
		}
		if cs.State.Waiting == nil {
			continue
		}
		if _, ok := fatal[cs.State.Waiting.Reason]; ok {
			return cs.State.Waiting.Reason, cs.State.Waiting.Message, true
		}
	}
	return "", "", false
}

// syntheticError stamps an error event the host emits on the wire stream
// (NOT one received from the Pod). Used when the streaming pipeline
// itself breaks.
func syntheticError(msg string) wire.Event {
	payload, _ := json.Marshal(map[string]string{"message": msg, "source": "KubernetesBackend"})
	return wire.Event{
		V:    wire.ProtocolVersion,
		Type: wire.EventError,
		Ts:   time.Now().UTC(),
		Data: payload,
	}
}

// syntheticSessionEnded synthesizes a session.ended event when the in-Pod
// agentctl exits without producing one (e.g. crash, OOM, image pull
// failure). Without this, agentctl run would exit 0 silently. Codex pass
// 1 of slice 4.3 caught the silent-exit-on-Pod-failure gap.
func syntheticSessionEnded(reason, msg string) wire.Event {
	payload, _ := json.Marshal(map[string]string{
		"reason":  reason,
		"message": msg,
		"source":  "KubernetesBackend",
	})
	return wire.Event{
		V:    wire.ProtocolVersion,
		Type: wire.EventSessionEnded,
		Ts:   time.Now().UTC(),
		Data: payload,
	}
}

// syntheticWarning wraps a non-NDJSON log line as a `warning` event so
// the operator sees it without it being mistaken for protocol traffic.
func syntheticWarning(line string) wire.Event {
	payload, _ := json.Marshal(map[string]any{
		"kind":    "pod_log_non_ndjson",
		"line":    line,
		"message": "Pod emitted a non-NDJSON line; surfaced as a warning so it isn't lost",
	})
	return wire.Event{
		V:    wire.ProtocolVersion,
		Type: wire.EventWarning,
		Ts:   time.Now().UTC(),
		Data: payload,
	}
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}
	return err == io.EOF || err == context.Canceled || err.Error() == "context canceled"
}

// validateCompiledSpecForK8s rejects CompiledSpec fields the slice-4.3
// Pod can't handle. The in-Pod agentctl re-compiles the YAML we hand it,
// but with no access to the host's registry directories — so any field
// that carries an absolute Entrypoint path (custom Pi-extension tools,
// extensions, skills, subagents) would fail to resolve. Same for
// installs[], which would need a writable filesystem the image's
// non-root UID can't get. Slice 4.4 will mount the registry as
// additional ConfigMaps and lift these restrictions.
//
// What this DOES allow:
//   - Built-in tools (Entrypoint==""): bash/read/edit/write.
//   - spec.mcpServers — stdio/sse/http servers, as long as the command
//     path or URL is reachable from inside the Pod.
//   - spec.guardrails — passed through verbatim.
//   - spec.persona, spec.model, spec.task — the core surface.
//
// Codex pass 1 of slice 4.3 caught the silent field-drop in the original
// marshalCompiledSpecAsAgentYAML implementation.
func validateCompiledSpecForK8s(spec adl.CompiledSpec) error {
	for _, t := range spec.Tools {
		if t.Entrypoint != "" {
			return fmt.Errorf(
				"KubernetesBackend (v0.4.3) does not yet support custom Pi-extension tools — "+
					"tool %q resolves to an entrypoint outside the Pod image (%s). "+
					"Restrict the spec to built-in tools (bash/read/edit/write) or wait for "+
					"slice 4.4, which adds ConfigMap-mounted registries.",
				t.Name, t.Entrypoint,
			)
		}
	}
	if len(spec.Extensions) > 0 {
		return fmt.Errorf("KubernetesBackend (v0.4.3) does not yet support spec.extensions[] — slice 4.4 will add ConfigMap-mounted registries")
	}
	if len(spec.Skills) > 0 {
		return fmt.Errorf("KubernetesBackend (v0.4.3) does not yet support spec.skills[] — slice 4.4 will add ConfigMap-mounted registries")
	}
	if len(spec.Subagents) > 0 {
		return fmt.Errorf("KubernetesBackend (v0.4.3) does not yet support spec.subagents[] — slice 4.4 will add ConfigMap-mounted registries")
	}
	if len(spec.Installs) > 0 {
		return fmt.Errorf("KubernetesBackend (v0.4.3) does not yet support spec.installs[] — the agent-runtime-base image is immutable and runs as non-root")
	}
	if spec.SessionID != nil && *spec.SessionID != "" {
		return fmt.Errorf("KubernetesBackend (v0.4.3) does not yet support --resume; persistent sessions require a stable storage backend")
	}
	return nil
}

// marshalCompiledSpecAsAgentYAML reconstructs an Agent YAML from a
// CompiledSpec. The Pod's agentctl wants the ADL Agent shape, not the
// CompiledSpec internal representation. validateCompiledSpecForK8s
// rejected unsupported fields upstream, so the only conditional sections
// here are persona, built-in tools, mcpServers, and guardrails.
//
// Slice 4.4 will replace this with a `agentctl run-compiled` subcommand
// that takes the JSON CompiledSpec directly, eliminating the round-trip.
func marshalCompiledSpecAsAgentYAML(spec adl.CompiledSpec) (string, error) {
	specBody := map[string]any{
		"model":   spec.Model,
		"task":    spec.Task,
		"runtime": spec.Runtime,
	}
	// Preserve built-in tools by name only (Entrypoint=="" — validated above).
	tools := make([]map[string]any, 0, len(spec.Tools))
	for _, t := range spec.Tools {
		entry := map[string]any{"name": t.Name}
		if t.Config != nil {
			entry["config"] = t.Config
		}
		tools = append(tools, entry)
	}
	specBody["tools"] = tools

	if spec.Persona != nil && (spec.Persona.Role != "" || spec.Persona.Instructions != "") {
		specBody["persona"] = spec.Persona
	}
	if len(spec.MCPServers) > 0 {
		specBody["mcpServers"] = spec.MCPServers
	}
	if spec.Guardrails != nil {
		specBody["guardrails"] = spec.Guardrails
	}
	// Slice 5.1: preserve spec.observability so the in-Pod agentctl
	// honors `tracing: true` when its env has OTEL_EXPORTER_OTLP_ENDPOINT
	// set (typically injected from a separate Secret/ConfigMap by the
	// operator). Without this the field would be silently dropped by
	// the round-trip and tracing on K8s runs would never fire. Codex
	// pass 3 of slice 5.1 caught the gap.
	if spec.Observability != nil {
		specBody["observability"] = spec.Observability
	}
	// Slice 7.2: deliberately DO NOT emit spec.outputSchema into the
	// in-Pod YAML.
	//
	// Codex pass 5 of slice 7.2 caught the release-coordination bug:
	// the default in-Pod image (`defaultKubernetesImage` —
	// agent-runtime-base:0.1.5) embeds an older ADL schema that
	// pre-dates outputSchema. Writing the field into the Secret
	// would make the in-Pod `agentctl run` fail validation BEFORE
	// the agent gets a chance to run, breaking every default K8s
	// run that uses `spec.outputSchema` (and `--output-file` is
	// host-side anyway, so the in-Pod child can't honor the field
	// even when it does parse it).
	//
	// The host-side output-file machinery is unaffected: the host
	// CLI has the schema in its own CompiledSpec and validates the
	// captured assistant text from the wire stream as the K8s Pod
	// streams events back. Once a future slice bumps the default
	// runtime image to a build that ships the outputSchema-aware
	// schema, this round-trip can be re-added (with a regression
	// test that pins image-version → schema-fields compatibility).

	meta := map[string]any{"name": spec.Metadata.Name}
	// Preserve the full metadata block so K8s runs match local runs for
	// specs that set owner/description (opencode's `name` summarization
	// uses description). Codex pass 3 of slice 4.3 caught the drop.
	if spec.Metadata.Owner != "" {
		meta["owner"] = spec.Metadata.Owner
	}
	if spec.Metadata.Description != "" {
		meta["description"] = spec.Metadata.Description
	}
	doc := map[string]any{
		"apiVersion": "agent-controller.dev/v1alpha1",
		"kind":       "Agent",
		"metadata":   meta,
		"spec":       specBody,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ptr returns a pointer to its argument. Used for K8s API fields that
// take *bool / *int32 / *string for tri-state semantics (set vs absent).
func ptr[T any](v T) *T { return &v }

// randomSuffix returns nBytes of crypto-random entropy hex-encoded. Used
// to disambiguate resource names across agentctl processes submitting
// to the same namespace concurrently. Falls back to a fixed string only
// on the unreachable crypto/rand-read-failed path so tests / generation
// never wedges. K8s resource names must match DNS-1123; hex satisfies it.
func randomSuffix(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "norand"
	}
	return hex.EncodeToString(buf)
}

// Compile-time check that KubernetesBackend implements Backend. Catches
// interface drift without needing a runtime test.
var _ Backend = (*KubernetesBackend)(nil)
