/**
 * Smoke tests for @opencode-ai/sdk integration.
 *
 * Slice 2.3 of the v0.2 plan installed the SDK. This file confirms the
 * SDK loads cleanly, exposes the entry points subsequent slices need,
 * and is shape-compatible with our planned wiring (buildOpencodeConfig
 * → createOpencode → client.session.* → SSE events).
 *
 * What this file does NOT do: invoke createOpencode for real. That
 * spawns an opencode server process and is exercised in slice 2.4
 * where the actual session-driving code lands. Keeping the smoke
 * tests pure-import lets CI run them without network or process
 * setup.
 */
import { describe, it, expect } from "vitest";
import * as opencode from "@opencode-ai/sdk";

describe("opencode SDK install smoke (slice 2.3)", () => {
  it("@opencode-ai/sdk exposes createOpencode + createOpencodeServer + createOpencodeClient", () => {
    expect(typeof opencode.createOpencode).toBe("function");
    expect(typeof opencode.createOpencodeServer).toBe("function");
    expect(typeof opencode.createOpencodeClient).toBe("function");
  });

  it("@opencode-ai/sdk exposes the OpencodeClient class", () => {
    expect(typeof opencode.OpencodeClient).toBe("function"); // class constructor
  });

  // Note: an earlier draft of this file included a "buildOpencodeConfig
  // output is shape-compatible with ServerOptions.config" assertion,
  // but it cast through `any` to make the test pass — which neutered
  // the type check it claimed to perform. Codex pass 1 of slice 2.3
  // flagged this. The real shape-compatibility check lands in slice
  // 2.4, where buildOpencodeConfig's output is actually assigned to
  // createOpencode's options.config in a tsc-compiled (non-test) file.
  // Any drift between the SDK's Config type and our mapper will fail
  // the runtime-opencode build at that point — which is the right
  // signal, since runtime-opencode would not be usable if it can't
  // produce a config opencode accepts.
});
