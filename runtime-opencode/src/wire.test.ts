import { describe, it, expect } from "vitest";
import { emit, stamp } from "./wire.js";

describe("wire", () => {
  it("stamps events with version, timestamp, and session id", () => {
    const ev = stamp("s1", "tool.call", { toolName: "get_time" });
    expect(ev.v).toBe(1);
    expect(ev.type).toBe("tool.call");
    expect(ev.sessionId).toBe("s1");
    expect(ev.data).toEqual({ toolName: "get_time" });
    expect(new Date(ev.ts).toString()).not.toBe("Invalid Date");
  });

  it("emit writes one NDJSON line", () => {
    const lines: string[] = [];
    const write = (s: string) => lines.push(s);
    emit(write, stamp("s1", "session.started", {}));
    expect(lines).toHaveLength(1);
    expect(lines[0].endsWith("\n")).toBe(true);
    const parsed = JSON.parse(lines[0]);
    expect(parsed.type).toBe("session.started");
  });
});
