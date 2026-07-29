# `agentctl serve` — HTTP/SSE reference

`agentctl serve` wraps an ADL agent spec in a long-lived HTTP server so external
clients — schedulers, UIs, integration tests — can open sessions and exchange
turns without spawning a process per request.

```bash
agentctl serve examples/hello.yaml --port 8080
```

Works with every adapter; the spec's `runtime.type` selects which one.

## Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | `8080` | TCP port to listen on |
| `--in-memory` | off | Use an in-memory session store instead of SQLite |
| `--max-concurrent-turns` | `8` | Max in-flight turns before returning 429 |
| `--max-sessions` | `1000` | Max active sessions before create returns 429 |
| `--session-ttl` | `168h` | Idle session TTL swept in the background |
| `--shutdown-grace` | `25s` | Max time to drain in-flight turns on SIGTERM |

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness probe — always 200 |
| `GET` | `/readyz` | Readiness probe — 200 until draining, then 503 |
| `POST` | `/v1/sessions` | Create a session. Body optional — omit it, send `{}`, or send `{"inputs":{...}}` for `${inputs.*}` interpolation. Returns 201 `{id,agentName,runtimeType,status,createdAt}` |
| `GET` | `/v1/sessions` | List sessions (all statuses); `?status=active` filters to active |
| `GET` | `/v1/sessions/{id}` | Get a single session |
| `DELETE` | `/v1/sessions/{id}` | Delete a session (204) |
| `POST` | `/v1/sessions/{id}/turns` | Run a turn; body `{"input":"..."}`; streams `text/event-stream` |

**Error codes:** 409 session busy · 429 turn cap or max-sessions · 503 draining.

**SSE frames:** `event: <type>\ndata: <json>\n\n`, ending on `session.ended`.

## Example

```bash
agentctl serve examples/hello.yaml --port 8080 &

SESSION=$(curl -s -X POST http://localhost:8080/v1/sessions \
  -H 'Content-Type: application/json' -d '{}' \
  | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

curl -N -X POST "http://localhost:8080/v1/sessions/${SESSION}/turns" \
  -H 'Content-Type: application/json' \
  -d '{"input":"Hello!"}' --no-buffer
```

A complete runnable script is at [`examples/serve-client.sh`](../examples/serve-client.sh).

## Not yet implemented

Metrics and auth (OIDC / API-key middleware) are deferred to v0.8.x. TLS is
expected to be terminated by a sidecar or load-balancer proxy.
