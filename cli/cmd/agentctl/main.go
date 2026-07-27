package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/codes"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/observability"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/registry"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// version is the agentctl release tag. Set at build time via
// `-ldflags="-X 'main.version=...'"` — see .github/workflows/release.yml.
// Falls back to "dev" for local builds. Surfaced as an OTel resource
// attribute on every span so traces can be correlated to a specific
// CLI build.
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "agentctl",
		Short: "Agent Controller CLI",
	}
	root.AddCommand(newValidateCmd(), newCompileCmd(), newRunCmd(), newChatCmd(), newServeCmd(), newSessionsCmd(), NewInstallCmd(), newWorkspaceMCPCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [adl.yaml]",
		Short: "Validate an ADL file against the schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := parseAndValidate(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}
}

func newCompileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compile [adl.yaml]",
		Short: "Compile an ADL file and print the CompiledSpec as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseValidateCompile(args[0])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(spec)
		},
	}
}

func newRunCmd() *cobra.Command {
	var taskOverride string
	var rawOut string
	// --ndjson-stdout: emit raw wire NDJSON to stdout instead of the human-
	// formatted `[type] {...}` lines `printEvent` produces. The Kubernetes
	// backend passes this flag to the in-Pod agentctl so `kubectl logs`
	// streams raw wire events that the host can decode directly via
	// wire.Decode. Codex pass 4 of slice 4.3 caught the human-format vs
	// NDJSON mismatch that made every K8s run look like an error.
	var ndjsonStdout bool
	var resumeID string
	var noStalenessCheck bool
	var bindingPath string
	// Slice 7.1 (v0.7 Option-B pivot): repeatable `--input KEY=VALUE`.
	// External schedulers (Maestro / Airflow / Temporal) parameterize
	// an Agent's spec.task at the CLI boundary via these flags +
	// `${inputs.<key>}` interpolation. See inputs.go for the syntax.
	var inputFlags []string
	// Slice 7.2: `--output-file <path>` captures the agent's final
	// assistant message (or, with spec.outputSchema, the validated JSON
	// extracted from it) so schedulers can pass results between steps.
	// See output.go for capture/validate/write semantics.
	var outputFile string
	// Slice 7.4: `--input-file <path>` merges a JSON object of inputs
	// (multi-input DAG handoff); `--skip-if-output-exists` makes a re-run
	// of an already-completed step a fast no-op (scheduler idempotency).
	// See inputs.go / output.go for semantics.
	var inputFile string
	var skipIfOutputExists bool
	// Slice 7.5: `--workspace <dir>` injects a stdio MCP server (agentctl
	// itself) exposing durable memory tools (remember/recall/note_append/
	// list_outputs) backed by a SQLite store in <dir>. Harness-agnostic —
	// reaches Pi and opencode via spec.mcpServers. See workspace_mcp.go.
	var workspaceDir string
	c := &cobra.Command{
		Use:   "run [adl.yaml]",
		Short: "Run an ADL agent locally",
		Args:  cobra.ExactArgs(1),
		// Named return so the OTel span defer below can observe the
		// actual error being returned, regardless of which `:=` in the
		// body produced it. Codex pass 5 of slice 5.1 caught spans
		// closing as Ok status when early-return errors used a fresh
		// local err variable.
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			spec, err := parseValidateCompile(args[0])
			if err != nil {
				return err
			}
			if taskOverride != "" {
				spec.Task = taskOverride
			}

			// Slice 7.2: pre-compile the output schema BEFORE the
			// backend runs so a malformed `spec.outputSchema` fails
			// fast — no tokens spent, no tools invoked, the operator
			// sees the typo immediately. Codex pass 3 caught the
			// original ordering that left compilation until after
			// the run completed.
			compiledOutputSchema, err := prepareOutputCapture(outputFile, spec.OutputSchema)
			if err != nil {
				return err
			}

			// Slice 7.4: `--skip-if-output-exists` makes a re-run of an
			// already-completed step a fast no-op, so a scheduler can
			// retry a partially-failed DAG without re-spending tokens on
			// steps that already produced output. Checked HERE — after
			// the cheap/deterministic output-config validation above, but
			// BEFORE reading input files, interpolating, or initializing
			// tracing/backends — so the skip path stays cheap and free of
			// side effects.
			if skipIfOutputExists {
				if outputFile == "" {
					return fmt.Errorf("--skip-if-output-exists requires --output-file")
				}
				exists, eerr := outputAlreadyExists(outputFile)
				if eerr != nil {
					return eerr
				}
				if exists {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"[skip] --output-file %q already exists; skipping run\n", outputFile)
					return nil
				}
			}

			// Slice 7.1: parse `--input KEY=VALUE` (7.4 adds the
			// `KEY=@<path>` file form). Slice 7.4: `--input-file` merges a
			// JSON object of inputs on top, rejecting any key supplied via
			// BOTH channels. Interpolation (below) runs AFTER --task so the
			// override can itself reference `${inputs.<key>}`.
			inputs, err := parseInputFlags(inputFlags)
			if err != nil {
				return err
			}
			// Distinguish "--input-file not given" from "--input-file given
			// an empty value" via Changed(). A wrapper that expands an unset
			// path (`--input-file "$PARAMS_JSON"` with PARAMS_JSON unset)
			// passes an empty string; treating that as "no file" would
			// silently skip the file AND the interpolation-intent path, so a
			// `${inputs.foo}` task would reach the model literally. Error
			// instead. Codex pass 4 of slice 7.4.
			if cmd.Flags().Changed("input-file") && inputFile == "" {
				return fmt.Errorf("--input-file was given an empty path")
			}
			if inputFile != "" {
				if err := mergeInputFile(inputs, inputFile); err != nil {
					return err
				}
			}

			// Interpolate when the caller signalled input INTENT via a flag
			// (`--input` and/or `--input-file`) — NOT when the resulting
			// map happens to be non-empty.
			//
			// Codex pass 3 of slice 7.1 caught the K8s recursion bug: when
			// KubernetesBackend.Resolve marshals the host-resolved spec into
			// a Secret, the in-Pod `agentctl run` reads back the
			// ALREADY-RENDERED task. If the host operator passed
			// `--input snippet='${inputs.foo}'` (a documented opaque value),
			// the rendered task contains literal `${inputs.foo}` text — and
			// the in-Pod child, with no input flags forwarded, must NOT
			// re-interpolate (it would fail with `unknown inputs: foo`).
			// Gating on caller intent instead of `Contains(spec.Task,
			// "${inputs.")` (content sniff) preserves the opaque-value
			// contract.
			//
			// Codex pass 1 of slice 7.4: gate on the FLAGS, not
			// `len(inputs) > 0`. An explicit `--input-file {}` (empty
			// object) yields an empty map but is still input intent — a
			// `${inputs.foo}` reference should then fail loudly for the
			// missing key rather than being sent to the model literally.
			// The in-Pod child still passes neither flag, so it stays on
			// the no-interpolation path. (See shouldInterpolateInputs.)
			if shouldInterpolateInputs(inputFlags, inputFile) {
				resolved, unused, ierr := interpolateInputs(spec.Task, inputs)
				if ierr != nil {
					return ierr
				}
				// Codex pass 2 of slice 7.1: re-enforce the
				// schema's `task` minLength=1 invariant. The
				// schema validation ran BEFORE interpolation, so a
				// template like `"${inputs.prompt}"` with
				// `--input prompt=` would render to empty and
				// reach the backend — bypassing the contract.
				// Trim before checking so a whitespace-only
				// resolution (`task: " ${inputs.x} "` with
				// `--input x=`) also fails fast rather than
				// pretending to be a task.
				if strings.TrimSpace(resolved) == "" {
					return fmt.Errorf(
						"spec.task is empty after --input interpolation; " +
							"check that --input KEY values aren't empty for keys referenced by spec.task")
				}
				spec.Task = resolved
				if len(unused) > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"[warning] --input keys not referenced in spec.task: %s\n",
						strings.Join(unused, ", "))
				}
			}

			// Initialize the OTel tracer provider. Two-condition opt-in:
			// the spec must declare `spec.observability.tracing: true`
			// AND the operator must point OTEL_EXPORTER_OTLP_ENDPOINT
			// at a collector. Either one missing → no-op provider. Slice
			// 5.1 of v0.5.0; spec opt-in lets a single spec deliberately
			// disable tracing even on a host that has the env exporter
			// configured (e.g. for highly sensitive runs).
			otelCtx := cmd.Context()
			tracingRequested := spec.Observability != nil && spec.Observability.Tracing
			var otelShutdown func(context.Context) error = func(context.Context) error { return nil }
			if tracingRequested {
				otelShutdown, err = observability.InitTracerProvider(otelCtx, version)
				if err != nil {
					return fmt.Errorf("init OTel: %w", err)
				}
				// Slice 5.2: when agentctl runs inside a Kubernetes Pod
				// launched by an outer agentctl, KubernetesBackend.Submit
				// injects TRACEPARENT into the Pod container env. Extract
				// it here — *after* InitTracerProvider installs the W3C
				// propagator — so StartRootSpan opens its span as a child
				// of the host `agentctl.run` span. No-op when TRACEPARENT
				// is unset (local CLI invocation).
				otelCtx = observability.ExtractTraceContextFromEnv(otelCtx)
			}
			// flushOTel drains buffered spans and shuts the provider down.
			// Called explicitly before any os.Exit() below (os.Exit
			// bypasses deferred funcs); also deferred as a backstop so a
			// normal return cleans up too.
			otelFlushed := false
			flushOTel := func() {
				if otelFlushed {
					return
				}
				otelFlushed = true
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = otelShutdown(sctx)
			}
			defer flushOTel()

			// Open the agentctl.run root span IMMEDIATELY after spec
			// parse so every later failure path (--resume mismatch,
			// invalid --binding, backend setup, staleness check) ends
			// up in the trace. Binding name + backend type + session id
			// get added via SetLateAttributes once known.
			// Codex pass 4 of slice 5.1 caught the early-return blind
			// spot.
			ctx, span := observability.StartRootSpan(otelCtx, observability.RunAttributes{
				AgentName:     spec.Metadata.Name,
				ModelProvider: spec.Model.Provider,
				ModelName:     spec.Model.Name,
				RuntimeType:   spec.Runtime.Type,
			})
			spanEnded := false
			defer func() {
				if spanEnded {
					return
				}
				// Read the named return variable so spans capture errors
				// from any return path, including `return fmt.Errorf(...)`
				// that bypasses the outer `err`.
				if runErr != nil {
					span.SetStatus(codes.Error, runErr.Error())
					span.RecordError(runErr)
				}
				span.End()
			}()

			if resumeID != "" {
				// --resume works on both adapters as of slice 8.1: Pi via
				// SessionManager.continueRecent, opencode via its persisted session store.
				spec.SessionID = &resumeID
				observability.SetLateAttributes(span, observability.RunAttributes{SessionID: resumeID})
			}

			// v0.3.3b: --binding <path> loads a RuntimeBinding YAML and
			// passes it to LocalBackend.Resolve(), which then runs the
			// selector + capability matcher. Without --binding the run
			// flows through Resolve(spec, nil) — the v0.2.x default
			// behavior — exactly as in v0.3.3a.
			var binding *adl.RuntimeBinding
			if bindingPath != "" {
				binding, err = loadAndValidateBinding(bindingPath)
				if err != nil {
					return err
				}
				observability.SetLateAttributes(span, observability.RunAttributes{
					BindingName: binding.Metadata.Name,
					BackendType: binding.Spec.Target.Type,
				})
			} else {
				// No binding → LocalBackend by default.
				observability.SetLateAttributes(span, observability.RunAttributes{BackendType: "local"})
			}

			// Slice 7.5: --workspace <dir> gives the agent durable,
			// harness-agnostic memory by injecting a stdio MCP server
			// (agentctl itself, via the hidden __workspace-mcp subcommand)
			// into spec.mcpServers. The tools (workspace_remember / recall /
			// note_append / list_outputs) reach BOTH the Pi and opencode
			// adapters through the normal mcpServers wiring. The injection
			// must happen here — after compile, before be.Resolve — so the
			// resolved spec the backend ships already carries it.
			// Distinguish "--workspace not given" from "--workspace given an
			// empty value" (a wrapper expanding an unset var as
			// `--workspace ""`). Treating the latter as omitted would
			// silently drop the requested durable memory. Codex pass 5 of
			// slice 7.5 (same class as slice 7.4's --input-file guard).
			if cmd.Flags().Changed("workspace") && workspaceDir == "" {
				return fmt.Errorf("--workspace was given an empty path")
			}
			if workspaceDir != "" {
				targetType := "local"
				if binding != nil {
					targetType = binding.Spec.Target.Type
				}
				if targetType == "kubernetes" {
					return fmt.Errorf(
						"--workspace is not supported with a kubernetes target: the " +
							"workspace SQLite store is host-local and a Pod can't reach it. " +
							"Use a shared volume + the file-handoff flags (--output-file / " +
							"--input KEY=@<path>) for cross-step state under Kubernetes")
				}
				// Pi-adapter caveat (codex pass 1 of slice 7.5): the Pi
				// adapter switches to `noTools: "builtin"` whenever ANY MCP
				// server is present, which suppresses the built-in
				// read/bash/edit/write tools — Pi's API can't keep declared
				// built-ins AND allow unknown MCP tool names at the same
				// time. So injecting the workspace server would silently
				// drop a Pi agent's declared built-in tools. Warn rather
				// than fail (the agent may not need those built-ins). The
				// opencode adapter grants each declared tool independently
				// and is unaffected.
				isPiAdapter := spec.Runtime.Type != "local-opencode" && spec.Runtime.Type != "local-codex"
				if isPiAdapter {
					builtins := declaredBuiltinTools(&spec)
					if len(builtins) > 0 {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"[warning] --workspace adds an MCP server, and the Pi adapter "+
								"disables built-in tools when any MCP server is present, so this "+
								"run will lose its declared built-in tool(s): %s. Use "+
								"runtime.type: local-opencode (unaffected), or drop --workspace "+
								"and hand off via files (--output-file / --input KEY=@<path>).\n",
							strings.Join(builtins, ", "))
					}
				}
				if err := injectWorkspaceMCPServer(&spec, workspaceDir); err != nil {
					return err
				}
			}

			// Pick the backend based on the active Binding's target.type.
			// Without a Binding, fall through to LocalBackend (preserves the
			// v0.2.x / v0.3.x default path). Slice 4.3 added the kubernetes
			// case — submission via client-go from the agent-runtime-base
			// image.
			var be backend.Backend
			switch {
			case binding != nil && binding.Spec.Target.Type == "kubernetes":
				kbe, kerr := backend.NewKubernetesBackend(backend.KubernetesConfig{})
				if kerr != nil {
					return fmt.Errorf("KubernetesBackend: %w", kerr)
				}
				be = kbe
			default:
				// Local target (binding=nil OR target.type=local). Determine
				// the runtime command. A binding can override the adapter
				// path via target.runtimeCommand — equivalent to setting
				// AGENT_CONTROLLER_RUNTIME at run time but scoped to this
				// binding. When the binding sets it, we honor it; otherwise
				// fall through to the cwd-relative lookup.
				var runtimeCmd []string
				if binding != nil && binding.Spec.Target.RuntimeCommand != "" {
					runtimeCmd = []string{"node", binding.Spec.Target.RuntimeCommand}
				} else {
					runtimeCmd, err = resolveRuntimeCommand(spec.Runtime.Type)
					if err != nil {
						return err
					}
				}

				// Refuse to launch if the chosen adapter's dist/ is older
				// than its src/. Closes Round-2 finding #4. Skipped when:
				//   - --no-staleness-check is set
				//   - AGENT_CONTROLLER_RUNTIME points at an explicit binary
				//   - the active binding supplies its own target.runtimeCommand
				//     (the cwd-relative adapter the staleness check validates
				//     is irrelevant in that case — the binding chose another
				//     binary; codex pass 1 of slice 3.3b caught this)
				//
				// Adapter directory is derived from spec.runtime.type so the
				// staleness check tracks whichever adapter is actually
				// launching. local/local-pi → runtime/; local-opencode →
				// runtime-opencode/; local-codex → runtime-codex/.
				usesBindingCommand := binding != nil && binding.Spec.Target.RuntimeCommand != ""
				if !noStalenessCheck && os.Getenv("AGENT_CONTROLLER_RUNTIME") == "" && !usesBindingCommand {
					wd, _ := os.Getwd()
					adapterDir := "runtime"
					if spec.Runtime.Type == "local-opencode" {
						adapterDir = "runtime-opencode"
					} else if spec.Runtime.Type == "local-codex" {
						adapterDir = "runtime-codex"
					} else if spec.Runtime.Type == "local-claude" {
						adapterDir = "runtime-claude"
					}
					srcDir := filepath.Join(wd, adapterDir, "src")
					distDir := filepath.Join(wd, adapterDir, "dist")
					if err := checkRuntimeStaleness(srcDir, distDir, adapterDir); err != nil {
						return err
					}
				}
				be = backend.NewLocalBackend(backend.LocalConfig{RuntimeCommand: runtimeCmd})
			}

			// Note: NOT using cmd.Context() — runtime subprocesses route
			// signals through backend.Stop so Ctrl-C doesn't kill the
			// child abruptly via exec.CommandContext.
			_ = ctx // span context flows into Resolve/Submit below via the existing `ctx` variable

			// Optional raw NDJSON sidecar — the E2E harness asserts on it.
			// Open BEFORE Resolve so capability-matcher warnings (slice
			// 3.3b) also land in the sidecar, not just on stdout. Codex
			// pass 1 of slice 3.3b caught the timing bug.
			var rawFile *os.File
			if rawOut != "" {
				rawFile, err = os.Create(rawOut)
				if err != nil {
					return fmt.Errorf("open raw output: %w", err)
				}
				defer rawFile.Close()
			}
			writeRaw := func(ev wire.Event) {
				if rawFile == nil {
					return
				}
				raw, _ := json.Marshal(ev)
				_, _ = rawFile.Write(append(raw, '\n'))
			}

			// v0.3.3b: pass the parsed binding (if any) to Resolve. The
			// matcher inside LocalBackend.Resolve checks the selector
			// runtimeType and the capability set, and either emits a
			// `warning` event per unmet requirement (warn-but-proceed)
			// or refuses to start when target.strict is true.
			run, warnings, err := be.Resolve(ctx, spec, binding)
			if err != nil {
				return err
			}
			// Helper picks between NDJSON (the wire format the Kubernetes
			// backend reads back via kubectl logs) and the human-formatted
			// printEvent output. Codex pass 4 of slice 4.3.
			writeStdout := func(ev wire.Event) {
				if ndjsonStdout {
					raw, _ := json.Marshal(ev)
					_, _ = cmd.OutOrStdout().Write(append(raw, '\n'))
					return
				}
				printEvent(cmd.OutOrStdout(), ev)
			}

			// Warnings from Resolve are prepended to the canonical wire
			// stream so they appear before any session.started event.
			// Also written to the raw sidecar so --raw-out consumers see
			// every wire event the operator saw on stdout.
			for _, w := range warnings {
				writeStdout(w)
				writeRaw(w)
			}

			h, err := be.Submit(ctx, run)
			if err != nil {
				return err
			}

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				if _, ok := <-sigCh; ok {
					_ = be.Stop(h)
				}
			}()
			defer signal.Stop(sigCh)

			exitCode := 0
			// When --resume is in play the persistent session id was
			// already set on the span before the loop; don't let the
			// first wire event overwrite it with the runtime's
			// ephemeral session id. Codex pass 10 of slice 5.1.
			capturedSessionID := resumeID != ""
			// Slice 7.2: accumulate the LAST assistant message text so
			// `--output-file` can write it after a clean run. We track
			// the latest one rather than concatenating because:
			//   (a) single-turn `agentctl run` (the dominant case)
			//       produces exactly one assistant reply, and
			//   (b) multi-turn runs are rare for `run` (chat uses
			//       `agentctl chat`); when they do happen the FINAL
			//       reply is the scheduler-consumable result, not the
			//       full transcript.
			var lastAssistantMessage string
			for ev := range be.Events(h) {
				writeStdout(ev)
				writeRaw(ev)
				// Capture the runtime-generated session id on the root
				// span the first time we see it. Without this, fresh runs
				// (no --resume) have no session.id attribute, breaking
				// trace/session correlation in the collector. Codex pass
				// 9 of slice 5.1 caught the gap.
				if !capturedSessionID && ev.SessionID != "" {
					observability.SetLateAttributes(span, observability.RunAttributes{SessionID: ev.SessionID})
					capturedSessionID = true
				}
				if ev.Type == wire.EventMessage {
					// Slice 7.2: capture assistant text for --output-file.
					// We only look at role=="assistant" — user/system
					// messages echoed into the wire stream are not the
					// scheduler-consumable result. tool.result events
					// flow through EventToolResult, not EventMessage.
					var m struct {
						Role string `json:"role"`
						Text string `json:"text"`
					}
					if err := json.Unmarshal(ev.Data, &m); err == nil && m.Role == "assistant" {
						lastAssistantMessage = m.Text
					}
				}
				if ev.Type == wire.EventError {
					exitCode = 1
				}
				if ev.Type == wire.EventSessionEnded {
					var ended struct{ Reason string }
					_ = json.Unmarshal(ev.Data, &ended)
					switch ended.Reason {
					case "cancelled":
						exitCode = 130
					case "error":
						exitCode = 1
					}
				}
			}
			// Slice 7.2: write --output-file only on a clean exit. A
			// run that ended in error or was cancelled leaves the path
			// untouched, so a scheduler reading the file knows it
			// contains the result of the LAST successful run (or
			// nothing if no run has succeeded yet). Doing this before
			// span.End so a finalize failure still flows through the
			// span status branch below.
			if exitCode == 0 && outputFile != "" {
				if err := finalizeOutput(outputFile, lastAssistantMessage, compiledOutputSchema); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", err.Error())
					exitCode = 1
				}
			}
			// Span status: Ok for a clean run, Error for any non-zero
			// exit (including reason=cancelled, which sets exit 130).
			// Treating cancellation as Error in the trace is what
			// observability platforms expect — "interrupted, didn't
			// complete" is a non-success outcome even if the user
			// chose it.
			if exitCode != 0 {
				span.SetStatus(codes.Error, fmt.Sprintf("agentctl run exit %d", exitCode))
			} else {
				span.SetStatus(codes.Ok, "")
			}
			span.End()
			spanEnded = true
			if exitCode != 0 {
				// Flush OTel before os.Exit so the closing spans actually
				// reach the collector (os.Exit bypasses deferred funcs).
				flushOTel()
				os.Exit(exitCode)
			}
			return nil
		},
	}
	c.Flags().StringVar(&taskOverride, "task", "", "override spec.task from the YAML")
	c.Flags().StringVar(&rawOut, "raw-out", "", "append raw NDJSON events to this file")
	c.Flags().BoolVar(&ndjsonStdout, "ndjson-stdout", false,
		"emit raw wire NDJSON to stdout (used by the KubernetesBackend so kubectl logs returns parseable wire events)")
	c.Flags().StringVar(&resumeID, "resume", "", "resume a named persistent session by id")
	c.Flags().StringVar(&bindingPath, "binding", "",
		"path to a RuntimeBinding YAML; activates the capability matcher")
	c.Flags().BoolVar(&noStalenessCheck, "no-staleness-check", false,
		"skip the runtime/dist freshness check; useful only for hot-reload workflows")
	c.Flags().StringArrayVar(&inputFlags, "input", nil,
		"set an input value as KEY=VALUE (or KEY=@<path> to read the value from a file, max 1 MiB); "+
			"referenced from spec.task as ${inputs.KEY}; repeatable")
	c.Flags().StringVar(&inputFile, "input-file", "",
		"merge inputs from a JSON object file (scalar values); a key set via both "+
			"--input and --input-file is an error")
	c.Flags().StringVar(&outputFile, "output-file", "",
		"write the agent's final assistant message to <path> on successful exit; "+
			"with spec.outputSchema set, the message is parsed as JSON, validated, "+
			"and the validated JSON is written instead")
	c.Flags().BoolVar(&skipIfOutputExists, "skip-if-output-exists", false,
		"if --output-file already exists, skip the run and exit 0 (scheduler idempotency)")
	c.Flags().StringVar(&workspaceDir, "workspace", "",
		"directory for durable agent memory; injects MCP tools "+
			"(workspace_remember/recall/note_append/list_outputs) backed by SQLite in <dir>. "+
			"Not supported with a kubernetes target")
	return c
}

// agentctlSessionsDir returns the directory where agentctl stores named
// session subdirectories. Mirrors the path the runtime constructs:
// <HOME>/.pi/agent/sessions/agentctl/
func agentctlSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".pi", "agent", "sessions", "agentctl")
}

func newSessionsCmd() *cobra.Command {
	sessions := &cobra.Command{
		Use:   "sessions",
		Short: "Manage agent sessions",
	}
	sessions.AddCommand(newSessionsListCmd(), newSessionsSweepCmd())
	return sessions
}

func newSessionsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List persisted sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := agentctlSessionsDir()
			entries, err := os.ReadDir(dir)
			if os.IsNotExist(err) {
				fmt.Fprintln(cmd.OutOrStdout(), "no sessions yet")
				return nil
			}
			if err != nil {
				return fmt.Errorf("read sessions dir: %w", err)
			}

			type sessionEntry struct {
				id       string
				modified string
				size     int64
			}
			var sessions []sessionEntry

			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				subDir := filepath.Join(dir, e.Name())
				// Find the most-recently-modified .jsonl file in this subdir.
				subEntries, err := os.ReadDir(subDir)
				if err != nil {
					continue
				}
				var latestMod string
				var totalSize int64
				for _, f := range subEntries {
					if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
						continue
					}
					fi, err := f.Info()
					if err != nil {
						continue
					}
					totalSize += fi.Size()
					ts := fi.ModTime().UTC().Format("2006-01-02T15:04:05Z")
					if ts > latestMod {
						latestMod = ts
					}
				}
				if latestMod == "" {
					continue // empty subdir — skip
				}
				sessions = append(sessions, sessionEntry{
					id:       e.Name(),
					modified: latestMod,
					size:     totalSize,
				})
			}

			if len(sessions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no sessions yet")
				return nil
			}

			// Sort descending by modified time so newest is first.
			sort.Slice(sessions, func(i, j int) bool {
				return sessions[i].modified > sessions[j].modified
			})

			fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-20s  %s\n", "ID", "LAST MODIFIED", "SIZE")
			for _, s := range sessions {
				fmt.Fprintf(cmd.OutOrStdout(), "%-40s  %-20s  %d bytes\n", s.id, s.modified, s.size)
			}
			return nil
		},
	}
}

func parseAndValidate(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	doc, err := adl.Parse(data)
	if err != nil {
		return nil, err
	}
	v, err := adl.NewValidator()
	if err != nil {
		return nil, err
	}
	if err := v.Validate(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// loadAndValidateBinding reads a RuntimeBinding YAML from disk, runs it
// through the kind-dispatching schema validator, and parses the typed
// shape. Returns an error if the file isn't a RuntimeBinding (e.g. a user
// pointed `--binding` at an Agent file) so the CLI fails before launching
// any subprocess. Added in v0.3.3b alongside the --binding flag.
func loadAndValidateBinding(path string) (*adl.RuntimeBinding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read binding %s: %w", path, err)
	}
	doc, err := adl.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse binding %s: %w", path, err)
	}
	v, err := adl.NewValidator()
	if err != nil {
		return nil, err
	}
	if err := v.Validate(doc); err != nil {
		return nil, err
	}
	if kind, _ := doc["kind"].(string); kind != "RuntimeBinding" {
		return nil, fmt.Errorf(
			"--binding requires a kind: RuntimeBinding document (got kind: %q from %s)",
			kind, path,
		)
	}
	binding, err := adl.ParseBinding(data)
	if err != nil {
		return nil, err
	}
	return binding, nil
}

func parseValidateCompile(path string) (adl.CompiledSpec, error) {
	doc, err := parseAndValidate(path)
	if err != nil {
		return adl.CompiledSpec{}, err
	}
	// agentctl compile/run only operate on kind: Agent. The validator
	// added kind-dispatch in v0.3.2 and accepts RuntimeBinding too, so we
	// must reject non-Agent documents here — otherwise a Binding file
	// would pass validation and produce a near-empty CompiledSpec. Codex
	// pass 1 of slice 3.2 caught this.
	if kind, _ := doc["kind"].(string); kind != "Agent" {
		return adl.CompiledSpec{}, fmt.Errorf(
			"agentctl compile/run requires kind: Agent (got kind: %q from %s). "+
				"To validate a RuntimeBinding use `agentctl validate <file>` instead",
			kind, path,
		)
	}
	root, err := projectRoot()
	if err != nil {
		return adl.CompiledSpec{}, err
	}
	idx, err := registry.Scan(root)
	if err != nil {
		return adl.CompiledSpec{}, fmt.Errorf("scan registry: %w", err)
	}
	return adl.Compile(doc, idx)
}

// projectRoot is the directory that contains tools/ and extensions/.
// MVP convention: it's the directory the CLI is invoked from. Post-MVP
// the registry will also resolve from ~/.agent-controller/registry/.
func projectRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd, nil
}

// resolveRuntimeCommand picks the argv to launch the agent runtime
// adapter, dispatching on spec.runtime.type.
//
// Routing:
//   - "local" (legacy v0.1.x alias) or "local-pi" → runtime/dist/index.js (Pi adapter)
//   - "local-opencode"                            → runtime-opencode/dist/index.js (opencode adapter)
//   - "local-codex"                               → runtime-codex/dist/index.js (codex adapter)
//   - anything else                               → returned as a structured error citing the field
//
// $AGENT_CONTROLLER_RUNTIME, when set, overrides the dispatch entirely.
// This is the user's escape hatch for running a custom adapter binary,
// and is honored regardless of spec.runtime.type. Useful for development
// and for the dist-staleness bypass path.
func resolveRuntimeCommand(runtimeType string) ([]string, error) {
	if env := os.Getenv("AGENT_CONTROLLER_RUNTIME"); env != "" {
		return []string{"node", env}, nil
	}
	wd, _ := os.Getwd()

	// Each branch resolves to a built dist/index.js under the chosen
	// adapter directory. The staleness check (slice 0.3) is dispatched
	// from the caller against the same directory pair, so we keep the
	// lookups symmetric.
	switch runtimeType {
	case "", "local", "local-pi":
		candidate := filepath.Join(wd, "runtime", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return []string{"node", candidate}, nil
		}
		return nil, fmt.Errorf(
			"Pi runtime not found at %s — set AGENT_CONTROLLER_RUNTIME or run `npm --prefix runtime run build`",
			candidate,
		)
	case "local-opencode":
		candidate := filepath.Join(wd, "runtime-opencode", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return []string{"node", candidate}, nil
		}
		return nil, fmt.Errorf(
			"opencode runtime not found at %s — run `npm --prefix runtime-opencode run build`",
			candidate,
		)
	case "local-codex":
		candidate := filepath.Join(wd, "runtime-codex", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return []string{"node", candidate}, nil
		}
		return nil, fmt.Errorf(
			"codex runtime not found at %s — run `npm --prefix runtime-codex run build`",
			candidate,
		)
	case "local-claude":
		candidate := filepath.Join(wd, "runtime-claude", "dist", "index.js")
		if _, err := os.Stat(candidate); err == nil {
			return []string{"node", candidate}, nil
		}
		return nil, fmt.Errorf(
			"claude runtime not found at %s — run `npm --prefix runtime-claude run build`",
			candidate,
		)
	default:
		return nil, fmt.Errorf(
			"unsupported spec.runtime.type %q — expected one of: local | local-pi | local-opencode | local-codex | local-claude",
			runtimeType,
		)
	}
}

func printEvent(w io.Writer, ev wire.Event) {
	fmt.Fprintf(w, "[%s] %s\n", ev.Type, string(ev.Data))
}
