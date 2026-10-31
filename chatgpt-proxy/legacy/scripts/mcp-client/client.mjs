#!/usr/bin/env node
/**
 * chatgpt-proxy MCP debug client.
 *
 * Connects to the relay's /mcp endpoint using the OFFICIAL MCP TypeScript
 * SDK — the same protocol ChatGPT Web uses. If this works, ChatGPT Web will
 * work against the same URL.
 *
 * Usage:
 *   MCP_URL=http://127.0.0.1:8080/mcp MCP_TOKEN=dev-client node client.mjs
 */
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const url = process.env.MCP_URL || "http://127.0.0.1:8080/mcp";
const token = process.env.MCP_TOKEN || "";

const transport = new StreamableHTTPClientTransport(new URL(url), {
  requestInit: {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  },
});

const client = new Client({ name: "chatgpt-proxy-debug-client", version: "1.0.0" });

try {
  await client.connect(transport);
  console.log("✔ connected & initialized");

  const tools = await client.listTools();
  console.log(`✔ tools/list → ${tools.tools.length} tools:`);
  for (const t of tools.tools) console.log(`  - ${t.name}`);

  const info = await client.callTool({ name: "system_info", arguments: {} });
  console.log("✔ tools/call system_info →");
  for (const c of info.content) console.log("  " + c.text.trim().replace(/\n/g, "\n  "));

  const ws = await client.callTool({ name: "workspace_list", arguments: {} });
  console.log("✔ tools/call workspace_list →");
  for (const c of ws.content) console.log("  " + c.text.trim().replace(/\n/g, "\n  "));

  console.log("✔ all good — the relay speaks real MCP");
  await client.close();
  process.exit(0);
} catch (err) {
  console.error("✘ MCP client failed:", err);
  process.exit(1);
}
