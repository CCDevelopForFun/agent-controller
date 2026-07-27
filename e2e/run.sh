#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Adapter selector. Defaults to the legacy "pi" target so existing CI
# behavior is preserved. Override to test the opencode adapter end-to-end:
#   ADAPTER=opencode AGENT_CONTROLLER_RUN_LIVE=1 ANTHROPIC_API_KEY=... ./e2e/run.sh
ADAPTER="${ADAPTER:-pi}"
case "$ADAPTER" in
  pi|opencode|codex|claude) ;;
  *) echo "ADAPTER must be 'pi', 'opencode', 'codex', or 'claude' (got: $ADAPTER)" >&2; exit 2 ;;
esac

# ── codex hermetic tier ────────────────────────────────────────────────────────
# ADAPTER=codex runs hermetically by default (no OpenAI, no real codex CLI).
# A fake-codex shim is prepended to PATH so the runtime-codex adapter can
# complete without any real credentials. Gate the live tier (real codex CLI +
# OPENAI_API_KEY) behind AGENT_CONTROLLER_RUN_LIVE=1 + OPENAI_API_KEY, exactly
# mirroring the opencode live-tier gate.
if [[ "$ADAPTER" == "codex" ]]; then
  if [[ "${AGENT_CONTROLLER_RUN_LIVE:-0}" != "1" ]]; then
    # Hermetic tier: inject fake-codex shim, dummy key, build everything needed.
    echo "==> ADAPTER=codex hermetic tier (fake-codex shim, no OpenAI required)"

    echo "==> building runtime-codex"
    (cd runtime-codex && npm run build)
    export AGENT_CONTROLLER_RUNTIME="$PWD/runtime-codex/dist/index.js"

    echo "==> building cli"
    (cd cli && go build -o bin/agentctl ./cmd/agentctl)

    # Prepend the directory containing fake-codex.sh as a "codex" command.
    SHIM_DIR="$(cd "$(dirname "$0")" && pwd)"
    # The shim is named fake-codex.sh; expose it as "codex" via a temp symlink dir.
    FAKE_BIN="$(mktemp -d)"
    ln -s "${SHIM_DIR}/fake-codex.sh" "${FAKE_BIN}/codex"
    export PATH="${FAKE_BIN}:${PATH}"
    export OPENAI_API_KEY="test-key"

    trap 'rm -rf "${FAKE_BIN}" /tmp/run.ndjson /tmp/run.out' EXIT

    echo "==> running (hermetic, adapter=codex, spec=examples/hello-codex.yaml)"
    ./cli/bin/agentctl run examples/hello-codex.yaml --raw-out /tmp/run.ndjson > /tmp/run.out

    echo "==> asserting"
    grep -q '"type":"session.started"' /tmp/run.ndjson || { echo "missing session.started"; cat /tmp/run.ndjson; exit 1; }
    grep -q '"type":"session.ended"'   /tmp/run.ndjson || { echo "missing session.ended";   cat /tmp/run.ndjson; exit 1; }
    echo "ok (adapter=codex, hermetic)"
    exit 0
  fi

  # Live tier: real codex CLI required.
  if [[ -z "${OPENAI_API_KEY:-}" ]]; then
    echo "ADAPTER=codex AGENT_CONTROLLER_RUN_LIVE=1 set but OPENAI_API_KEY is empty. Aborting." >&2
    exit 2
  fi
  if ! command -v codex >/dev/null 2>&1; then
    echo "ADAPTER=codex but \`codex\` CLI is not on PATH." >&2
    echo "Install it with \`npm install -g @openai/codex\` or follow https://github.com/openai/codex" >&2
    exit 2
  fi

  echo "==> building runtime-codex"
  (cd runtime-codex && npm run build)
  export AGENT_CONTROLLER_RUNTIME="$PWD/runtime-codex/dist/index.js"
  SPEC="examples/hello-codex.yaml"
  # Fall through to the shared live-run block below.
fi

# ── claude hermetic tier ───────────────────────────────────────────────────────
# ADAPTER=claude runs hermetically by default: it exercises the compile-time
# rejection path and the adapter's own unsupported-spec path, neither of which
# needs a model. No PATH shim is required — the Claude Agent SDK ships its own
# executable, unlike the opencode and codex CLIs. The live tier is gated behind
# AGENT_CONTROLLER_RUN_LIVE=1 + ANTHROPIC_API_KEY.
if [[ "$ADAPTER" == "claude" ]]; then
  echo "==> building runtime-claude"
  (cd runtime-claude && npm run build)

  echo "==> building cli"
  (cd cli && go build -o bin/agentctl ./cmd/agentctl)

  echo "==> validate + compile examples/hello-claude.yaml"
  cli/bin/agentctl validate examples/hello-claude.yaml
  cli/bin/agentctl compile examples/hello-claude.yaml >/dev/null

  echo "==> compile must reject provider openai on local-claude"
  tmpdir="$(mktemp -d -t claude-bad-XXXXXX)"
  tmpspec="$tmpdir/bad.yaml"
  sed -e 's/provider: anthropic/provider: openai/' \
      -e 's/name: claude-opus-4-6/name: gpt-5.5/' \
      examples/hello-claude.yaml > "$tmpspec"
  if cli/bin/agentctl compile "$tmpspec" >/dev/null 2>&1; then
    echo "FAIL: expected compile to reject provider openai on local-claude" >&2
    rm -rf "$tmpdir"; exit 1
  fi
  rm -rf "$tmpdir"
  echo "ok (compile rejects non-anthropic provider)"

  if [[ "${AGENT_CONTROLLER_RUN_LIVE:-0}" != "1" ]]; then
    echo "==> adapter rejects an unsupported spec on stdin"
    out="$(echo '{"v":1,"metadata":{"name":"t"},"model":{"provider":"openai","name":"gpt-5.5"},"task":"hi","tools":[],"extensions":[],"skills":[],"runtime":{"type":"local-claude"}}' \
      | node runtime-claude/dist/index.js || true)"
    echo "$out" | grep -q '"type":"error"' || { echo "FAIL: expected an error event" >&2; exit 1; }
    echo "$out" | grep -q 'spec.model.provider' || { echo "FAIL: error must name the field" >&2; exit 1; }
    echo "ok (adapter=claude, hermetic)"
    exit 0
  fi

  if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    echo "ADAPTER=claude AGENT_CONTROLLER_RUN_LIVE=1 set but ANTHROPIC_API_KEY is empty. Aborting." >&2
    exit 1
  fi

  echo "==> running (live, adapter=claude, spec=examples/hello-claude.yaml)"
  export AGENT_CONTROLLER_RUNTIME="$PWD/runtime-claude/dist/index.js"
  cli/bin/agentctl run examples/hello-claude.yaml

  # ── live tool-execution tier ────────────────────────────────────────────────
  #
  # !!! AUTHORED BUT NEVER EXECUTED !!!
  # This block was written during the pre-merge review fix wave on a machine
  # with no ANTHROPIC_API_KEY, so it has never been run against a real model.
  # Everything below is derived from behavior that WAS verified without a key
  # (the SDK's `system`/`init` message reports the exact tool list it was given,
  # and e2e/fake-mcp-server.mjs was smoke-tested standalone over stdio), but the
  # assertions themselves are unproven. Expect to debug them on first run.
  #
  # Why it exists: the three Critical defects found in review (allowlist not
  # enforced, N session.started per session, session ids not bridged) all
  # survived six task-level reviews because no test — hermetic or live — ever
  # executed a tool. The hermetic tier covers rejection paths only, and the
  # live tier above runs a tool-free hello.

  echo "==> live tool tier 1/2: declared tools are the only tools"
  cli/bin/agentctl run e2e/claude-live-tools.yaml --raw-out /tmp/claude-tools.ndjson >/dev/null

  # Exactly one session.started per session. `case "system"` used to match all
  # 28 system subtypes, so a single session emitted one per system message.
  started="$(grep -c '"type":"session.started"' /tmp/claude-tools.ndjson || true)"
  if [[ "$started" != "1" ]]; then
    echo "FAIL: expected exactly 1 session.started, got ${started}" >&2
    cat /tmp/claude-tools.ndjson >&2; exit 1
  fi

  # The spec declares `tools: [read]`. Bash must never appear: before the fix,
  # spec.tools[] went to Options.allowedTools (auto-approval) instead of
  # Options.tools (the restriction), leaving the full default toolset live.
  if grep '"type":"tool.call"' /tmp/claude-tools.ndjson | grep -q '"toolName":"Bash"'; then
    echo "FAIL: Bash tool.call emitted for a spec declaring only tools: [read]" >&2
    cat /tmp/claude-tools.ndjson >&2; exit 1
  fi

  # Positive half — without it "no Bash" passes trivially when the agent calls
  # no tools at all, which is exactly the hole that let the defect through.
  if ! grep '"type":"tool.call"' /tmp/claude-tools.ndjson | grep -q '"toolName":"Read"'; then
    echo "FAIL: expected a Read tool.call; the granted tool never executed" >&2
    cat /tmp/claude-tools.ndjson >&2; exit 1
  fi
  grep -q 'AGENTCTL_E2E_READ_SENTINEL_4b7ad2' /tmp/claude-tools.ndjson || {
    echo "FAIL: Read tool.result did not carry the fixture sentinel" >&2
    cat /tmp/claude-tools.ndjson >&2; exit 1
  }
  echo "ok (exactly one session.started; Read granted, Bash absent)"

  echo "==> live tool tier 2/2: a declared MCP tool actually executes"
  # e2e/fake-mcp-server.mjs is a local stdio MCP server — no network, no npx
  # download. Reaching the sentinel proves spec.mcpServers[] became
  # Options.mcpServers AND the `mcp__<server>` allow rule auto-approved the
  # call; without that rule the SDK raises "canUseTool callback is not
  # provided." instead of executing.
  cli/bin/agentctl run e2e/claude-live-mcp.yaml --raw-out /tmp/claude-mcp.ndjson >/dev/null

  started="$(grep -c '"type":"session.started"' /tmp/claude-mcp.ndjson || true)"
  if [[ "$started" != "1" ]]; then
    echo "FAIL: expected exactly 1 session.started, got ${started}" >&2
    cat /tmp/claude-mcp.ndjson >&2; exit 1
  fi
  grep '"type":"tool.call"' /tmp/claude-mcp.ndjson | grep -q 'echo_sentinel' || {
    echo "FAIL: no tool.call for the declared MCP tool" >&2
    cat /tmp/claude-mcp.ndjson >&2; exit 1
  }
  grep '"type":"tool.result"' /tmp/claude-mcp.ndjson | grep -q 'AGENTCTL_E2E_MCP_SENTINEL_9f21c4' || {
    echo "FAIL: MCP tool.result did not carry the sentinel — the tool did not execute" >&2
    cat /tmp/claude-mcp.ndjson >&2; exit 1
  }
  rm -f /tmp/claude-tools.ndjson /tmp/claude-mcp.ndjson
  echo "ok (declared MCP tool executed)"

  echo "ok (adapter=claude, live)"
  exit 0
fi

if [[ "${AGENT_CONTROLLER_RUN_LIVE:-0}" != "1" ]]; then
  echo "Live E2E skipped: set AGENT_CONTROLLER_RUN_LIVE=1 and ANTHROPIC_API_KEY to exercise the real LLM path."
  echo ""
  echo "For hermetic coverage without an API key:"
  echo "  - Pi adapter:        npm --prefix runtime test"
  echo "                       (exercises runSession() end-to-end against pi-ai's faux provider)"
  echo "  - opencode adapter:  npm --prefix runtime-opencode test"
  echo "                       (unit-tests the config mapper, event translator, and adapter plumbing;"
  echo "                        the opencode subprocess itself is not yet covered hermetically — see"
  echo "                        docs/architecture/harness-matrix.md, Phase 2 follow-up #1)"
  echo "  - codex adapter:     ADAPTER=codex ./e2e/run.sh"
  echo "                       (hermetic fake-codex shim covers the full adapter pipeline)"
  echo "  - claude adapter:    ADAPTER=claude ./e2e/run.sh"
  echo "                       (hermetic: compile rejection + adapter rejection paths)"
  exit 0
fi

if [[ "$ADAPTER" != "codex" ]] && [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "AGENT_CONTROLLER_RUN_LIVE=1 set but ANTHROPIC_API_KEY is empty. Aborting." >&2
  exit 2
fi

case "$ADAPTER" in
  pi)
    echo "==> building runtime (pi)"
    (cd runtime && npm run build)
    export AGENT_CONTROLLER_RUNTIME="$PWD/runtime/dist/index.js"
    # examples/hello.yaml exercises Pi-only surface: custom `get_time` tool,
    # `audit-log` extension, `example-time-skill`. Asserts tool.call/tool.result.
    SPEC="examples/hello.yaml"
    ;;
  opencode)
    echo "==> building runtime-opencode"
    (cd runtime-opencode && npm run build)
    export AGENT_CONTROLLER_RUNTIME="$PWD/runtime-opencode/dist/index.js"
    # Precondition: opencode CLI must be on PATH. The adapter checks this
    # at startup and emits a clear error if missing — surface it here too.
    if ! command -v opencode >/dev/null 2>&1; then
      echo "ADAPTER=opencode but \`opencode\` CLI is not on PATH." >&2
      echo "Install it with \`npm install -g opencode-ai\` or follow https://opencode.ai/docs/" >&2
      exit 2
    fi
    # opencode rejects custom Pi-extension tools and any spec.extensions[]
    # at compile time. examples/hello-opencode.yaml is the opencode-compatible
    # twin of hello.yaml: persona + task only, no tools / extensions / skills.
    # Slice 2.6 codex pass 1 caught the original sed substitution that
    # reused hello.yaml verbatim and would have failed at compile time.
    SPEC="examples/hello-opencode.yaml"
    ;;
  codex)
    # Live codex path — runtime + SPEC already set above; just fall through.
    SPEC="examples/hello-codex.yaml"
    ;;
esac

echo "==> building cli"
(cd cli && go build -o bin/agentctl ./cmd/agentctl)

trap 'rm -f /tmp/run.ndjson /tmp/run.out' EXIT

echo "==> running (live API call, adapter=${ADAPTER}, spec=${SPEC})"
./cli/bin/agentctl run "$SPEC" --raw-out /tmp/run.ndjson > /tmp/run.out

echo "==> asserting"
grep -q '"type":"session.started"' /tmp/run.ndjson || { echo "missing session.started"; cat /tmp/run.ndjson; exit 1; }
# tool.call / tool.result are only asserted on the Pi path because
# examples/hello.yaml declares the `get_time` tool. The opencode-compatible
# spec (examples/hello-opencode.yaml) declares `tools: []`, so no tool
# invocation can occur and asserting these would always fail.
if [[ "$ADAPTER" == "pi" ]]; then
  grep -q '"type":"tool.call"' /tmp/run.ndjson       || { echo "missing tool.call";       cat /tmp/run.ndjson; exit 1; }
  grep -q '"type":"tool.result"' /tmp/run.ndjson     || { echo "missing tool.result";     cat /tmp/run.ndjson; exit 1; }
fi
grep -q '"type":"session.ended"' /tmp/run.ndjson   || { echo "missing session.ended";   cat /tmp/run.ndjson; exit 1; }
echo "ok (adapter=${ADAPTER})"
