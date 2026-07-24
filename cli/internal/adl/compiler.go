package adl

import (
	"fmt"
	"sort"
	"strings"

	"github.com/CCDevelopForFun/agent-controller/cli/internal/registry"
)

// Compile takes a parsed (and presumably validated) ADL doc plus a manifest
// index and produces a CompiledSpec ready to ship to the runtime.
//
// The caller is responsible for running Validate before Compile — Compile
// assumes the doc shape matches the schema and panics on type assertion
// failure for unexpected types. (Practically: validate first.)
func Compile(doc map[string]any, idx *registry.ManifestIndex) (CompiledSpec, error) {
	var out CompiledSpec
	out.V = 1

	meta, _ := doc["metadata"].(map[string]any)
	out.Metadata = SpecMetadata{
		Name:        getString(meta, "name"),
		Owner:       getString(meta, "owner"),
		Description: getString(meta, "description"),
	}

	spec, _ := doc["spec"].(map[string]any)

	model, _ := spec["model"].(map[string]any)
	out.Model = Model{
		Provider: getString(model, "provider"),
		Name:     getString(model, "name"),
	}
	if t, ok := model["temperature"].(float64); ok {
		out.Model.Temperature = &t
	}

	if persona, ok := spec["persona"].(map[string]any); ok {
		out.Persona = &Persona{
			Role:         getString(persona, "role"),
			Instructions: getString(persona, "instructions"),
		}
	}

	out.Task = getString(spec, "task")

	// Pi ships these built-in tools at the runtime level (see Pi's
	// `defaultActiveToolNames` in agent-session.js). They don't require a
	// manifest in our local registry — declaring them in spec.tools[] just
	// adds them to the active allowlist passed to Pi's createAgentSession.
	// The runtime adapter knows to skip entrypoint loading for these names.
	piBuiltinTools := map[string]bool{
		"bash":  true,
		"read":  true,
		"edit":  true,
		"write": true,
	}

	out.Tools = []ResolvedRef{}
	for _, raw := range getList(spec, "tools") {
		t := raw.(map[string]any)
		name := getString(t, "name")
		var ref ResolvedRef
		if piBuiltinTools[name] {
			// Pi built-in: no manifest lookup, no entrypoint. The adapter
			// surfaces this purely as an allowlist entry to createAgentSession.
			ref = ResolvedRef{Name: name, Builtin: true}
		} else {
			m, ok := idx.Lookup("Tool", name)
			if !ok {
				return out, fmt.Errorf("tool %q not found in registry", name)
			}
			ref = ResolvedRef{Name: name, Entrypoint: m.Spec.Entrypoint}
		}
		if cfg, ok := t["config"].(map[string]any); ok {
			// v0.1.11: built-in tools cannot accept a `config` block at the
			// spec.tools[] level. Pi's built-in bash/read/edit/write don't
			// consume our AGENT_CONTROLLER_EXT_CONFIG env, and opencode has
			// no per-built-in config concept either, so a config block on a
			// built-in is a silent governance no-op — the user thinks they're
			// restricting access, but nothing enforces it. Reject loudly and
			// direct users at the catalog-supplied @gotgenes/pi-permission-system
			// extension for actual bash allowlisting. The opencode adapter
			// has a redundant runtime check for defense-in-depth; this is
			// the canonical rejection point.
			if ref.Builtin && len(cfg) > 0 {
				keys := make([]string, 0, len(cfg))
				for k := range cfg {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return out, fmt.Errorf(
					"spec.tools declares built-in %q with a config block (%v) — "+
						"Pi built-in tools have no per-tool config plumbing and the "+
						"runtime would silently ignore this. For bash command "+
						"allowlisting use the @gotgenes/pi-permission-system extension "+
						"(declare it under spec.extensions[].source: "+
						"npm:@gotgenes/pi-permission-system with the policy as its "+
						"config). See docs/features.md for a working example",
					name, keys,
				)
			}
			ref.Config = cfg
		}
		out.Tools = append(out.Tools, ref)
	}

	out.Extensions = []ResolvedRef{}
	for _, raw := range getList(spec, "extensions") {
		e := raw.(map[string]any)
		name := getString(e, "name")
		source := getString(e, "source")
		var ref ResolvedRef
		if source != "" {
			// Self-contained extension: skip registry lookup; runtime will
			// install and resolve the entrypoint via source at session start.
			ref = ResolvedRef{Name: name, Source: source}
		} else {
			m, ok := idx.Lookup("Extension", name)
			if !ok {
				return out, fmt.Errorf("extension %q not found in registry", name)
			}
			ref = ResolvedRef{Name: name, Entrypoint: m.Spec.Entrypoint}
		}
		if cfg, ok := e["config"].(map[string]any); ok {
			ref.Config = cfg
		}
		out.Extensions = append(out.Extensions, ref)
	}

	out.Skills = []ResolvedRef{}
	for _, raw := range getList(spec, "skills") {
		s := raw.(map[string]any)
		name := getString(s, "name")
		m, ok := idx.Lookup("Skill", name)
		if !ok {
			return out, fmt.Errorf("skill %q not found in registry", name)
		}
		ref := ResolvedRef{Name: name, Entrypoint: m.Spec.Entrypoint}
		out.Skills = append(out.Skills, ref)
	}

	out.Subagents = []ResolvedRef{}
	for _, raw := range getList(spec, "subagents") {
		s := raw.(map[string]any)
		name := getString(s, "name")
		m, ok := idx.Lookup("Subagent", name)
		if !ok {
			return out, fmt.Errorf("subagent %q not found in registry", name)
		}
		ref := ResolvedRef{Name: name, Entrypoint: m.Spec.Entrypoint}
		out.Subagents = append(out.Subagents, ref)
	}

	// MCPServers are passed through unchanged — no manifest resolution needed.
	// The runtime writes them to <cwd>/.pi/mcp.json for pi-mcp-extension.
	out.MCPServers = []MCPServer{}
	for _, raw := range getList(spec, "mcpServers") {
		s := raw.(map[string]any)
		srv := MCPServer{
			Name:      getString(s, "name"),
			Transport: getString(s, "transport"),
			Lifecycle: getString(s, "lifecycle"),
			Command:   getString(s, "command"),
			URL:       getString(s, "url"),
		}
		if args, ok := s["args"].([]any); ok {
			for _, a := range args {
				if str, ok := a.(string); ok {
					srv.Args = append(srv.Args, str)
				}
			}
		}
		if env, ok := s["env"].(map[string]any); ok {
			srv.Env = make(map[string]string, len(env))
			for k, v := range env {
				if str, ok := v.(string); ok {
					srv.Env[k] = str
				}
			}
		}
		if headers, ok := s["headers"].(map[string]any); ok {
			srv.Headers = make(map[string]string, len(headers))
			for k, v := range headers {
				if str, ok := v.(string); ok {
					srv.Headers[k] = str
				}
			}
		}
		out.MCPServers = append(out.MCPServers, srv)
	}

	// Installs are passed through unchanged — they are consumed by
	// `agentctl install`, not the runtime.
	for _, raw := range getList(spec, "installs") {
		if pkg, ok := raw.(string); ok {
			out.Installs = append(out.Installs, pkg)
		}
	}

	rt, _ := spec["runtime"].(map[string]any)
	out.Runtime = RuntimeConfig{Type: getString(rt, "type")}
	// runtime.requirements (v0.3.1) — additive free-form capability map.
	// The schema enforces additionalProperties:{type:boolean}, so we can
	// safely assert each value as bool. Both "field absent" and "field
	// declared as {}" produce a nil map; the omitempty tag on the JSON
	// side drops the field in both cases. See types.go for the rationale
	// for not distinguishing those two states at v0.3.1.
	if reqs, ok := rt["requirements"].(map[string]any); ok && len(reqs) > 0 {
		out.Runtime.Requirements = make(map[string]bool, len(reqs))
		for k, v := range reqs {
			if b, ok := v.(bool); ok {
				out.Runtime.Requirements[k] = b
			}
		}
	}

	if g, ok := spec["guardrails"].(map[string]any); ok {
		out.Guardrails = &Guardrails{
			HallucinationDetector: getString(g, "hallucinationDetector"),
		}
	}

	// Slice 7.2: pass spec.outputSchema through verbatim. The CLI only
	// activates it when `--output-file <path>` is passed; the compiler
	// doesn't validate the schema document itself (any valid JSON Schema
	// is accepted — the runtime validator will reject if it doesn't
	// compile).
	//
	// Codex pass 1 of slice 7.2 caught the empty-map drop: `outputSchema:
	// {}` is a VALID JSON Schema that accepts any JSON value and is the
	// natural way to require JSON-syntax validation without further
	// constraints. The presence check is `ok` only (key exists) — not
	// `len(osch) > 0` — so an empty schema still flips the
	// CLI into "extract + parse JSON" mode.
	if osch, ok := spec["outputSchema"].(map[string]any); ok {
		// Pointer assignment so an empty map survives JSON marshal
		// (see CompiledSpec.OutputSchema docs).
		out.OutputSchema = &osch
	}

	if o, ok := spec["observability"].(map[string]any); ok {
		// Slice 5.1: pass spec.observability through to CompiledSpec.
		// The CLI checks Observability.Tracing to decide whether to
		// init OTel. Codex pass 1 of slice 5.1 caught the silent drop.
		// Slice 5.3: also surface CaptureContent so adapters can honor
		// the opt-in for prompt/completion content in spans.
		tracing := false
		if v, ok := o["tracing"].(bool); ok {
			tracing = v
		}
		captureContent := false
		if v, ok := o["captureContent"].(bool); ok {
			captureContent = v
		}
		out.Observability = &Observability{
			Tracing:        tracing,
			CaptureContent: captureContent,
		}
	}

	// Adapter-compatibility check (v0.3.4). Each runtime adapter declares
	// what ADL surface it cannot honor; failing here surfaces the gap at
	// `agentctl compile` time so CI gates that stop at compile catch it,
	// instead of letting the adapter crash at startup. The same checks
	// remain in the adapter (defense in depth) so a hand-crafted
	// CompiledSpec that bypasses the compiler still gets rejected before
	// any model call. See harness-matrix.md Phase 2 follow-up #6.
	if checker, ok := adapterCheckers[out.Runtime.Type]; ok {
		if err := checker(out); err != nil {
			return out, err
		}
	}

	return out, nil
}

// adapterCheckers maps runtime.type values to a post-Compile validation
// function that catches adapter-specific incompatibilities at compile
// time. v0.3.4 adds the opencode entry; future adapters add their own
// entries here.
var adapterCheckers = map[string]func(CompiledSpec) error{
	"local-opencode": checkOpencodeIncompatibilities,
	"local-codex":    checkCodexIncompatibilities,
}

// checkOpencodeIncompatibilities mirrors the rejections that
// runtime-opencode/src/index.ts performs at adapter startup. Keeping the
// runtime check too (defense in depth) is intentional — see the comments
// in that file. Cases checked here:
//
//   - spec.extensions[] non-empty (Pi extension modules don't run in opencode)
//   - spec.installs[] non-empty (deprecated; use spec.extensions[].source on Pi)
//   - spec.tools[] entries that are custom Pi-extension tools (have
//     entrypoint set but builtin is false). opencode can't load Pi-format
//     JS modules.
//
// spec.sessionId (from --resume) is checked in the CLI's run command,
// not here — the compiler runs before --resume is applied to the spec.
func checkOpencodeIncompatibilities(spec CompiledSpec) error {
	var problems []string
	if len(spec.Extensions) > 0 {
		problems = append(problems, fmt.Sprintf(
			"spec.extensions (%d declared) — Pi extension JS modules don't run inside opencode",
			len(spec.Extensions)))
	}
	if len(spec.Installs) > 0 {
		problems = append(problems, fmt.Sprintf(
			"spec.installs (%d entries) — deprecated; use spec.extensions[].source on the Pi adapter (runtime.type: local)",
			len(spec.Installs)))
	}
	var customTools []string
	for _, t := range spec.Tools {
		if t.Entrypoint != "" && !t.Builtin {
			customTools = append(customTools, t.Name)
		}
	}
	if len(customTools) > 0 {
		sort.Strings(customTools)
		problems = append(problems, fmt.Sprintf(
			"spec.tools[] contains custom Pi-extension tools that cannot run on opencode: %v. "+
				"Only Pi built-in tools (bash, read, edit, write) are supported on the opencode adapter",
			customTools))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"spec declares capabilities not supported by runtime.type: local-opencode:\n"+
			"  - %s\nEither remove these fields or switch to runtime.type: local (Pi adapter) which supports them",
		strings.Join(problems, "\n  - "),
	)
}

// checkCodexIncompatibilities mirrors the rejections that
// runtime-codex performs at adapter startup. Cases checked here:
//
//   - spec.model.provider != "openai" — codex only supports the OpenAI API
//   - spec.extensions[] non-empty — Pi extension modules don't run in codex
//   - spec.installs[] non-empty — deprecated; use spec.extensions[].source on Pi
//   - spec.tools[] entries that are custom Pi-extension tools (have
//     entrypoint set but builtin is false) — codex can only load Pi built-in
//     tools (bash, read, edit, write) via its sandbox
//   - spec.subagents[] non-empty — codex has no subagent concept
func checkCodexIncompatibilities(spec CompiledSpec) error {
	var problems []string
	if spec.Model.Provider != "openai" {
		problems = append(problems, fmt.Sprintf(
			"spec.model.provider %q — the codex adapter supports only provider: openai (got %q); "+
				"use runtime.type: local / local-pi / local-opencode for anthropic/google",
			spec.Model.Provider, spec.Model.Provider))
	}
	if len(spec.Extensions) > 0 {
		problems = append(problems, fmt.Sprintf(
			"spec.extensions (%d declared) — Pi extension JS modules don't run inside codex",
			len(spec.Extensions)))
	}
	if len(spec.Installs) > 0 {
		problems = append(problems, fmt.Sprintf(
			"spec.installs (%d entries) — deprecated; use spec.extensions[].source on the Pi adapter (runtime.type: local)",
			len(spec.Installs)))
	}
	var customTools []string
	for _, t := range spec.Tools {
		if t.Entrypoint != "" && !t.Builtin {
			customTools = append(customTools, t.Name)
		}
	}
	if len(customTools) > 0 {
		sort.Strings(customTools)
		problems = append(problems, fmt.Sprintf(
			"spec.tools[] contains custom Pi-extension tools that cannot run on codex: %v. "+
				"Only Pi built-in tools (bash, read, edit, write) are supported on the codex adapter",
			customTools))
	}
	if len(spec.Subagents) > 0 {
		problems = append(problems, fmt.Sprintf(
			"spec.subagents (%d declared) — codex has no subagent concept; "+
				"use runtime.type: local (Pi adapter) for multi-agent workflows",
			len(spec.Subagents)))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf(
		"spec declares capabilities not supported by runtime.type: local-codex:\n"+
			"  - %s\nEither remove these fields or switch to runtime.type: local (Pi adapter) which supports them",
		strings.Join(problems, "\n  - "),
	)
}

func getString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func getList(m map[string]any, k string) []any {
	if v, ok := m[k].([]any); ok {
		return v
	}
	return nil
}
