package backend

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// LocalConfig configures the local subprocess backend. RuntimeCommand is the
// argv used to invoke the agent-runtime — typically ["node", "dist/index.js"]
// but configurable so tests can inject a shell-script stand-in.
type LocalConfig struct {
	RuntimeCommand []string
}

// LocalBackend runs the runtime as a child process and ferries the wire
// protocol over its stdin/stdout.
type LocalBackend struct {
	cfg LocalConfig

	mu       sync.Mutex
	sessions map[SessionHandle]*localSession
	counter  uint64
}

type localSession struct {
	cmd    *exec.Cmd
	events chan wire.Event
	errCh  chan error
}

func NewLocalBackend(cfg LocalConfig) *LocalBackend {
	return &LocalBackend{cfg: cfg, sessions: map[SessionHandle]*localSession{}}
}

func (b *LocalBackend) Capabilities() Caps {
	return Caps{SupportsStreaming: true, SupportsMCP: false, MaxConcurrency: 1}
}

// Resolve delegates to the shared matcher (selector + capability check).
// Slice 4.3 extracted the policy logic into matcher.go so KubernetesBackend
// applies the same rules. Pre-4.3 the logic lived inline here.
func (b *LocalBackend) Resolve(ctx context.Context, spec adl.CompiledSpec, binding *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	_ = ctx // not used today; reserved for future remote-resolution work
	return matchBinding(spec, binding)
}

func (b *LocalBackend) Submit(ctx context.Context, run adl.ResolvedRunSpec) (SessionHandle, error) {
	spec := run.Spec
	if len(b.cfg.RuntimeCommand) == 0 {
		return "", fmt.Errorf("LocalBackend: RuntimeCommand is empty")
	}
	cmd := exec.CommandContext(ctx, b.cfg.RuntimeCommand[0], b.cfg.RuntimeCommand[1:]...)
	cmd.Env = injectTraceparent(ctx, os.Environ())

	// All pipes must be wired BEFORE cmd.Start() — Go's os/exec API
	// requires it. Setting them up after Start returns an error and the
	// pipe is unusable.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}

	// Capture stderr synchronously by assigning cmd.Stderr. The
	// alternative — StderrPipe() + an io.Copy goroutine — races against
	// cmd.Wait(): we'd read stderrBuf.String() while the goroutine is
	// still draining. Direct assignment lets os/exec do the draining for
	// us and guarantees the buffer is complete by the time Wait returns.
	stderrBuf := &bytesBuffer{}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start runtime: %w", err)
	}

	// Write the CompiledSpec and close stdin so the runtime knows the
	// payload is complete.
	go func() {
		defer stdin.Close()
		enc := json.NewEncoder(stdin)
		_ = enc.Encode(spec)
	}()

	sess := &localSession{
		cmd:    cmd,
		events: make(chan wire.Event, 16),
		errCh:  make(chan error, 1),
	}

	go func() {
		defer close(sess.events)

		var sawSessionEnded bool
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			ev, err := wire.Decode(scanner.Bytes())
			if err != nil {
				// Surface decode errors as in-band error events so the
				// CLI's exit-code logic sees them.
				sess.events <- syntheticErrorEvent("wire decode: " + err.Error())
				continue
			}
			if ev.Type == wire.EventSessionEnded {
				sawSessionEnded = true
			}
			sess.events <- ev
		}
		// scanner.Scan() returns false for both EOF (normal) and errors (e.g.
		// oversized line, I/O error). If the scanner stopped due to an error
		// and the session never completed, emit a synthetic error event so the
		// CLI cannot report a truncated run as successful.
		if scanErr := scanner.Err(); scanErr != nil && !sawSessionEnded {
			sess.events <- syntheticErrorEvent("stdout scan: " + scanErr.Error())
			// Drain the remaining stdout so the child process is not blocked
			// on a full pipe buffer and can exit cleanly for cmd.Wait().
			_, _ = io.Copy(io.Discard, stdout)
		}

		waitErr := cmd.Wait()
		if waitErr != nil && !sawSessionEnded {
			// The runtime exited non-zero before emitting session.ended
			// (startup crash, stdin parse failure, etc.). Synthesize an
			// error event carrying the captured stderr so the CLI can't
			// report a failed run as successful.
			sess.events <- syntheticErrorEvent(fmt.Sprintf(
				"agent-runtime exited: %v\nstderr:\n%s", waitErr, stderrBuf.String(),
			))
		}
		sess.errCh <- waitErr
	}()

	b.mu.Lock()
	b.counter++
	h := SessionHandle(fmt.Sprintf("local-%d", b.counter))
	b.sessions[h] = sess
	b.mu.Unlock()
	return h, nil
}

func (b *LocalBackend) Events(h SessionHandle) <-chan wire.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sessions[h]; ok {
		return s.events
	}
	return closedEvents()
}

func (b *LocalBackend) Stop(h SessionHandle) error {
	b.mu.Lock()
	s, ok := b.sessions[h]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown session %s", h)
	}
	if s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(interruptSignal)
	select {
	case err := <-s.errCh:
		return err
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		return fmt.Errorf("runtime did not exit after SIGINT; killed")
	}
}

func closedEvents() <-chan wire.Event {
	c := make(chan wire.Event)
	close(c)
	return c
}

func syntheticErrorEvent(msg string) wire.Event {
	data, _ := json.Marshal(map[string]string{"message": msg})
	return wire.Event{
		V:         wire.ProtocolVersion,
		Type:      wire.EventError,
		Ts:        time.Now().UTC(),
		SessionID: "local",
		Data:      data,
	}
}

// bytesBuffer is a tiny thread-safe append-only buffer used to capture
// runtime stderr until exit. We avoid bytes.Buffer's lack of concurrency
// guarantees by guarding writes with a mutex; Reads only happen after the
// runtime exits.
type bytesBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *bytesBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
