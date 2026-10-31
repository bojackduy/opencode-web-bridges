# chatgpt-proxy

An **MCP server** that lets any AI agent (Hermes Agent, Claude Desktop,
Codebuff-style assistants, …) use **ChatGPT Web** — for free, through your own
logged-in session, no API key.

The agent calls `ask_chatgpt(prompt)`, the proxy drives chatgpt.com in a
dedicated Chrome profile via **CDP** (Chrome DevTools Protocol), and returns
the answer as plain text.

```
┌────────────────────┐   MCP (stdio / HTTP)    ┌───────────────┐   CDP    ┌───────────────┐
│  AI agent          │ ───────────────────────▶│ chatgpt-proxy │ ───────▶ │ Chrome        │
│  (Hermes, you, …)  │   ask_chatgpt(prompt)   │  (local MCP   │          │ (chatgpt.com) │
│                    │ ◀───────────────────────│   server)     │ ◀─────── │ your session  │
└────────────────────┘   answer as text        └───────────────┘          └───────────────┘
```

- **Uses your ChatGPT Web account** — subject to that account's usage limits (login once, session
  persists in a dedicated Chrome profile).
- **Browser automation, not API** — uses the real web UI via CDP, so it
  survives UI changes better than reverse-engineered endpoints.
- **Standard MCP** — works with any MCP client over `stdio` or Streamable HTTP.

## Quick start

```bash
# 1. Build
go build -o bin/chatgpt-proxy ./cmd/proxy

# 2. First run — open Chrome and log in once
./bin/chatgpt-proxy -wait-login
#    → a Chrome window opens on chatgpt.com; log in there.
#    → the session is saved to ~/.chatgpt-proxy and reused forever.

# 3. Run the MCP server (stdio — what most agents spawn)
./bin/chatgpt-proxy
```

After step 3 the proxy reads MCP messages from stdin and writes responses to
stdout. Wire it into any MCP client.

### Connect Hermes Agent (stdio)

Add to your Hermes `~/.hermes/` MCP config:

```yaml
mcp_servers:
  - name: chatgpt
    command: /Users/you/.../chatgpt-proxy/bin/chatgpt-proxy
```

Then Hermes can call `ask_chatgpt` like any other tool.

### Streamable HTTP mode (for remote / other clients)

```bash
./bin/chatgpt-proxy -transport http -addr 127.0.0.1:8787
# POST http://127.0.0.1:8787/mcp   (SSE or JSON)
# GET  http://127.0.0.1:8787/healthz
```

## Tools

| Tool             | Description                                                                 |
|------------------|-----------------------------------------------------------------------------|
| `ask_chatgpt`    | Send a prompt to ChatGPT Web and return the answer as text.                  |
| `chatgpt_status` | Check that the bridge is ready (logged in, composer present, profile path). |

### `ask_chatgpt` arguments

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
| `-chrome-port`   | `9222` (auto-picks if busy)   | Chrome remote-debugging port                   |
| `-profile`       | `~/.chatgpt-proxy`            | Chrome profile dir (holds the ChatGPT login)   |
| `-chrome`        | auto-detected                 | Chrome binary path override                    |
| `-open`          | —                             | Open Chrome with the profile and exit          |
| `-wait-login`    | —                             | Open Chrome and poll until logged in, then exit |
| `-background`    | `false`                       | Run Chrome in background (off-screen/minimized)|

## API (HTTP mode, for debugging)

```bash
# initialize
curl -X POST http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"curl","version":"1"}}}'

# ask
curl -X POST http://127.0.0.1:8787/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_chatgpt","arguments":{"prompt":"hello"}}}'
```

## Project layout

```
cmd/proxy/              MCP server entrypoint (stdio + HTTP)
internal/mcp/           MCP / JSON-RPC protocol types
internal/chatgpt/       Chrome CDP automation of chatgpt.com
internal/server/        MCP dispatch + tool implementations + transports
legacy/                 the earlier relay/agent (outbound tunnel) — kept for reference
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `not logged in` | The Chrome window opened but you didn't log in. Run `-wait-login` and log in once. |
| `chatgpt composer not found` | Page layout changed, or a modal is up. Open the proxy Chrome window and check, then retry. |
| `timed out waiting for an answer` | Long response or slow network. Raise `timeout_seconds`. |
| `chrome binary not found` | Pass `-chrome /path/to/chrome`, or install Chrome. |
| Response looks truncated | The proxy auto-clicks "Continue generating" up to 3 times; very long answers still stop there. |
| Port 9222 was busy | The proxy auto-picks the next free port — no action needed. |

> ⚠️ This uses your personal ChatGPT session. Only expose the HTTP transport to
> agents you trust; `stdio` mode binds to nothing on the network.

## Tests

```bash
go test ./...
```

Covers MCP dispatch (initialize, tools/list, tools/call, error paths, SSE +
JSON HTTP responses). The browser layer is verified by the live flow — run
`-wait-login` once, then call `ask_chatgpt`.

## ChatGPT Web as an OpenCode model

Build the separate OpenAI-compatible adapter (the MCP binary and port 8787
remain available):

```sh
go build -o bin/chatgpt-openai ./cmd/chatgpt-openai
./bin/chatgpt-openai -addr 127.0.0.1:8788
```

The installed service is `com.chatgpt-proxy.openai`. It connects to the logged-in
Chrome profile `~/.chatgpt-proxy` on port 9222 and creates **its own tab**, rather
than navigating the MCP tab. `/readyz` probes that tab without navigating;
immediately after startup it can report 503 while the page hydrates.
The profile's `openai-tab-id` file reserves that target ID so a newly started
MCP client also skips the adapter tab; stale IDs after Chrome exits are harmless.

The global OpenCode provider is `chatgpt-web/chatgpt-web`:

```json
{
  "provider": {
    "chatgpt-web": {
      "name": "ChatGPT Web (browser tool bridge)",
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "http://127.0.0.1:8788/v1" },
      "models": {
        "chatgpt-web": {
          "name": "ChatGPT Web (tools, persistent sessions)",
          "tool_call": true,
          "attachment": true,
          "reasoning": false,
          "temperature": false,
          "modalities": { "input": ["text", "image", "video"], "output": ["text"] },
          "limit": { "context": 128000, "output": 32768 }
        }
      }
    }
  }
}
```

**Quit and restart OpenCode after config changes**, then select this model in
`/models`, or run `opencode run -m chatgpt-web/chatgpt-web 'your prompt'`.
No API key is required. The configured token limits are client-side budgets,
not a claim about the selected ChatGPT Web model's actual context window.

### Tools and conversation identity

All **new adapter chats** start inside the dedicated **OpenCode Bridge** ChatGPT
Project. On first use the adapter reuses its named sidebar control or creates the
Project using the logged-in browser's Create project dialog. Its `/g/g-p-.../project`
URL is cached in `~/.chatgpt-proxy/openai-project-url` (mode 0600). Before submitting
a fresh turn the adapter verifies the named Project composer; a missing/deleted
Project or changed DOM fails closed rather than creating a main-page chat.
Existing mapped chats from the earlier adapter remain in place and are not moved,
deleted, or renamed. MCP `ask_chatgpt` retains its original separate behavior.
If discovery/creation fails, open the logged-in profile, create/reuse OpenCode
Bridge, and save that Project's URL in the cache file. Do not paste a chat URL there.
Project UI/memory features are ChatGPT-owned; use only the supplied conversation
for sensitive work and review your account's memory settings. Browser mapping is
isolated per session, not a guarantee about ChatGPT's account-level memory.

ChatGPT receives the external client's function schemas and a text protocol:

```text
<opencode_bridge_call>
{"name":"get_weather","arguments":{"city":"Hanoi"}}
</opencode_bridge_call>
```

The distinct wrapper is prompt-emulated, not a native ChatGPT tool. The adapter checks the function name and
JSON-object arguments against JSON Schema and `tool_choice`, and returns native
OpenAI `tool_calls`. **OpenCode executes the tools**, then supplies their results
on the next provider turn. Unknown names, bad JSON, schema violations and missing
required calls produce an OpenAI-shaped HTTP error, never executable calls.
Completed protocol failures return HTTP 422 (`invalid_request_error`), including
streaming requests before any SSE data or tool calls are emitted. Exact rejected
fingerprints cannot be resubmitted, even after continuation. A distinct new
ordinary user message must end the authoritative client transcript to continue
in a fresh thread; v2 may rebuild its system/history or retain failed assistant
placeholders without preserving the rejected request's byte prefix. Changing
only schemas, history, or tool results does not authorize continuation. Volatile
v2 `<system-update>` user-role metadata is not ordinary input; comparison ignores
it without removing it from the context. Appended undelivered tool calls/results
cannot be laundered by adding a user message afterward. The new
thread receives full current tool schemas. Unknown submitted turns still require
recovery and retain their Dirty journal. Their completion guard also returns
terminal 422 with the distinct uncertainty/recovery instruction, because installed
v2 retries 409 indefinitely; this changes classification, not recovery safety.
Recovery/resync endpoint conflicts still use 409. Explicit per-session project
setup failures before typing/submission are safe to retry only if no conversation
URL was reached; they are not unknown submitted turns.

Tool wrappers mixed with prose or Markdown fences are rejected. Ordinary Markdown
code examples are not executable bridge requests.
External `$ref` schema downloads are disabled; local schema references work.
SSE tool deltas contain `index`, stable call IDs and JSON argument strings, followed
by `finish_reason: "tool_calls"` and `[DONE]`.

Session identity comes from `X-Session-Id`, `x-opencode-session`, or
`x-session-affinity` (in that priority order). The inspected OpenCode source,
`packages/opencode/src/session/llm/request.ts`, sends `X-Session-Id` and
`x-session-affinity` for custom providers. Other clients must
send one of these headers themselves.

OpenCode's title generator uses the **same session ID** as its main agent but
different system/user messages. Install `docs/opencode-session-lanes.ts` as an
OpenCode plugin and restart OpenCode to load it. It registers one hook per
host, both keyed off the request's agent, and both adding `X-OpenCode-Request-Lane`
for the `chatgpt-web` provider: `main` for primary agents (including agent
switches), and `title`, `summary`, or `compaction` for internal agents.
Auxiliary lanes have separate persistent journals and cannot overwrite or dirty
the main thread. Clients may explicitly use `auxiliary` for other background
requests. Unknown lanes return HTTP400. Without the hook/header, the adapter
preserves legacy main-session behavior; it does not guess a request's purpose
from tool counts or prompt text.

v1 uses the `chat.headers` hook. v2 has no such hook, so it registers
`session` `model.request` instead, which carries the same agent plus a mutable
per-request header bag seeded from the request's existing headers. Lane identity
therefore travels per request and **one** `chatgpt-web` provider entry serves
every lane. Earlier revisions instead registered one visible provider per lane
(`chatgpt-web-title`, `-summary`, `-compaction`) with the lane baked into static
headers, because the per-request hook was assumed not to exist; that showed four
duplicate "ChatGPT Web" entries in the v2 model picker.

On a genuine prefix difference, logs report `prefix_mismatch` with the first
zero-based message index, structural field, original content encodings,
incoming/saved counts, and content-free value shapes (`str:<bytes>:<lines>`,
`parts:<n>`, `null`). They contain no message text, tool argument values,
call IDs, or media payloads. A mismatch still causes guarded reset/replay;
diagnostics do not waive actual edits, truncation, or tool/media identity.

### Stateful thread architecture (thread = context)

The Web bridge is **stateful**: the adapter submits a prompt into one persistent
ChatGPT conversation and ChatGPT's server keeps the context. The OpenAI request
still carries the whole client history (that is how `@ai-sdk/openai-compatible`
works), but the adapter does **not** forward it verbatim:

- **Continuation (delta):** when the incoming history extends the exact canonical
  stored prefix, only the new messages are sent; tool schemas and `tool_choice` are
  restated for the turn. The adapter's own assistant turn is remembered and never
  replayed.
- **Switch away and back:** OpenCode's transcript is authoritative; the Web thread
  is cached context. Completed exchanges from another provider are imported only
  from the missing suffix on the **same Project thread**. User questions, labelled
  assistant answers, historical tool calls/results and media attachments are
  retained. Browser submissions cannot insert native assistant roles, so these
  are inert JSON transcripts in a user message, enclosed in `BRIDGE HISTORY UPDATE`
  / `END HISTORY UPDATE`. They are context, not questions to answer or tools to run.
  A small gap shares one submission with a separate `CURRENT REQUEST`; large gaps
  use bounded history-only chunks requiring exactly `ACK`, swallowed internally,
  followed by the current request with **current** tool schemas. Unexpected import
  output (including tool wrappers) fails closed and never becomes `tool_calls`.
  The durable synchronized prefix advances only for fully imported messages after
  acknowledgement; final-turn uncertainty keeps the dirty journal and blocks
  resubmission (terminal 422 on completion requests).
  Edited/branched/compacted prefixes create a new Project thread, leaving the old
   one untouched. Canonical comparison preserves media and message names while
   normalizing equivalent SDK text/null-content and JSON argument encodings,
   plus five proven-benign regeneration classes (classified on real captured
   transcripts, two independent sessions): skill `<location>` root-spelling /
   variant-dir / trash-UUID flips, co-located skill `<description>` changes,
   the conditional compress-reminder block, per-resume `<dcp-message-id>`
   regeneration on user/tool messages, and the assistant platform envelope
   (bridge stores the bare answer; OpenCode re-emits it enveloped). Skill
   names, agent prompt, environment, references, MCP text, genuine
   user/assistant/tool edits, truncation, compaction, and tool/media identity
   stay byte-exact and still force reset; anything unrecognized fails safe.
- **Fresh / mismatched history (chunked replay):** a new OpenCode session, a reused
  session ID whose history no longer matches (client compaction), or state lost
  after a restart has no usable thread. Instead of concatenating the history into
  one composer paste, the adapter **replays it as a sequence of bounded turns**
  into a single thread (each composed turn `<= -max-chars`, default 200000 bytes),
  then sends the final turn that asks for the answer. The first replayed turn
  creates the thread; the rest continue it. An oversized single message is split
  at rune boundaries so the per-turn bound always holds.
- `-max-chars` governs the **per-turn chunk size**, never the total history: the
  old total-prompt `413` cap is gone, so a long history is accepted and replayed.
- The thread URL is persisted **turn by turn**, so a crash mid-replay keeps a
  resume target instead of losing the whole session.

### Measured thread retention (honest)

A live replay of **481,686 chars / 506 messages** (~120k tokens at ~4 bytes/token)
into a fresh thread, in 5 bounded ~98.7k-char turns, still recalled a codeword
placed in the very first chunk. A larger **961,494-char / 1,010-message** probe
replayed 5 of 10 bounded turns but each turn then slowed sharply and the probe was
stopped, so **no ceiling above the 481k-char measurement was proven**. The true
server-side ceiling was **not** measured. `limit.context` is therefore set to
**128000** tokens, a conservative, evidence-aligned client budget. This is *not* a
claim that ChatGPT Web has a 1M-token window; do not raise it further without a new
replay test that still recalls early content. OpenCode compacts to this budget, so
the adapter replays at most ~128k tokens worth of context.

- Each session stores its actual ChatGPT conversation URL (`/c/...` or Project
  `/g/g-p-.../c/...`). Switching sessions
  restores that URL, including after restarting launchd.
- Only new messages are sent on continuation. The adapter's returned assistant
  turn is remembered, so it is not replayed as a user prompt. Full tool schemas
  are taught **once per thread** (fresh thread or changed tool set); later turns
  carry only a compact reminder naming the available functions, since the thread
  holds the schemas server-side. `tool_choice` is restated every turn.
- Exact last-request retries return the saved parsed answer and **same completion
  ID, creation time and tool IDs** (including after restarting the adapter).
  Browser transactions are serialized across all sessions, including concurrent
  duplicate requests.
- State lives in `~/.chatgpt-proxy/openai-sessions.json` (atomic replacement, mode
  0600). It contains conversation text and cached results; treat it as private.
- **No session header:** isolated fresh chat per request. No prefix-based guessing
  or cross-client conversation reuse.
- **Divergent/compacted history:** re-established by replaying the full client
  history into a **new** thread (`decision=reset-replay`); the previous thread is
  left untouched. This supports clients that compact history under the same ID.
- A failure/cancellation after a turn is admitted marks it uncertain on disk,
  recording whether it happened during history replay or the final
  answer-seeking turn. Interrupts during replay resume automatically on the
  next request (replay chunks are inert ACKs; the final turn never submitted,
  so nothing can execute twice). Interrupts during the final turn trigger a
  bounded read-only adoption: if the thread shows the completed answer to the
  exact pending prompt, it is cached and the request continues (exact retries
  hit the duplicate cache with zero resubmissions); otherwise the session
  returns terminal 422. Pre-submission failures (e.g. navigation that reached
  no conversation) clear the flag so a retry can proceed. Inspect the
  dedicated browser tab and logged thread URL, repair the underlying
  issue, then use the validated recovery or explicit same-session resync described
  below. Do not clear the journal and retry a potentially
  executed tool turn blindly.

### Project-per-session (opt-in, default off)

Set `CHATGPT_BRIDGE_PROJECT_PER_SESSION=1` in the adapter's environment (then
restart it) so each OpenCode session gets its own dedicated ChatGPT project.
Unset (or any other value) keeps today's shared-project behavior byte-for-byte.

- **Naming:** `OpenCode Bridge — <repo-basename>/<ses-short8>`, or with the
  session short ID alone when no repo is detectable in the request context.
  Creation reuses the proven `ensureProject` UI-dialog flow, generalized per
  session name.
- **Eager start:** a new session's first thread is minted in its own project
  from turn one (logged as `decision=fresh-project`) — new sessions never
  touch the shared project unless proved pre-submission setup fails and invokes
  the mandatory fallback. Explicit resyncs still behave exactly as today.
- **Rotation replaces reset-replay:** on the same prefix-mismatch triggers
  that today cause reset-replay (compaction, edits, truncation), the adapter
  opens a fresh working chat in the session's project whose single first turn
  is a compact context card (session goal from early messages, recent
  assistant conclusions, full tool results — composed deterministically
  from adapter state, no model calls) plus recent turns verbatim (up to 16
  within budget, degrading rung by rung to the legacy card shape when over
  budget, falling back to reset-replay only if nothing fits). No replay
  pastes, ever. Old chats stay in the project for project-level
  retrieval (Phase-0 probe: fresh-chat cross-chat retrieval works with and
  without a card — see `phase0-report.md`).
- **Steady state unchanged:** delta continuation, per-thread tool teaching,
  dirty/uncertainty guards, resync and interrupt recovery all keep working.
- **Fallback mandatory:** explicitly proved pre-submission project setup/navigation
  failure with no reached conversation
  drops back to the shared-project thread for that turn (logged as
  `rotation_fallback` on the mismatch path, `eager_fallback` on first-turn
  minting — the latter only fires pre-submission, so it can never double-send).
  Existing sessions without a stored project stay
  grandfathered on the shared project. A possibly submitted turn never falls
  back: its Dirty journal remains protected and completion retries terminate
  with 422 rather than generating another turn.
- State gains one field per session (`Project`, the project URL) in
  `~/.chatgpt-proxy/openai-sessions.json`; old state files load unchanged.

### Parallelism (one browser, session-scoped turns)

Two opencode sessions via the model interleave only across sessions: with
`-max-parallel=2` (or `CHATGPT_BRIDGE_MAX_PARALLEL=2`) each active session
gets an owned dedicated tab from a bounded pool of 2, so distinct sessions
generate simultaneously while requests for the SAME session (turns, recover,
resync, duplicates) still serialize. A canceled session keeps its uncertain
journal; its peer completes independently. Default `-max-parallel=1` preserves
the legacy strictly-serial behavior (whole-turn in-process gate plus
exclusive cross-process `bridge.lock`). In parallel mode the profile guard is
held shared during generations (legacy exclusive holders still exclude all
owned turns), setup/reconnect/project creation take only short locks, and
`/readyz` reports `{ready, max_parallel, active, available}` — a busy pool
stays 200, it does not 503. Limits: one adapter process per address/state
file (a second writer is refused the state-owner lock); manual browser
interaction mid-turn can still disturb a generation; account-side throttling
was not hit during verification but remains possible — on refusal or
collision, restart with `-max-parallel=1`. See the verified record in
[BACKLOG.md](BACKLOG.md).

### Logs, readiness and repair
```sh
curl -s http://127.0.0.1:8788/healthz
curl -s http://127.0.0.1:8788/readyz
curl -s http://127.0.0.1:9222/json/version
tail -f /tmp/chatgpt-openai.log
launchctl print gui/$(id -u)/com.chatgpt-proxy.openai
launchctl kickstart -k gui/$(id -u)/com.chatgpt-proxy.openai
```

Logs show session ID, `fresh`/`delta`/`duplicate-cache`, prompt byte count, tool
count, stored thread URL, and `BACKEND FAILURE`/`REPAIR` instructions. Prompts and
answers are not dumped into logs. For login failures, use the Chrome window with
the dedicated profile (or `./bin/chatgpt-proxy -wait-login`). For DOM failures,
inspect the visible page and update `internal/chatgpt/chatgpt.go`'s composer,
send-button and assistant/code-block selectors; rebuild **both** binaries and
restart the adapter (restart your MCP client to load its rebuilt binary).
The current observed renderer uses `h4` "ChatGPT said:" and
`[data-markdown-copy="code-block"]`; older selectors are retained.
Project composers also use `button[aria-label="Send"]`. Large clipboard pastes
can become attachments; the bridge clicks **Show in text field** before submitting
so the external protocol remains text. If either control changes, inspect the
Project page and repair these selectors rather than blindly retrying the session.

### Verification and current limits

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o bin/chatgpt-openai ./cmd/chatgpt-openai
go build -o bin/chatgpt-proxy ./cmd/proxy
python3 docs/verify-openai.py
# After restarting the adapter, verify a saved live-test thread:
python3 docs/verify-openai.py --resume
```

The deterministic fake-backend Go tests exercise single-user tool requests,
JSON/SSE contracts, tool-result roundtrips, bad arguments/names, duplicate and
concurrent retries, reload from disk, session isolation, readiness and errors.
They also cover switch-away/back gap-only imports, small/large historical sections,
current tool schemas, rejected import wrappers, acknowledged-prefix persistence
with dirty restart protection, media edits/import forwarding and `reasoning:false`.
The opt-in Python check performs real JSON and SSE `get_weather` requests,
returns synthetic weather results, checks remembered followups and non-navigating
readiness, and switches back to the first session's thread.

### HTML-to-Markdown response extraction

ChatGPT Web renders assistant answers as HTML. The bridge extracts **only the
identity-scoped current assistant answer** (via stable `data-chatgpt-search-message-ids`
or `data-message-id` tokens, bounded by the last user message) and converts its
rendered DOM to GitHub-flavored Markdown. This avoids flattening headings,
tables, inline code, quotes, and lists to plain text.

**Supported Markdown constructs** (tested in `internal/chatgpt/response_test.go`):

| Construct | HTML source | Markdown output |
|---|---|---|
| Headings | `<h1>`–`<h6>` | `# `–`###### ` |
| Paragraphs | `<p>`, `<div>`, `<section>`, `<article>` | blank-line separated |
| Bold | `<strong>`, `<b>` | `**text**` |
| Italic | `<em>`, `<i>` | `*text*` |
| Strikethrough | `<del>`, `<s>`, `<strike>` | `~~text~~` |
| Inline code | `<code>` | `` `code` `` (safe fence length, padding when needed) |
| Fenced code blocks | `<pre><code class="language-x">`, `[data-markdown-copy="code-block"]` | ````x\ncode\n```` (preserves whitespace, language, nested backticks) |
| Nested lists | `<ul>`, `<ol start="n">` + `<li>` | `- ` / `1. ` with indentation |
| Blockquotes | `<blockquote>` | `> ` prefix per line |
| Links | `<a href="...">` | `[text](<url> "title")` (URL escaped, title quoted) |
| Tables | `<table><thead><tr><th>`, `<tbody><tr><td>` | `| H | H |\n| --- | --- |\n| C | C |` (pipes/newlines escaped, `<br>` preserved) |
| Horizontal rules | `<hr>` | `---` |
| Line breaks | `<br>` | trailing `  \n` (hard break) |

**Excluded from extraction** (removed before serialization):

- Buttons (`<button>`), toolbars (`[role="toolbar"]`), copy/regenerate controls
- Hidden elements (`[hidden]`, `[aria-hidden="true"]`), `<script>`, `<style>`
- Reasoning/thought blocks (`[data-testid="thought-block"]`, `[data-thinking]`) — captured separately as `reasoning`
- Elements marked `[data-markdown-copy="exclude"]`
- Page footer, sidebar, user messages, stale/duplicate assistant nodes

**Wire protocol preservation**: If the extracted answer consists **only** of one or more
`<opencode_bridge_call>{...}</opencode_bridge_call>` wrappers (the OpenCode tool
protocol), the raw text is returned unchanged — no Markdown processing, no fence
decoration, JSON whitespace and backslashes preserved byte-for-byte. The historical
`ACK` acknowledgement is also returned verbatim.

**Identity scoping**: The probe tracks message IDs across turns. Only the newest
assistant node after the last user message is extracted. Collapsed reasoning,
virtualized turns, and footer markdown never override the scoped answer.

**Limitations**:

- The conversion runs on ChatGPT's *rendered* HTML, not the original Markdown.
  ChatGPT's renderer may lose some semantic nuance (e.g., list nesting depth
  beyond what the DOM shows, or original fence length).
- Selectors depend on ChatGPT's current DOM structure (`h4` "ChatGPT said:",
  `[data-conversation-role="assistant"]`, `[data-markdown-copy="code-block"]`,
  `.markdown`, `[data-talvt-turn-state]`). If ChatGPT changes its renderer,
  selectors in `internal/chatgpt/response.js` and `internal/chatgpt/chatgpt.go`
  must be updated — the tests use fixture HTML matching the current selectors.
- Nested code blocks inside lists/blockquotes inherit container indentation.
  The test suite verifies the current layout; extreme nesting may differ.
- Table column alignment is not preserved (GitHub-flavored Markdown doesn't
  support it in the pipe syntax).
- The bridge does **not** prompt the model to output Markdown; it relies entirely
  on DOM conversion. If ChatGPT's HTML renderer changes output quality, the
  Markdown quality changes with it.

This remains a browser/text bridge: tool compliance depends on the Web model;
it is not a native function-calling API. SSE is **buffered until the complete
answer is validated**, not token-by-token; errors before that point are HTTP JSON
errors. Request cancellation stops CDP actions but cannot roll back a message
already submitted in ChatGPT. Text only; API sampling/output-token options are
not enforced by the Web UI; usage fields are zero because real token accounting
is unavailable. `-max-chars` (default 200000 bytes) bounds each replayed turn,
never the total history: a long history is chunked into the thread and replayed in
bounded turns instead of being rejected with `413`. One browser transaction runs
at a time.
The adapter's model ID is an alias; the actual model is the one selected in the
ChatGPT Web UI, and this adapter does not change that selection.

### Model-switch catch-up evidence (2026-10-09)

- `go test -race ./...`, `go vet ./...`, and both binary builds passed after the
  synchronization change; the installed launchd adapter was rebuilt and restarted.
- Live session `switch-gap-1791519634309569000` began in Project thread
  `https://chatgpt.com/g/g-p-6ac7ec14b33c819188504f4c90e5d6af-opencode-bridge/c/6ac86b9b-a130-83ec-839b-65548d798107`.
  A synthetic recorded other-provider segment then supplied five completed
  exchanges (12 missing messages, including `legacy_inventory` call/result).
  This uses the same OpenAI transcript shape OpenCode sends after switching back,
  without spending other-provider credits.
- The seal `ORCHID-309571000` appeared only in the other-provider answer and crate
  `CRATE-000175903` only in its tool result. The current question contained neither;
  ChatGPT recalled both, with zero executable tool calls, on the same Project URL.
  The available schema had changed to `current_inventory` and tool choice was none.
- Logs show `decision=delta delta_messages=13 turns=1 tools=1`: only the 12 missing
  historical messages plus the current question, excluding the cached initial
  question/answer. Exact retry returned the identical completion ID/time/body and
  logged `duplicate-cache`; readiness probes retained the thread URL.
- After another launchd restart, the same session recalled both gap-only values
  with `delta_messages=1 turns=1` and the identical Project URL. Its resumed
  request also passed the exact cached-completion retry check.
- Large catch-up, strict ACK consumption, currently allowed wrappers in an import
  response, cancellation after an acknowledged chunk, current-schema final calls,
  edited/compacted reset, and image/video forwarding are deterministic fake-backend
  regressions. Unexpected import output returns an error without tool execution;
  reload retains the acknowledged prefix and the dirty journal prevents replay.

### Prior live evidence (2026-10-09; before the distinct-wrapper update)

The following evidence was recorded by the prior interrupted implementation.
The updated wrapper and Project placement must be live-verified separately.

- JSON and SSE weather checks both produced `get_weather({"city":"Hanoi"})`,
  accepted the synthetic `temperature_c: 23` result, and answered `COBALT-73`
  on followup. Exact retries preserved the tool-call ID.
- OpenCode 1.18.35, using `@ai-sdk/openai-compatible`, executed the real benign
  command `printf 'OPENCODE_TOOL_OK\n'`: `tool_use` status `completed`, exit 0,
  output `OPENCODE_TOOL_OK\n`; its following model turn returned `OPENCODE_TOOL_OK`.
- OpenCode session `ses_ee320584dffe2851Sux1Ij5D85` mapped to
  `https://chatgpt.com/c/6ac7e75e-ad64-83ec-969e-e3e0ecbca19e`.
  Adapter logs showed `fresh delta_messages=2`, then `delta delta_messages=1`
  for the tool result. After `launchctl kickstart -k`, resuming that same session
  produced `AMBER-42` with `delta delta_messages=1` and the same stored URL.
- The real OpenCode test used an explicit temporary workspace, a test-only
  agent with only bash enabled, and disabled external plugins/MCP servers for
  that subprocess. These overrides did not change the global agent/MCP setup.

### Current distinct-wrapper/Project evidence (2026-10-09)

- `go test -race ./...`, `go vet ./...`, and both binary builds passed; the existing
  launchd adapter was restarted with the rebuilt binary.
- The updated live verification script passed JSON and SSE weather tool calls,
  exact retry IDs, supplied-result roundtrips (23°C), remembered `COBALT-73`
  followups, non-navigating readiness, and interleaved restoration.
- JSON session `live-weather-1791487176558073000` mapped to
  `https://chatgpt.com/g/g-p-6ac7ec14b33c819188504f4c90e5d6af-opencode-bridge/c/6ac7ecd0-9b64-83ec-ae60-369e6635faef`.
- SSE session `live-weather-1791487247631968000` mapped to a distinct Project chat,
  `https://chatgpt.com/g/g-p-6ac7ec14b33c819188504f4c90e5d6af-opencode-bridge/c/6ac7ed14-3478-83ec-8a82-d00ae28f5dfa`.
- Both fresh chats are under the same verified OpenCode Bridge Project, not the
  main-page chat route. The existing Project was discovered and its URL cached.
- Real OpenCode benign tool execution through the updated wrapper: session
  `ses_ee270ee0bffe57YL6r0xnMEzI1`, a 23532-byte request returned `tool_calls=1`
  in Project thread
  `https://chatgpt.com/g/g-p-6ac7ec14b33c819188504f4c90e5d6af-opencode-bridge/c/6ac81446-1c54-83ec-9116-d43a4d8c5186`;
  OpenCode ran the real `bash` tool (`completed`, exit 0, output `BRIDGE_TOOL_OK`),
  sent the result back as a delta request, and its final reply contained
  `BRIDGE_TOOL_OK`. This required a fix: ChatGPT's Project composer converts a
  large **paste** into a file attachment (empty composer, nothing submits), so the
  adapter now inserts with `execCommand('insertText')` and, if a renderer still
  attaches the payload, clicks **Show in text field** until the composer holds it.

### Stateful-thread rework evidence (2026-10-09)

All from the reworked adapter running under launchd (`-max-chars 100000`), then
restarted once:

- **Live (a) — server-side context, no resend.** Session
  `threadstate-multiturn-1791501619125`: turn 1 gave codeword `AMBER-42`, turn 2 a
  second fact, turn 3 asked for the *first* codeword. Adapter logs show
  `delta_messages=1` for turns 2 and 3 on the same thread
  `.../c/6ac82537-ce20-83ec-b387-238f396a51b8`, and the model answered `AMBER-42`
  without the adapter resending it — the thread held the context.
- **Live (b) — chunked replay of a long history.** Fresh session
  `threadstate-replay-1791501754596`, total **481,686 chars / 506 messages**, was
  split into **5 turns** (~98,744 / 98,766 / 98,766 / 98,766 bytes + final) by the
  adapter (`decision=fresh ... turns=5`, then `replayed chunk=1..4/5`), all in one
  thread `.../c/6ac825bf-...`; elapsed 138.1s; the model answered `ZEBRA-99`, a
  codeword in the first chunk. A second probe at **961,494 chars / 1,010
  messages** split into 10 turns. The full history was never a single paste.
- **Live (c) — real OpenCode tool execution.** Session
  `ses_ee22a5961ffeegmK60g6Jq4pIS`: a fresh request returned `tool_calls=1`;
  OpenCode ran the real benign `bash` tool (`printf 'BRIDGE_TOOL_OK\n'`) and its
  final reply contained `BRIDGE_TOOL_OK`.
- **Live (d) — same-URL resume after restart.** After `launchctl kickstart -k`, the
  resumed session `ses_ee22a5961ffeegmK60g6Jq4pIS` resolved `decision=delta` to the
  **same** stored thread `.../c/6ac8265a-...` and recalled `INDIGO-91`.
- Deterministic fake-backend tests cover delta continuation, chunked replay,
  per-turn size enforcement, rune-safe splitting, no single-giant-paste path,
  mismatch→reset-replay, session isolation and the tool-call roundtrip.

### Multimodal (image / video / reasoning) — 2026-10-09

- **Image input works (live-verified).** A request with an `image_url` data URL
  (solid-red 64×64 PNG) was answered `Red`. The adapter strips the base64 from the
  text prompt and uploads the file through the composer's
  `input[type=file][accept="image/*,video/*"]`, then submits and verifies the send.
- **Video input works (live-verified).** A request with a `video_url` data URL
  (ffmpeg red 1s mp4) was answered `Red`, via the same attachment path.
- **Reasoning is NOT exposed by this account/model (honest blocker).** The picker
  is a "Thinking effort" power slider (`[role=slider]`, `aria-valuenow` 0..2, levels
  reading `Instant, 1 of 3` … `High, 3 of 3`). The adapter raises it to max
  (`High`) and can send with `reasoning_effort`, but the answer DOM contains **no**
  reasoning trace (`Thought for …`, no `details/summary`, no reasoning container)
  for the tested prompt, so `reasoning` stays `false` and no `reasoning_content` is
  emitted. If your account/model shows a reasoning panel, adjust the scraper in
  `internal/chatgpt/chatgpt.go` (`waitForAnswer`) and the selector in
  `internal/chatgpt/multimodal.go` (`selectThinking`).
- Config: `chatgpt-web` advertises `attachment:true` and
  `modalities.input:[text,image,video]`; `reasoning:false` because it is unproven.

### Known limits (honest)

- Text/JSON only for output. Sampling/output-token options are not enforced by the
  Web UI, and the actual model is whatever the ChatGPT Web UI has selected.
- Browser/session isolation is per OpenCode session, **not** a guarantee about
  ChatGPT's account-level memory; do not rely on it for sensitive data.
- Image/video parts must be `data:` base64 URLs (remote URLs are rejected).
- Reasoning output is not scraped on this account (see above).
- The server-side thread window was **not** measured to its ceiling; the
  `limit.context=128000` budget is conservative and backed by the ≥481k-char
  retention test above, not a measured maximum. Do not raise it to 1M without a
  new replay test that proves the thread still recalls early content.

### Tool-schema teach-once evidence (2026-10-09)

- Symptom: selecting the model as the OpenCode driver failed every request with
  `current tool schemas/instructions exceed the per-turn chunk budget`, because
  OpenCode attaches ~108 tools (~120KB of schemas) and the adapter pasted the
  whole dump into every turn against a 100000-byte budget.
- Fix: full schemas are taught once per thread (fresh thread or changed tool
  set); continuations carry a compact reminder. `-max-chars` raised to 200000
  (single-paste probes proved 300k-char prompts work; 607k fails). The budget
  error now reports turn bytes, schema bytes, budget, and teach state.
- Live: `opencode run "hi there"` answered; a follow-up file-listing task
  produced `tool_calls=1` (teach), then `tool_calls=1` with `teach_full=false`
  (reminder), then the grounded final answer — all on one Project thread.
- A navigation failure that reaches no conversation URL clears the dirty journal
  instead of wedging the session (`IsNavigateError` + test); genuinely uncertain
  turns still remain protected (completion guard now terminal 422).

### OpenCode-side tuning (required for driver use)

- Set a local `small_model` (e.g. `ollama/llama3.1:8b`). Otherwise OpenCode uses
  the session model for title generation: every new session burns a bridge turn
  on a title, creates a junk Project thread, and poisons the stored prefix so
  all later turns reset-replay with full schema re-teach.
- Disable client timeouts for this provider (`"chunkTimeout": false,
  "timeout": false`): long silent browser turns are normal; a client abort
  during the final answer-seeking turn requires recovery when the outcome cannot
  be adopted. Replay-only interruptions resume safely without executable calls.
- Quit and restart OpenCode after any config change; sessions keep old config.
- Consecutive turns on one thread skip the reload (`already on ..., skipping
  reload`); expect ~15-20s short turns, longer when the model writes a lot —
  most of that is ChatGPT's own generation, not automation overhead.
## Recovering a completed but missed browser response

The October 9 incident was a missed completed browser response, followed by
OpenCode's **response-header timeout** (`ProviderHeaderTimeoutError`, 300000 ms).
The stored assistant finished at 16:13:19; the proxy continued reporting zero
answer bytes until cancellation at 16:17:49. This was not a database-tool
failure, chunk timeout, or the later manual cancellation.

Browser extraction now tracks assistant message IDs rather than requiring the
number of rendered assistants to increase. ChatGPT can collapse reasoning or
virtualize earlier turns. Extraction is scoped after the latest user turn and
does not fall back to arbitrary page markdown, user text, or the footer. A
generation that ends with no extractable answer gets a 15-second rendering grace
before an actionable error; a missing generation start is bounded separately.
The exact count transition in the incident was not captured, so count collapse
is a reproduced failure mechanism rather than a claimed historical observation.

An uncertain session remains blocked from automatic resubmission. To recover a
finished stored response **without executing any tools**, send
`POST /v1/sessions/recover` with the original `X-Session-Id` header. New journals
retain the pending request, prompt and final phase, so an empty body is sufficient.
A supplied request must match that durable original binding; it cannot fill in
missing evidence. Dirty journals missing phase, request or prompt return terminal
HTTP422 with `error.code=incomplete_journal` from completions and recovery. Their
submission outcome is unknown, not proved unsubmitted. Use explicit completed-
history resync below, never manual file edits or a guessed original request.
Explicit replay-phase journals remain resumable and are not incomplete finals.
Recovery requires exact equality with the full stored user prompt (visible user text can be
truncated), an unambiguous finished assistant response, and normal tool-schema
validation. Any mismatch leaves the dirty journal untouched.

A successful recovery persists the validated response and returns only metadata.
Retrying the exact original completion request then reads that cache. Recovery
itself never submits a browser prompt, emits executable tool calls, or reruns
historical tools. Do not manually clear `Dirty` or reconstruct a request by
guessing missing tool results. Keep private tool results out of diagnostic logs.

### Same-session inert-history resync

When an old journal lacks provably exact request/tool schemas, use
`POST /v1/sessions/resync` with the same session header and a body
`{"messages": [...]}` containing the recorded journal transcript plus **all
completed tool results** from the authoritative client transcript. Result IDs
must account for each outstanding recorded call exactly once. Preserve the
client's text encodings and metadata; never substitute guessed or regenerated
results. The endpoint keeps `Dirty=true`, retains the old thread URL, and only
arms permission for a **new user message** following that transcript. It does
not submit anything or return the uncertain response's calls.
The final message must contain distinct ordinary user text: an old request,
tool result, or `<system-update>` metadata alone cannot consume this permission,
even when compaction requires re-anchoring the reviewed transcript.

The next new user request uses a fresh Project thread. The entire completed
transcript is imported with the inert-history instruction and ACK-only replies
before the new request is submitted. Arming is consumed before the first browser
submission; a failed import stays dirty and cannot automatically resubmit.

Resync may also reconcile later completed authoritative history, including when
the recorded journal ends with outstanding calls: supply those recorded results
first, then later completed exchanges. Every assistant tool
call matched by exactly one non-empty tool result, no dangling calls, and the
recorded prefix included. The same new-user-message flow then applies.

For `ses_ef3b3f92bffeN5XqnzcP7ohakk`, `/v1/sessions/recover` cannot prove the
pending outcome: the journal holds one system message, there is no pending
request, and the web thread contains synthetic turns that do not match the
authoritative local transcript (118 messages). Do not clear `Dirty` and do not
POST under this session ID. Resync is now **armed**: the 118-message
authoritative transcript (read-only OpenCode database: 35 user, 83
assistant, 51 completed tool pairs, all outputs non-empty) plus the recorded
system message — 170 wire messages — was accepted (`resync_armed: true`,
persisted with the old thread URL retained). To continue it, send a NEW user
message in the same OpenCode session; the completed transcript is imported
inertly into a fresh Project thread as described above and no historical
tool is rerun. Until then the session stays dirty and any new request is
rejected — that rejection is the safeguard working, not a bug.

### Recovery state and explicit abandonment

`GET /v1/sessions/state` with the same session header answers "what is this chat's
recovery state, and what can I actually do" without reading a runbook. It is
read-only — no submission, no browser work, no journal write — and returns enums,
booleans, counts and sizes only: `state`, `can_recover`, `can_resync`,
`thread_url_present`, `completed_messages`, `pending_prompt_bytes`,
`outstanding_tool_calls`, a plain-English `summary`, and the `actions` that are
really available. The classes are derived from the resolvers' own predicates, so
`can_recover` is exactly the `recoverSession` acceptance test and `can_resync` is
exactly the `resyncSession` one. No prompt text, tool arguments, call IDs or
message bodies are ever returned.

`POST /v1/sessions/abandon` with `{"sessions":["ses_…"],"reason":"…"}` clears the
pending-turn binding of sessions named **explicitly**. It refuses the whole
request for an empty id, a wildcard, `all`/`any`, more than 64 ids, a
non-POST method, or any `Origin` header (it is an operator mutation, not a
browser API), and returns 503 while a turn is in flight rather than queueing.
Only `Dirty`, `Phase`, the pending request/prompt/repair prompt and the armed
resync are cleared: the completed transcript, thread URL, cache identity,
result, tool fingerprint and every evidence field are retained, and the response
accounts for delivered calls, recorded results and outstanding calls before the
clear. Persistence happens before the response; a failed write rolls back and
returns 500. Per-session outcomes are `abandoned`, `not_pending` (including a
known-rejected output, which already has a safe continuation) or `unknown`.
Abandonment is a deliberate boundary, never proof the old turn succeeded.

Terminal failures shown in chat are actionable: one plain-English sentence about
what is unknown, an explicit statement that nothing was resent and no tools were
re-run, and the single thing the user can say next. HTTP runbooks live in
`error.details` and the logs, never in the sentence.

Live continuity proof (isolated session, codeword recall): fresh +
`decision=delta` + `decision=delta` on one Project thread, service
restart with journal reload, then a fourth resumed turn `decision=delta`
on the same thread with correct recall — no `prefix_mismatch`, no
reset-replay on any resumed turn.

Limitations: clients without the lane header keep legacy main-session behavior
until restarted (auxiliary contamination from old clients is still possible);
the bridge does not guess request purpose from tool counts or prompt text.

For `ses_ee035130effeV7wPsSu4gtJ25h`, resync is armed with 15 authoritative
messages. The three completed SQL call inputs and result bodies were matched
against the read-only OpenCode database; their added DCP message-ID metadata was
also verified. No SQL calls were rerun. **Send a new message in the same OpenCode
session**, for example: “Summarize the schema from the completed results already
in this conversation. Do not call any tools.” Do not retry the failed original
request. The old ChatGPT thread remains available for inspection.

SSE output remains buffered until normal tool-schema validation finishes; no raw
browser calls are streamed. No provider/global timeout or compaction settings
were changed. The client's 300-second response-header limit still applies to a
legitimately long generation; the finished-empty bug now produces a bounded
actionable error rather than silently waiting for that timeout.
