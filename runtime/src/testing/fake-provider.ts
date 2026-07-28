/**
 * Fake LLM provider for hermetic E2E tests.
 *
 * Closes debt #5 (fake-provider E2E for hermetic CI) and unlocks Phase 2
 * of the v0.2 plan — the opencode adapter needs to assert wire-event
 * parity against the same example specs without burning real model
 * credentials on every CI run.
 *
 * This module is a thin convenience layer over pi-ai's built-in
 * `registerFauxProvider`. We add:
 *
 *   - A module-level singleton (`activeFake`) that holds the current
 *     registration so the runtime adapter can swap models at session
 *     start without the test code having to pass the registration in.
 *   - Re-exports of the pi-ai faux helpers (`fauxText`, `fauxToolCall`,
 *     `fauxAssistantMessage`) so tests have a single import path.
 *   - `resolveFakeModelIfRequested(model)` for the adapter: when
 *     AGENT_CONTROLLER_USE_FAKE_PROVIDER=1 and a fake is installed,
 *     returns the fake model in place of the resolved real one.
 */
// IMPORTANT: when multiple copies of @earendil-works/pi-ai exist in the
// dependency tree (top-level + nested under pi-coding-agent's
// node_modules), each copy has its own module-level api-registry. If we
// import faux from the top-level copy, registerFauxProvider records the
// new api ONLY in that copy's registry — pi-coding-agent's streamFn then
// calls into a different pi-ai instance whose registry knows nothing
// about our fake, and the agent loop fails with
// "No API provider registered for api: fake-test".
//
// To avoid this, resolve the faux module through pi-coding-agent's path
// so we register against the SAME pi-ai instance pi-coding-agent uses
// internally. Type imports below come from the static top-level copy
// (types are identical across copies, so this is safe).
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { existsSync } from "node:fs";
import type {
  FauxResponseStep,
  FauxProviderRegistration,
  RegisterFauxProviderOptions,
} from "@earendil-works/pi-ai";
import type { Model, Api, TextContent, ThinkingContent, ToolCall, AssistantMessage } from "@earendil-works/pi-ai";

// Resolve faux.js to the same pi-ai instance that pi-coding-agent uses
// internally. When npm leaves a nested copy at
// runtime/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/,
// pi-coding-agent's streamFn dispatches through that nested copy's
// api-registry. Registering the faux on the top-level pi-ai copy would
// have no effect — they're distinct module instances with distinct
// registries.
//
// Strategy: probe for the nested copy first, fall back to top-level.
// Bypass pi-ai's "import"-only exports map by importing the file URL
// directly (CommonJS `require` can't navigate an exports map with only
// an "import" condition).
//
// Resolution is LAZY (not at module load) for two reasons:
//   (1) adapter.test.ts hoists vi.mock("node:fs") calls above its
//       fakeFs const. If fake-provider's module-load code touches fs,
//       it executes before fakeFs is initialized.
//   (2) Tests that mock @earendil-works/pi-coding-agent (most of
//       adapter.test.ts) never actually call our fake — there's no
//       reason to do the probe work.

type FauxModuleShape = {
  registerFauxProvider: (opts?: RegisterFauxProviderOptions) => FauxProviderRegistration;
  fauxText: (text: string) => TextContent;
  fauxThinking: (thinking: string) => ThinkingContent;
  fauxToolCall: (
    name: string,
    args: Record<string, unknown>,
    options?: { id?: string },
  ) => ToolCall;
  fauxAssistantMessage: (
    content:
      | string
      | (TextContent | ThinkingContent | ToolCall)
      | (TextContent | ThinkingContent | ToolCall)[],
    options?: {
      stopReason?: AssistantMessage["stopReason"];
      errorMessage?: string;
      responseId?: string;
      timestamp?: number;
    },
  ) => AssistantMessage;
};

let cachedFauxModule: FauxModuleShape | undefined;

/**
 * Pre-load the faux module so the synchronous helpers (`fauxText`,
 * `fauxToolCall`, `fauxAssistantMessage`) work immediately. Tests
 * typically call this once in `beforeAll`; subsequent calls are no-ops.
 *
 * Without preload, the helpers throw because they need the cached
 * module. `installFakeProvider` also primes the cache as a side effect.
 */
export async function preloadFakeProvider(): Promise<void> {
  await loadFauxModule();
}

async function loadFauxModule(): Promise<FauxModuleShape> {
  if (cachedFauxModule) return cachedFauxModule;
  const here = dirname(fileURLToPath(import.meta.url));
  const runtimeRoot = resolve(here, "..", ".."); // runtime/src/testing → runtime/
  // pi-ai moved registerFauxProvider/unregisterApiProviders out of
  // providers/faux.js into compat.js (0.82.x); the faux* helpers are exported from
  // both. Load whichever of the two exists and merge, compat last so it wins, so
  // this keeps working on either layout.
  const nested = "node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/dist";
  const direct = "node_modules/@earendil-works/pi-ai/dist";
  const candidates = [
    resolve(runtimeRoot, `${nested}/providers/faux.js`),
    resolve(runtimeRoot, `${direct}/providers/faux.js`),
    resolve(runtimeRoot, `${nested}/compat.js`),
    resolve(runtimeRoot, `${direct}/compat.js`),
  ];
  const found = candidates.filter((p) => existsSync(p));
  if (!found.length) {
    throw new Error(
      "fake-provider: could not locate pi-ai's faux.js or compat.js under " +
      "runtime/node_modules. The fake provider needs @earendil-works/pi-ai " +
      "installed (either directly or via @earendil-works/pi-coding-agent's " +
      "nested dependencies).",
    );
  }
  const merged: Record<string, unknown> = {};
  for (const path of found) {
    Object.assign(merged, await import(pathToFileURL(path).href));
  }
  if (typeof merged.registerFauxProvider !== "function") {
    throw new Error(
      "fake-provider: pi-ai exposed no registerFauxProvider in " +
      `${found.join(", ")}. The faux provider API may have moved again.`,
    );
  }
  cachedFauxModule = merged as unknown as FauxModuleShape;
  return cachedFauxModule;
}

// Eager exports re-exposed as the same callable shape, but delegating to
// the lazily-loaded module. Tests use them synchronously after
// `installFakeProvider` (which is now async); see the helper below.
export const fauxText: FauxModuleShape["fauxText"] = (text) => {
  if (!cachedFauxModule) {
    throw new Error("fake-provider: call installFakeProvider() before fauxText() to load the faux module.");
  }
  return cachedFauxModule.fauxText(text);
};
export const fauxThinking: FauxModuleShape["fauxThinking"] = (thinking) => {
  if (!cachedFauxModule) {
    throw new Error("fake-provider: call installFakeProvider() before fauxThinking() to load the faux module.");
  }
  return cachedFauxModule.fauxThinking(thinking);
};
export const fauxToolCall: FauxModuleShape["fauxToolCall"] = (name, args, options) => {
  if (!cachedFauxModule) {
    throw new Error("fake-provider: call installFakeProvider() before fauxToolCall() to load the faux module.");
  }
  return cachedFauxModule.fauxToolCall(name, args, options);
};
export const fauxAssistantMessage: FauxModuleShape["fauxAssistantMessage"] = (content, options) => {
  if (!cachedFauxModule) {
    throw new Error("fake-provider: call installFakeProvider() before fauxAssistantMessage() to load the faux module.");
  }
  return cachedFauxModule.fauxAssistantMessage(content, options);
};

export type { FauxResponseStep };

/**
 * Holds the currently-active faux registration. Tests should call
 * `installFakeProvider(responses)` in `beforeEach` and
 * `clearFakeProvider()` in `afterEach` to keep state hermetic between
 * tests.
 *
 * undefined ⇒ no fake installed; the adapter behaves normally.
 */
let activeFake: FauxProviderRegistration | undefined;

/** Sentinel api id the fake provider registers under. */
export const FAKE_API = "fake-test";

/**
 * Register the faux api-provider with pi-ai and arm it with the given
 * scripted responses. Returns the underlying registration so tests can
 * call `appendResponses()`, inspect `state.callCount`, etc. as needed.
 *
 * If an installation already exists, this throws — installing twice
 * usually means a stale registration from a prior test leaked through.
 * Call `clearFakeProvider()` first.
 */
export async function installFakeProvider(
  responses: FauxResponseStep[],
): Promise<FauxProviderRegistration> {
  if (activeFake) {
    throw new Error(
      "fake-provider: another installation is already active. " +
      "Call clearFakeProvider() in your test's afterEach before reinstalling.",
    );
  }
  const { registerFauxProvider } = await loadFauxModule();
  const reg = registerFauxProvider({
    api: FAKE_API,
    // Pretend to be the anthropic provider so the adapter's
    // ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL logic is a no-op for the
    // fake — Pi's anthropic auth path is skipped entirely once we
    // override the model's api.
    provider: "anthropic",
    models: [{ id: "fake-model", name: "fake-model" }],
  });
  reg.setResponses(responses);
  activeFake = reg;
  return reg;
}

/**
 * Unregister the fake provider and clear the singleton. Safe to call
 * when no fake is installed.
 */
export function clearFakeProvider(): void {
  if (activeFake) {
    activeFake.unregister();
    activeFake = undefined;
  }
}

/** Returns the currently-active faux registration, or undefined. */
export function getActiveFakeProvider(): FauxProviderRegistration | undefined {
  return activeFake;
}

/**
 * Adapter hook: if the env var AGENT_CONTROLLER_USE_FAKE_PROVIDER is
 * set AND a fake has been installed via installFakeProvider(), return
 * the fake model so pi-ai routes through our scripted stream. Otherwise
 * return undefined and the adapter uses pi-ai.getModel() as normal.
 *
 * Splitting the decision this way (env var + in-process installation)
 * means production code paths can never accidentally activate the fake
 * — the env var alone does nothing without a script.
 */
export function resolveFakeModelIfRequested(): Model<Api> | undefined {
  if (process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER !== "1") return undefined;
  if (!activeFake) {
    // Env var set but no installation — surface a clear warning so the
    // test author notices, but don't throw (other code paths may set
    // the env var transitively).
    process.stderr.write(
      "[agent-controller] WARNING: AGENT_CONTROLLER_USE_FAKE_PROVIDER=1 but no " +
      "fake provider is installed. The runtime will fall back to the real model. " +
      "Call installFakeProvider() before runSession().\n",
    );
    return undefined;
  }
  return activeFake.getModel() as Model<Api>;
}
