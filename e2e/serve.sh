#!/usr/bin/env bash
# e2e/serve.sh — hermetic + optional live E2E tests for `agentctl serve`.
#
# Hermetic tier (no API key required):
#   ./e2e/serve.sh
#
# Live tier (requires ANTHROPIC_API_KEY; runs a real turn over SSE):
#   AGENT_CONTROLLER_RUN_LIVE=1 ANTHROPIC_API_KEY=... ./e2e/serve.sh
#
# The hermetic tier exercises server startup, /healthz, POST /v1/sessions,
# and GET /v1/sessions/{id} using --in-memory + a self-contained spec.
# The live tier additionally POSTs a turn and asserts the SSE stream.
set -euo pipefail
cd "$(dirname "$0")/.."

TEST_PORT=18080
BASE_URL="http://localhost:${TEST_PORT}"
SPEC="examples/serve-test.yaml"
SERVER_PID=""

cleanup() {
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -f /tmp/agentctl-serve-e2e.log \
        /tmp/agentctl-serve-session.json \
        /tmp/agentctl-serve-live-session.json \
        /tmp/agentctl-serve-turn.sse
}
trap cleanup EXIT

# ── Build ────────────────────────────────────────────────────────────────────

echo "==> building cli"
(cd cli && mkdir -p bin && go build -o bin/agentctl ./cmd/agentctl)

# ── Start server ──────────────────────────────────────────────────────────────

echo "==> starting agentctl serve on port ${TEST_PORT} (--in-memory)"
# AGENT_CONTROLLER_RUNTIME is set to a placeholder so resolveRuntimeCommand
# does not require a built runtime adapter — the runtime is not invoked for
# the hermetic tests (session create / get / healthz don't run turns).
AGENT_CONTROLLER_RUNTIME="/dev/null" \
  ./cli/bin/agentctl serve "${SPEC}" \
    --port "${TEST_PORT}" \
    --in-memory \
    2>/tmp/agentctl-serve-e2e.log &
SERVER_PID=$!

# Wait for server to be ready (up to 10 s).
echo "==> waiting for server to be ready"
ready=0
for i in $(seq 1 20); do
  if curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if [[ "${ready}" -eq 0 ]]; then
  echo "FAIL: server did not become ready in 10 s" >&2
  echo "--- server stderr ---" >&2
  cat /tmp/agentctl-serve-e2e.log >&2
  exit 1
fi

# ── Hermetic tests ────────────────────────────────────────────────────────────

echo "==> test: GET /healthz → 200"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "${BASE_URL}/healthz")
[[ "${STATUS}" == "200" ]] || { echo "FAIL: /healthz returned ${STATUS}"; exit 1; }
echo "    ok"

echo "==> test: POST /v1/sessions → 201 + id"
HTTP_CODE=$(curl -s -o /tmp/agentctl-serve-session.json -w "%{http_code}" \
  -X POST "${BASE_URL}/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d '{}')
[[ "${HTTP_CODE}" == "201" ]] || { echo "FAIL: POST /v1/sessions returned ${HTTP_CODE}"; cat /tmp/agentctl-serve-session.json; exit 1; }
SESSION_ID=$(python3 -c "import sys,json; print(json.load(open('/tmp/agentctl-serve-session.json'))['id'])")
[[ -n "${SESSION_ID}" ]] || { echo "FAIL: session id is empty"; exit 1; }
echo "    ok (id=${SESSION_ID})"

echo "==> test: GET /v1/sessions/${SESSION_ID} → 200"
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "${BASE_URL}/v1/sessions/${SESSION_ID}")
[[ "${HTTP_CODE}" == "200" ]] || { echo "FAIL: GET /v1/sessions/${SESSION_ID} returned ${HTTP_CODE}"; exit 1; }
echo "    ok"

echo ""
echo "ok (hermetic)"

# ── Live tier (gated on AGENT_CONTROLLER_RUN_LIVE=1 + ANTHROPIC_API_KEY) ─────

if [[ "${AGENT_CONTROLLER_RUN_LIVE:-0}" != "1" ]]; then
  echo ""
  echo "Live E2E skipped: set AGENT_CONTROLLER_RUN_LIVE=1 and ANTHROPIC_API_KEY to exercise the real LLM path."
  exit 0
fi

if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "AGENT_CONTROLLER_RUN_LIVE=1 set but ANTHROPIC_API_KEY is empty. Aborting." >&2
  exit 2
fi

# Stop the --in-memory server and restart with a real runtime adapter.
# (API key check is above so we don't rebuild unnecessarily when it's missing.)
kill "${SERVER_PID}" 2>/dev/null || true
wait "${SERVER_PID}" 2>/dev/null || true
SERVER_PID=""

echo "==> building runtime (pi)"
(cd runtime && npm run build)
export AGENT_CONTROLLER_RUNTIME="$PWD/runtime/dist/index.js"

echo "==> restarting agentctl serve with live runtime on port ${TEST_PORT}"
./cli/bin/agentctl serve "${SPEC}" \
  --port "${TEST_PORT}" \
  --in-memory \
  2>/tmp/agentctl-serve-e2e.log &
SERVER_PID=$!

ready=0
for i in $(seq 1 40); do
  if curl -sf "${BASE_URL}/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
[[ "${ready}" -eq 1 ]] || { echo "FAIL: live server did not become ready"; exit 1; }

echo "==> creating live session"
HTTP_CODE=$(curl -s -o /tmp/agentctl-serve-live-session.json -w "%{http_code}" \
  -X POST "${BASE_URL}/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d '{}')
[[ "${HTTP_CODE}" == "201" ]] || { echo "FAIL: create session returned ${HTTP_CODE}"; exit 1; }
LIVE_SESSION_ID=$(python3 -c "import sys,json; print(json.load(open('/tmp/agentctl-serve-live-session.json'))['id'])")

echo "==> test: POST /v1/sessions/${LIVE_SESSION_ID}/turns → SSE stream contains session.ended"
curl -sN -X POST "${BASE_URL}/v1/sessions/${LIVE_SESSION_ID}/turns" \
  -H 'Content-Type: application/json' \
  -d '{"input":"Say hello in one sentence."}' \
  --no-buffer \
  > /tmp/agentctl-serve-turn.sse

grep -q 'session.ended' /tmp/agentctl-serve-turn.sse \
  || { echo "FAIL: SSE stream missing session.ended"; cat /tmp/agentctl-serve-turn.sse; exit 1; }
echo "    ok"

echo ""
echo "ok (live)"
