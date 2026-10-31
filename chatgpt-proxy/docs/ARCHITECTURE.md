# Architecture

## Overview

```
┌────────────────────┐   MCP (stdio / HTTP)    ┌───────────────┐   CDP (WebSocket)   ┌──────────────────────┐
│  AI agent          │ ───────────────────────▶│ chatgpt-proxy │ ───────────────────▶│ Chrome (dedicated    │
│  (MCP client)      │   initialize /          │  (Go process) │   navigate/type/    │ profile)             │
│                    │   tools/list /          │               │   evaluate          │  chatgpt.com         │
│                    │   tools/call            │               │                     │  (your login)        │
│                    │ ◀───────────────────────│               │ ◀───────────────────│                      │
└────────────────────┘   result (text)         └───────────────┘                     └──────────────────────┘
```

Two layers, both hand-rolled where it matters:

1. **MCP layer** (`internal/mcp`, `internal/server`) — JSON-RPC 2.0 framing,
   `initialize` / `ping` / `tools/list` / `tools/call`. Transport-agnostic.
2. **Browser layer** (`internal/chatgpt`) — chromedp driving chatgpt.com in a
   dedicated Chrome profile.

## MCP layer

- `internal/mcp` — protocol types (`Request`, `Response`, `Tool`,
  `ToolResult`, …) and helpers. Deliberately hand-rolled (no SDK) to keep the
  wire format explicit.
- `internal/server/server.go` — the `Handler`, which dispatches JSON-RPC to
  the two tools:
  - `ask_chatgpt` → `chatgpt.Client.Ask(prompt, new_session, timeout)`
  - `chatgpt_status` → `chatgpt.Client.Status()`
- `internal/server/transports.go` — two transports:
  - **stdio** — newline-delimited JSON on stdin/stdout (what Hermes / Claude
    Desktop spawn).
  - **Streamable HTTP** — `POST /mcp` returning SSE (`text/event-stream`) or
    JSON depending on the `Accept` header; `DELETE /mcp` → 202; stateless
    (`Mcp-Session-Id` issued but not validated).

## Browser layer (`internal/chatgpt`)

### Chrome lifecycle

The client owns **one Chrome instance** (launched on demand with a dedicated
profile) and **one tab**, serialized by a mutex.

1. `ensureBrowser` scans ports `9222..9231` for an existing DevTools server
   (`GET /json/version`) and attaches if found — this is how a restart reuses
   the already-logged-in session.
2. Otherwise it launches Chrome with `--remote-debugging-port` +
   `--user-data-dir=~/.chatgpt-proxy`, then polls for the port (up to 20s).
   The port is **auto-picked** if the default is taken (a stray listener on
   9222 once wedged us — see HANDOFF bug #2).
3. `connect` creates the chromedp allocator **rooted at `c.life`**
   (`context.Background()`), never at a per-request context, so the browser
   connection survives across MCP calls.

### The chromedp teardown gotcha

chromedp's `RemoteAllocator.Allocate` binds browser teardown to the **context
of the first `Run`**:

```go
a.wg.Go(func() {
    <-ctx.Done()   // ctx = first Run's context
    Cancel(ctx)    // cancels the tab context
    cancel()       // closes the websocket
})
```

If the first `Run` used a short-lived `context.WithTimeout(tab, …)`, the
connection dies the moment that timeout context is cancelled. The fix
(`tabCtx`) forces allocation with a **no-op `chromedp.Run` on the long-lived
tab context** immediately after `NewContext`.

### ask_chatgpt flow

```
Status:      Navigate(chatgpt.com) → probe DOM (composer/sidebar/login) → Status
Ask:         Navigate → (optional) click "New chat" → type prompt → submit →
             poll until the assistant reply is stable (stop button gone, text
             unchanged across 2 polls) → return text
```

DOM selectors (single source of truth, re-tune here if chatgpt.com changes):

| Element | Selector |
|---|---|
| composer | `#prompt-textarea` |
| New chat | `a[href="/"]` (+ text fallback `"New chat"`) |
| send button | `[data-testid="send-button"]` |
| stop generating | `[data-testid="stop-button"]` |
| continue | `[data-testid="continue-generate-button"]` |
| assistant message | `[data-message-author-role="assistant"]` (last) |

Typing uses `document.execCommand('insertText', …)` (fires React input
events); submitting prefers the send button and falls back to Enter. Long
answers auto-click "Continue generating" up to 3 times.

## Code map

| อยากทำอะไร | แก้ที่ไหน |
|---|---|
| เพิ่ม tool ใหม่ | `internal/server/server.go` (`tools()`, `dispatch`, `callTool`) |
| แก้วิธีคุยกับ chatgpt.com | `internal/chatgpt/chatgpt.go` |
| เปลี่ยน transport / เพิ่ม auth | `internal/server/transports.go` + `cmd/proxy/main.go` |
| เปลี่ยน MCP wire format | `internal/mcp/mcp.go` |
