#!/usr/bin/env bash
# serve-client.sh — minimal example client for `agentctl serve`.
#
# Usage:
#   agentctl serve examples/hello.yaml --port 8080 &
#   ./examples/serve-client.sh [base-url]
#
# The script creates a session, runs a turn, and streams the SSE response.
# It requires only curl and python3 (no jq).
set -euo pipefail

BASE_URL="${1:-http://localhost:8080}"

# 1. Create a session.
echo "==> POST ${BASE_URL}/v1/sessions"
BODY=$(curl -sf -X POST "${BASE_URL}/v1/sessions" \
  -H 'Content-Type: application/json' \
  -d '{}')
echo "    response: ${BODY}"

# Extract the session id using python3 (no jq required).
SESSION_ID=$(echo "${BODY}" | python3 -c "import sys, json; print(json.load(sys.stdin)['id'])")
echo "    session id: ${SESSION_ID}"

# 2. Run a turn and stream SSE events.
echo ""
echo "==> POST ${BASE_URL}/v1/sessions/${SESSION_ID}/turns (streaming SSE)"
curl -sN -X POST "${BASE_URL}/v1/sessions/${SESSION_ID}/turns" \
  -H 'Content-Type: application/json' \
  -d '{"input":"Hello! What can you help me with?"}' \
  --no-buffer

echo ""
echo "done."
