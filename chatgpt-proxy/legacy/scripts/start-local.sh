#!/usr/bin/env bash
# chatgpt-proxy — start relay + agent + public HTTPS tunnel in one command.
#
# Run this in YOUR terminal, keep it open, then paste the printed URL into
# ChatGPT Web → Settings → MCP servers.
#
#   ./scripts/start-local.sh          # normal mode — runs until Ctrl+C
#   ./scripts/start-local.sh --verify # proof mode — full MCP check via the
#                                     # public URL, then shuts down itself
#
# Env overrides:
#   AGENT_TOKEN   agent↔relay secret (default: random)
#   CLIENT_TOKEN  client↔relay secret (default: random)
#   WORKSPACE_ROOT  comma-separated dirs the agent may touch (default: this project)
#   RELAY_PORT    local relay port (default 8080)
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
MODE="${1:-}"

AGENT_TOKEN="${AGENT_TOKEN:-$(openssl rand -hex 16)}"
CLIENT_TOKEN="${CLIENT_TOKEN:-$(openssl rand -hex 16)}"
WORKSPACE_ROOT="${WORKSPACE_ROOT:-$ROOT}"
RELAY_PORT="${RELAY_PORT:-8080}"

echo "🔨 building binaries..."
go build -o bin/relay ./cmd/relay
go build -o bin/agent ./cmd/agent

echo "🚀 starting relay on :${RELAY_PORT} ..."
env AGENT_TOKEN="$AGENT_TOKEN" CLIENT_TOKEN="$CLIENT_TOKEN" RELAY_ADDR=":${RELAY_PORT}" ./bin/relay > /tmp/cgp-relay.log 2>&1 &
RELAY_PID=$!

echo "🤖 starting agent (workspace: ${WORKSPACE_ROOT}) ..."
env RELAY_URL="ws://127.0.0.1:${RELAY_PORT}/ws/agent" AGENT_TOKEN="$AGENT_TOKEN" AGENT_NAME="$(hostname -s)" WORKSPACE_ROOT="$WORKSPACE_ROOT" ./bin/agent > /tmp/cgp-agent.log 2>&1 &
AGENT_PID=$!

echo "🌐 opening public HTTPS tunnel (cloudflared http2) ..."
cloudflared tunnel --url "http://127.0.0.1:${RELAY_PORT}" --protocol http2 --no-autoupdate > /tmp/cgp-tunnel.log 2>&1 &
TUNNEL_PID=$!

cleanup() {
  echo ""
  echo "🛑 shutting down..."
  kill "$TUNNEL_PID" "$AGENT_PID" "$RELAY_PID" 2>/dev/null || true
  exit 0
}
trap cleanup INT TERM

# wait for the tunnel URL
PUBLIC_URL=""
echo "⏳ waiting for tunnel..."
for _ in $(seq 1 30); do
  PUBLIC_URL="$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' /tmp/cgp-tunnel.log 2>/dev/null | head -1 || true)"
  [ -n "$PUBLIC_URL" ] && break
  sleep 2
done
sleep 5  # let the edge start routing

if [ -z "$PUBLIC_URL" ]; then
  echo "❌ tunnel failed — check /tmp/cgp-tunnel.log"
  cleanup
fi

# wait until the public URL actually answers
HEALTHY=0
echo "⏳ verifying public URL..."
for _ in $(seq 1 30); do
  if curl -s --max-time 5 "$PUBLIC_URL/healthz" 2>/dev/null | grep -q '"ok":true'; then
    HEALTHY=1
    break
  fi
  sleep 2
done
if [ "$HEALTHY" != 1 ]; then
  echo "❌ public URL never became healthy: $PUBLIC_URL"
  echo "   logs: /tmp/cgp-relay.log /tmp/cgp-agent.log /tmp/cgp-tunnel.log"
  cleanup
fi

echo ""
echo "──────────────────────────────────────────────────────────────"
echo "  ✅ READY — connect ChatGPT Web now"
echo ""
echo "  MCP server URL:   ${PUBLIC_URL}/mcp"
echo "  Auth type:        Bearer"
echo "  Bearer token:     ${CLIENT_TOKEN}"
echo ""
echo "  ChatGPT Web → Settings → MCP servers → Add server"
echo "  (keep this terminal open — Ctrl+C to stop)"
echo "──────────────────────────────────────────────────────────────"
echo ""
echo "logs: /tmp/cgp-relay.log  /tmp/cgp-agent.log  /tmp/cgp-tunnel.log"
echo "health: curl ${PUBLIC_URL}/healthz"

# ---- proof mode: exercise the real MCP protocol through the public URL ----
if [ "$MODE" = "--verify" ]; then
  echo ""
  echo "🔎 verifying MCP through public URL..."
  MCP_ID=0
  SESSION_ID=""
  FAILED=0

  mcp() {  # $1=method  $2=params-json (may be "")  $3=expected substring
    local method="$1" params="$2" expect="$3" resp
    MCP_ID=$((MCP_ID + 1))
    local body="{\"jsonrpc\":\"2.0\",\"id\":${MCP_ID},\"method\":\"${method}\""
    [ -n "$params" ] && body="${body},\"params\":${params}"
    body="${body}}"
    resp="$(curl -sS --max-time 20 -X POST "$PUBLIC_URL/mcp" \
      -H "Content-Type: application/json" \
      -H "Accept: application/json, text/event-stream" \
      -H "Authorization: Bearer $CLIENT_TOKEN" \
      ${SESSION_ID:+-H "Mcp-Session-Id: $SESSION_ID"} \
      -d "$body" 2>/dev/null || true)"
    if [ -z "$SESSION_ID" ]; then
      SESSION_ID="$(curl -sSI --max-time 20 -X POST "$PUBLIC_URL/mcp" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $CLIENT_TOKEN" \
        -d "$body" 2>/dev/null | grep -i '^Mcp-Session-Id:' | tr -d '\r' | awk '{print $2}' || true)"
    fi
    if echo "$resp" | grep -q "$expect"; then
      echo "  ✅ $method"
    else
      echo "  ❌ $method — expected '$expect' not found in response"
      echo "     response: $(echo "$resp" | head -c 300)"
      FAILED=1
    fi
  }

  mcp "initialize" \
    '{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"verify","version":"1.0"}}' \
    "serverInfo"
  mcp "tools/list" "" "workspace_list"
  mcp "tools/call" '{"name":"system_info","arguments":{}}' "hostname"

  echo ""
  if [ "$FAILED" = 0 ]; then
    echo "✅ VERIFY PASSED — stack works end-to-end via public HTTPS"
  else
    echo "❌ VERIFY FAILED — see checks above; logs: /tmp/cgp-*.log"
  fi
  echo "🔚 shutting down (verify mode)..."
  kill "$TUNNEL_PID" "$AGENT_PID" "$RELAY_PID" 2>/dev/null || true
  exit "$FAILED"
fi

# block until the user stops it
wait "$TUNNEL_PID"
