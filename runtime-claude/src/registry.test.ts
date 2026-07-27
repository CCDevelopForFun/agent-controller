import { describe, it, expect } from "vitest";
import { parseFrontmatter } from "./registry.js";

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
});
