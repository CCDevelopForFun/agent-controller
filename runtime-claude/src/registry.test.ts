import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseFrontmatter, readSkillBodies, readSubagentBodies } from "./registry.js";

describe("parseFrontmatter", () => {
  it("splits YAML frontmatter from the body", () => {
    const raw = "---\nname: reviewer\ndescription: reviews code\n---\nYou review code.\n";
    const { attrs, body } = parseFrontmatter(raw);
    expect(attrs.name).toBe("reviewer");
    expect(attrs.description).toBe("reviews code");
    expect(body.trim()).toBe("You review code.");
  });

  it("returns the whole input as body when no frontmatter is present", () => {
    const { attrs, body } = parseFrontmatter("just a body\n");
    expect(attrs).toEqual({});
    expect(body.trim()).toBe("just a body");
  });

  it("strips surrounding quotes from values", () => {
    const { attrs } = parseFrontmatter('---\ndescription: "quoted value"\n---\nb\n');
    expect(attrs.description).toBe("quoted value");
  });

  it("ignores comment and blank lines in frontmatter", () => {
    const { attrs } = parseFrontmatter("---\n# a comment\n\nname: x\n---\nb\n");
    expect(attrs).toEqual({ name: "x" });
  });

  it("keeps colons that appear inside a value", () => {
    const { attrs } = parseFrontmatter("---\ndescription: uses a:b syntax\n---\nb\n");
    expect(attrs.description).toBe("uses a:b syntax");
  });

  it("parses CRLF input identically to the LF form", () => {
    const crlf = parseFrontmatter("---\r\nname: x\r\n---\r\nbody\r\n");
    const lf = parseFrontmatter("---\nname: x\n---\nbody\n");
    expect(crlf.attrs).toEqual(lf.attrs);
    expect(crlf.body).toBe(lf.body);
  });

  it("returns the whole input as body when there is no closing frontmatter delimiter", () => {
    const raw = "---\nname: x\nno closing delimiter here\n";
    const { attrs, body } = parseFrontmatter(raw);
    expect(attrs).toEqual({});
    expect(body).toBe(raw);
  });

  it("does not mis-split when the body contains a horizontal-rule line", () => {
    const raw = "---\nname: x\n---\nIntro.\n\n---\n\nMore text after the rule.\n";
    const { attrs, body } = parseFrontmatter(raw);
    expect(attrs).toEqual({ name: "x" });
    expect(body).toContain("Intro.");
    expect(body).toContain("---");
    expect(body).toContain("More text after the rule.");
  });
});

describe("readSkillBodies", () => {
  let root: string;

  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), "runtime-claude-registry-"));
  });

  afterEach(() => {
    rmSync(root, { recursive: true, force: true });
  });

  it("returns the skill body with frontmatter stripped when the file exists", () => {
    mkdirSync(join(root, "skills", "iso-time"), { recursive: true });
    writeFileSync(
      join(root, "skills", "iso-time", "SKILL.md"),
      "---\nname: iso-time\n---\nUse ISO-8601 timestamps.\n",
    );
    const out = readSkillBodies(root, [{ name: "iso-time" }]);
    expect(out).toEqual([{ name: "iso-time", body: "Use ISO-8601 timestamps." }]);
  });

  it("skips a missing skill file without throwing, and warns on stderr", () => {
    const spy = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    const out = readSkillBodies(root, [{ name: "missing-skill" }]);
    expect(out).toEqual([]);
    expect(spy).toHaveBeenCalledWith(expect.stringContaining("missing-skill"));
    spy.mockRestore();
  });

  it("skips a frontmatter-only skill file (empty body) and warns on stderr", () => {
    mkdirSync(join(root, "skills", "empty-skill"), { recursive: true });
    writeFileSync(join(root, "skills", "empty-skill", "SKILL.md"), "---\nname: empty-skill\n---\n");
    const spy = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    const out = readSkillBodies(root, [{ name: "empty-skill" }]);
    expect(out).toEqual([]);
    expect(spy).toHaveBeenCalledWith(expect.stringContaining("empty-skill"));
    spy.mockRestore();
  });
});

describe("readSubagentBodies", () => {
  let root: string;

  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), "runtime-claude-registry-"));
    mkdirSync(join(root, "agents"), { recursive: true });
  });

  afterEach(() => {
    rmSync(root, { recursive: true, force: true });
  });

  it("maps name, description, tools, and model from frontmatter, and the body to prompt", () => {
    writeFileSync(
      join(root, "agents", "reviewer.md"),
      "---\nname: reviewer\ndescription: reviews code\nmodel: claude-haiku-4-5\ntools: [Read, Bash]\n---\nYou review code.\n",
    );
    const out = readSubagentBodies(root, [{ name: "reviewer" }]);
    expect(out).toEqual([
      {
        name: "reviewer",
        description: "reviews code",
        tools: ["Read", "Bash"],
        model: "claude-haiku-4-5",
        prompt: "You review code.",
      },
    ]);
  });

  it("parses a comma-separated (non-bracketed) tools value into an array", () => {
    writeFileSync(
      join(root, "agents", "reviewer.md"),
      "---\nname: reviewer\ndescription: reviews code\ntools: Read, Bash\n---\nBody.\n",
    );
    const out = readSubagentBodies(root, [{ name: "reviewer" }]);
    expect(out[0].tools).toEqual(["Read", "Bash"]);
  });

  it("skips a missing subagent file without throwing, and warns on stderr", () => {
    const spy = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    const out = readSubagentBodies(root, [{ name: "missing-agent" }]);
    expect(out).toEqual([]);
    expect(spy).toHaveBeenCalledWith(expect.stringContaining("missing-agent"));
    spy.mockRestore();
  });
});
