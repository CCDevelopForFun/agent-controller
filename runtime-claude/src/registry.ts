/**
 * Local-registry readers: turn <root>/skills/<name>/SKILL.md and
 * <root>/agents/<slug>.md into the plain-data shapes the invocation builders
 * consume.
 *
 * The frontmatter parser is deliberately minimal — the same flat key: value
 * subset the Pi and opencode adapters rely on. It is not a YAML engine.
 */

import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import type { ResolvedRef } from "./types.js";
import type { SkillBody, SubagentBody } from "./claude-invocation.js";

/** Split leading `---` YAML frontmatter from the markdown body. */
export function parseFrontmatter(raw: string): {
  attrs: Record<string, string>;
  body: string;
} {
  const normalized = raw.replace(/\r\n/g, "\n");
  if (!normalized.startsWith("---\n")) {
    return { attrs: {}, body: normalized };
  }
  const end = normalized.indexOf("\n---", 3);
  if (end === -1) {
    return { attrs: {}, body: normalized };
  }

  const block = normalized.slice(4, end);
  const body = normalized.slice(end + 4).replace(/^\n/, "");

  const attrs: Record<string, string> = {};
  for (const line of block.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const idx = trimmed.indexOf(":");
    if (idx === -1) continue;
    const key = trimmed.slice(0, idx).trim();
    let value = trimmed.slice(idx + 1).trim();
    if (
      (value.startsWith('"') && value.endsWith('"') && value.length > 1) ||
      (value.startsWith("'") && value.endsWith("'") && value.length > 1)
    ) {
      value = value.slice(1, -1);
    }
    if (key) attrs[key] = value;
  }

  return { attrs, body };
}

/** Parse a `tools` frontmatter value in either YAML-array or comma form. */
function parseToolsList(value: string | undefined): string[] | undefined {
  if (!value) return undefined;
  const inner = value.startsWith("[") && value.endsWith("]") ? value.slice(1, -1) : value;
  const parts = inner
    .split(",")
    .map((s) => s.trim().replace(/^["']|["']$/g, ""))
    .filter(Boolean);
  return parts.length > 0 ? parts : undefined;
}

/**
 * Read each declared skill's SKILL.md, strip frontmatter, return the body.
 * A missing file is skipped with a stderr warning rather than failing the run —
 * matching the codex adapter's tolerance.
 */
export function readSkillBodies(root: string, refs: ResolvedRef[]): SkillBody[] {
  const out: SkillBody[] = [];
  for (const ref of refs) {
    const file = join(root, "skills", ref.name, "SKILL.md");
    if (!existsSync(file)) {
      process.stderr.write(`[runtime-claude] WARNING: skill "${ref.name}" not found at ${file}\n`);
      continue;
    }
    const { body } = parseFrontmatter(readFileSync(file, "utf8"));
    const trimmed = body.trim();
    if (trimmed) {
      out.push({ name: ref.name, body: trimmed });
    } else {
      process.stderr.write(
        `[runtime-claude] WARNING: skill "${ref.name}" at ${file} has an empty body after stripping frontmatter; skipping.\n`,
      );
    }
  }
  return out;
}

/**
 * Read each declared subagent's <root>/agents/<slug>.md, parse its frontmatter
 * (name / description / model / tools) and use the body as the agent prompt.
 */
export function readSubagentBodies(root: string, refs: ResolvedRef[]): SubagentBody[] {
  const out: SubagentBody[] = [];
  for (const ref of refs) {
    const file = join(root, "agents", `${ref.name}.md`);
    if (!existsSync(file)) {
      process.stderr.write(
        `[runtime-claude] WARNING: subagent "${ref.name}" not found at ${file}\n`,
      );
      continue;
    }
    const { attrs, body } = parseFrontmatter(readFileSync(file, "utf8"));
    out.push({
      name: attrs.name || ref.name,
      description: attrs.description || `Subagent ${ref.name}`,
      tools: parseToolsList(attrs.tools),
      model: attrs.model || undefined,
      prompt: body.trim(),
    });
  }
  return out;
}
