import type { WireEvent, WireEventType } from "./types.js";

/** Build a wire envelope from a session id, event type, and payload. */
export function stamp<T>(sessionId: string, type: WireEventType, data: T): WireEvent<T> {
  return { v: 1, type, ts: new Date().toISOString(), sessionId, data };
}

/** Write one wire event as a single NDJSON line via the provided writer. */
export function emit<T>(write: (s: string) => void, ev: WireEvent<T>): void {
  write(JSON.stringify(ev) + "\n");
}
