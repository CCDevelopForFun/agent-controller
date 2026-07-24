#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Adapter selector. Defaults to the legacy "pi" target so existing CI
# behavior is preserved. Override to test the opencode adapter end-to-end:
#   ADAPTER=opencode AGENT_CONTROLLER_RUN_LIVE=1 ANTHROPIC_API_KEY=... ./e2e/run.sh
ADAPTER="${ADAPTER:-pi}"
case "$ADAPTER" in
  pi|opencode|codex) ;;
  *) echo "ADAPTER must be 'pi', 'opencode', or 'codex' (got: $ADAPTER)" >&2; exit 2 ;;
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
