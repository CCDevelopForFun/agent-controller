// Package backend abstracts how a CompiledSpec is executed.
//
// MVP ships LocalBackend (spawn agent-runtime as a subprocess). Future
// backends — AgentCoreBackend, K8sBackend — implement the
// same interface; the CLI does not change.
//
// v0.3.3a split the run path into two explicit phases:
//
//	Resolve(spec, binding) → (ResolvedRunSpec, warnings, err)
//	Submit(resolved)       → SessionHandle
//
// Pre-v0.3.3 there was only Submit(spec). Resolve gives backends a
// hook to inspect/validate the chosen RuntimeBinding before any
// subprocess starts — and (in slice 3.3b) to emit `warning` wire
// events when the spec's runtime.requirements aren't met by the
// binding's target. Today (3.3a) the LocalBackend's Resolve is a
// no-op; the matcher lands in 3.3b.
package backend

import (
	"context"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

type Backend interface {
	// Resolve picks (or validates) a binding for the given spec and
	// produces an executable run shape. Backends may emit wire `warning`
	// or `error` events here — callers should drain the returned slice
	// before reading Events(handle), so warnings appear in the canonical
	// stream order. v0.3.3a: LocalBackend.Resolve is a no-op when
	// `binding` is nil (wraps the spec verbatim). Slice 3.3b adds the
	// capability matcher that fires warnings.
	Resolve(ctx context.Context, spec adl.CompiledSpec, binding *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error)
	// Submit starts the run for a resolved spec.
	Submit(ctx context.Context, run adl.ResolvedRunSpec) (SessionHandle, error)
	Events(h SessionHandle) <-chan wire.Event
	Stop(h SessionHandle) error
	Capabilities() Caps
}

type SessionHandle string

type Caps struct {
	SupportsStreaming bool
	SupportsMCP       bool
	MaxConcurrency    int
}
