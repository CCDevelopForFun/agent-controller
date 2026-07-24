import { afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import {
  clearFakeProvider,
  fauxAssistantMessage,
  fauxText,
  fauxToolCall,
  getActiveFakeProvider,
  installFakeProvider,
  preloadFakeProvider,
  resolveFakeModelIfRequested,
} from "./fake-provider.js";

describe("fake-provider module", () => {
  beforeAll(async () => {
    // Warm the faux module cache so the synchronous content helpers
    // (fauxText etc.) work without needing an install first.
    await preloadFakeProvider();
  });
  beforeEach(() => {
    delete process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER;
    clearFakeProvider();
  });
  afterEach(() => {
    clearFakeProvider();
    delete process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER;
  });

  it("installFakeProvider returns a registration object with the expected api", async () => {
    const reg = await installFakeProvider([fauxAssistantMessage(fauxText("hi"))]);
    expect(reg.api).toBe("fake-test");
    expect(reg.models.length).toBeGreaterThan(0);
    expect(reg.models[0].name).toBe("fake-model");
  });

  it("installFakeProvider throws if called twice without clear", async () => {
    await installFakeProvider([fauxAssistantMessage(fauxText("a"))]);
    await expect(
      installFakeProvider([fauxAssistantMessage(fauxText("b"))]),
    ).rejects.toThrow(/already active/);
  });

  it("clearFakeProvider unregisters and drops singleton; safe to call twice", async () => {
    await installFakeProvider([fauxAssistantMessage(fauxText("a"))]);
    expect(getActiveFakeProvider()).toBeDefined();
    clearFakeProvider();
    expect(getActiveFakeProvider()).toBeUndefined();
    // Second call is a no-op, not a throw.
    expect(() => clearFakeProvider()).not.toThrow();
  });

  it("resolveFakeModelIfRequested returns undefined when env var is absent", async () => {
    await installFakeProvider([fauxAssistantMessage(fauxText("hi"))]);
    expect(process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER).toBeUndefined();
    expect(resolveFakeModelIfRequested()).toBeUndefined();
  });

  it("resolveFakeModelIfRequested returns the fake model when env var=1 AND fake is installed", async () => {
    process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER = "1";
    await installFakeProvider([fauxAssistantMessage(fauxText("hi"))]);
    const model = resolveFakeModelIfRequested();
    expect(model).toBeDefined();
    expect(model!.api).toBe("fake-test");
    expect(model!.name).toBe("fake-model");
  });

  it("resolveFakeModelIfRequested warns on stderr when env var set but no fake installed", () => {
    process.env.AGENT_CONTROLLER_USE_FAKE_PROVIDER = "1";
    const writes: string[] = [];
    const orig = process.stderr.write.bind(process.stderr);
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (process.stderr.write as any) = (chunk: string) => {
      writes.push(String(chunk));
      return true;
    };
    try {
      const model = resolveFakeModelIfRequested();
      expect(model).toBeUndefined();
      expect(writes.some((w) => w.includes("AGENT_CONTROLLER_USE_FAKE_PROVIDER=1"))).toBe(true);
    } finally {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      (process.stderr.write as any) = orig;
    }
  });

  it("re-exports fauxText / fauxToolCall / fauxAssistantMessage from pi-ai", async () => {
    // Helpers are lazily-loaded; install once so the underlying module
    // is in cache before calling them.
    await installFakeProvider([fauxAssistantMessage(fauxText("warmup"))]);
    expect(typeof fauxText).toBe("function");
    expect(typeof fauxToolCall).toBe("function");
    expect(typeof fauxAssistantMessage).toBe("function");
    const t = fauxText("hello");
    expect(t.type).toBe("text");
    expect(t.text).toBe("hello");
  });
});
