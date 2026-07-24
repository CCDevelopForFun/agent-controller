/**
 * Normalize ANTHROPIC_BASE_URL to opencode's expectations.
 *
 * Pi and opencode disagree about whether the env var includes `/v1`:
 *   - Pi (pi-ai → @anthropic-ai/sdk) treats it as the API root WITHOUT `/v1`
 *     and appends `/v1/messages` itself.
 *   - opencode (Vercel `@ai-sdk/anthropic`) treats it as already-versioned
 *     and appends just `/messages`.
 *
 * To let operators set ONE env var that works with either adapter, we append
 * `/v1` here if the configured URL doesn't already include a `/vN` suffix.
 * The standard `api.anthropic.com/v1` endpoint is preserved as-is.
 *
 * Lives in its own module (no side effects) so the unit tests can import
 * it without dragging in `src/index.ts`'s top-level `main()` call. Codex
 * pass 1 of slice 4.2.1 caught the original co-location issue.
 */
export function normalizeAnthropicBaseUrlForOpencode(value: string | undefined): string | undefined {
  if (!value) return value;
  // Strip trailing slashes so endsWith / regex checks are simple.
  const stripped = value.replace(/\/+$/, "");
  if (/\/v\d+$/.test(stripped)) {
    return stripped;
  }
  return `${stripped}/v1`;
}
