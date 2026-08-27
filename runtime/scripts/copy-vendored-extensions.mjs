// Bundle the repo's Pi extensions into runtime/dist/ so the npm-published
// adapter is self-contained. Without this, the published @agent-controller/
// runtime package would reference extensions/subagent via a relative path
// that points outside the package tree — the adapter resolves it at session
// start and fails when spec.subagents is non-empty.
//
// extensions/subagent is a thin shim; the subagent implementation it wraps
// lives in @earendil-works/pi-coding-agent's own examples/ and is resolved
// from node_modules at runtime, so it is not copied here.
//
// Runs as part of `runtime/package.json` "build" — see also runtime/src/
// adapter.ts which probes runtime/dist/extensions/subagent first, then
// falls back to the source-tree layout (../../extensions/subagent).

import { cpSync, existsSync, mkdirSync, rmSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const runtimeDir = resolve(__dirname, "..");
const repoRoot = resolve(runtimeDir, "..");

const vendored = [
  // (source-relative-to-repo-root, dest-relative-to-runtime/dist)
  ["extensions/subagent", "extensions/subagent"],
];

for (const [src, dst] of vendored) {
  const source = resolve(repoRoot, src);
  const dest = resolve(runtimeDir, "dist", dst);
  if (!existsSync(source)) {
    console.error(`copy-vendored-extensions: ${source} not found`);
    process.exit(1);
  }
  mkdirSync(dirname(dest), { recursive: true });
  // Clear the destination first: cpSync merges rather than replaces, so a file
  // deleted from the source would otherwise linger in dist/ across local
  // incremental builds and get published.
  rmSync(dest, { recursive: true, force: true });
  cpSync(source, dest, { recursive: true });
  console.log(`copy-vendored-extensions: ${src} → dist/${dst}`);
}
