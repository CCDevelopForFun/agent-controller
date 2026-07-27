/**
 * Invocation builders for the Claude Agent SDK runtime adapter.
 *
 * This module is intentionally pure: every export is a function over plain
 * data with no I/O, so the whole surface is unit-testable without a live
 * model or an SDK session.
 */

import type { CompiledSpec, ResolvedRef } from "./types.js";

/**
 * Returns true when a ResolvedRef is a custom Pi-extension tool (entrypoint
 * set, not a Pi built-in). Built-ins are always safe even if an entrypoint is
 * incidentally set.
 */
function isCustomPiExtensionTool(t: ResolvedRef): boolean {
  return Boolean(t.entrypoint) && !t.builtin;
}

/**
 * Throws with a field-naming error when `spec` declares features the Claude
 * Agent SDK adapter cannot honour. Mirrors
 * cli/internal/adl/compiler.go::checkClaudeIncompatibilities — the compiler is
 * the canonical gate; this is defense in depth for hand-crafted CompiledSpecs
 * that bypass `agentctl compile`.
 *
 * Note: spec.subagents[] is NOT rejected — the SDK registers them natively.
 */
export function assertClaudeCompatible(spec: CompiledSpec): void {
  if (spec.model.provider !== "anthropic") {
    throw new Error(
      `runtime-claude: spec.model.provider is "${spec.model.provider}" but the ` +
        `Claude Agent SDK adapter supports only provider "anthropic". ` +
        `Use runtime.type: local / local-pi / local-opencode for openai/google.`,
    );
  }

  if ((spec.extensions ?? []).length > 0) {
    throw new Error(
      `runtime-claude: spec.extensions[] is non-empty. Pi-format extension JS ` +
        `modules cannot run inside the Claude Agent SDK. Remove extensions or ` +
        `target runtime.type: local.`,
    );
  }

  if ((spec.installs ?? []).length > 0) {
    throw new Error(
      `runtime-claude: spec.installs[] is non-empty. The claude adapter does not ` +
        `support the deprecated installs field. Use spec.extensions[].source on ` +
        `the Pi adapter instead.`,
    );
  }

  const offenders = (spec.tools ?? []).filter(isCustomPiExtensionTool).map((t) => t.name);
  if (offenders.length > 0) {
    throw new Error(
      `runtime-claude: spec.tools[] contains custom Pi-extension tools that ` +
        `cannot run on the claude adapter: ${offenders.sort().join(", ")}. ` +
        `Only Pi built-in tools (bash, read, edit, write) are supported.`,
    );
  }
}
