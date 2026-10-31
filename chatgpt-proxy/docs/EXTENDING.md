# Extending chatgpt-proxy

## Add a new tool

1. **Implement the browser action** in `internal/chatgpt/chatgpt.go` (a new
   method on `*Client`, e.g. `NewChat`, `AskImage`, `History`), returning
   plain data + `error`.
2. **Expose it** in `internal/server/server.go`:
   - add the name to the `Tool…` consts,
   - add an entry in `tools()` (name, description, `inputSchema`),
   - add a case in `callTool` that parses `p.Arguments` and calls the client,
     returning `mcp.TextResult(...)` or `mcp.ErrorResult(...)`.
3. **Add a unit test** in `internal/server/server_test.go` using the `fakeCG`
   scripted client (extend the interface in `server.go` if needed).
4. Run `gofmt -w internal && go build ./... && go vet ./... && go test ./...`.

Example — a `chatgpt_history` tool that lists recent conversation titles:

```go
// internal/chatgpt/chatgpt.go
func (c *Client) History(ctx context.Context) ([]string, error) {
    c.mu.Lock(); defer c.mu.Unlock()
    tab, err := c.tabCtx(ctx)
    if err != nil { return nil, err }
    var raw string
    js := `JSON.stringify([...document.querySelectorAll('nav a')].map(a => (a.textContent||'').trim()).filter(Boolean))`
    if err := chromedp.Run(tab, chromedp.Evaluate(js, &raw)); err != nil { return nil, err }
    var out []string
    json.Unmarshal([]byte(raw), &out)
    return out, nil
}
```

## Re-tune selectors (when chatgpt.com changes UI)

All page selectors live in **`internal/chatgpt/chatgpt.go`**. If a step starts
failing (e.g. "New chat button not found"), dump the live DOM to find the new
markup. Quick way: run `./bin/chatgpt-proxy -open`, then in DevTools on that
window inspect the element, and update the selector/JS.

Selector locations:

| What | Where |
|---|---|
| login probe | `probeState` |
| New chat click | `startNewChat` |
| type prompt | `typePrompt` |
| submit | `submit` |
| wait/read answer | `waitForAnswer` (`stateJS`) |

Keep selectors as simple and text/anchor-based as possible — they survive
minor redesigns better than deeply-nested `data-testid` chains.

## Tool ideas (from the original inspiration list)

- `ask_chatgpt_image` — attach an image file to the prompt (file input in the
  composer).
- `chatgpt_history` — list past conversations.
- `chatgpt_continue` — resume the last conversation by title.
- `chatgpt_model` — read/switch the active model via the model picker.

The browser layer already gives you the full page — anything the web UI can do
is reachable via CDP + a selector.
