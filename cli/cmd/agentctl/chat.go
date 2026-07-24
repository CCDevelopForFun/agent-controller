package main

// Slice 6.3 — `agentctl chat <spec.yaml>` REPL.
//
// Multi-turn interactive surface for long-running agents. Each line
// the user types is a prompt; the adapter runs a single turn against
// it and the loop repeats. Session id is stable across turns so Pi's
// own session loader (file-based under ~/.pi/agent/sessions/agentctl/)
// keeps the conversation context for the underlying model.
//
// Layering:
//
//   chat
//   ├─ open SessionStore (SQLite by default; --in-memory overrides)
//   ├─ Create or Get session (--resume <id> reuses an existing one)
//   ├─ pick backend (LocalBackend; --binding for K8s comes later)
//   └─ LOOP per user input:
//      ├─ build a per-turn spec (Task = input, SessionID = sess.ID)
//      ├─ backend.Submit, drain events, print to stdout
//      └─ Update sess.LastActiveAt in the SessionStore
//
// MVP scope (deferred to later slices):
//   - opencode `--resume` isn't supported (per slice 3.4 rejection);
//     chat refuses to launch against `runtime.type: local-opencode`
//     with a clear message. Slice 6.x or 7.x re-enables it once
//     opencode's resume story lands.
//   - K8s `--binding` chat (each turn spawning a fresh Pod) is
//     deferred — slice 6.6 acceptance will revisit.
//   - Per-turn span linkage under a single chat-root OTel span lands
//     in slice 6.5; today the chat command holds a single root span
//     that wraps the whole REPL (every turn nests adapter spans under
//     it via the slice 5.2 TRACEPARENT chain).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/backend"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/observability"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/sessions"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

func newChatCmd() *cobra.Command {
	var resumeID string
	var inMemory bool
	var noStalenessCheck bool

	c := &cobra.Command{
		Use:   "chat [adl.yaml]",
		Short: "Open an interactive multi-turn chat with an agent",
		Long: `Chat with an agent across many turns. Each line you type is sent as ` +
			`a prompt; the agent runs one turn and prints its response. Type ` +
			`/exit or send EOF (Ctrl-D) to end the session.

` +
			`The session id is stable for the duration of the chat. Pi's own ` +
			`session loader keeps the conversation context across turns; pass ` +
			`--resume <id> to continue a prior chat.`,
		Args: cobra.ExactArgs(1),
		// Named return so the deferred session-cleanup below can
		// observe whether RunE itself is returning an error and mark
		// the session record StatusFailed instead of leaking an
		// "active" record for a chat that never actually ran.
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			specPath := args[0]
			ctx := cmd.Context()

			// 1) Parse + validate + compile the spec. This is REQUIRED
			//    even on --resume because (a) we still need a valid
			//    spec file to look up the session (cobra requires
			//    args[0]), and (b) we want the YAML structure
			//    validated against the current schema. On resume we
			//    later REPLACE this with the stored Session.Spec so
			//    the conversation continues under the spec the model
			//    saw originally — see openOrResumeChatSession +
			//    codex pass 5 of slice 6.3.
			//
			//    Note: the local-opencode rejection moved to AFTER
			//    session resume (codex pass 6) — if the user resumes
			//    a Pi session and their YAML drifted to opencode,
			//    the EFFECTIVE spec (stored Session.Spec) is what
			//    determines runtime, not the drifted YAML.
			spec, err := parseValidateCompile(specPath)
			if err != nil {
				return err
			}

			// 2) Open the SessionStore. SQLite by default for durability;
			//    --in-memory falls back to MemoryStore for ephemeral
			//    chats (useful in tests + when the operator wants no
			//    on-disk trace).
			store, err := openChatStore(inMemory)
			if err != nil {
				return err
			}
			defer store.Close()

			// 3) Create or resume the session. --resume <id> loads an
			//    existing Session and refuses to launch if the stored
			//    spec is for a different agent (cross-agent session
			//    reuse would put the model in a confused state). New
			//    sessions get a fresh id and the current spec snapshot.
			sess, prevSnap, err := openOrResumeChatSession(ctx, store, spec, resumeID)
			if err != nil {
				// Slice 6.4: --resume against an expired session
				// emits session.expired on the wire so a scripted
				// consumer can detect TTL-induced bail without
				// parsing error strings. The session record itself
				// stays StatusExpired in the store.
				if errors.Is(err, errSessionExpired) {
					payload, _ := json.Marshal(struct {
						SessionID    string    `json:"sessionId"`
						LastActiveAt time.Time `json:"lastActiveAt"`
					}{sess.ID, prevSnap.LastActiveAt})
					printEvent(cmd.OutOrStdout(), wire.Event{
						V:         wire.ProtocolVersion,
						Type:      wire.EventSessionExpired,
						Ts:        time.Now().UTC(),
						SessionID: sess.ID,
						Data:      payload,
					})
					return fmt.Errorf(
						"chat --resume %s: session has expired (last active %s); "+
							"start a new chat or sweep with a larger TTL",
						sess.ID, prevSnap.LastActiveAt.Format(time.RFC3339))
				}
				return err
			}
			// Codex pass 5 of slice 6.3: on --resume, USE THE STORED
			// SPEC for the rest of the chat. The model has been
			// having a conversation under the original spec (model,
			// persona, tools, runtime); applying drift from the
			// current YAML mid-conversation would change tools or
			// persona under the model without it knowing — exactly
			// the "confused state" the cross-agent check guards
			// against, just for the same agent name. We don't try
			// to detect or merge drift; the stored snapshot wins.
			// New sessions (resumeID == "") use the freshly-parsed
			// spec; nothing to override.
			if resumeID != "" {
				if !specsEquivalent(spec, sess.Spec) {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"[resume] spec on disk differs from the session's stored snapshot; "+
							"using the stored spec for this chat (session %s).\n", sess.ID)
				}
				spec = sess.Spec
				// Slice 6.4: emit session.resumed with the prior
				// status + the prior LastActiveAt (captured BEFORE
				// the touch). Observability dashboards use these
				// to compute time-since-last-activity and to
				// distinguish a resume of an ended-but-restarted
				// session from a paused-and-continued one.
				resumedPayload, _ := json.Marshal(struct {
					SessionID            string    `json:"sessionId"`
					AgentName            string    `json:"agentName"`
					CreatedAt            time.Time `json:"createdAt"`
					PreviousLastActiveAt time.Time `json:"previousLastActiveAt"`
					PreviousStatus       string    `json:"previousStatus"`
				}{
					SessionID:            sess.ID,
					AgentName:            sess.AgentName,
					CreatedAt:            sess.CreatedAt,
					PreviousLastActiveAt: prevSnap.LastActiveAt,
					PreviousStatus:       string(prevSnap.Status),
				})
				printEvent(cmd.OutOrStdout(), wire.Event{
					V:         wire.ProtocolVersion,
					Type:      wire.EventSessionResumed,
					Ts:        time.Now().UTC(),
					SessionID: sess.ID,
					Data:      resumedPayload,
				})
			}
			// Codex pass 3 of slice 6.3: defer a cleanup that marks
			// the session StatusFailed if RunE returns an error
			// BEFORE the normal-exit block sets it to StatusEnded.
			// Without this, any failure between session creation and
			// the REPL loop's clean exit (OTel init, runtime
			// resolution, staleness check, stdin scanner error mid-
			// loop) leaves a ghost "active" record in `sessions ls`
			// even though chat never actually ran or already aborted.
			// The flag is flipped at the end of RunE so the deferred
			// fn knows the normal-exit path already wrote StatusEnded.
			sessionMarkedTerminal := false
			defer func() {
				if sessionMarkedTerminal {
					return
				}
				if runErr == nil {
					return // unreachable in practice — normal exit sets the flag
				}
				// Use a fresh context — the original may already be
				// cancelled (e.g. SIGTERM teardown). 2s budget is
				// generous for a single SQLite UPDATE.
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = store.Update(cleanupCtx, sessions.Session{
					ID:           sess.ID,
					Status:       sessions.StatusFailed,
					LastActiveAt: time.Now().UTC(),
					AdapterState: sess.AdapterState,
				})
			}()

			// At this point sess.ID is the stable id we'll pass into the
			// adapter as spec.SessionID for every turn, AND `spec` is
			// the effective spec (the stored one on resume, the
			// freshly-parsed one on new sessions). Both adapters run chat as of slice 8.1
			// (opencode resume is wired via its persisted session store).

			// 4) Init OTel for the whole REPL (one root span covering
			//    all turns). The host root span's TRACEPARENT propagates
			//    to the adapter subprocess via slice 5.2's
			//    LocalBackend env injection, so adapter spans nest
			//    under it automatically. Slice 6.5 will refactor to
			//    per-turn child spans under this root.
			otelCtx := ctx
			tracingRequested := spec.Observability != nil && spec.Observability.Tracing
			otelShutdown := func(context.Context) error { return nil }
			if tracingRequested {
				otelShutdown, err = observability.InitTracerProvider(otelCtx, version)
				if err != nil {
					return fmt.Errorf("init OTel: %w", err)
				}
				otelCtx = observability.ExtractTraceContextFromEnv(otelCtx)
			}
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = otelShutdown(sctx)
			}()

			spanCtx, span := observability.StartRootSpan(otelCtx, observability.RunAttributes{
				AgentName:     spec.Metadata.Name,
				ModelProvider: spec.Model.Provider,
				ModelName:     spec.Model.Name,
				RuntimeType:   spec.Runtime.Type,
				SessionID:     sess.ID,
			})
			// Codex pass 4 of slice 6.3: observe the named-return
			// runErr so any early failure (runtime resolution,
			// staleness check, prompt read error) is recorded on
			// the root span as ERROR instead of leaking through as
			// UNSET — matches the run command's pattern (codex pass
			// 5 of slice 5.1 caught the same gap there).
			defer func() {
				if runErr != nil {
					span.SetStatus(codes.Error, runErr.Error())
					span.RecordError(runErr)
				}
				span.End()
			}()
			ctx = spanCtx

			// 5) Pick the backend. K8s --binding for chat is deferred
			//    (every turn would spawn a Pod, which is wasteful);
			//    chat MVP runs against LocalBackend only.
			runtimeCmd, err := resolveRuntimeCommand(spec.Runtime.Type)
			if err != nil {
				return err
			}
			if !noStalenessCheck && os.Getenv("AGENT_CONTROLLER_RUNTIME") == "" {
				wd, _ := os.Getwd()
				adapterDir := "runtime"
				srcDir := wd + string(os.PathSeparator) + adapterDir + string(os.PathSeparator) + "src"
				distDir := wd + string(os.PathSeparator) + adapterDir + string(os.PathSeparator) + "dist"
				if err := checkRuntimeStaleness(srcDir, distDir, adapterDir); err != nil {
					return err
				}
			}
			be := backend.NewLocalBackend(backend.LocalConfig{RuntimeCommand: runtimeCmd})

			// 6) REPL loop. stdin scanner reads one line at a time;
			//    /exit and EOF both end the chat cleanly.
			fmt.Fprintf(cmd.OutOrStdout(), "agentctl chat — agent %q, session %s\n",
				spec.Metadata.Name, sess.ID)
			fmt.Fprintln(cmd.OutOrStdout(), "type /exit or send EOF (Ctrl-D) to end.")

			// Top-level signal handling.
			//
			//   SIGTERM (always): a process manager wants the chat
			//     dead — end the REPL. Codex pass 1 of slice 6.3.
			//
			//   SIGINT (idle-only): Ctrl-C while the user is sitting
			//     at the prompt would otherwise take Go's default
			//     signal path and exit the process WITHOUT running
			//     the deferred session-store cleanup, leaving the
			//     session record `active` forever. Codex pass 4 of
			//     slice 6.3 caught this. While a turn is active,
			//     runChatTurn installs its OWN SIGINT handler that
			//     cancels the turn only; the per-turn handler wins
			//     the signal delivery (Go's signal.Notify is
			//     fan-out, so both channels would normally see the
			//     same Ctrl-C — but the per-turn channel's signal.Stop
			//     in defer means it's only listening while a turn
			//     runs). Outside a turn, only THIS handler sees the
			//     signal, so the path is unambiguous.
			// Codex pass 7 of slice 6.3: SINGLE signal channel, no
			// fan-out. The previous design used two channels (one
			// for idle, one inside runChatTurn) gated by an
			// `inTurn` flag. That race is unfixable with a flag
			// alone: the idle goroutine can take a signal then be
			// descheduled BEFORE checking the flag, and by the time
			// it wakes the turn has ended → flag=false → REPL
			// ends, even though the user pressed Ctrl-C MID-TURN.
			//
			// The single-channel design centralizes signal dispatch:
			// every SIGINT/SIGTERM flows through `sigCh`, the
			// dispatcher goroutine branches based on whether a turn
			// is active (tracked via `turnCancel`, a pointer set by
			// the REPL loop). When a turn is active, SIGINT is
			// converted into a "cancel turn" signal forwarded via
			// `turnCancelCh`; runChatTurn listens for it and stops
			// the backend + cancels the turn ctx. When idle,
			// SIGINT ends the REPL. SIGTERM always ends the REPL.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigCh)
			replCtx, cancelREPL := context.WithCancel(ctx)
			defer cancelREPL()

			// `turnCancelCh` carries the cancel signal from the
			// dispatcher to whichever runChatTurn is currently
			// active. Buffer of 1 so the dispatcher never blocks;
			// the REPL drains it before each turn to flush any
			// signal queued between turns.
			turnCancelCh := make(chan struct{}, 1)
			// `turnActive` is the dispatcher's view of REPL state.
			// Only the main REPL goroutine writes it; the
			// dispatcher only reads.
			var turnActive atomic.Bool
			// `lastTurnEndedAt` is the wall-clock nanosecond
			// timestamp set when the REPL clears turnActive. Codex
			// pass 8 of slice 6.3 caught the unfixable-with-flag
			// race: signal queued during turn, dispatched AFTER
			// turnActive flips false. Pure atomic state can never
			// tell the dispatcher whether a signal was "in flight"
			// from before the flag flipped — kernel signal delivery
			// is asynchronous. The grace window biases routing
			// toward the just-finished turn (where a stray cancel
			// is a benign no-op) over routing toward REPL-end
			// (which would end the chat against user intent). 50ms
			// covers normal signal delivery latency by ~50x while
			// still letting a deliberate Ctrl-C at the idle prompt
			// (user has had a moment to read the prompt) end the
			// REPL.
			var lastTurnEndedAt atomic.Int64
			const idleGraceNs int64 = 50 * 1000 * 1000 // 50ms

			go func() {
				for {
					select {
					case sig := <-sigCh:
						if sig == syscall.SIGTERM {
							fmt.Fprintln(cmd.ErrOrStderr(), "\n(received SIGTERM — ending chat)")
							cancelREPL()
							return
						}
						// SIGINT. Bias: when in doubt, treat as
						// turn-cancel (benign no-op if turn already
						// ended) rather than REPL-end (catastrophic
						// to user intent).
						if turnActive.Load() {
							select {
							case turnCancelCh <- struct{}{}:
							default:
							}
							continue
						}
						// turnActive is false now — but was a turn
						// running when this signal was QUEUED?
						// Check the grace window.
						if last := lastTurnEndedAt.Load(); last > 0 {
							sinceEnd := time.Now().UnixNano() - last
							if sinceEnd < idleGraceNs {
								// Recently ended turn — route to
								// turn channel (will be drained
								// before next turn if not consumed).
								select {
								case turnCancelCh <- struct{}{}:
								default:
								}
								continue
							}
						}
						fmt.Fprintln(cmd.ErrOrStderr(), "\n(Ctrl-C at idle prompt — ending chat)")
						cancelREPL()
						return
					case <-replCtx.Done():
						return
					}
				}
			}()

			// Read stdin in a goroutine and surface lines via a
			// channel. The main loop selects on (replCtx.Done,
			// inputCh, scanErrCh) so a SIGTERM that arrives while
			// the user is idle at the prompt actually terminates the
			// process — pre-fix, scanner.Scan() blocked on a
			// uncancellable read syscall and the SIGTERM-driven
			// replCtx cancellation went unnoticed until the user
			// pressed Enter. Codex pass 2 of slice 6.3 caught this.
			//
			// The scanner goroutine is left running on REPL exit
			// (no way to interrupt a stdin read without closing
			// stdin, which is too invasive for a wrapped
			// cmd.InOrStdin()). The process is exiting anyway —
			// the leaked goroutine dies with main().
			scanner := bufio.NewScanner(cmd.InOrStdin())
			// Default token size is 64 KiB which truncates long pasted
			// prompts mid-line. Bump to 1 MiB — matches the host wire
			// stdout buffer in cli/internal/backend/local.go.
			scanner.Buffer(make([]byte, 64*1024), 1<<20)
			inputCh := make(chan string)
			scanErrCh := make(chan error, 1)
			go func() {
				for scanner.Scan() {
					inputCh <- scanner.Text()
				}
				// Codex pass 4 of slice 6.3: do NOT close inputCh on
				// the error path. If we closed both channels (signal
				// error AND signal EOF), the main loop's select could
				// pick the closed inputCh first and treat a real
				// scanner error (e.g. a line exceeding the 1 MiB cap)
				// as a clean EOF. Closing inputCh ONLY on clean EOF
				// makes the two terminal signals mutually exclusive.
				if err := scanner.Err(); err != nil {
					scanErrCh <- err
					return
				}
				close(inputCh)
			}()

			// Slice 6.4: track which exit path the REPL takes so we
			// emit the right terminal lifecycle event:
			//   - "ended" — explicit /exit (user explicitly closed)
			//   - "paused" — EOF (Ctrl-D), SIGTERM, idle SIGINT
			//     (user stepped away; the session can be resumed)
			//
			// `ended` is more deliberate; `paused` says "come back
			// later". Resume-fitness logic in slice 6.6 will use
			// this to prefer paused over ended when picking a
			// session to suggest.
			exitReason := sessions.StatusPaused

			// Slice 6.5: 1-based per-turn counter. Stamped on each
			// chat.turn span so observability tools can navigate
			// "turn 1 → turn 2 → ..." without timestamp arithmetic.
			// Reset per chat invocation; --resume of the same
			// session starts fresh at 1 because the chat-root span
			// is also fresh.
			var turnIndex int64
		repl:
			for {
				if err := replCtx.Err(); err != nil {
					// SIGTERM (or upstream cancel) already fired —
					// exit without prompting. Default exitReason
					// (paused) applies.
					break
				}
				fmt.Fprint(cmd.OutOrStdout(), "> ")
				var rawInput string
				select {
				case <-replCtx.Done():
					// SIGTERM arrived while we were about to read.
					// Default exitReason (paused) applies.
					break repl
				case err := <-scanErrCh:
					return fmt.Errorf("read prompt: %w", err)
				case line, ok := <-inputCh:
					if !ok {
						fmt.Fprintln(cmd.OutOrStdout(), "(EOF — ending chat)")
						// EOF (Ctrl-D) — paused. Default applies.
						break repl
					}
					rawInput = line
				}
				// Codex pass 1 of slice 6.3: keep the RAW input as
				// the dispatched Task. Trimming would lose intentional
				// leading whitespace (code blocks with indentation,
				// poetry, etc.). Trim only for the empty-line + /exit
				// dispatch decision.
				trimmed := strings.TrimSpace(rawInput)
				if trimmed == "" {
					continue
				}
				if trimmed == "/exit" {
					fmt.Fprintln(cmd.OutOrStdout(), "(ending chat)")
					// Explicit /exit — ended.
					exitReason = sessions.StatusEnded
					break repl
				}

				// Run one turn. runChatTurn handles the per-turn
				// spawn / event-stream / store-update lifecycle. We
				// keep going on any single-turn error (e.g. a
				// transient provider failure) — chat is supposed to
				// be resilient; killing the REPL on one bad turn is
				// not the right UX.
				//
				// Signal flow (single-channel design, codex pass 7):
				//   1. Set turnActive = true so the dispatcher
				//      routes SIGINT to turnCancelCh instead of
				//      ending the REPL.
				//   2. Drain any stale signal from turnCancelCh
				//      before the turn starts — a Ctrl-C from the
				//      previous turn could in principle linger if
				//      the dispatcher wrote to the buffer just as
				//      runChatTurn was returning.
				//   3. Run the turn.
				//   4. Clear turnActive. After this, the dispatcher
				//      treats SIGINT as REPL-end. There's no race
				//      with in-flight signals because every signal
				//      that ever entered the system has been routed
				//      exactly once by the single dispatcher.
				// Codex slice 6.4 pass 3: pre-turn expiration check.
				// If `sessions sweep` already retired this row while
				// the user was sitting at the prompt, the post-turn
				// Update would only catch it AFTER the model
				// dispatch — wasting a turn (and tokens). Touching
				// the row via Update BEFORE runChatTurn lets the
				// Store's atomic terminal-Expired guard fail fast.
				// Best-effort: a sweep that races BETWEEN this
				// touch and runChatTurn still gets caught by the
				// post-turn Update below.
				if preUpdErr := store.Update(ctx, sessions.Session{
					ID:           sess.ID,
					Status:       sessions.StatusActive,
					LastActiveAt: time.Now().UTC(),
					AdapterState: sess.AdapterState,
				}); preUpdErr != nil {
					if errors.Is(preUpdErr, sessions.ErrSessionExpired) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"\n[chat] session %s was swept to StatusExpired before this turn started; ending chat.\n",
							sess.ID)
						expiredPayload, _ := json.Marshal(struct {
							SessionID string `json:"sessionId"`
						}{sess.ID})
						printEvent(cmd.OutOrStdout(), wire.Event{
							V:         wire.ProtocolVersion,
							Type:      wire.EventSessionExpired,
							Ts:        time.Now().UTC(),
							SessionID: sess.ID,
							Data:      expiredPayload,
						})
						sessionMarkedTerminal = true
						return nil
					}
					// Other errors aren't fatal — log and continue
					// into the turn dispatch.
					fmt.Fprintf(cmd.ErrOrStderr(), "[session-store pre-turn touch] %v\n", preUpdErr)
				}

				select {
				case <-turnCancelCh:
				default:
				}
				turnActive.Store(true)
				turnIndex++
				if err := runChatTurn(replCtx, cmd, be, &spec, sess.ID, rawInput, turnCancelCh, turnIndex); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "[turn error] %v\n", err)
				}
				// Order matters: record the end timestamp BEFORE
				// clearing turnActive. If we cleared first, the
				// dispatcher could see turnActive=false AND
				// lastTurnEndedAt=0 (stale) in the same instant,
				// missing the grace window. Recording the timestamp
				// first means the dispatcher either sees turnActive=true
				// (still in turn) or turnActive=false with a fresh
				// timestamp (inside grace).
				lastTurnEndedAt.Store(time.Now().UnixNano())
				turnActive.Store(false)
				// Always update LastActiveAt — even on turn error.
				// The session is still alive; the user can retry.
				//
				// Codex slice 6.4 pass 2: if `sessions sweep`
				// retired this row during the turn, the Store's
				// terminal-Expired guard returns ErrSessionExpired.
				// The session is dead under us — emit a
				// session.expired wire event and exit the REPL.
				// Other Update errors are non-fatal (the turn
				// already ran; an operator can see the metadata
				// staleness via `sessions ls`).
				if updErr := store.Update(ctx, sessions.Session{
					ID:           sess.ID,
					Status:       sessions.StatusActive,
					LastActiveAt: time.Now().UTC(),
					AdapterState: sess.AdapterState,
				}); updErr != nil {
					if errors.Is(updErr, sessions.ErrSessionExpired) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"\n[chat] session %s was swept to StatusExpired during this turn; ending chat.\n",
							sess.ID)
						expiredPayload, _ := json.Marshal(struct {
							SessionID string `json:"sessionId"`
						}{sess.ID})
						printEvent(cmd.OutOrStdout(), wire.Event{
							V:         wire.ProtocolVersion,
							Type:      wire.EventSessionExpired,
							Ts:        time.Now().UTC(),
							SessionID: sess.ID,
							Data:      expiredPayload,
						})
						sessionMarkedTerminal = true
						return nil
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "[session-store update] %v\n", updErr)
				}
			}

			// 7) Mark the session ended/paused in the store. Errors
			//    here are informational — the chat already happened;
			//    failing to update the metadata shouldn't make the
			//    command itself fail. Use a fresh context (replCtx
			//    may already be cancelled by SIGTERM) so the update
			//    lands even on a shutdown-driven exit.
			//    Slice 6.4: status reflects the exit path (ended on
			//    explicit /exit, paused on EOF / SIGTERM / idle
			//    SIGINT). The matching session.paused / session.ended
			//    wire event is emitted just below; the existing
			//    session.ended emit from the adapter is per-TURN,
			//    not per-chat — the chat-level lifecycle event is
			//    new in 6.4.
			endCtx, endCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer endCancel()
			endNow := time.Now().UTC()

			// Codex slice 6.4 pass 2: rely on the Store's atomic
			// terminal-Expired guard instead of a Get-then-Update
			// pattern. Update returns ErrSessionExpired iff the row
			// was swept while we were running — that branch emits
			// the session.expired wire event and skips the
			// paused/ended emission.
			updErr := store.Update(endCtx, sessions.Session{
				ID:           sess.ID,
				Status:       exitReason,
				LastActiveAt: endNow,
				AdapterState: sess.AdapterState,
			})
			switch {
			case updErr == nil:
				// Emit the matching wire event. printEvent renders
				// as "[session.paused] {...}" in human mode; raw
				// NDJSON in --ndjson-stdout (the chat command
				// doesn't yet expose this flag; slice 6.6
				// acceptance will).
				lifecycleType := wire.EventSessionPaused
				if exitReason == sessions.StatusEnded {
					lifecycleType = wire.EventSessionEnded
				}
				terminalPayload, _ := json.Marshal(struct {
					SessionID string `json:"sessionId"`
					Reason    string `json:"reason"`
				}{sess.ID, string(exitReason)})
				printEvent(cmd.OutOrStdout(), wire.Event{
					V:         wire.ProtocolVersion,
					Type:      lifecycleType,
					Ts:        endNow,
					SessionID: sess.ID,
					Data:      terminalPayload,
				})
			case errors.Is(updErr, sessions.ErrSessionExpired):
				// Sweep intervened. Emit session.expired (the
				// resume-side emit is in `chat --resume`; this
				// covers the running-chat case) and inform the
				// operator on stderr. Don't overwrite the chosen
				// terminal status — the row stays Expired.
				fmt.Fprintf(cmd.ErrOrStderr(),
					"[chat] session %s was swept to StatusExpired during this chat; not overwriting status on exit.\n",
					sess.ID)
				expiredPayload, _ := json.Marshal(struct {
					SessionID string `json:"sessionId"`
				}{sess.ID})
				printEvent(cmd.OutOrStdout(), wire.Event{
					V:         wire.ProtocolVersion,
					Type:      wire.EventSessionExpired,
					Ts:        endNow,
					SessionID: sess.ID,
					Data:      expiredPayload,
				})
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "[session-store update on exit] %v\n", updErr)
			}

			// Tell the deferred cleanup not to overwrite the chosen
			// terminal status with StatusFailed on the happy path.
			sessionMarkedTerminal = true
			return nil
		},
	}

	c.Flags().StringVar(&resumeID, "resume", "", "resume a prior chat by session id")
	c.Flags().BoolVar(&inMemory, "in-memory", false, "use ephemeral in-memory store instead of the default SQLite store")
	c.Flags().BoolVar(&noStalenessCheck, "no-staleness-check", false, "skip the dist/ vs src/ staleness check (matches `run`)")
	return c
}

// openChatStore opens the session store the chat command will use.
// SQLite-backed by default; --in-memory falls back to MemoryStore.
// The caller MUST defer Close.
func openChatStore(inMemory bool) (sessions.Store, error) {
	if inMemory {
		return sessions.NewMemoryStore(), nil
	}
	path, err := sessions.DefaultSQLiteStorePath()
	if err != nil {
		return nil, fmt.Errorf("resolve session-store path: %w", err)
	}
	store, err := sessions.NewSQLiteStore(path)
	if err != nil {
		return nil, fmt.Errorf("open session store at %s: %w", path, err)
	}
	return store, nil
}

// errSessionExpired surfaces a resume against a session the sweep
// marked StatusExpired. Distinct from a generic NotFound so the
// caller can emit a `session.expired` wire event before bailing.
// Slice 6.4.
var errSessionExpired = errors.New("session expired")

// resumeSnapshot captures the pre-touch state of a session that
// just got resumed. Empty Status means "new session, not a resume" —
// callers use that as the sentinel. Populated fields are used to
// emit the `session.resumed` wire event with the prior status +
// time-since-last-touch payload that observability dashboards
// rely on.
type resumeSnapshot struct {
	Status       sessions.SessionStatus
	LastActiveAt time.Time
}

// openOrResumeChatSession either creates a new Session for this chat
// or loads an existing one by id (--resume). Cross-agent reuse — a
// resumed session whose stored spec.metadata.name disagrees with the
// current spec — is REJECTED. The model has been seeing the prior
// agent's persona; swapping mid-stream would produce a confused state
// the user almost never wants.
//
// On resume the second return value carries the session's status +
// LastActiveAt as they were BEFORE the function bumped them to
// Active/now — callers use those to populate the `session.resumed`
// wire event. A zero-valued snapshot signals "new session".
//
// Returns errSessionExpired when --resume targets a session whose
// Status is StatusExpired. Slice 6.4.
func openOrResumeChatSession(
	ctx context.Context,
	store sessions.Store,
	spec adl.CompiledSpec,
	resumeID string,
) (sessions.Session, resumeSnapshot, error) {
	now := time.Now().UTC()

	if resumeID != "" {
		existing, err := store.Get(ctx, resumeID)
		if errors.Is(err, sessions.ErrNotFound) {
			return sessions.Session{}, resumeSnapshot{}, fmt.Errorf(
				"chat --resume %s: no such session in the store", resumeID,
			)
		}
		if err != nil {
			return sessions.Session{}, resumeSnapshot{}, fmt.Errorf("load session %s: %w", resumeID, err)
		}
		// Codex slice 6.4 pass 4: check StatusExpired BEFORE the
		// cross-agent compatibility check. If a swept session also
		// happens to have drifted to a different agent name, the
		// cross-agent error would shadow the expiry signal and
		// `newChatCmd` would never emit `session.expired`. The
		// expired session isn't resumable under either spec, so
		// surfacing the TTL lifecycle event always takes precedence.
		if existing.Status == sessions.StatusExpired {
			return existing, resumeSnapshot{
				Status:       sessions.StatusExpired,
				LastActiveAt: existing.LastActiveAt,
			}, errSessionExpired
		}
		if existing.AgentName != spec.Metadata.Name {
			return sessions.Session{}, resumeSnapshot{}, fmt.Errorf(
				"chat --resume %s: session was created for agent %q but the current spec is for %q. "+
					"Cross-agent session reuse would put the model in a confused state — "+
					"either resume against the original agent's spec or start a new session.",
				resumeID, existing.AgentName, spec.Metadata.Name,
			)
		}
		snap := resumeSnapshot{
			Status:       existing.Status,
			LastActiveAt: existing.LastActiveAt,
		}
		// Refresh LastActiveAt + Status so concurrent listings see a
		// freshly-touched record.
		existing.Status = sessions.StatusActive
		existing.LastActiveAt = now
		if err := store.Update(ctx, existing); err != nil {
			// Codex slice 6.4 pass 3: if `sessions sweep` raced
			// between our Get above and this Update, the Store
			// returns ErrSessionExpired. Propagate via the
			// errSessionExpired channel so the caller emits
			// session.expired with the right snapshot — wrapping
			// as a generic touch error would lose the wire signal.
			if errors.Is(err, sessions.ErrSessionExpired) {
				return existing, resumeSnapshot{
					Status:       sessions.StatusExpired,
					LastActiveAt: snap.LastActiveAt,
				}, errSessionExpired
			}
			return sessions.Session{}, resumeSnapshot{}, fmt.Errorf("touch session %s: %w", resumeID, err)
		}
		return existing, snap, nil
	}

	// New session — derive an id with the same s_<base36-millis> shape
	// the adapter uses for its wire-event sessionId. Collision is
	// astronomically unlikely at millisecond resolution; the
	// SessionStore Create will reject any actual collision.
	sess := sessions.Session{
		ID:           fmt.Sprintf("s_%x", now.UnixNano()),
		AgentName:    spec.Metadata.Name,
		RuntimeType:  spec.Runtime.Type,
		Status:       sessions.StatusActive,
		CreatedAt:    now,
		LastActiveAt: now,
		Spec:         spec,
	}
	if err := store.Create(ctx, sess); err != nil {
		return sessions.Session{}, resumeSnapshot{}, fmt.Errorf("create session: %w", err)
	}
	return sess, resumeSnapshot{}, nil
}

// specsEquivalent reports whether two CompiledSpecs would drive the
// adapter identically. JSON-roundtrip equality is the cheapest
// good-enough check given that the spec is JSON-serializable end to
// end. Used by --resume to detect (and warn about) drift between the
// on-disk YAML and the stored session snapshot.
func specsEquivalent(a, b adl.CompiledSpec) bool {
	// time.Time fields don't exist on CompiledSpec at this layer, so
	// marshal+compare is deterministic. If a future field introduces
	// non-canonical JSON (map-ordering), this is the place to fix it.
	aJSON, errA := jsonMarshalDeterministic(a)
	bJSON, errB := jsonMarshalDeterministic(b)
	if errA != nil || errB != nil {
		// Marshal failure here would be a programmer bug; treat as
		// drift and let the resume warning surface it.
		return false
	}
	return string(aJSON) == string(bJSON)
}

func jsonMarshalDeterministic(v any) ([]byte, error) {
	// encoding/json marshals struct fields in declaration order and
	// map keys in lexical order (since Go 1.12), so straight Marshal
	// is already deterministic for the CompiledSpec shape. No need
	// for a custom canonicalizer.
	return json.Marshal(v)
}

// runChatTurn dispatches one turn: spec.Task gets the user's input,
// spec.SessionID gets the stable session id, backend.Submit runs the
// adapter, events stream to stdout, and the function returns when
// the adapter emits `session.ended` (or the event channel closes).
//
// We pass spec by pointer ONLY to read the immutable parts (Metadata,
// Model, Tools, etc.). The mutations (Task + SessionID) happen on a
// LOCAL copy so the caller's spec stays untouched across turns —
// otherwise each turn would see the cumulative state from the prior
// turn instead of the original spec.
// cancelCh is non-nil when called from the REPL — every Ctrl-C
// arrives via this channel, sent by the central signal dispatcher
// in newChatCmd. Tests pass nil and runChatTurn skips the cancel
// goroutine entirely. Codex pass 7 of slice 6.3 replaced the
// in-turn signal.Notify with this single-channel design to
// eliminate the fan-out race the previous flag-based approach
// couldn't close.
//
// turnIndex is the 1-based sequence number of this turn within the
// chat. Stamped on the `chat.turn` OTel span (slice 6.5) so trace
// consumers can navigate "first turn → second turn → ..." within a
// single chat without parsing timestamps. When tracing is off the
// SDK's no-op tracer makes the span free.
func runChatTurn(
	ctx context.Context,
	cmd *cobra.Command,
	be backend.Backend,
	specPtr *adl.CompiledSpec,
	sessionID string,
	userInput string,
	cancelCh <-chan struct{},
	turnIndex int64,
) (turnErr error) {
	// Slice 6.5: open a `chat.turn` span as a child of the chat-root
	// (the caller's ctx). The adapter dispatch below uses
	// turnSpanCtx, so the TRACEPARENT slice 5.2 injects into the
	// adapter env is the turn span's — adapter spans nest under
	// THIS turn, not the chat-root. The trace tree becomes:
	//
	//   agentctl.run (chat root)
	//   ├── chat.turn 1
	//   │   └── agent.session (adapter)
	//   │       ├── gen_ai.chat
	//   │       └── gen_ai.tool.call
	//   ├── chat.turn 2
	//   │   └── agent.session
	//   │       └── ...
	//   └── ...
	//
	// Named return `turnErr` lets the deferred span close observe
	// the error and mark the span ERROR. Matches the chat-root
	// pattern (codex pass 4 of slice 6.3 / pass 5 of slice 5.1).
	// Codex pass 1 of slice 6.5: use the shared `observability.Tracer()`
	// (instrumentation scope `github.com/CCDevelopForFun/agent-controller/cli`)
	// so chat.turn spans live under the same scope as `agentctl.run`
	// and adapter spans — backend dashboards filtering by scope
	// see ALL agentctl spans uniformly. A separate `agentctl/chat`
	// scope (the original choice here) would have silently dropped
	// turn spans from existing saved queries.
	turnSpanCtx, turnSpan := observability.Tracer().Start(ctx, "chat.turn",
		trace.WithAttributes(
			attribute.Int64("chat.turn.index", turnIndex),
			attribute.Int("chat.turn.prompt.bytes", len(userInput)),
			attribute.String("agent_controller.session.id", sessionID),
		),
	)
	defer func() {
		if turnErr != nil {
			turnSpan.SetStatus(codes.Error, turnErr.Error())
			turnSpan.RecordError(turnErr)
		}
		turnSpan.End()
	}()
	ctx = turnSpanCtx

	turn := *specPtr
	turn.Task = userInput
	turn.SessionID = &sessionID

	run, warnings, err := be.Resolve(ctx, turn, nil)
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	for _, w := range warnings {
		printEvent(cmd.OutOrStdout(), w)
	}

	h, err := be.Submit(ctx, run)
	if err != nil {
		return fmt.Errorf("submit: %w", err)
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cancelCh != nil {
		go func() {
			select {
			case <-cancelCh:
				fmt.Fprintln(cmd.ErrOrStderr(), "\n(cancelling turn — session stays open; /exit to end chat)")
				_ = be.Stop(h)
				cancel()
			case <-turnCtx.Done():
				return
			}
		}()
	}

	// turnErr is the named return — assigning to it from inside the
	// event loop and returning bare lets the deferred span close
	// observe whichever error path fired. Slice 6.5.
	for ev := range be.Events(h) {
		printEvent(cmd.OutOrStdout(), ev)
		if ev.Type == wire.EventError {
			turnErr = errors.New("turn ended with error event")
		}
		if ev.Type == wire.EventSessionEnded {
			// Codex pass 1 of slice 6.3: a terminal `session.ended`
			// with reason="error" (or "cancelled") is the adapter
			// reporting failure WITHOUT also emitting a separate
			// `error` event — chat would otherwise report a failed
			// turn as successful. Mirror the run command's switch on
			// reason here.
			var ended struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(ev.Data, &ended)
			switch ended.Reason {
			case "error":
				if ended.Message != "" {
					turnErr = fmt.Errorf("turn ended with error: %s", ended.Message)
				} else {
					turnErr = errors.New("turn ended with reason=error")
				}
			case "cancelled":
				turnErr = errors.New("turn was cancelled")
			}
		}
	}
	return
}
