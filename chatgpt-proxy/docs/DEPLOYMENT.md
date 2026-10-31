# Running & connecting agents

## 1. Build

```bash
go build -o bin/chatgpt-proxy ./cmd/proxy
```

## 2. First-time login (once)

```bash
./bin/chatgpt-proxy -wait-login
```

A Chrome window opens on chatgpt.com using a **dedicated profile**
(`~/.chatgpt-proxy`) — your normal Chrome is untouched. Log in there once; the
session persists and every later run reuses it.

> If you ever get signed out, just run `-wait-login` again.

## 3. Run the MCP server

```bash
# stdio — the default; spawn this from your agent's MCP config
./bin/chatgpt-proxy

# or HTTP, for remote/non-stdio clients
./bin/chatgpt-proxy -transport http -addr 127.0.0.1:8787
```

## 4. Connect an agent

### Hermes Agent (stdio)

Add to your Hermes MCP config (`~/.hermes/` — see `hermes-config-web` for the
UI) a server pointing at the binary:

```yaml
mcp_servers:
  - name: chatgpt
    command: /absolute/path/to/chatgpt-proxy/bin/chatgpt-proxy
```

Hermes will spawn it as a stdio subprocess and gain `ask_chatgpt` +
`chatgpt_status`.

### Claude Desktop (stdio)

In `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "chatgpt": {
      "command": "/absolute/path/to/bin/chatgpt-proxy"
    }
  }
}
```

### Any HTTP MCP client

Point it at `http://127.0.0.1:8787/mcp` (SSE transport), or expose it over a
tunnel/HTTPS if the client is remote.

## 5. Verify

```bash
# HTTP mode
curl http://127.0.0.1:8787/healthz
# → {"ok":true}

curl -X POST http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chatgpt_status","arguments":{}}}'
# → {"content":[{"type":"text","text":"{\"logged_in\":true,...}"}]}
```

## Security notes

- **stdio mode binds to nothing** — safe by default.
- **HTTP mode has no auth yet** (`P0` in HANDOFF). Keep it on loopback
  (`127.0.0.1`) and only expose it to agents you trust, or put an
  authenticating reverse proxy in front.
- This uses **your personal ChatGPT session** — treat any agent that can reach
  `ask_chatgpt` as being able to chat as you.
