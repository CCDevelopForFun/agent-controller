//go:build ignore

// Package classifier shows how to call `agentctl run` from a Temporal
// workflow. The agent classifies text sentiment; the workflow wraps it
// as a single Activity so Temporal's retry / heartbeat / timeout
// machinery applies.
//
// Pattern
//
//  1. The Workflow function ClassifyWorkflow stays deterministic —
//     Temporal replays it on worker restarts, so it MUST NOT shell
//     out directly. All side effects (process exec, file I/O) live
//     in the Activity.
//  2. The Activity RunAgentctl invokes `agentctl run` with --input
//     and --output-file, then reads + parses the result. With
//     spec.outputSchema set, the JSON is GUARANTEED to match the
//     shape declared in text-classifier.yaml — the Activity can
//     json.Unmarshal directly without re-validating.
//  3. Temporal handles retries (e.g. on transient model errors)
//     via the configured RetryPolicy. The Activity is idempotent
//     — re-running agentctl with the same input is safe.
//
// To run:
//
//	go run ./cmd/worker     # registers the Workflow + Activity
//	temporal workflow start --type ClassifyWorkflow --input '"I love it"'
//
// The example assumes:
//   - `agentctl` is on PATH inside the Temporal worker process.
//   - The agent spec is at /opt/agents/text-classifier.yaml.
//   - The worker has write access to /tmp.
//
// Production deployments would instead pass the spec path + workspace
// dir through the Activity input so the Workflow stays portable.
package classifier

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ClassificationResult mirrors spec.outputSchema in
// examples/schedulers/text-classifier.yaml. `agentctl run` validates
// the agent's reply against that schema before writing the file, so
// json.Unmarshal here can't see a malformed shape — only a network
// error or a process failure would surface as a Go error.
type ClassificationResult struct {
	Label      string  `json:"label"`
	Confidence float64 `json:"confidence"`
}

// RunAgentctl is the Activity that shells out to `agentctl run`. It
// MUST be deterministic-free — Temporal will retry it on failure and
// the Workflow's replay only sees the final result, not the side
// effects.
func RunAgentctl(ctx context.Context, text string) (ClassificationResult, error) {
	// Key the result path on (RunID, ActivityID, Attempt) so concurrent
	// activity attempts (e.g. a retry on a different worker, or two
	// RunAgentctl activities running in parallel inside the same
	// Workflow) don't clobber each other's files. RunID alone is too
	// coarse — every retry shares it. Codex pass 3 of slice 7.3 caught
	// the original RunID-only path as a real concurrency bug.
	info := activity.GetInfo(ctx)
	outPath := filepath.Join(
		os.TempDir(),
		fmt.Sprintf("agentctl-%s-%s-attempt-%d.json",
			info.WorkflowExecution.RunID, info.ActivityID, info.Attempt),
	)

	cmd := exec.CommandContext(ctx, "agentctl", "run",
		"--input", "text="+text,
		"--output-file", outPath,
		"/opt/agents/text-classifier.yaml",
	)
	// exec.CommandContext's DEFAULT cancel behavior is Process.Kill()
	// (SIGKILL). That bypasses agentctl's own SIGTERM/SIGINT cleanup, so
	// when Temporal cancels or times out this Activity (and then retries
	// it) the agentctl process — and the runtime subprocess + in-flight
	// model request it owns — can keep running, orphaned. Send SIGTERM
	// instead so agentctl tears down cleanly. Codex pass 6 of slice 7.3.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	// Don't trust a wedged process forever: if agentctl hasn't exited
	// within this grace period after the SIGTERM, Go escalates to
	// SIGKILL. WaitDelay also bounds how long cmd.Wait blocks on any
	// lingering child I/O after the process exits.
	cmd.WaitDelay = 15 * time.Second
	// Stream agentctl's stderr to the activity log so a failed run is
	// debuggable without ssh-ing into the worker.
	cmd.Stderr = os.Stderr

	// Temporal only DELIVERS cancellation to an Activity that heartbeats,
	// so beat periodically. Without this, ctx is not canceled until
	// StartToCloseTimeout and the graceful-shutdown path above never runs
	// on an early cancellation. Requires HeartbeatTimeout on the Activity
	// options (set in ClassifyWorkflow below).
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()

	if err := cmd.Run(); err != nil {
		// Wrap as a non-retryable error if it's a config / schema
		// problem (deterministic), retryable otherwise. The simple
		// version below treats every failure as retryable — a real
		// deployment would discriminate on exit code (1 = config /
		// validation error, others = runtime).
		return ClassificationResult{}, fmt.Errorf("agentctl run: %w", err)
	}
	defer os.Remove(outPath)

	data, err := os.ReadFile(outPath)
	if err != nil {
		return ClassificationResult{}, fmt.Errorf("read result file: %w", err)
	}
	var r ClassificationResult
	if err := json.Unmarshal(data, &r); err != nil {
		// Unreachable if spec.outputSchema covers the shape, but
		// guard anyway — defense in depth against schema drift.
		return ClassificationResult{}, temporal.NewNonRetryableApplicationError(
			"agentctl output not valid JSON", "OutputDecodeError", err)
	}
	return r, nil
}

// ClassifyWorkflow is the Temporal Workflow entry point. It MUST stay
// deterministic — no time.Now, no random, no I/O. All side effects go
// through Activities.
func ClassifyWorkflow(ctx workflow.Context, text string) (ClassificationResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		// HeartbeatTimeout must exceed the Activity's heartbeat interval
		// (30s in RunAgentctl). It also gates cancellation delivery: the
		// server only forwards a cancel to the Activity on its next
		// heartbeat, so without this the SIGTERM graceful-shutdown path
		// in RunAgentctl can't fire until StartToCloseTimeout.
		HeartbeatTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	})

	var result ClassificationResult
	if err := workflow.ExecuteActivity(ctx, RunAgentctl, text).Get(ctx, &result); err != nil {
		return ClassificationResult{}, err
	}
	return result, nil
}
