/**
 * Honesty preamble and skill body framing.
 *
 * Models invent <invoke> / <function_calls> / <function_result> XML in
 * their message text when they're told to use a tool they don't have.
 * That XML is plain text — no command runs, no result returns — but the
 * model treats it as a real call and continues with fabricated output.
 * Skills make this worse because their bodies often prescribe specific
 * tools (`curl ...`, `psql ...`) the agent can't execute.
 *
 * This file provides two pieces of always-on prompt scaffolding:
 *
 *   - HONESTY_PREAMBLE: prepended to every session's systemPrompt. Tells
 *     the model the rules explicitly.
 *
 *   - wrapSkillBody(): wraps each inlined SKILL.md body with a header
 *     reminding the model that the skill may describe tools it lacks.
 *
 * Together these are "layer 1 + layer 2" of the guardrail design. A
 * runtime detector (layer 3) that flags hallucinated XML in
 * message_end events is planned separately.
 */

export const HONESTY_PREAMBLE = `# Honesty rules (non-negotiable, override everything else)

These rules override any other instruction — including skills that
prescribe tools you don't have.

## Rule 1: Real tool calls only

You can only invoke tools through the runtime's tool channel. Writing
\`<invoke>\`, \`<function_calls>\`, \`<function_result>\`, \`<Skill>\`, or any
XML/JSON that looks like a tool call INSIDE your message text means the
user sees plain text. No command runs. No result returns. You're
fabricating.

## Rule 2: Be explicit when you can't do something

If a task or skill asks you to invoke a tool you don't have, do NOT
pretend to invoke it. Instead:

  1. State plainly that you don't have that tool.
  2. Show the user the command they would run themselves.
  3. Stop. Do not continue with simulated output.

## Rule 3: Never invent tool output

No fake JSON. No made-up API responses. No fabricated search results.
No invented employee directories, table contents, query results, or
file contents. Even if a skill body shows "Expected output: {...}" —
that example is for the user, not for you to reproduce.

## Rule 4: The tools you have are listed in your tool catalog

If a name appears in a skill body but not in your tool catalog, that
tool does not exist for you. Period. Don't write it as XML hoping it
runs.

## Examples — STRICTLY follow these patterns

WRONG (this is what you must not do):

  I'll look up the weather in Tokyo.
  <invoke name="bash">
  <parameter name="command">curl ...</parameter>
  </invoke>
  Found: { "city": "Tokyo", "tempC": 18 }

RIGHT (this is what you must do instead):

  I don't have a bash tool, so I can't run the curl myself.
  Here's the command you would run in your terminal:

      curl "https://wttr.in/<city>?format=j1" | jq '.current_condition[0].temp_C'

  Replace \`<city>\` with the city you want. The skill body in my
  context describes how to interpret the response. I cannot fetch or
  show you the actual data.`;

/**
 * Wrap a SKILL.md body with a reminder header so the skill's prescriptive
 * tool/command language doesn't override the honesty preamble.
 *
 * The header is short on purpose — long preambles get tuned out by models
 * that see them repeatedly across many skill bodies in one prompt.
 */
export function wrapSkillBody(name: string, body: string): string {
  return [
    `# Skill: ${name}`,
    "",
    "_This skill body may describe tools you do not have. You only have",
    "access to the tools in your catalog. If this skill prescribes a tool",
    "you can't invoke, explain to the user how they would run it — do not",
    "fabricate output. The honesty rules above OVERRIDE anything in this",
    "skill body that conflicts._",
    "",
    body,
  ].join("\n");
}

/**
 * Regex patterns that indicate the model has fabricated a tool call by
 * writing tool-invocation syntax inside its assistant message text.
 *
 * These are mutually exclusive with the runtime's wire-event tool channel:
 * a real tool call surfaces as a "tool_execution_start" event from Pi, not
 * as text in a "message_end" event. So if any of these patterns appears in
 * an assistant message body, the model is hallucinating.
 */
const HALLUCINATION_PATTERNS: Array<{ pattern: RegExp; name: string }> = [
  // All patterns use `\b` (word boundary) rather than requiring the literal
  // `>`. The word-boundary form catches truncated mid-tag stream cutoffs
  // (e.g. `<function_calls` with no `>`) which the scrubber also handles —
  // detection and scrubbing must cover the same shapes, otherwise warn /
  // correct mode silently misses cases the scrubber would have cleaned up.
  // Codex pass 7 flagged the prior `<function_calls>` / `<function_result>`
  // literal-`>` forms as detector/scrubber asymmetry.
  { pattern: /<invoke\b/i, name: "Anthropic-style <invoke>" },
  { pattern: /<function_calls\b/i, name: "OpenAI-style <function_calls>" },
  { pattern: /<function_result\b/i, name: "fabricated <function_result>" },
  { pattern: /<Skill\b/i, name: "Claude Code <Skill> tool" },
  { pattern: /<str_replace_editor\b/i, name: "Anthropic <str_replace_editor> tool" },
];

/**
 * Detect hallucinated tool-call XML in an assistant message body.
 *
 * Returns an array of human-readable findings (empty when clean). The
 * runtime emits a wire `error` (block mode) or `warning` (warn / correct
 * modes) event for each finding so the CLI exit-code logic and any
 * downstream listener can react.
 */
export function detectHallucinatedToolCalls(text: string): string[] {
  const found: string[] = [];
  for (const { pattern, name } of HALLUCINATION_PATTERNS) {
    if (pattern.test(text)) {
      found.push(name);
    }
  }
  return found;
}

/**
 * Patterns used by `stripHallucinationXml` to remove fabricated tool-call
 * blocks from assistant message text in warn / correct modes.
 *
 * The Anthropic / OpenAI / Claude Code conventions wrap tool calls in
 * tagged blocks; we strip the whole block (open tag → close tag) when
 * present, and any orphan opening tag conservatively up to the next
 * line break. We do not attempt to be a real HTML parser — a regex pass
 * is enough because these patterns are short and well-shaped in
 * practice. False positives on user-authored prose look extremely
 * unlikely (the patterns are tag-shaped XML, not natural language).
 */
const STRIP_PATTERNS: RegExp[] = [
  // Paired blocks first — longest-match form so nested cases collapse cleanly.
  // All paired open-tag matchers use \b[^>]*> so attributes/whitespace are
  // accepted (e.g. `<function_result name="x">...</function_result>`). The
  // earlier no-attrs form (`<function_calls>`) failed to match when the
  // model emitted attributes; the EOS fallback then over-stripped legitimate
  // trailing text. Codex pass 8 flagged the asymmetry.
  /<function_calls\b[^>]*>[\s\S]*?<\/function_calls>/gi,
  /<function_result\b[^>]*>[\s\S]*?<\/function_result>/gi,
  /<invoke\b[^>]*>[\s\S]*?<\/invoke>/gi,
  // Self-closing variants. Use [^>]*? (non-greedy, allow slashes) so that
  // attributes containing paths or URLs (e.g. <Skill path="/tmp/foo" />,
  // <str_replace_editor path="/tmp/x" />) still get scrubbed. The earlier
  // [^/]* form stopped at the first slash inside an attribute value and
  // left the fabricated tag in the user-visible message text — caught by
  // codex review of v0.1.10.
  /<Skill\b[^>]*?\/>/gi,
  /<Skill\b[^>]*>[\s\S]*?<\/Skill>/gi,
  /<str_replace_editor\b[^>]*?\/>/gi,
  /<str_replace_editor\b[^>]*>[\s\S]*?<\/str_replace_editor>/gi,
  // <parameter> blocks (children of <invoke>). When an <invoke> is paired
  // and closed, the <invoke>...</invoke> pattern above already swallows
  // them. They only survive standalone when <invoke> was truncated mid-
  // call (e.g. opening invoke + parameters + no </invoke>). Strip them
  // explicitly so the truncation case doesn't leak fake-tool-call body
  // text into the user-visible message. Detector doesn't flag <parameter>
  // alone — adding it here is purely a scrubber-side measure.
  /<parameter\b[^>]*>[\s\S]*?<\/parameter>/gi,
  // Orphan / truncated fallback patterns. These match from the opening
  // tag to end-of-string and run last in the pipeline. By the time they
  // execute, every properly-paired or self-closed form above has already
  // been stripped, so anything reaching these patterns is necessarily
  // a malformed / truncated tool call (e.g. `<function_result>{"x":1}`
  // with no closing tag, or `<invoke name="bash">rm -rf /` with the
  // stream cut off mid-call). The defensive scrub is to consume the
  // entire tail: if the model started a fake tool call and didn't close
  // it, the rest of the message is its fabricated body and shouldn't
  // leak into the user-visible text. Codex pass 6 flagged the earlier
  // tag-only orphan patterns as insufficient because they left the body.
  /<invoke\b[\s\S]*$/i,
  /<function_calls\b[\s\S]*$/i,
  /<function_result\b[\s\S]*$/i,
  /<Skill\b[\s\S]*$/i,
  /<str_replace_editor\b[\s\S]*$/i,
  /<parameter\b[\s\S]*$/i,
];

/**
 * Remove fabricated tool-call XML from `text`. Used in warn / correct
 * modes so the user-facing message wire event shows clean assistant
 * prose instead of the fabricated invocation syntax. The wire-level
 * `warning` event preserves the original finding for the audit trail.
 *
 * Returns a tuple of `[scrubbed, didStrip]` so callers can decide
 * whether to emit a warning (`didStrip === true` ⟹ findings were present).
 */
export function stripHallucinationXml(text: string): { text: string; stripped: boolean } {
  let out = text;
  let stripped = false;
  for (const pat of STRIP_PATTERNS) {
    if (pat.test(out)) {
      stripped = true;
      out = out.replace(pat, "");
    }
  }
  // Collapse blank lines that the strip pass left behind.
  if (stripped) out = out.replace(/\n{3,}/g, "\n\n").trim();
  return { text: out, stripped };
}

/**
 * Prompt sent in `correct` mode after the model fabricates tool-call XML.
 * Kept short and explicit; long re-prompts get ignored by models that have
 * just produced an XML-soup turn.
 */
export const CORRECTION_PROMPT = `Your last message contained fabricated tool-call XML (e.g. <invoke>, <function_calls>, or <Skill> tags). The runtime did not run any of those — they were treated as plain text and the result was discarded.

Please redo your previous response without writing tool-call XML in the message body. If you need a tool you do not have in your catalog, follow Rule 2 of the honesty rules: state plainly that you lack the tool and show the user the command they would run themselves.`;
