/**
 * Subagent tool — a thin shim over Pi's own first-party subagent extension.
 *
 * Pi ships the subagent extension as a bundled example
 * (`<pi>/examples/extensions/subagent/index.ts`, included in the package's
 * npm `files`). We used to carry a ~600-line vendored fork of it, which drifted
 * from upstream between pi releases. Instead we load upstream in place and
 * override only the behaviors that conflict with agent-controller's model.
 *
 * The adapter (runtime/src/adapter.ts) resolves the absolute upstream path
 * from the pinned `@earendil-works/pi-coding-agent` install and passes it in
 * via AC_UPSTREAM_SUBAGENT_PATH. Pi loads this file through jiti with an alias
 * map covering `@earendil-works/*` and `typebox`, so the dynamic import below
 * resolves upstream's own imports (pi-tui, pi-agent-core, typebox) out of pi's
 * nested node_modules — the reason the fork had to strip them is gone.
 *
 * Upstream deltas this shim applies, all inside the tool call:
 *
 *   1. agentScope — upstream defaults to "user" and lets the model choose
 *      (`params.agentScope ?? "user"`), which reaches ~/.pi/agent/agents/*.md.
 *      agent-controller's allowlist is `spec.subagents[]`, materialized into
 *      <cwd>/.pi/agents/, so we force "project". Honoring the model's value
 *      would let the parent delegate to agents the spec never declared.
 *   2. confirmProjectAgents — upstream defaults to true. Every project agent
 *      here came from the ADL spec the user chose to run, so the spec IS the
 *      approval; prompting again would stall `agentctl chat`.
 *   3. description — upstream's is built at registration time and embeds
 *      getAgentDir() plus an instruction to set `agentScope: "both"`. That is
 *      model-visible, leaks a home-directory path into the prompt, and
 *      advertises an escape we reject. Replaced.
 *   4. process.argv[1] — upstream's getPiInvocation spawns
 *      `node <process.argv[1]>` when that path exists. Under agent-controller
 *      argv[1] is the adapter (dist/index.js), which expects a CompiledSpec on
 *      stdin, so children would hang. Point it at the real pi CLI instead.
 *   5. PI_CODING_AGENT_DIR — upstream spawns without an `env` override, so
 *      children inherit ours. The adapter writes a models.json holding the
 *      ANTHROPIC_BASE_URL gateway override into a local agent dir; without
 *      this the child hits the hardcoded api.anthropic.com.
 *
 * Deltas 4 and 5 are process-global, so they are applied for the duration of
 * the tool call and restored afterwards rather than being set once at load
 * time. Both are read by upstream inside `execute` (argv[1] by
 * getPiInvocation, the env var by the inherited spawn env), so the narrow
 * window is sufficient — and it keeps the parent session's own getAgentDir()
 * (auth.json, settings.json, sessions) pointing at the user's real ~/.pi for
 * everything outside the call.
 *
 * Pi executes a batch of tool calls concurrently by default
 * (`toolExecution: "parallel"`), so two `subagent` calls in one turn can
 * overlap. The overrides are therefore reference-counted: the first call in
 * applies them, the last call out restores them. Naive per-call save/restore
 * would corrupt state — the second call would save the first call's already
 * mutated argv[1] as the "original", and the first call's restore would repoint
 * argv[1] at the adapter while the second is still spawning children, sending
 * them to a process that expects a CompiledSpec on stdin. Both calls always
 * want the same values (they come from env vars the adapter sets once before
 * the session starts), which is what makes sharing one applied state correct.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

/** Absolute path to upstream's example entrypoint; set by the adapter. */
const UPSTREAM_PATH_ENV = "AC_UPSTREAM_SUBAGENT_PATH";
/** Absolute path to the resolved pi CLI; set by the adapter. */
const PI_BIN_ENV = "AC_PI_BIN";
/** Local pi agent dir holding the gateway models.json; set by the adapter. */
const AGENT_DIR_ENV = "AC_SUBAGENT_AGENT_DIR";

const DESCRIPTION = [
  "Delegate tasks to specialized subagents with isolated context.",
  "Modes: single (agent + task), parallel (tasks array), chain (sequential with {previous} placeholder).",
  "Only the subagents declared in this agent's spec are available; the agentScope parameter is ignored.",
].join(" ");

/** Number of subagent calls currently inside the override window. */
let activeCalls = 0;
/** State captured when the window opened; only valid while activeCalls > 0. */
let savedState: { argv1: string; agentDir: string | undefined; argvOverridden: boolean } | undefined;

/**
 * Apply the two process-global deltas, returning a release function.
 *
 * Reference-counted so overlapping tool calls share one applied state — see
 * the parallel-execution note in the file header. Exported for
 * subagent-upstream.test.ts, which asserts the nesting directly; Pi only ever
 * uses this module's default export.
 *
 * `process.argv[1]` is only overridden when the resolved pi CLI is a .js file,
 * because upstream runs it as `node <path>`. A natively compiled pi (bun
 * binary) would need `command` rather than `args[0]`; agent-controller installs
 * pi from npm, so that path is not exercised, and upstream's own PATH fallback
 * still applies when we leave argv[1] alone.
 */
export function applyProcessOverrides(): () => void {
  if (activeCalls === 0) {
    const piBin = process.env[PI_BIN_ENV];
    const agentDir = process.env[AGENT_DIR_ENV];
    const argvOverridden = Boolean(piBin?.endsWith(".js"));

    savedState = {
      argv1: process.argv[1],
      agentDir: process.env.PI_CODING_AGENT_DIR,
      argvOverridden,
    };

    if (argvOverridden && piBin) process.argv[1] = piBin;
    if (agentDir) process.env.PI_CODING_AGENT_DIR = agentDir;
  }
  activeCalls++;

  let released = false;
  return () => {
    // Guard against a double release decrementing the count twice and
    // restoring while another call is still in flight.
    if (released) return;
    released = true;
    activeCalls--;
    if (activeCalls > 0 || !savedState) return;

    if (savedState.argvOverridden) process.argv[1] = savedState.argv1;
    if (savedState.agentDir === undefined) delete process.env.PI_CODING_AGENT_DIR;
    else process.env.PI_CODING_AGENT_DIR = savedState.agentDir;
    savedState = undefined;
  };
}

/**
 * Wrap the ExtensionAPI so upstream's `registerTool` call is intercepted and
 * its `execute` is replaced with one that pins the parameters we govern.
 * Everything else on the tool (parameters schema, renderCall, renderResult)
 * passes through untouched, so the TUI rendering is upstream's.
 */
function wrapApi(pi: ExtensionAPI): ExtensionAPI {
  return new Proxy(pi, {
    get(target, prop, receiver) {
      if (prop !== "registerTool") return Reflect.get(target, prop, receiver);

      return (tool: Record<string, unknown>) => {
        const upstreamExecute = tool.execute as (...args: unknown[]) => Promise<unknown>;

        tool.description = DESCRIPTION;
        tool.execute = async (
          toolCallId: unknown,
          params: Record<string, unknown>,
          ...rest: unknown[]
        ) => {
          const governed = {
            ...params,
            agentScope: "project",
            confirmProjectAgents: false,
          };
          const restore = applyProcessOverrides();
          try {
            return await upstreamExecute.call(tool, toolCallId, governed, ...rest);
          } finally {
            restore();
          }
        };

        return (target as unknown as { registerTool: (t: unknown) => unknown }).registerTool(tool);
      };
    },
  });
}

export default async function (pi: ExtensionAPI) {
  const upstreamPath = process.env[UPSTREAM_PATH_ENV];
  if (!upstreamPath) {
    throw new Error(
      `[agent-controller] ${UPSTREAM_PATH_ENV} is not set — the adapter must resolve ` +
        `Pi's bundled subagent example before loading this shim.`,
    );
  }

  const upstream = (await import(upstreamPath)) as { default?: (api: ExtensionAPI) => unknown };
  if (typeof upstream.default !== "function") {
    throw new Error(
      `[agent-controller] ${upstreamPath} has no default export — Pi's bundled ` +
        `subagent example has moved or changed shape for this pi version.`,
    );
  }

  await upstream.default(wrapApi(pi));
}
