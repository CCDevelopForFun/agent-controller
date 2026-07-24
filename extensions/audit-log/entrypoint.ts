import { appendFileSync } from "node:fs";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

interface AuditConfig {
  path: string;
}

/**
 * Read this extension's ADL config out of the AGENT_CONTROLLER_EXT_CONFIG
 * env var. The adapter (runtime/src/adapter.ts) sets this before
 * createAgentSession runs, populating it from each ResolvedRef.config in
 * the CompiledSpec. We use an env-var convention rather than Pi's
 * settingsManager because the latter's API is not stable enough at the
 * time of writing; migrating is a v0.2 follow-up tracked in the spec.
 */
function readConfig(): AuditConfig {
  const raw = process.env.AGENT_CONTROLLER_EXT_CONFIG ?? "{}";
  const all = JSON.parse(raw) as Record<string, unknown>;
  const own = (all["audit-log"] as AuditConfig | undefined) ?? { path: "./audit.log" };
  return own;
}

export default function (pi: ExtensionAPI) {
  const config = readConfig();
  const append = (record: object) => {
    appendFileSync(config.path, JSON.stringify(record) + "\n");
  };

  pi.on("tool_call", (event) => {
    append({ ts: new Date().toISOString(), event: "tool_call", tool: event.toolName, input: event.input });
  });

  pi.on("tool_result", (event) => {
    append({ ts: new Date().toISOString(), event: "tool_result", tool: event.toolName, isError: event.isError });
  });
}
