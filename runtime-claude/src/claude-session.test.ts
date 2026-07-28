import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import {
  SDK_SESSION_ID_FILENAME,
  persistSdkSessionId,
  readPersistedSdkSessionId,
  sdkSessionStateDir,
} from "./claude-session.js";
import { deriveSdkSessionUuid } from "./claude-invocation.js";

// Real temp directories, like registry.test.ts — the whole point of this suite
// is the on-disk behavior that decides whether turn 2 resumes.
let root: string;
let originalXdg: string | undefined;

beforeEach(() => {
  root = mkdtempSync(join(tmpdir(), "claude-session-test-"));
  originalXdg = process.env.XDG_DATA_HOME;
  process.env.XDG_DATA_HOME = root;
});

afterEach(() => {
  if (originalXdg === undefined) delete process.env.XDG_DATA_HOME;
  else process.env.XDG_DATA_HOME = originalXdg;
  rmSync(root, { recursive: true, force: true });
});

describe("sdkSessionStateDir", () => {
  it("keys the directory by the derived UUID, under the agent-controller data dir", () => {
    const uuid = deriveSdkSessionUuid("s_abc");
    const dir = sdkSessionStateDir(uuid);
    expect(dir).toBe(join(root, "agent-controller", "claude-sessions", uuid));
  });

  it("never puts the raw agentctl session id in a path component", () => {
    // A raw id with path metacharacters must not be able to escape the data
    // dir, which is why the directory is keyed by the derived UUID.
    const raw = "../../etc/passwd";
    const dir = sdkSessionStateDir(deriveSdkSessionUuid(raw));
    expect(dir).not.toContain("..");
    expect(dir).not.toContain("passwd");
    expect(dir.startsWith(join(root, "agent-controller", "claude-sessions"))).toBe(true);
  });

  it("gives two different agentctl sessions non-colliding directories", () => {
    const a = sdkSessionStateDir(deriveSdkSessionUuid("s_aaa"));
    const b = sdkSessionStateDir(deriveSdkSessionUuid("s_bbb"));
    expect(a).not.toBe(b);
  });

  it("gives the same agentctl session the same directory across processes", () => {
    expect(sdkSessionStateDir(deriveSdkSessionUuid("s_abc"))).toBe(
      sdkSessionStateDir(deriveSdkSessionUuid("s_abc")),
    );
  });

  it("falls back to ~/.local/share when XDG_DATA_HOME is unset", () => {
    delete process.env.XDG_DATA_HOME;
    expect(sdkSessionStateDir("u")).toContain(join(".local", "share", "agent-controller"));
  });
});

describe("readPersistedSdkSessionId", () => {
  it("returns undefined when the state file is absent — the first-turn path", () => {
    const dir = sdkSessionStateDir(deriveSdkSessionUuid("s_fresh"));
    expect(readPersistedSdkSessionId(dir)).toBeUndefined();
  });

  it("returns undefined when the directory itself does not exist", () => {
    expect(readPersistedSdkSessionId(join(root, "nope", "nothing", "here"))).toBeUndefined();
  });

  it("ignores an empty or whitespace-only state file rather than resuming \"\"", () => {
    const dir = join(root, "empty");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, SDK_SESSION_ID_FILENAME), "   \n");
    expect(readPersistedSdkSessionId(dir)).toBeUndefined();
  });

  it("ignores garbage content instead of forwarding it to Options.resume", () => {
    const dir = join(root, "garbage");
    mkdirSync(dir, { recursive: true });
    for (const junk of ["not-a-uuid", "s_abc", "<html>500</html>", "0".repeat(5000)]) {
      writeFileSync(join(dir, SDK_SESSION_ID_FILENAME), junk);
      expect(readPersistedSdkSessionId(dir), `junk ${junk.slice(0, 20)}`).toBeUndefined();
    }
  });

  it("degrades to undefined rather than throwing when the state file is unreadable", () => {
    // A directory where the file should be: readFileSync throws EISDIR.
    const dir = join(root, "unreadable");
    mkdirSync(join(dir, SDK_SESSION_ID_FILENAME), { recursive: true });
    expect(() => readPersistedSdkSessionId(dir)).not.toThrow();
    expect(readPersistedSdkSessionId(dir)).toBeUndefined();
  });

  it("tolerates a trailing newline, as any editor would leave", () => {
    const dir = join(root, "newline");
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, SDK_SESSION_ID_FILENAME), "c3dcb283-cfda-52bd-b6d0-d5abb2f76986\n");
    expect(readPersistedSdkSessionId(dir)).toBe("c3dcb283-cfda-52bd-b6d0-d5abb2f76986");
  });
});

describe("persistSdkSessionId", () => {
  it("creates the directory tree and writes the id", () => {
    const dir = sdkSessionStateDir(deriveSdkSessionUuid("s_new"));
    const res = persistSdkSessionId(dir, "11111111-2222-5333-8444-555555555555");
    expect(res.ok).toBe(true);
    expect(readFileSync(join(dir, SDK_SESSION_ID_FILENAME), "utf8")).toBe(
      "11111111-2222-5333-8444-555555555555",
    );
  });

  it("reports a failure instead of throwing when the write cannot happen", () => {
    const locked = join(root, "locked");
    mkdirSync(locked, { recursive: true });
    chmodSync(locked, 0o500); // read+execute only: mkdir/write inside must fail
    try {
      const res = persistSdkSessionId(join(locked, "child"), "11111111-2222-5333-8444-555555555555");
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.reason).toBeTruthy();
        expect(res.file).toContain(SDK_SESSION_ID_FILENAME);
      }
    } finally {
      chmodSync(locked, 0o700);
    }
  });

  it("is idempotent — re-recording the same id does not rewrite the file", () => {
    const dir = sdkSessionStateDir(deriveSdkSessionUuid("s_idem"));
    const id = "11111111-2222-5333-8444-555555555555";
    expect(persistSdkSessionId(dir, id).ok).toBe(true);
    // Make the file unwritable; an idempotent re-record must not need to touch it.
    const file = join(dir, SDK_SESSION_ID_FILENAME);
    chmodSync(file, 0o400);
    try {
      expect(persistSdkSessionId(dir, id).ok).toBe(true);
      expect(readPersistedSdkSessionId(dir)).toBe(id);
    } finally {
      chmodSync(file, 0o600);
    }
  });

  it("overwrites a stale id when the SDK adopts a different one", () => {
    const dir = sdkSessionStateDir(deriveSdkSessionUuid("s_heal"));
    persistSdkSessionId(dir, "11111111-2222-5333-8444-555555555555");
    persistSdkSessionId(dir, "99999999-8888-5777-8666-555555555555");
    expect(readPersistedSdkSessionId(dir)).toBe("99999999-8888-5777-8666-555555555555");
  });
});

// The behavior these helpers exist for: turn 1 must leave behind exactly what
// turn 2 needs to resume, and turn 2 must not re-take the first-turn branch.
// The defect this suite was written to lock down was an ordering one — the id
// recorded only after query() returned, so a SIGTERM mid-turn left a transcript
// with no record of it and every later turn hard-failed with
// "Session ID <id> is already in use." (observed against the SDK).
describe("two-turn round trip", () => {
  const AGENTCTL_ID = "s_mfaq1x2y3z4a5b6c";

  it("turn 1 records the derived id up front; turn 2 reads it back and resumes", () => {
    const derived = deriveSdkSessionUuid(AGENTCTL_ID);
    const dir = sdkSessionStateDir(derived);

    // Turn 1: nothing on disk -> first-turn path -> record the derived id
    // BEFORE any session is opened.
    expect(readPersistedSdkSessionId(dir)).toBeUndefined();
    expect(persistSdkSessionId(dir, derived).ok).toBe(true);

    // Turn 2 (fresh process, same agentctl id): resolves the same directory and
    // finds the id, so it takes the resume path instead of re-deriving a
    // first-turn sessionId.
    const dir2 = sdkSessionStateDir(deriveSdkSessionUuid(AGENTCTL_ID));
    expect(dir2).toBe(dir);
    expect(readPersistedSdkSessionId(dir2)).toBe(derived);
  });

  it("a crash after the record still leaves turn 2 on the resume path", () => {
    // This is the ordering guarantee: the record happens before query(), so it
    // survives a death at any point during the turn.
    const derived = deriveSdkSessionUuid(AGENTCTL_ID);
    const dir = sdkSessionStateDir(derived);
    persistSdkSessionId(dir, derived);
    // ...process dies here, no post-run bookkeeping runs at all...
    expect(readPersistedSdkSessionId(sdkSessionStateDir(deriveSdkSessionUuid(AGENTCTL_ID)))).toBe(
      derived,
    );
  });

  it("a different agentctl session is unaffected by the first one's state", () => {
    const a = deriveSdkSessionUuid("s_aaa");
    persistSdkSessionId(sdkSessionStateDir(a), a);
    expect(readPersistedSdkSessionId(sdkSessionStateDir(deriveSdkSessionUuid("s_bbb")))).toBeUndefined();
  });
});
