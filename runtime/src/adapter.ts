import { createAgentSession, DefaultResourceLoader, SessionManager, getAgentDir } from "@earendil-works/pi-coding-agent";
import { getBuiltinModel } from "@earendil-works/pi-ai/providers/all";
import { join, basename, resolve, dirname } from "node:path";
import { mkdirSync, writeFileSync, existsSync, readFileSync, copyFileSync, realpathSync, readdirSync, unlinkSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { homedir } from "node:os";

import type { CompiledSpec, HallucinationMode, MCPServer, Persona, WireEvent } from "./types.js";
import { stamp } from "./wire.js";
import {
  CORRECTION_PROMPT,
  HONESTY_PREAMBLE,
  detectHallucinatedToolCalls,
  stripHallucinationXml,
  wrapSkillBody,
} from "./honesty.js";
import {
  resolveFakeModelIfRequested,
  resolveFakeModelRuntimeIfRequested,
} from "./testing/fake-provider.js";
import {
  EVENTS_API_VERSION_V1ALPHA1,
  initAdapterTracing,
  type AdapterTracing,
} from "./observability.js";

// Read once at module load; runtime/package.json is the source of truth.
// Mirrored into OTel resource attributes as service.version, so spans
// are searchable by adapter version in the backend.
const RUNTIME_PACKAGE_VERSION: string = (() => {
  try {
    const pkgPath = fileURLToPath(new URL("../package.json", import.meta.url));
    return JSON.parse(readFileSync(pkgPath, "utf8")).version as string;
  } catch {
    return "0.0.0";
  }
})();

/**
 * Resolve the effective hallucination-detector mode for this session.
 * Defaults to "block" when the spec omits the guardrails block or the field.
 * Unknown string values fall back to "block" with a stderr warning so a
 * typo in the spec fails safe rather than silently downgrading guardrails.
 */
function resolveHallucinationMode(spec: CompiledSpec): HallucinationMode {
  const raw = spec.guardrails?.hallucinationDetector;
  if (!raw) return "block";
  if (raw === "warn" || raw === "block" || raw === "correct") return raw;
  process.stderr.write(
    `[agent-controller] WARNING: unknown spec.guardrails.hallucinationDetector value ` +
    `"${raw}"; falling back to "block".\n`,
  );
  return "block";
}

// Use createRequire so we can resolve CommonJS/ESM packages by name from the
// runtime package root, regardless of whether the runtime itself is ESM.
const _require = createRequire(import.meta.url);

/**
 * Build the object that goes into <cwd>/.pi/mcp.json.
 * pi-mcp-extension expects the servers keyed by name (not an array).
 */
function buildMcpJson(servers: MCPServer[]): object {
  const mcpServers: Record<string, object> = {};
  for (const s of servers) {
    // Build a minimal server config — omit undefined/empty fields so
    // pi-mcp-extension's Zod validation doesn't complain.
    const entry: Record<string, unknown> = { transport: s.transport };
    if (s.lifecycle) entry.lifecycle = s.lifecycle;
    if (s.command) entry.command = s.command;
    if (s.args && s.args.length > 0) entry.args = s.args;
    if (s.env && Object.keys(s.env).length > 0) entry.env = s.env;
    if (s.url) entry.url = s.url;
    if (s.headers && Object.keys(s.headers).length > 0) entry.headers = s.headers;
    mcpServers[s.name] = entry;
  }
  return {
    settings: { toolPrefix: "mcp" },
    mcpServers,
  };
}

/**
 * Write <cwd>/.pi/mcp.json from the ADL mcpServers list.
 *
 * If the file already exists and its contents differ from what we'd write,
 * this throws an error to avoid silently clobbering config — and, crucially,
 * to avoid a concurrency hazard: this is a single per-cwd file, so silently
 * overwriting it would let two overlapping runs from the same cwd race and
 * cross-load each other's MCP servers (codex pass 3 of slice 7.5). When the
 * contents are identical (idempotent re-run — e.g. the same
 * `agentctl run --workspace <dir>` again) we skip the write.
 *
 * Implication for `agentctl run --workspace`: the injected memory server's
 * args carry the workspace path, so reusing the SAME workspace from a cwd
 * is idempotent, but a DIFFERENT workspace from the same cwd fails loudly
 * (run each step from its own working dir, or use runtime.type:
 * local-opencode, whose per-run config dir has no such constraint).
 */
function writeMcpJson(cwd: string, servers: MCPServer[]): void {
  const dir = join(cwd, ".pi");
  const filePath = join(dir, "mcp.json");
  const payload = buildMcpJson(servers);
  const newContent = JSON.stringify(payload, null, 2);

  mkdirSync(dir, { recursive: true });
  try {
    // Atomic create (flag "wx" fails with EEXIST if the file exists) so
    // two concurrent runs from the same cwd can't both pass an existence
    // check and then clobber each other — exactly one wins the create and
    // the loser falls through to reconcile below. Closes the first-write
    // TOCTOU that a check-then-write left open. Codex pass 5 of slice 7.5.
    writeFileSync(filePath, newContent, { flag: "wx" });
    return;
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
      throw err;
    }
    // The file already existed (a prior run, or a concurrent winner) —
    // reconcile against what we would have written.
  }

  const existing = readFileSync(filePath, "utf8");
  if (existing.trim() === newContent.trim()) {
    // Identical — idempotent, nothing to do.
    return;
  }
  if (existing.trim() === "") {
    // The wx winner created the file but may not have finished writing it
    // yet (the window between O_CREAT and write) — i.e. a concurrent run
    // from the same cwd. Report THAT, not a misleading "different
    // contents". Full concurrent-same-cwd support is the per-run config
    // isolation follow-up; until then run each step from its own working
    // directory or use runtime.type: local-opencode. Codex pass 6 of
    // slice 7.5.
    throw new Error(
      `Cannot write MCP config: ${filePath} is being created by a concurrent run ` +
      `from the same working directory. Run each step from its own working ` +
      `directory, or use runtime.type: local-opencode.`,
    );
  }
  throw new Error(
    `Cannot write MCP config: ${filePath} already exists with different contents.\n` +
    `Remove or reconcile the file before running an agent with spec.mcpServers.`,
  );
}

/**
 * Materialize subagent .md files into <cwd>/.pi/agents/ so Pi's subagent
 * extension can discover them via discoverAgents(cwd, "both"|"project").
 *
 * Each subagent's entrypoint is the absolute path to a .md file in our
 * agents/ directory. We copy it to <cwd>/.pi/agents/<slug>.md.
 *
 * If the destination already exists with identical content, we no-op
 * (idempotent). If different, we overwrite — .pi/agents/ is project-local
 * and fully owned by agent-controller.
 */
function writeAgentFiles(cwd: string, subagents: Array<{ name: string; entrypoint: string }>): void {
  if (subagents.length === 0) return;
  const agentsDir = join(cwd, ".pi", "agents");
  mkdirSync(agentsDir, { recursive: true });

  // Build the set of filenames we are about to write so we can identify stale entries.
  const declaredBasenames = new Set(subagents.map((s) => basename(s.entrypoint)));

  // Remove any pre-existing .md files in agentsDir that are NOT in the declared set.
  // This enforces the ADL allowlist: only spec.subagents[] agents are present in
  // .pi/agents/, preventing the subagent extension (scoped to "project") from
  // discovering stale or injected agent files.
  if (existsSync(agentsDir)) {
    let entries: string[];
    try {
      entries = readdirSync(agentsDir);
    } catch {
      entries = [];
    }
    for (const entry of entries) {
      if (!entry.endsWith(".md")) continue;
      if (!declaredBasenames.has(entry)) {
        try {
          unlinkSync(join(agentsDir, entry));
        } catch (err) {
          // Fail closed: if we can't remove a stale agent file, the project
          // scope still exposes it to the subagent discovery, which defeats
          // the ADL allowlist enforcement. Better to abort the run with a
          // clear error than silently leave an undeclared agent reachable.
          const path = join(agentsDir, entry);
          const msg = err instanceof Error ? err.message : String(err);
          throw new Error(
            `Failed to remove stale agent file ${path}: ${msg}. ` +
            `agent-controller cannot guarantee the subagent allowlist while ` +
            `undeclared .md files remain in .pi/agents/. Delete the file ` +
            `manually or fix the permissions, then re-run.`,
          );
        }
      }
    }
  }

  for (const subagent of subagents) {
    const destPath = join(agentsDir, basename(subagent.entrypoint));
    if (existsSync(destPath)) {
      const existing = readFileSync(destPath, "utf8");
      const incoming = readFileSync(subagent.entrypoint, "utf8");
      if (existing === incoming) {
        // Identical — idempotent, skip.
        continue;
      }
      // Overwrite — agent-controller owns this directory.
    }
    copyFileSync(subagent.entrypoint, destPath);
  }
}

/**
 * Write <cwd>/.pi/agent/models.json so that child `pi` processes spawned by
 * the subagent extension route through the same Anthropic gateway as the
 * parent adapter session.
 *
 * When ANTHROPIC_BASE_URL is set, standalone `pi` does not pick it up
 * automatically — pi's anthropic provider always passes `baseURL: model.baseUrl`
 * (hardcoded to api.anthropic.com) explicitly to the SDK, overriding any env
 * var. Pi's `models.json` supports a `providers.<name>.baseUrl` override that
 * IS applied at model-resolution time, so we use that mechanism.
 *
 * We write the file to <cwd>/.pi/agent/ (not the global ~/.pi/agent/) and tell
 * child pi processes to use that directory via PI_CODING_AGENT_DIR, keeping
 * the override project-local and non-destructive to global user config.
 *
 * Returns the local agent-dir path (for PI_CODING_AGENT_DIR), or undefined
 * when ANTHROPIC_BASE_URL is not set.
 */
function writeSubagentModelsJson(cwd: string): string | undefined {
  const anthropicBaseUrl = process.env.ANTHROPIC_BASE_URL;
  if (!anthropicBaseUrl) return undefined; // nothing to override

  const localAgentDir = join(cwd, ".pi", "agent");
  const filePath = join(localAgentDir, "models.json");

  // Pi's models.json format: providers.<name>.baseUrl overrides the built-in
  // model baseUrl. An empty models list means "override-only" (no custom models
  // added, just the provider URL replaced). The auth.json must also exist so pi
  // doesn't try to write auth to a missing location.
  const payload: Record<string, unknown> = {
    providers: {
      anthropic: {
        baseUrl: anthropicBaseUrl,
      },
    },
  };
  const newContent = JSON.stringify(payload, null, 2);

  mkdirSync(localAgentDir, { recursive: true });

  if (existsSync(filePath)) {
    const existing = readFileSync(filePath, "utf8");
    if (existing.trim() === newContent.trim()) {
      return localAgentDir; // idempotent
    }
  }

  writeFileSync(filePath, newContent, "utf8");

  // Ensure auth.json exists (pi writes to it at startup; if missing it will
  // fail to start when PI_CODING_AGENT_DIR points to an empty directory).
  const authPath = join(localAgentDir, "auth.json");
  if (!existsSync(authPath)) {
    writeFileSync(authPath, "{}", "utf8");
  }

  return localAgentDir;
}

/**
 * Copy tool extensions declared in the CompiledSpec into the local agent dir's
 * extensions/ folder so child `pi` processes (spawned by the subagent extension)
 * can load them via the default PI_CODING_AGENT_DIR discovery path.
 *
 * Pi's DefaultResourceLoader auto-discovers extensions from
 * <agentDir>/extensions/<name>/index.ts (or index.js). When we redirect
 * PI_CODING_AGENT_DIR to our local .pi/agent/, we copy each tool's entrypoint
 * there named as index.ts so Pi's package-manager discovery picks it up.
 *
 * Each tool's entrypoint is copied to:
 *   <localAgentDir>/extensions/<name>/index.ts
 */
function copyToolExtensionsToLocalAgentDir(
  localAgentDir: string,
  tools: Array<{ name: string; entrypoint: string }>,
): void {
  if (tools.length === 0) return;
  const extDir = join(localAgentDir, "extensions");
  mkdirSync(extDir, { recursive: true });
  for (const tool of tools) {
    const toolDir = join(extDir, tool.name);
    mkdirSync(toolDir, { recursive: true });
    // Pi discovers extensions by looking for index.ts or index.js in each subdir
    // under <agentDir>/extensions/. Always write as index.ts regardless of the
    // original entrypoint filename so auto-discovery works.
    const destPath = join(toolDir, "index.ts");
    if (existsSync(destPath)) {
      const existing = readFileSync(destPath, "utf8");
      const incoming = readFileSync(tool.entrypoint, "utf8");
      if (existing === incoming) continue; // idempotent
    }
    copyFileSync(tool.entrypoint, destPath);
  }
}

/**
 * Build the session's system prompt. Always starts with the honesty
 * preamble (see runtime/src/honesty.ts) so the model has clear rules
 * against fabricated tool calls before it sees anything else. Persona
 * role and instructions are appended after when present.
 *
 * Returns a non-empty string in every case — there is no scenario where
 * an agent-controller session should run without the honesty rules.
 */
function buildSystemPrompt(persona?: Persona): string {
  const parts: string[] = [HONESTY_PREAMBLE];
  if (persona?.role) parts.push(`# Role\n${persona.role}`);
  if (persona?.instructions) parts.push(`# Instructions\n${persona.instructions}`);
  return parts.join("\n\n");
}

/**
 * Resolve the pi binary for the runtime process.
 *
 * Precedence:
 *  1. AC_PI_BIN env var (set by adapter when spawning subagents)
 *  2. PI_BIN env var (user override)
 *  3. runtime/node_modules/.bin/pi (sibling of THIS file's node_modules)
 *  4. System pi on PATH via `which`
 */
function resolvePiBinForRuntime(): string | undefined {
  if (process.env.AC_PI_BIN && existsSync(process.env.AC_PI_BIN)) {
    return process.env.AC_PI_BIN;
  }
  if (process.env.PI_BIN && existsSync(process.env.PI_BIN)) {
    return process.env.PI_BIN;
  }
  // Walk up from this file to find node_modules/.bin/pi
  const __filename2 = fileURLToPath(import.meta.url);
  const __dirname2 = dirname(__filename2);
  const candidates = [
    join(__dirname2, "..", "node_modules", ".bin", "pi"),
    join(__dirname2, "..", "..", "node_modules", ".bin", "pi"),
  ];
  for (const c of candidates) {
    try {
      const real = realpathSync(c);
      if (existsSync(real)) return real;
    } catch { /* not found */ }
  }
  // Fall back to system PATH
  const which = spawnSync("which", ["pi"], { encoding: "utf8" });
  if (which.status === 0) {
    const p = which.stdout.trim();
    if (p && existsSync(p)) return p;
  }
  return undefined;
}

/**
 * Resolve a source-bound extension: install it if needed, read its
 * pi.extensions manifest, and return the absolute entrypoint path.
 *
 * @param source  — e.g. "npm:pi-mcp-extension"
 * @returns absolute path to the extension entrypoint
 * @throws  when source scheme is unsupported, pi is missing, install fails,
 *          or the package declares no extensions
 */
export function resolveSourceBoundExtension(source: string): string {
  // ── Parse source ──────────────────────────────────────────────────────────
  if (!source.startsWith("npm:")) {
    throw new Error(
      `Unsupported source scheme in "${source}". ` +
      `Only "npm:" is supported at v0.1.6.`,
    );
  }
  const pkgName = source.slice("npm:".length);
  if (!pkgName) {
    throw new Error(`source "${source}" has an empty package name.`);
  }

  // ── Locate installed package ───────────────────────────────────────────────
  // Pi installs npm packages to ~/.pi/agent/npm/node_modules/<name>/
  // OR they may already be in the runtime's own node_modules (e.g. pi-mcp-extension).
  const piAgentDir = getAgentDir();
  const piManagedPath = join(piAgentDir, "npm", "node_modules", pkgName);

  // Runtime's own node_modules (resolved via require)
  let runtimeNodeModulesPath: string | undefined;
  try {
    // createRequire-based resolver anchored to this file
    const req = createRequire(import.meta.url);
    const resolved = req.resolve(`${pkgName}/package.json`);
    // resolved is /abs/.../pkgName/package.json → dirname is the package root
    runtimeNodeModulesPath = dirname(resolved);
  } catch { /* not found in runtime node_modules */ }

  // Pick the first path that has a package.json
  let pkgRoot: string | undefined;
  if (runtimeNodeModulesPath && existsSync(join(runtimeNodeModulesPath, "package.json"))) {
    pkgRoot = runtimeNodeModulesPath;
  } else if (existsSync(join(piManagedPath, "package.json"))) {
    pkgRoot = piManagedPath;
  }

  // ── Install if missing ────────────────────────────────────────────────────
  if (!pkgRoot) {
    // Safety valve: if auto-installation is disabled, emit a clear error.
    if (process.env.AGENT_CONTROLLER_NO_AUTO_INSTALL === "1") {
      throw new Error(
        `Auto-installation is disabled (AGENT_CONTROLLER_NO_AUTO_INSTALL=1). ` +
        `Run \`agentctl install ${source}\` first, then re-run.`,
      );
    }

    const piBin = resolvePiBinForRuntime();
    if (!piBin) {
      throw new Error(
        `pi binary not found. Cannot auto-install "${source}".\n` +
        `Run \`agentctl install ${source}\` manually, or install pi and retry.\n` +
        `Alternatively, set the PI_BIN environment variable to point at pi.`,
      );
    }

    const result = spawnSync(piBin, ["install", source], {
      stdio: ["ignore", "inherit", "inherit"],
      encoding: "utf8",
    });

    if (result.error) {
      throw new Error(
        `Failed to spawn pi to install "${source}": ${result.error.message}`,
      );
    }
    if (result.status !== 0) {
      throw new Error(
        `pi install ${source} failed with exit code ${result.status ?? "(signal)"}. ` +
        `Check pi output above for details.`,
      );
    }

    // After install, Pi places the package at ~/.pi/agent/npm/node_modules/<name>/
    if (existsSync(join(piManagedPath, "package.json"))) {
      pkgRoot = piManagedPath;
    } else {
      throw new Error(
        `pi install ${source} appeared to succeed but the package was not found at ` +
        `${piManagedPath}. Check Pi's installation output.`,
      );
    }
  }

  // ── Read pi.extensions from package.json ─────────────────────────────────
  const pkgJsonPath = join(pkgRoot, "package.json");
  let pkgJson: Record<string, unknown>;
  try {
    pkgJson = JSON.parse(readFileSync(pkgJsonPath, "utf8")) as Record<string, unknown>;
  } catch (err) {
    throw new Error(
      `Failed to read package.json for "${pkgName}" at ${pkgJsonPath}: ${(err as Error).message}`,
    );
  }

  const piBlock = pkgJson.pi as Record<string, unknown> | undefined;
  const extensionsArr = piBlock?.extensions as unknown[] | undefined;
  if (!Array.isArray(extensionsArr) || extensionsArr.length === 0) {
    throw new Error(
      `Package "${pkgName}" declares no Pi extensions (pi.extensions is absent or empty in its package.json). ` +
      `Cannot use it as a source-bound extension.`,
    );
  }

  const relEntrypoint = extensionsArr[0] as string;
  return resolve(pkgRoot, relEntrypoint);
}

/**
 * runSession assembles a Pi session from the CompiledSpec, subscribes to
 * its events, submits the task as the initial prompt, and resolves when
 * the session ends. emit is invoked once per outgoing wire event.
 *
 * Extension configuration is passed to extensions via the
 * AGENT_CONTROLLER_EXT_CONFIG env var (JSON object keyed by extension name).
 * This is a deliberate MVP convention; v0.2 will migrate to Pi's
 * settingsManager once that API stabilizes (tracked in spec §12 research
 * item 5).
 */
export async function runSession(
  spec: CompiledSpec,
  emit: (ev: WireEvent) => void,
): Promise<void> {
  // Emit deprecation warning if spec.installs[] is non-empty.
  // spec.installs[] is deprecated in favour of spec.extensions[].source.
  if (spec.installs && spec.installs.length > 0) {
    process.stderr.write(
      "[agent-controller] DEPRECATION WARNING: `spec.installs[]` is deprecated; " +
      "prefer `spec.extensions[].source` instead.\n",
    );
  }

  // Resolve source-bound extensions (spec.extensions[].source is set).
  // For each such entry, install the package if needed and determine its
  // entrypoint; then treat it exactly like a locally-resolved extension.
  const resolvedExtensionPaths = spec.extensions.map((e) => {
    if (e.source) {
      // Source-bound: install if needed and resolve entrypoint.
      return resolveSourceBoundExtension(e.source);
    }
    // Normal registry-resolved extension.
    return e.entrypoint;
  });

  const entrypointPaths = [
    // Pi-builtin tools (bash/read/edit/write) have no entrypoint — they
    // contribute only to the active-tool allowlist (handled below where
    // toolAllowlist is built from spec.tools[].name). Filter them out of
    // additionalExtensionPaths so we don't try to load nonexistent paths.
    ...spec.tools.filter((t) => !t.builtin && t.entrypoint).map((t) => t.entrypoint!),
    ...resolvedExtensionPaths,
  ];

  // If spec.mcpServers is non-empty, write <cwd>/.pi/mcp.json and add the
  // pi-mcp-extension entrypoint to the loader paths. This must happen BEFORE
  // DefaultResourceLoader is constructed so the extension is loaded during reload().
  if (spec.mcpServers && spec.mcpServers.length > 0) {
    writeMcpJson(process.cwd(), spec.mcpServers);
    // Only add pi-mcp-extension if it wasn't already loaded via a source-bound
    // spec.extensions[] entry (e.g. { name: "pi-mcp-extension", source: "npm:..." }).
    // Deduplication prevents the extension from loading twice, which would cause
    // a "ResourceCollision" error from DefaultResourceLoader.
    const mcpAlreadyLoaded = spec.extensions.some(
      (e) => e.source === "npm:pi-mcp-extension" || e.name === "pi-mcp-extension",
    );
    if (!mcpAlreadyLoaded) {
      // Resolve the pi-mcp-extension entrypoint from the runtime's own node_modules.
      // The package.json "pi.extensions" field declares "./src/index.ts" as the
      // extension entrypoint — Pi loads .ts files via jiti (no build step needed).
      const mcpPkg = _require.resolve("pi-mcp-extension/src/index.ts");
      entrypointPaths.push(mcpPkg);
    }
  }

  // If spec.subagents is non-empty, materialize the .md files into
  // <cwd>/.pi/agents/ so Pi's subagent extension discovers them, then
  // add the vendored subagent extension entrypoint to the loader paths.
  // The subagent extension registers the "subagent" tool which the parent
  // agent uses to invoke child agents.
  if (spec.subagents && spec.subagents.length > 0) {
    // Subagents always have an entrypoint (the .md path). Narrow the type
    // for writeAgentFiles so it can use entrypoint directly without
    // having to guard for undefined.
    const resolvedSubagents = spec.subagents
      .filter((s): s is typeof s & { entrypoint: string } => Boolean(s.entrypoint));
    writeAgentFiles(process.cwd(), resolvedSubagents);
    // Write <cwd>/.pi/agent/models.json with providers.anthropic.baseUrl so
    // child `pi` processes use the same gateway as the parent session.
    // This only makes sense when ANTHROPIC_BASE_URL is set; when using the
    // default Anthropic SDK (just ANTHROPIC_API_KEY), skip the model override.
    const localAgentDir = writeSubagentModelsJson(process.cwd());
    if (localAgentDir) {
      process.env.AC_SUBAGENT_AGENT_DIR = localAgentDir;
    }

    // Copy parent's tool extensions into the local agent dir so child pi
    // sessions can discover and activate them (e.g. get_time).
    // This must run whenever subagents are declared, regardless of whether
    // ANTHROPIC_BASE_URL is set — tools must be available even with a bare
    // ANTHROPIC_API_KEY setup.
    {
      // Use the localAgentDir if available (custom gateway path) or fall back
      // to a default local agent dir so child pi can always find the tools.
      const agentDirForTools = localAgentDir ?? join(process.cwd(), ".pi", "agent");
      // Only entrypoint-backed tools can be copied (builtins are Pi-shipped).
      const resolvedTools = spec.tools
        .filter((t): t is typeof t & { entrypoint: string } => !t.builtin && Boolean(t.entrypoint));
      copyToolExtensionsToLocalAgentDir(agentDirForTools, resolvedTools);
    }
    // Expose the `pi` CLI binary path via AC_PI_BIN so the vendored subagent
    // extension can spawn the correct `pi` when `pi` is not on the system PATH.
    // The package's exports map blocks deep-path _require.resolve, so we look
    // for the .bin/pi symlink relative to node_modules, then fall back to the
    // known conventional path for our vendored package.
    //
    // Strategy: walk up from our own file to find a node_modules/.bin/pi that
    // resolves to a real file, or construct the known path from the package we
    // import from. We use __filename (adapter.ts/adapter.js) as the anchor.
    {
      const __filename2 = fileURLToPath(import.meta.url);
      const __dirname2 = dirname(__filename2);
      // Candidate paths to check, from most-local to most-global:
      const piCandidates = [
        join(__dirname2, "..", "node_modules", ".bin", "pi"),             // runtime/node_modules/.bin/pi
        join(__dirname2, "..", "..", "node_modules", ".bin", "pi"),       // root/node_modules/.bin/pi
        join(__dirname2, "..", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "cli.js"),
        join(__dirname2, "..", "node_modules", "@mariozechner", "pi-coding-agent", "dist", "cli.js"),
      ];
      for (const candidate of piCandidates) {
        try {
          // Use realpathSync to resolve symlinks (like .bin/pi -> ../pkg/dist/cli.js)
          const real = realpathSync(candidate);
          if (existsSync(real)) {
            process.env.AC_PI_BIN = real;
            break;
          }
        } catch {
          // not found, try next
        }
      }
    }
    // Resolve the vendored subagent extension. Try the source-tree layout
    // FIRST (../../extensions/subagent relative to runtime/dist) so a
    // developer's edits to extensions/subagent/* take effect on the next
    // run without rebuilding; only fall back to the in-package bundled
    // copy (dist/extensions/subagent, populated by
    // scripts/copy-vendored-extensions.mjs) when the source-tree path
    // doesn't exist — which is the case for npm-installed adapters where
    // only dist/ ships. Push the in-package path if neither exists; Pi
    // surfaces a clear file-not-found at session start. Codex passes 3+4
    // of slice 4.2 caught this.
    const __filename = fileURLToPath(import.meta.url);
    const __dirname = dirname(__filename);
    const subagentSourceTree = resolve(
      __dirname,
      "..",
      "..",
      "extensions",
      "subagent",
      "entrypoint.ts",
    );
    const subagentInPackage = resolve(
      __dirname,
      "extensions",
      "subagent",
      "entrypoint.ts",
    );
    const subagentExtPath = existsSync(subagentSourceTree)
      ? subagentSourceTree
      : subagentInPackage;
    entrypointPaths.push(subagentExtPath);
  }

  // Populate the extension-config env var BEFORE constructing the loader
  // or session — extensions read it inside their default export, which
  // executes during session construction.
  //
  // Include both spec.tools[].config and spec.extensions[].config entries,
  // keyed by name. Tools are loaded as Pi extension entrypoints and have no
  // other config channel. Extensions win on name collision (last-write), but
  // we emit a warning so the conflict is visible.
  const extConfig: Record<string, unknown> = {};
  for (const t of spec.tools) {
    if (t.config) extConfig[t.name] = t.config;
  }
  for (const e of spec.extensions) {
    if (e.config) {
      if (e.name in extConfig) {
        process.stderr.write(
          `[agent-controller] WARNING: tool and extension share the name "${e.name}"; extension config wins.\n`,
        );
      }
      extConfig[e.name] = e.config;
    }
  }
  process.env.AGENT_CONTROLLER_EXT_CONFIG = JSON.stringify(extConfig);

  // Skill paths: each skill entrypoint is the absolute path to SKILL.md.
  // Pi's loadSkillsFromDir treats a directory containing SKILL.md as a skill
  // root, and additionalSkillPaths accepts either files or directories.
  // We pass the parent directory of each SKILL.md so Pi uses its standard
  // directory-based discovery rule.
  //
  // Use path.dirname() (already imported from node:path) for platform-safe
  // parent-dir derivation — avoids manual split("/") which breaks on Windows.
  const skillDirs = spec.skills
    // Skills always have an entrypoint (the SKILL.md path) — no source/builtin
    // shortcut applies to skills. Filter defensively in case the compiler ever
    // emits a malformed ResolvedRef.
    .filter((s): s is typeof s & { entrypoint: string } => Boolean(s.entrypoint))
    .map((s) =>
      // s.entrypoint = "<root>/skills/<name>/SKILL.md"
      // dirname gives "<root>/skills/<name>" which Pi treats as a skill root.
      dirname(s.entrypoint),
    );

  // Pi's formatSkillsForPrompt only injects each skill's frontmatter
  // {name, description, filePath} into the system prompt. The body is
  // lazy-loaded by the agent via the `read` tool when it decides the skill
  // matches the task — but our ADL allowlist typically excludes `read`,
  // and even when it doesn't, the model frequently won't bother loading
  // skills it could be following.
  //
  // For ADL-declared skills the user clearly opted in, so we additionally
  // inline each skill's body (everything after the YAML frontmatter) into
  // appendSystemPrompt. The body is then unconditionally active for the
  // session, matching the declarative-governance semantics users expect.
  // The lazy/read-tool path still works for skills the model wants to
  // re-read or inspect mid-task.
  const skillBodies: string[] = [];
  for (const s of spec.skills) {
    if (!s.entrypoint) continue; // defensive: skip malformed skill refs
    try {
      const raw = readFileSync(s.entrypoint, "utf8");
      // Strip YAML frontmatter: leading "---\n...\n---\n".
      const stripped = raw.replace(/^---\s*\n[\s\S]*?\n---\s*\n?/, "");
      if (stripped.trim().length > 0) {
        skillBodies.push(wrapSkillBody(s.name, stripped.trim()));
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      process.stderr.write(
        `[agent-controller] WARNING: could not read skill ${s.name} at ${s.entrypoint}: ${msg}\n`,
      );
    }
  }
  // appendSystemPrompt is string[] (Pi joins with "\n\n"); pass an array.
  const skillsAppend = skillBodies.length > 0 ? skillBodies : undefined;

  // DefaultResourceLoader requires cwd and agentDir to resolve paths;
  // we use the process working directory and a local .agent-controller dir
  // as sensible defaults for the MVP runner.
  //
  // systemPrompt is derived from spec.persona (if present) and passed here so
  // the resource loader serves it to Pi — the resourceLoader.getSystemPrompt()
  // method is called internally by createAgentSession when building the agent.
  //
  // noExtensions: true prevents DefaultResourceLoader.reload() from scanning
  // ~/.pi/agent/extensions/ and <cwd>/.pi/extensions/ for ambient extensions.
  // Without this, extensions outside the ADL allowlist would silently load even
  // when noTools: "builtin" is set. We still pass additionalExtensionPaths so
  // the spec-declared tools and extensions are registered normally.
  //
  // noSkills: true suppresses Pi's default skill scan from ~/.pi/agent/skills/
  // and <cwd>/.pi/skills/. Only ADL-declared skills (via additionalSkillPaths)
  // are loaded, keeping the environment hermetic and consistent with the ADL
  // allowlist principle used for extensions.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const resourceLoader = new DefaultResourceLoader({
    cwd: process.cwd(),
    agentDir: process.cwd() + "/.agent-controller",
    additionalExtensionPaths: entrypointPaths,
    additionalSkillPaths: skillDirs,
    systemPrompt: buildSystemPrompt(spec.persona),
    // appendSystemPrompt: each declared skill's body is concatenated and
    // appended to the system prompt verbatim. This makes skills *active by
    // default* rather than lazy-loadable via the read tool — matches what
    // users expect when they declare a skill in spec.skills[].
    appendSystemPrompt: skillsAppend,
    noExtensions: true,
    // noSkills suppresses the default skill scan from ~/.pi/agent/skills/ and
    // <cwd>/.pi/skills/. We always set it to keep the environment hermetic;
    // ADL-declared skills reach the loader exclusively via additionalSkillPaths.
    noSkills: true,
  } as any);

  // Must call reload() explicitly: createAgentSession only calls reload() when
  // it constructs DefaultResourceLoader itself. When we pass a pre-built loader
  // it assumes the caller has already loaded it. Without this call no extensions
  // (get_time, audit-log) are active.
  await (resourceLoader as any).reload();

  // Fail fast if any ADL-declared entrypoint failed to load. Pi records load
  // failures in extensionsResult.errors as { path, error } pairs but continues
  // silently — without this check, Pi's tool allowlist would simply contain a
  // name that maps to no implementation, and the run would proceed without
  // the declared tools.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const extensionsResult = (resourceLoader as any).getExtensions?.() as
    | { errors?: Array<{ path: string; error: string }> }
    | undefined;
  if (extensionsResult?.errors?.length) {
    const declared = new Set(entrypointPaths);
    const relevant = extensionsResult.errors.filter((e) => declared.has(e.path));
    if (relevant.length > 0) {
      const summary = relevant
        .map((e) => `  - ${e.path}: ${e.error}`)
        .join("\n");
      throw new Error(
        `Failed to load ADL-declared extensions:\n${summary}\n` +
        `These are listed in spec.tools[]/spec.extensions[] but the runtime ` +
        `could not load their entrypoints. Check the manifest's entrypoint path.`,
      );
    }
  }

  // E2E test hook: when AGENT_CONTROLLER_USE_FAKE_PROVIDER=1 and a fake
  // has been installed via the testing helper, use it in place of the
  // real provider. The env var alone does nothing without a script.
  // See runtime/src/testing/fake-provider.ts.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let model: any = resolveFakeModelIfRequested();
  if (!model) {
    // getBuiltinModel uses branded literal generics; cast provider/name to `any`
    // since at runtime they come from user YAML and cannot be statically typed.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    model = getBuiltinModel(spec.model.provider as any, spec.model.name as any) as any;
  }
  if (!model) {
    throw new Error(
      `Model ${spec.model.provider}/${spec.model.name} not found. ` +
      `Check provider and model name; pi-ai may not support this combination.`,
    );
  }

  // If ANTHROPIC_BASE_URL is set (e.g. a corporate LLM gateway or local
  // dev proxy), override the model's hardcoded baseUrl so Pi routes
  // requests there. Skipped for the fake provider (its baseUrl points
  // to http://fake.invalid which is never dialed). We also ensure
  // ANTHROPIC_API_KEY is non-empty so Pi's env-key check passes; the
  // proxy itself handles auth, so any placeholder value works.
  if (process.env.ANTHROPIC_BASE_URL && spec.model.provider === "anthropic" && model.api !== "fake-test") {
    model.baseUrl = process.env.ANTHROPIC_BASE_URL;
    if (!process.env.ANTHROPIC_API_KEY) {
      process.env.ANTHROPIC_API_KEY = "proxy-managed";
    }
  }

  const sessionId = `s_${Date.now().toString(36)}`;

  // Slice 5.3: initialize OTel for this adapter run. The factory returns
  // a no-op when spec.observability.tracing is false OR the OTLP endpoint
  // env var is unset — same two-condition gate as the host (cli/internal/
  // observability/otel.go). When live, the factory also opens the
  // `agent.session` root span nested under the host's TRACEPARENT
  // (slice 5.2 env injection).
  //
  // Span-side session id prefers `spec.sessionId` (the persistent
  // resumed id passed via --resume <id>) so adapter spans index under
  // the same `agent_controller.session.id` the host root span uses.
  // The wire-event `sessionId` stays ephemeral (s_<timestamp>) — wire
  // semantics predate --resume and changing them would break existing
  // consumers. Codex pass 5 of slice 5.3 caught the divergence.
  const tracing: AdapterTracing = initAdapterTracing({
    spec,
    sessionId: spec.sessionId ?? sessionId,
    packageVersion: RUNTIME_PACKAGE_VERSION,
  });

  // Wrap the caller's emit so every wire event carries the v1alpha1
  // namespace + traceparent (slice 5.2 envelope fields) when tracing is
  // active. When off, the wrapper is the identity — no per-event cost.
  // Function-param reassignment is intentional: every subsequent emit()
  // in this function should go through the wrapper, and threading a
  // separate name through the body would touch dozens of call sites
  // without changing semantics.
  // eslint-disable-next-line no-param-reassign
  emit = wrapEmitWithTracing(emit, tracing);

  // Slice 5.3 codex pass 2: ensure the agent.session span gets flushed
  // even when setup (createAgentSession / bindExtensions / etc.)
  // throws BEFORE reaching the inner try/catch around session.prompt.
  // Without this outer try/catch, an early throw escapes runSession
  // with the session span still open — the failure never reaches
  // telemetry and the BatchSpanProcessor shuts down with an unended
  // root.
  try {

  // When spec.sessionId is set (via --resume <id>), open/continue the named
  // persistent session under <agentDir>/sessions/agentctl/<id>/.
  // On first run the dir is empty so continueRecent creates a new session;
  // on subsequent runs it picks up the most-recently-modified file in that dir.
  // When spec.sessionId is absent, use an in-memory session (default Pi behaviour).
  const sessionManager = spec.sessionId
    ? SessionManager.continueRecent(
        process.cwd(),
        join(getAgentDir(), "sessions", "agentctl", spec.sessionId),
      )
    : SessionManager.inMemory(process.cwd());

  // Enforce the ADL tool allowlist by passing the spec-declared tool names as
  // Pi's `tools` option. This is *both* the activation list and the allowlist:
  //   - Pi's built-in tools (read, bash, edit, write) are NOT in the list, so
  //     they are filtered out at the registry level.
  //   - Only tools whose names appear in this array become active in the
  //     model's tool catalog. Extension-registered tools whose names match
  //     `spec.tools[].name` get activated; others stay registered but inactive.
  //
  // IMPORTANT — MCP interaction:
  //   When spec.mcpServers is non-empty, pi-mcp-extension registers MCP tools
  //   AFTER session start (during the session_start event, after connecting to
  //   the MCP server). Pi's _refreshToolRegistry filters tools against
  //   `allowedToolNames` (derived from the `tools` option) when adding them to
  //   `_toolRegistry`. With `tools: []`, `allowedToolNames` becomes an empty
  //   Set (truthy), so `isAllowedTool(name)` always returns false — MCP tools
  //   never enter `_toolRegistry` and `setActiveTools(["mcp_…"])` silently
  //   no-ops. The model receives zero tools and hallucinates XML <invoke> tags.
  //
  //   Fix: when mcpServers is non-empty, use `noTools: "builtin"` to suppress
  //   Pi's built-in read/bash/edit/write tools without setting `allowedToolNames`
  //   to an empty Set. With `allowedToolNames = undefined`, every registered
  //   tool can enter `_toolRegistry`, and Pi's "new tools" auto-activation logic
  //   (the `!options?.activeToolNames` branch in _refreshToolRegistry) activates
  //   MCP tools as they register. Declared spec.tools entrypoints also load via
  //   additionalExtensionPaths and auto-activate the same way.
  //
  //   Security note: `noExtensions: true` + `additionalExtensionPaths` (only
  //   declared entrypoints) already enforce the ADL allowlist at the loading
  //   level. The `tools:` option is redundant for that purpose when mcpServers
  //   is present; `noTools: "builtin"` is sufficient.
  //
  // When subagents are declared, automatically include the "subagent" tool
  // (registered by the vendored subagent extension) in the parent's allowlist.
  // Without this, the parent model would not be able to call the subagent tool
  // even though the extension is loaded.
  const hasMcpServers = spec.mcpServers && spec.mcpServers.length > 0;
  const toolAllowlist = [
    ...spec.tools.map((t) => t.name),
    ...(spec.subagents && spec.subagents.length > 0 ? ["subagent"] : []),
  ];
  // When the fake provider is active, hand pi a ModelRuntime whose catalog
  // resolves the model's provider to the scripted faux. Overriding the model
  // alone is not enough: ModelRuntime.prepareRequest() resolves the streaming
  // provider by `model.provider`, so without this the "hermetic" test reaches
  // the real Anthropic endpoint. Undefined in production, where the option is
  // omitted and pi builds its default runtime.
  const fakeModelRuntime = await resolveFakeModelRuntimeIfRequested();

  const { session } = await createAgentSession({
    model,
    ...(fakeModelRuntime ? { modelRuntime: fakeModelRuntime } : {}),
    resourceLoader,
    // When MCP servers are declared: omit `tools` so allowedToolNames stays
    // undefined (MCP tools can enter the registry and auto-activate), and use
    // noTools: "builtin" to suppress Pi's built-in read/bash/edit/write tools.
    // When no MCP servers: pass tools: toolAllowlist as before (explicit
    // allowlist that blocks builtins and activates only declared tool names).
    ...(hasMcpServers
      ? { noTools: "builtin" as const }
      : { tools: toolAllowlist }),
    sessionManager,
  });

  // Forward spec.model.temperature to the provider via session.agent.onPayload.
  // Pi's CreateAgentSessionOptions has no temperature field; onPayload is the
  // documented hook for injecting per-request provider parameters. The hook
  // receives the raw provider payload (e.g. the Anthropic messages body) and
  // returns a (possibly modified) copy.
  //
  // IMPORTANT: Anthropic's API rejects requests where `temperature` is set to
  // anything other than 1 while `thinking` (extended thinking) is enabled.
  // Pi enables thinking by default for Claude Sonnet. We therefore *only*
  // apply spec.model.temperature when thinking is NOT in the payload — when
  // it is, Pi's default temperature (which is compatible with thinking) wins
  // and the ADL temperature is silently ignored. This is a documented MVP
  // limitation; v0.2 will add a way to disable thinking from ADL.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  if (spec.model.temperature !== undefined) {
    const specTemperature = spec.model.temperature;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const prevOnPayload = (session as any).agent.onPayload as
      | ((payload: unknown, model: unknown) => unknown)
      | undefined;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (session as any).agent.onPayload = async (payload: any, m: any) => {
      const base = prevOnPayload ? await prevOnPayload(payload, m) : payload;
      if (typeof base === "object" && base !== null && !(base as any).thinking) {
        (base as any).temperature = specTemperature;
      }
      return base;
    };
  }

  // Track terminal failure conditions so the final session.ended reflects
  // whether Pi actually completed or errored. Pi reports provider/runtime
  // failures via assistant message_end with stopReason: "error" (and via
  // agent_end with an error field). Without this tracking, session.prompt
  // resolves normally and we'd report a failed run as "completed".
  let errorMessage: string | undefined;

  // Hallucination-guardrail state. `hallucinationMode` is fixed for the
  // session; `correctionRequested` is flipped when the subscriber sees a
  // hallucinated message_end and mode is "correct"; `correctionSent` is
  // flipped once the corrective re-prompt has actually been dispatched
  // post-turn, so we cap at one correction per outer task invocation.
  const hallucinationMode = resolveHallucinationMode(spec);
  let correctionRequested = false;
  let correctionSent = false;

  // Register the subscriber BEFORE the first emit and BEFORE prompting,
  // so no Pi event is missed.
  session.subscribe((piEvent: any) => {
    // Hallucinated tool-call detection runs ahead of translatePiEvent for
    // message_end so warn/correct modes can scrub the offending XML out of
    // the user-visible `message` event before it's emitted.
    //
    // The detector must be gated on role === "assistant". In correct mode
    // we send CORRECTION_PROMPT back via session.prompt(); that prompt
    // mentions <invoke>/<function_calls>/<Skill> by name as part of its
    // instructions to the model, so Pi's user-role message_end carries
    // exactly the XML patterns we detect. Without the role gate, the
    // runtime flags its own corrective re-prompt as a hallucination
    // (caught by codex review of v0.1.10).
    let scrubbedAssistantText: string | undefined;
    let hallucinationFindings: string[] = [];
    if (piEvent.type === "message_end" && piEvent.message?.role === "assistant") {
      const assistantText = extractAssistantText(piEvent.message);
      if (assistantText) {
        hallucinationFindings = detectHallucinatedToolCalls(assistantText);
        if (hallucinationFindings.length > 0 && hallucinationMode !== "block") {
          scrubbedAssistantText = stripHallucinationXml(assistantText).text;
        }
      }
    }

    // Emit the translated wire event. For warn/correct mode we substitute
    // a scrubbed `message` event in place of the default translation so
    // downstream consumers see clean assistant prose.
    //
    // Note: we deliberately do NOT mutate piEvent.message.content. Pi's
    // conversation state retains the original (unscrubbed) assistant
    // message. Two reasons:
    //   (a) The audit log + persisted session must reflect what actually
    //       happened. Scrubbing is a display concern, not a rewrite-history
    //       concern.
    //   (b) In correct mode the corrective re-prompt explicitly tells the
    //       model "your previous message contained fabricated tool-call
    //       XML." If we scrubbed the conversation history, the model would
    //       see a clean previous message and be unable to understand what
    //       it's being asked to correct.
    // Codex pass 4 flagged this as a possible issue; the design is
    // intentional. See examples/guardrails-correct.yaml for the flow.
    if (scrubbedAssistantText !== undefined) {
      const role = piEvent.message?.role ?? "unknown";
      emit(stamp(sessionId, "message", { text: scrubbedAssistantText, role }));
      // Note: scrubbed text is still the captured completion for tracing
      // purposes — the model produced it, even if we hide the
      // hallucinated XML from downstream consumers. Capturing the clean
      // version is the closer fit to what the model "intended" and
      // matches what wire-event consumers see.
      tracing.onAssistantMessage(role, scrubbedAssistantText);
    } else {
      const translated = translatePiEvent(sessionId, piEvent);
      if (translated) emit(translated);
    }

    // Slice 5.3: feed the OTel tracer with the same event stream.
    // Calls are no-ops when tracing is off.
    //
    // We dispatch on the events session.subscribe actually delivers —
    // pi-agent-core's AgentEvent union, NOT the extension-runner hooks
    // (before_provider_request / after_provider_response) which never
    // reach a session subscriber. Each agent turn is bounded by
    // turn_start / turn_end; tool calls fire between them and nest
    // inside the active LLM span. Codex pass 1 of slice 5.3 caught the
    // original (and silently dead) extension-hook wiring.
    switch (piEvent.type) {
      case "turn_start":
        tracing.onLLMStart();
        break;
      case "turn_end":
        tracing.onLLMEnd({
          inputTokens: piEvent.message?.usage?.input,
          outputTokens: piEvent.message?.usage?.output,
          stopReason: piEvent.message?.stopReason,
        });
        break;
      case "tool_execution_start":
        tracing.onToolStart(piEvent.toolName, piEvent.toolCallId, piEvent.args);
        break;
      case "tool_execution_end":
        tracing.onToolEnd(piEvent.toolCallId, piEvent.isError === true, piEvent.result);
        break;
      case "message_end": {
        // captureContent path: feed assistant text into the active LLM
        // span. The scrubbed-text branch above already handled the
        // hallucination-detector case.
        if (scrubbedAssistantText === undefined && piEvent.message?.role === "assistant") {
          const text = extractAssistantText(piEvent.message);
          if (text) tracing.onAssistantMessage("assistant", text);
        }
        break;
      }
    }

    if (piEvent.type === "message_end") {
      const stop = piEvent.message?.stopReason;
      if (stop === "error" || stop === "aborted") {
        const detail = piEvent.message?.errorMessage
          ? `: ${piEvent.message.errorMessage}`
          : "";
        errorMessage ??= `Pi message ended with stopReason=${stop}${detail}`;
      }

      if (hallucinationFindings.length > 0) {
        const baseMessage =
          `Assistant message contains fabricated tool-call XML: ${hallucinationFindings.join(", ")}. ` +
          `The model is hallucinating tool invocations instead of using the runtime's ` +
          `tool channel. Consider strengthening the persona, narrowing the active skill ` +
          `set, or adding the missing tool to spec.tools[].`;

        if (hallucinationMode === "block") {
          // Legacy v0.1.8 behavior: hard failure. Wire `error`, errorMessage
          // set so session ends with reason=error and the CLI exits non-zero.
          emit(stamp(sessionId, "error", {
            kind: "hallucinated_tool_call",
            mode: hallucinationMode,
            message: baseMessage,
            patterns: hallucinationFindings,
          }));
          errorMessage ??= `Assistant message contained fabricated tool-call XML (${hallucinationFindings.join(", ")})`;
        } else {
          // warn / correct: non-fatal. Emit a `warning` event so listeners
          // can surface the finding without ending the session.
          emit(stamp(sessionId, "warning", {
            kind: "hallucinated_tool_call",
            mode: hallucinationMode,
            message: baseMessage,
            patterns: hallucinationFindings,
          }));
          if (hallucinationMode === "correct" && !correctionSent) {
            // The actual re-prompt happens after session.prompt() returns
            // from the current turn — see the post-prompt block below.
            correctionRequested = true;
          }
        }
      }
    }
    if (piEvent.type === "agent_end" && piEvent.error) {
      errorMessage ??= String(piEvent.error?.message ?? piEvent.error);
    }
  });

  emit(stamp(sessionId, "session.started", {
    agentName: spec.metadata.name,
    model: spec.model,
  }));

  // Fire the Pi `session_start` lifecycle event by calling bindExtensions().
  // This is REQUIRED for extensions to initialise — without it pi-mcp-extension
  // never connects to MCP servers and never registers tools.
  //
  // bindExtensions() accepts an ExtensionBindings object whose fields are all
  // optional. We cast through `any` to avoid reconstructing the full
  // ExtensionUIContext interface (which has ~30 methods). We only provide
  // `notify` so that MCP start-up messages and errors are visible on stderr
  // without disturbing the wire stream. All other UI methods stay as Pi's
  // built-in noOpUIContext (set during ExtensionRunner construction).
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  await (session as any).bindExtensions({
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    uiContext: { notify: (msg: string) => process.stderr.write(`[pi-mcp] ${msg}\n`) } as any,
  });

  try {
    // session.prompt() is async and resolves when the agent finishes the
    // turn. This is the correct way to run a single-turn task per the
    // Pi SDK examples (see examples/sdk/01-minimal.ts).
    await session.prompt(spec.task);

    // Correct mode: if the model fabricated tool-call XML during the
    // primary turn, send one corrective re-prompt and let it redo its
    // last message without the XML. Pi doesn't expose a mid-stream
    // injection API, so the corrective turn shows up as a separate
    // message in the wire stream — that's intentional and visible.
    //
    // Skip the correction if a terminal failure already happened during
    // the primary turn (errorMessage set). The session is going to end
    // with reason=error regardless; making another model call would
    // burn tokens after a cancellation/provider-error without changing
    // the outcome. Codex pass 5 flagged this.
    if (correctionRequested && !correctionSent && !errorMessage) {
      correctionSent = true;
      await session.prompt(CORRECTION_PROMPT);
    }
  } catch (err) {
    errorMessage ??= err instanceof Error ? err.message : String(err);
  } finally {
    session.dispose();
  }

  if (errorMessage) {
    tracing.onError(errorMessage);
    emit(stamp(sessionId, "error", { message: errorMessage }));
    emit(stamp(sessionId, "session.ended", { reason: "error", message: errorMessage }));
    // Close the session span LAST so the BatchSpanProcessor has every
    // child span queued before it shuts down. await ensures the OTel
    // flush completes before runSession returns — otherwise the
    // adapter subprocess can exit before the OTLP exporter ships its
    // batch (and BatchSpanProcessor.shutdown() force-flushes on a
    // short timeout we own in observability.ts).
    await tracing.end("error", errorMessage);
  } else {
    emit(stamp(sessionId, "session.ended", { reason: "completed" }));
    await tracing.end("completed");
  }
  } catch (err) {
    // Codex pass 2 of slice 5.3: setup-failure escape path. Triggered
    // when something between the OTel init (above) and the inner
    // session.prompt try/catch (below) throws — e.g. createAgentSession,
    // bindExtensions, or an MCP server config failure. The inner
    // try/catch around session.prompt() already converts its own
    // errors to `errorMessage` and runs the happy-path block above,
    // so this catch only fires for the EARLY-setup escape.
    const message = err instanceof Error ? err.message : String(err);
    tracing.onError(message);
    // Emit a wire-stream terminator so a CLI consumer doesn't see a
    // session.started with no matching session.ended. We must NOT
    // re-throw after a successful emit — index.ts's outer catch would
    // then append a second `error` event after the terminal
    // session.ended (codex pass 3 of slice 5.3 caught this duplicate).
    // index.ts already tracks `sawError` via the wire-event listener
    // and exits non-zero when it sees the `error` we just emitted,
    // so re-throwing is unnecessary for exit-code propagation.
    let emittedTerminator = false;
    try {
      emit(stamp(sessionId, "error", { message }));
      emit(stamp(sessionId, "session.ended", { reason: "error", message }));
      emittedTerminator = true;
    } catch {
      // Swallow: keep the original throw intact for index.ts so the
      // process at least exits non-zero with stderr context. This
      // branch is only reached if the emit pipe itself is broken.
    }
    await tracing.end("error", message);
    if (!emittedTerminator) {
      throw err;
    }
  }
}

/**
 * Slice 5.3: overlay the v1alpha1 envelope fields (apiVersion +
 * traceparent) on outgoing wire events when tracing is active.
 *
 * Returns `emit` unchanged when the tracing object is a no-op (no
 * traceparent available) — the wrapper would still work in that case
 * but the per-event `getTraceparent()` call + spread are wasted, and
 * legacy v0.x consumers should keep seeing the legacy envelope shape
 * unless tracing actually fired.
 */
function wrapEmitWithTracing(
  base: (ev: WireEvent) => void,
  tracing: AdapterTracing,
): (ev: WireEvent) => void {
  if (!tracing.getTraceparent()) return base;
  return (ev: WireEvent) => {
    const traceparent = tracing.getTraceparent();
    if (!traceparent) {
      base(ev);
      return;
    }
    base({
      ...ev,
      apiVersion: EVENTS_API_VERSION_V1ALPHA1,
      traceparent,
    });
  };
}

function translatePiEvent(sessionId: string, piEvent: any): WireEvent | undefined {
  switch (piEvent.type) {
    // Pi emits tool_execution_start / tool_execution_end to session subscribers.
    // The tool_call / tool_result labels are extension-hook events, not subscriber events.
    case "tool_execution_start":
      return stamp(sessionId, "tool.call", {
        toolName: piEvent.toolName,
        callId: piEvent.toolCallId,
        args: piEvent.args,
      });
    case "tool_execution_end":
      return stamp(sessionId, "tool.result", {
        callId: piEvent.toolCallId,
        isError: piEvent.isError,
        content: piEvent.result,
      });
    case "message_end": {
      const text = extractAssistantText(piEvent.message);
      const role = piEvent.message?.role ?? "unknown";
      return stamp(sessionId, "message", { text, role });
    }
    case "before_provider_request":
      return stamp(sessionId, "model.request", { messageCount: piEvent.messageCount });
    case "after_provider_response":
      return stamp(sessionId, "model.response", {
        tokensIn: piEvent.tokensIn,
        tokensOut: piEvent.tokensOut,
        finishReason: piEvent.finishReason,
      });
    default:
      return undefined;
  }
}

/**
 * Extract the plain assistant text from a Pi AppMessage. Pi's message
 * content can be a string or an array of {type,text} blocks; we collect
 * the text blocks. Returns "" when there is no text content.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function extractAssistantText(message: any): string {
  const raw = message?.content;
  if (typeof raw === "string") return raw;
  if (Array.isArray(raw)) {
    return raw
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .filter((c: any) => c.type === "text")
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      .map((c: any) => c.text as string)
      .join("");
  }
  return "";
}
