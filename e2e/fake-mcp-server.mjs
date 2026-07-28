#!/usr/bin/env node
/**
 * Minimal stdio MCP server for the e2e live tool tier.
 *
 * Exists so `e2e/run.sh` can assert that a *declared* MCP tool actually
 * executes without reaching the network: no `npx` download, no remote
 * endpoint, just newline-delimited JSON-RPC on stdio (the MCP stdio
 * transport framing).
 *
 * It exposes exactly one tool, `echo_sentinel`, which returns a fixed
 * string. The e2e script greps the wire stream for that string in a
 * `tool.result` event, which can only appear if the whole path worked:
 * spec.mcpServers[] -> Options.mcpServers -> the `mcp__<server>` allow
 * rule -> a real tool execution -> event translation.
 *
 * Protocol surface implemented: initialize, notifications/initialized,
 * ping, tools/list, tools/call. Everything else answers -32601.
 */

const SENTINEL = "AGENTCTL_E2E_MCP_SENTINEL_9f21c4";

const TOOL = {
  name: "echo_sentinel",
  description:
    "Returns a fixed sentinel string. Call this tool whenever you are asked " +
    "for the sentinel value. Takes no arguments.",
  inputSchema: { type: "object", properties: {}, additionalProperties: false },
};

function send(msg) {
  process.stdout.write(JSON.stringify(msg) + "\n");
}

function reply(id, result) {
  send({ jsonrpc: "2.0", id, result });
}

function replyError(id, code, message) {
  send({ jsonrpc: "2.0", id, error: { code, message } });
}

function handle(msg) {
  const { id, method, params } = msg;

  // Notifications carry no id and must never be answered.
  if (id === undefined || id === null) return;

  switch (method) {
    case "initialize":
      // Echo the client's protocol version back so this fixture does not
      // go stale when the SDK bumps its negotiated version.
      reply(id, {
        protocolVersion: params?.protocolVersion ?? "2025-06-18",
        capabilities: { tools: {} },
        serverInfo: { name: "agentctl-e2e-fake-mcp", version: "0.0.1" },
      });
      return;
    case "ping":
      reply(id, {});
      return;
    case "tools/list":
      reply(id, { tools: [TOOL] });
      return;
    case "tools/call":
      if (params?.name !== TOOL.name) {
        replyError(id, -32602, `unknown tool: ${String(params?.name)}`);
        return;
      }
      reply(id, { content: [{ type: "text", text: SENTINEL }], isError: false });
      return;
    default:
      replyError(id, -32601, `method not found: ${String(method)}`);
  }
}

let buf = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buf += chunk;
  let nl;
  while ((nl = buf.indexOf("\n")) !== -1) {
    const line = buf.slice(0, nl).trim();
    buf = buf.slice(nl + 1);
    if (!line) continue;
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      continue; // Ignore malformed frames rather than dying mid-session.
    }
    handle(msg);
  }
});
process.stdin.on("end", () => process.exit(0));
