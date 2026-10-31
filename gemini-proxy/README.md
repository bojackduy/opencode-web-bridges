# gemini-proxy

An **MCP server** that lets any AI agent use **Gemini Web** — for free, through
your own logged-in session, no API key. Same pattern as `chatgpt-proxy`
(real logged-in browser + local model API), as a second engine.

The agent calls `ask_gemini(prompt)`, the proxy drives **gemini.google.com**
(consumer UI) in a dedicated Chrome profile via **CDP** (Chrome DevTools
Protocol), and returns the answer as Markdown text.

```
┌────────────────────┐   MCP (stdio / HTTP)    ┌───────────────┐   CDP    ┌───────────────┐
│  AI agent          │ ───────────────────────▶│ gemini-proxy  │ ───────▶ │ Chrome        │
│  (Hermes, you, …)  │   ask_gemini(prompt)    │  (local MCP   │          │ (gemini.       │
│                    │ ◀───────────────────────│   server)     │ ◀─────── │  google.com)  │
└────────────────────┘   answer as text        └───────────────┘          └───────────────┘
```

- **Uses your Google account's Gemini Web access** — subject to that account's
  usage limits (log in once, session persists in a dedicated Chrome profile).
- **Browser automation, not API** — uses the real web UI via CDP, so it
  survives UI changes better than reverse-engineered endpoints.
- **Standard MCP** — works with any MCP client over `stdio` or Streamable HTTP.
- **Isolation by default** — profile `~/.gemini-proxy`, CDP port **9223**,
  adapter port **8789**. Never shares the chatgpt bridge's profile (`~/.chatgpt-proxy`),
  CDP port (9222), or adapter port (8788).

## Quick start

```bash
# 1. Build
go build -o bin/gemini-proxy ./cmd/gemini-mcp

# 2. First run — open Chrome and log in once
./bin/gemini-proxy -wait-login
#    → a Chrome window opens on gemini.google.com; log in with Google there.
#    → the session is saved to ~/.gemini-proxy and reused forever.

# 3. Run the MCP server (stdio — what most agents spawn)
./bin/gemini-proxy
```

After step 3 the proxy reads MCP messages from stdin and writes responses to
stdout. Wire it into any MCP client.

### Connect Hermes Agent (stdio)

Add to your Hermes `~/.hermes/` MCP config:

```yaml
mcp_servers:
  - name: gemini
    command: /Users/you/.../gemini-proxy/bin/gemini-proxy
```

Then Hermes can call `ask_gemini` like any other tool.

### Streamable HTTP mode (for remote / other clients)

```bash
./bin/gemini-proxy -transport http -addr 127.0.0.1:8787
# POST http://127.0.0.1:8787/mcp   (SSE or JSON)
# GET  http://127.0.0.1:8787/healthz
```

## Tools

| Tool            | Description                                                                |
|-----------------|----------------------------------------------------------------------------|
| `ask_gemini`    | Send a prompt to Gemini Web and return the answer as text.                  |
| `gemini_status` | Check that the bridge is ready (logged in, composer present, profile path). |

### `ask_gemini` arguments

| Arg               | Type    | Default | Meaning                                  |
|-------------------|---------|---------|------------------------------------------|
| `prompt`          | string  | —       | **Required.** The prompt to send.        |
| `new_session`     | boolean | `true`  | Start a fresh conversation each call. Set `false` to continue the previous one. |
| `timeout_seconds` | number  | `600`   | Max seconds to wait for the answer.      |

## Flags

| Flag             | Default                       | Meaning                                        |
|------------------|-------------------------------|------------------------------------------------|
| `-transport`     | `stdio`                       | `stdio` or `http`                              |
| `-addr`          | `127.0.0.1:8787`              | Listen address (http transport)                |
| `-chrome-port`   | `9223`                       | Exact Chrome remote-debugging port (fail closed if occupied by another profile) |
| `-profile`       | `~/.gemini-proxy`             | Chrome profile dir (holds the Google login)    |
| `-chrome`        | auto-detected                 | Chrome binary path override                    |
| `-open`          | —                             | Open Chrome with the profile and exit          |
| `-wait-login`    | —                             | Open Chrome and poll until logged in, then exit |
| `-background`    | `false`                       | Run Chrome in background (off-screen/minimized)|

## Project layout

```
cmd/gemini-mcp/           MCP server entrypoint (stdio + HTTP)
cmd/gemini-openai/        OpenAI-compatible adapter (127.0.0.1:8789)
internal/mcp/             MCP / JSON-RPC protocol types
internal/gemini/          Chrome CDP automation of gemini.google.com
internal/server/          MCP dispatch + tool implementations + transports
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `not logged in` | The Chrome window opened but you didn't log in. Run `-wait-login` and log in with Google once. |
| `gemini composer not found` | Page layout changed, or a modal is up. Open the proxy Chrome window and check, then retry. Selectors live in `internal/gemini/gemini.go` (documented inventory). |
| `timed out waiting for an answer` | Long response or slow network. Raise `timeout_seconds`. |
| `chrome binary not found` | Pass `-chrome /path/to/chrome`, or install Chrome. |
| Port 9223 was busy | Verify the listener's profile. The bridge refuses another profile or neighboring port. |
| `native input failed: context deadline exceeded` on a large catalog | Rebuild both binaries: the editor now uses a batched genuine Quill user-source update instead of slow native per-line paste. No clipboard or context truncation. |
| `Quill input refused: foreign-draft` | No Send occurred. The editor contains unowned or altered text; the bridge refuses to clear or append to it. Inspect the owned tab rather than repeatedly submitting. |

> ⚠️ This uses your personal Google session. Only expose the HTTP transport to
> agents you trust; `stdio` mode binds to nothing on the network.

## Tests

```bash
go test ./...
```

Covers the tool-wrapper protocol, HTML→Markdown extraction, MCP dispatch,
adapter cache/protocol/profile isolation, and cancellation-context ownership.
Optional offline browser fixtures exercise large input, foreign drafts,
post-timeout exact-draft reuse, and canceled turns with no Send:

```sh
go test -tags browser ./internal/gemini -run TestComposer -v
# Optional executable override: GEMINI_TEST_CHROME=/path/to/chromium
```

These fixtures use a disposable local page, not a Google account or user profile.
The actual Gemini UI still requires a separate signed-in live check.

## Gemini Web as an OpenCode model

Build the separate OpenAI-compatible adapter (the MCP binary and port 8787
remain available):

```sh
go build -o bin/gemini-openai ./cmd/gemini-openai
./bin/gemini-openai -addr 127.0.0.1:8789
```

It connects to the logged-in Chrome profile `~/.gemini-proxy` on port 9223 and
creates **its own tab**. `/readyz` probes that tab without navigating.

Readiness is per session: a session holding an uncertain pending turn is reported
not-ready only to a probe that names it (`x-opencode-session-id`), while the
provider keeps reporting `ready:true` with a `pending` count and the pending
session ids. One stuck session therefore never makes the model unavailable for
everyone else.

Clearing a pending record that no code path can resolve — for example one written
before the recovery stack existed, with `pending` set and no stored prompt — is an
explicit operator action. It is never automatic, never a wildcard, and it refuses
while a turn is in flight:

```sh
curl -sX POST 127.0.0.1:8789/v1/sessions/abandon \
  -d '{"sessions":["ses_..."],"reason":"legacy pending record, no stored prompt"}'
```

Only the pending binding is cleared; the transcript, thread URL, duplicate-cache
identity and last result are retained, so the session keeps working. For any
pending record that still carries its prompt and thread, the adapter's own
recovery (exact adoption, hash-lineage auto-resync, rewrite reseed) runs first and
is never weakened by this endpoint.

The adapter speaks the exact `<opencode_bridge_call>` tool protocol from
chatgpt-proxy, so the plugin treats both engines identically. State lives in
`~/.gemini-proxy/openai-sessions.json` (atomic replacement, mode 0600).

Parent `opencode-web-bridges` now offers explicit optional Gemini registration
using a local-built MCP binary (see the parent README). Provider/model is
`gemini-web/gemini-web`, adapter base URL `http://127.0.0.1:8789/v1`.
No Gemini release assets are available yet; no automatic release fallback is
claimed. `bin/gemini-proxy` is the MCP binary built from `cmd/gemini-mcp`.

Signed-in Vietnamese DOM was verified on macOS. A genuine Quill `setText`
user-source update batches large catalogs; submission requires exact
`rich-textarea.__quill.getText()`
identity, not visual-only text. The localized send button is `Gửi tin nhắn`.
The prompt gets one explicit structural newline, including when it already ends
in newlines. Input never submits; a canceled input can finish late without Send.
Only an exact draft carrying this client's ownership marker can be reused after
a proved pre-submit failure, in the same pending thread. Unknown click outcomes
remain uncertain and are never automatically resubmitted.
CDP focus emulation keeps the owned page active for Gemini's code-block reveal
animations without selecting a preexisting tab or stealing OS keyboard focus
from the user's terminal. Exact Quill identity still rejects any unexpected
composer text before submission.
Fresh answers use stable `message-content` IDs and completed Markdown content,
excluding hidden screen-reader labels. Pending uncertain submissions fail
closed instead of being retried. Exact retries reuse completion and tool IDs.

Phase-1 limitations: serial owned tab, text only, no projects/concurrency or
recovery UX, bounded single-turn replay. SSE is buffered after validation.
After restarting the adapter or switching client sessions, history is replayed
in a fresh chat; persistent cache alone does not restore a live browser tab.
Profile verification requires lsof/ps on macOS/Linux; Windows fails closed.
Signed-in Gemini verification is on macOS; offline Chromium composer/cancellation
fixtures also run on actual Linux (not merely cross-compiled). Full single-turn
history/tool input remains subject to `-max-chars` (a byte budget); oversized
requests are rejected without dropping tools or context. History chunking/import
and large-context recovery remain future work, not full native API parity.
The optional v2 provider registers text-only modalities and reports context `0`
(unknown), not a guarantee of unlimited input. Its tool catalog and replayed
history still count toward the adapter's default 200,000-byte turn budget.
Backlog: token-aware context budgeting/compaction integration and chunked history
import; raising a model's advertised context does not implement either feature.
