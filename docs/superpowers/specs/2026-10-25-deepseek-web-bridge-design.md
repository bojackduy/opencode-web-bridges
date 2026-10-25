# DeepSeek Web → OpenCode bridge: design

Date: 2026-10-25
Status: proposed (built in the same pass, no interactive approval per YOLO mode)

## Intent

Give OpenCode a third web-session provider (`deepseek-web`) that talks to
`chat.deepseek.com` over plain HTTP + SSE. No Chrome automation, no DOM, no
clicking. Same account the user already uses in the browser.

The ChatGPT bridge's painful failure mode — "a previous reply could not be
confirmed" after revert/fork/model switch — exists because the model's answer
can only be read from a DOM that may not be addressable later. DeepSeek exposes
the completion as an SSE stream over an HTTP request, so the answer arrives on a
socket we control. That removes the whole class of uncertainty.

## Goals

- OpenAI-compatible `POST /v1/chat/completions` (stream and non-stream),
  `GET /v1/models`, `GET /healthz`.
- Native streaming, including `reasoning_content` for thinking models.
- Tool calls for the OpenCode agent, using the existing
  `<opencode_bridge_call>{json}</opencode_bridge_call>` envelope contract that
  the ChatGPT adapter already teaches and parses.
- Session continuity via DeepSeek's `chat_session_id` + `parent_message_id`
  chain when the client's history extends the last committed turn.
- Revert / fork / model switch / compaction work by construction: any history
  that is not a strict extension starts a fresh remote session and sends the
  rendered history. No dirty state, no "confirm", no recovery command.
- Content-free logs: sizes, counts, flags, durations, reason codes.

## Non-goals

- Native DeepSeek tool calling (the web API has none).
- File uploads, image input, web-search results as citations.
- Multi-account rotation.
- Release packaging / plugin-side auto-download (v1 is a locally built binary,
  like `gemini-proxy`).

## Approaches considered

**A. Run `nexus serve` as the adapter.** Cheapest, but its OpenAI surface
flattens every request into one prompt, never returns `tool_calls`, and keeps
one remote session per `user` field. OpenCode's agent loop would break on the
first tool call. Rejected.

**B. Fork `cmd/chatgpt-openai` and swap the backend.** The journal, reseed, and
recovery code is battle-tested, but most of it exists to compensate for DOM
read-back and browser uncertainty. DeepSeek does not need adoption, resync,
projects, rotation, or composer ownership. Forking would carry ~4 k lines of
now-meaningless guards. Rejected.

**C. New lean adapter, reuse nexus transport.** Port
`pkg/deepseek/{pow,stream,types,client,auth}.go` (MIT), drop the account pool,
the REPL, the CDP helper and the Ollama/Anthropic surfaces, and write the
agent-facing layer fresh (~600 lines). Chosen.

## Architecture

```
OpenCode ──/v1/chat/completions──▶ deepseek-openai
                                     │ canonicalise messages
                                     │ plan: append | reseed
                                     │ render prompt + tool preamble
                                     ▼
                          POST /api/v0/chat/completion  (SSE)
                                     │ x-ds-pow-response header
                                     ▼
                              chat.deepseek.com
```

### Transport (ported from nexus, MIT)

- Auth: `Authorization: Bearer <userToken>` plus optional `x-hif-*` headers and
  `Cookie`, read from `~/.deepseek-proxy/auth.json` or `DEEPSEEK_TOKEN`.
- PoW: `POST /api/v0/chat/create_pow_challenge {target_path}` then
  `DeepSeekHashV1` solved in-process (pure Go Keccak, no WASM).
- Session: `POST /api/v0/chat_session/create {}` → `chat_session_id`.
- Completion: `POST /api/v0/chat/completion` with
  `{chat_session_id, parent_message_id, model_type, prompt, thinking_enabled,
  search_enabled}` → `text/event-stream`. Frames carry JSON patches
  (`p`/`v`) into `response/fragments/-1/content`; `response/status=FINISHED`
  ends the turn; `ready` carries `response_message_id`.
- Read-back: `POST /api/v0/chat/history_messages {chat_session_id}` is probed
  when a stream breaks. If it answers, the interrupted turn is recoverable
  without resending. If it does not, the turn fails closed.

### History and thread planning

State per OpenCode session (`x-opencode-session-id`):

```go
type session struct {
    Remote   string     // deepseek chat_session_id
    Parent   string     // last assistant message id
    Messages []message  // committed canonical history
    Pending  string     // prompt hash of an in-flight turn, "" when idle
}
```

Per request, after canonicalisation (`content: null → ""`, tool arguments
normalised):

1. `append` when `Remote != ""`, `len(req.Messages) == len(s.Messages)+1`, and
   `prefix(req.Messages, s.Messages)`. Prompt = render of the single new turn.
2. `reseed` otherwise: create a fresh remote session, prompt = render of the
   whole history. This covers first turn, revert, fork, model switch, compact,
   and any drift the canonicaliser cannot see.
3. `retry` when the incoming canonical history equals the committed history and
   a `Pending` turn exists: never resend. Try read-back; if it fails, return
   409 with the pending marker and let the client start a new turn (which is a
   `reseed` by rule 2 because the history still equals the committed one).

Commit on success only: `s.Messages = req.Messages + assistant`,
`s.Parent = answer message id`, `s.Remote = remote id`.

A failed or interrupted turn therefore never wedges a session: the next request
is a reseed.

### Prompt rendering

- System messages → `[Instructions]` block at the top.
- Tool definitions → `toolPreamble` (copied from the ChatGPT adapter) plus the
  JSON schemas. The model answers with an envelope; the adapter parses it into
  OpenAI `tool_calls` and stores the assistant message with those calls, so the
  next turn's history shows `TOOL_CALL: name(args)` and `[Tool Result]`.
- Appended turns: only the messages after the committed prefix.
- Reseeded history: every message in order. Assistant tool calls are rendered
  from parsed calls, never from raw envelope text.

### Tool-call parsing

Reuse the ChatGPT adapter's rules: `<opencode_bridge_call>` (also escaped
underscores), no Markdown fences, `arguments` must be a JSON object, unknown
names rejected, an incomplete wrapper is an error. One answer may carry several
wrappers. Any prose around wrappers is dropped.

### Errors and status codes

- Auth missing/invalid → 401 with a one-line instruction (`-login`).
- PoW/session/completion transport failure before `ready` → 502, session state
  untouched, safe to retry as a new turn.
- Stream breaks after `ready` → 502 with `pending=true` in the body; the next
  new turn reseeds.
- Rate limit from DeepSeek → 429 passed through.

## Layout

```
deepseek-proxy/
  go.mod                       module github.com/bojackduy/deepseek-proxy
  bin/                         built binaries (gitignored)
  internal/deepseek/
    auth.go  pow.go  stream.go  client.go  types.go  (+ tests)
  cmd/deepseek-openai/
    main.go  handler.go  session.go  prompt.go  tools.go  (+ tests)
```

## Testing

- `pow_test.go`: published challenge → expected hash; solver finds a
  difficulty-1 answer.
- `stream_test.go`: real captured frames (snapshot, fragment append, incremental
  text, thinking fragment, FINISHED, hint error).
- `prompt_test.go`: append vs reseed planning, canonical prefix, revert shape,
  fork shape, identical retry.
- `handler_test.go`: `httptest` fake upstream — plain text stream, tool call
  stream, revert reseed, interrupted stream, 401, 429.
- `go vet ./...`, `go test ./...`, `go test -race ./cmd/deepseek-openai`.

## Risks

- **Token lifetime.** The web token expires; the adapter must surface 401
  clearly and never retry with a stale token in a loop.
- **Unknown history endpoint.** Read-back is best-effort until observed live.
  Without it, an interrupted turn is a failed turn, not a bricked session.
- **Rate limits.** Free web accounts throttle. No automatic resend means the
  user sees the 429 instead of a silent duplicate.
- **Model drift.** `model_type` values (`default`, `expert`) and thinking flags
  are from the reference implementation; a live probe is required once a token
  exists.
