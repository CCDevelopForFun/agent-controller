#!/usr/bin/env bash
# fake-codex.sh — hermetic codex shim for the ADAPTER=codex e2e tier.
#
# Intercepts the two codex sub-commands the runtime-codex adapter calls:
#
#   codex login --with-api-key
#     Reads (and discards) stdin, writes a dummy auth.json into $CODEX_HOME
#     (when set), and exits 0.
#
#   codex exec [args...]
#     Emits the minimal JSONL sequence that event-translator.ts maps to
#     session.started → message → session.ended, then exits 0.
#
# All other invocations exit 0 silently (safe no-op).
set -euo pipefail

subcmd="${1:-}"

case "$subcmd" in
  login)
    # Drain stdin (the adapter pipes the API key here).
    cat > /dev/null
    # Write a dummy auth.json into CODEX_HOME so seedCodexAuth skips on resume.
    if [[ -n "${CODEX_HOME:-}" ]]; then
      printf '{"provider":"openai","apiKey":"test-key"}\n' > "${CODEX_HOME}/auth.json"
    fi
    exit 0
    ;;

  exec)
    # Emit the canned JSONL that event-translator.ts maps to the three expected
    # wire events: session.started, message (assistant), session.ended.
    #
    # thread.started  → session.started
    # item.completed  → message{role:assistant}   (agent_message item type)
    # turn.completed  → session.ended{reason:completed}
    printf '{"type":"thread.started","thread_id":"fake-thread-001"}\n'
    printf '{"type":"turn.started","turn_id":"fake-turn-001"}\n'
    printf '{"type":"item.completed","item":{"id":"fake-item-001","type":"agent_message","text":"Hello! I am running under the codex adapter (hermetic fake)."}}\n'
    printf '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":12}}\n'
    exit 0
    ;;

  *)
    # Unknown sub-command — no-op.
    exit 0
    ;;
esac
