package backend

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/adl"
	"github.com/CCDevelopForFun/agent-controller/cli/internal/wire"
)

// matchBinding runs the selector + capability matcher and turns the result
// into the (ResolvedRunSpec, warnings, error) tuple every Backend.Resolve()
// implementation returns. Extracted from LocalBackend.Resolve in slice 4.3
// so KubernetesBackend (and future remote backends) don't duplicate the
// policy logic.
//
// Behavior summary:
//   - binding == nil: no-op pass-through.
//     Returns ResolvedRunSpec{Spec: spec, Binding: nil} with no warnings.
//   - binding != nil: enforce the selector + capability matcher.
//     - selector.runtimeType must equal spec.runtime.type. Mismatch is a
//       hard error regardless of `target.strict` (this is a configuration
//       wiring bug, not a capability gap).
//     - For each `k: true` entry in spec.runtime.requirements, the
//       binding's selector.capabilities[k] must also be `true`.
//       Unmet requirements emit one wire `warning` per requirement
//       (warn-but-proceed). When `binding.spec.target.strict` is true,
//       unmet requirements are promoted to a hard error before any
//       session.started event is emitted (see ROADMAP.md "Recorded
//       design decisions" for the policy rationale).
//
// The session ID stamped on warning events is empty — these events are
// emitted before Submit creates the runtime session. The CLI prepends
// them to the canonical stream so operators see them before any
// session.started event.
func matchBinding(spec adl.CompiledSpec, binding *adl.RuntimeBinding) (adl.ResolvedRunSpec, []wire.Event, error) {
	if binding == nil {
		return adl.ResolvedRunSpec{Spec: spec, Binding: nil}, nil, nil
	}

	// Selector check.
	if spec.Runtime.Type != binding.Spec.Selector.RuntimeType {
		return adl.ResolvedRunSpec{}, nil, fmt.Errorf(
			"binding %q targets runtime.type %q but the spec declares %q — "+
				"the selector does not match. Either rename the binding's "+
				"selector.runtimeType or rebind to a different binding.",
			binding.Metadata.Name, binding.Spec.Selector.RuntimeType, spec.Runtime.Type,
		)
	}

	// Capability matcher.
	missing := make([]string, 0, len(spec.Runtime.Requirements))
	for k, required := range spec.Runtime.Requirements {
		if !required {
			continue
		}
		if !binding.Spec.Selector.Capabilities[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)

	if len(missing) == 0 {
		return adl.ResolvedRunSpec{Spec: spec, Binding: binding}, nil, nil
	}

	if binding.Spec.Target.Strict {
		return adl.ResolvedRunSpec{}, nil, fmt.Errorf(
			"binding %q has target.strict: true but does not advertise "+
				"required capabilities %v. The run cannot proceed. Either "+
				"add the missing capabilities to the binding's "+
				"selector.capabilities, remove them from the spec's "+
				"runtime.requirements, switch to a binding whose target "+
				"actually provides them, or drop target.strict to "+
				"downgrade to warn-but-proceed.",
			binding.Metadata.Name, missing,
		)
	}

	// Warn-but-proceed: emit one wire warning per unmet requirement.
	now := time.Now().UTC()
	warnings := make([]wire.Event, 0, len(missing))
	for _, k := range missing {
		payload := map[string]any{
			"kind":        "unmet_runtime_requirement",
			"requirement": k,
			"binding":     binding.Metadata.Name,
			"message": fmt.Sprintf(
				"binding %q does not advertise capability %q; running anyway "+
					"(set target.strict: true to refuse runs with unmet "+
					"requirements, or add the capability to the binding's "+
					"selector.capabilities once the target actually provides it)",
				binding.Metadata.Name, k),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			// Unreachable: payload is map[string]any of strings/bools.
			return adl.ResolvedRunSpec{}, nil, fmt.Errorf("encode warning payload: %w", err)
		}
		warnings = append(warnings, wire.Event{
			V:         wire.ProtocolVersion,
			Type:      wire.EventWarning,
			Ts:        now,
			SessionID: "",
			Data:      raw,
		})
	}
	return adl.ResolvedRunSpec{Spec: spec, Binding: binding}, warnings, nil
}
