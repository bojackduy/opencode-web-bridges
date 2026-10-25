# Bridge logs — reading one turn end to end

Both web bridges (ChatGPT and Gemini) submit fire-and-forget: the prompt is
typed without verification and Send is clicked once. Every stage of that turn is
logged under a single **flow id**, so a whole request can be read as a timeline.

Nothing in these logs contains prompt or answer text — only byte counts, flags,
durations, selectors and errors. That is deliberate: the logs are safe to paste.

## Where the logs are

| What | Path |
| --- | --- |
| ChatGPT raw log | `/tmp/chatgpt-openai.log` |
| Gemini raw log | `/tmp/gemini-openai.log` |
| ChatGPT readable timelines | `/tmp/chatgpt-flows/<flow-id>.log` |
| ChatGPT finished-turn index | `/tmp/chatgpt-flows/index.log` |
| Gemini readable timelines | `/tmp/gemini-flows/<flow-id>.log` |
| Gemini finished-turn index | `/tmp/gemini-flows/index.log` |

`launchd` writes both raw logs through `StandardOutPath` / `StandardErrorPath`,
so they are append-only and vanish on reboot:

- `~/Library/LaunchAgents/com.chatgpt-proxy.openai.plist` → port 8788
- `~/Library/LaunchAgents/com.gemini-proxy.openai.plist` → port 8789

## Start the readable tailer

```sh
python3 scripts/flow-log-tail.py          # both bridges, follows forever
python3 scripts/flow-log-tail.py gemini   # one bridge
```

It follows the raw logs from the last offset (`/tmp/chatgpt-flow-tail.offset`,
`/tmp/gemini-flow-tail.offset`) and writes one file per flow id plus a one-line
summary per finished turn:

```sh
tail -f /tmp/gemini-flows/index.log
tail -f /tmp/gemini-flows/gemini-18e159f695e794a8.log
```

No tailer? Every fact below is also in the raw log:

```sh
grep 'gemini-flow' /tmp/gemini-openai.log | tail -40
grep 'chatgpt-flow' /tmp/chatgpt-openai.log | tail -40
grep 'id=<flow-id>' /tmp/gemini-openai.log       # one turn, all stages
```

## What a healthy turn looks like

Gemini (adapter line first, then the browser stages):

```
[gemini-flow] id=gemini-18e159f695e794a8 session=seam-tes stage=request fresh=true prompt_bytes=23 tools=0 stream=false
[gemini-flow] id=gemini-18e159f695e794a8 stage=start prompt_bytes=23 new_session=true timeout=10m0s
[gemini-flow] id=gemini-18e159f695e794a8 stage=prepare start new_session=true
[gemini-flow] id=gemini-18e159f695e794a8 stage=prepare result=ok prior_answers=0 duration=6.447s
[gemini-flow] id=gemini-18e159f695e794a8 stage=composer_wait start prompt_bytes=23
[gemini-flow] id=gemini-18e159f695e794a8 stage=composer_focus result=ok
[gemini-flow] id=gemini-18e159f695e794a8 stage=composer_insert result=inserted bytes=23 duration=31ms
[gemini-flow] id=gemini-18e159f695e794a8 stage=send_wait start
[gemini-flow] id=gemini-18e159f695e794a8 stage=send_ready enabled=true waited=52ms
[gemini-flow] id=gemini-18e159f695e794a8 stage=send_click selector=button[aria-label="Gửi tin nhắn"] clicked=true err=<nil>
[gemini-flow] id=gemini-18e159f695e794a8 stage=submitted
[gemini-flow] id=gemini-18e159f695e794a8 stage=send_observed result=submitted retries=1 waited=848ms
[gemini-flow] id=gemini-18e159f695e794a8 stage=stream_wait start
[gemini-flow] id=gemini-18e159f695e794a8 stage=stream_poll answer_bytes=446 generating=false
[gemini-flow] id=gemini-18e159f695e794a8 stage=finish err=<nil> answer_bytes=2 images=0 duration=20.005s
[gemini-flow] id=gemini-18e159f695e794a8 session=seam-tes stage=answer bytes=2 duration=20.005s
[gemini-flow] id=gemini-18e159f695e794a8 session=seam-tes stage=protocol_validation result=accepted tool_calls=0 images=0
[gemini-flow] id=gemini-18e159f695e794a8 stage=response stream=false answer_bytes=1 tool_calls=0
```

Readable form of the same turn:

```
10:37:22  REQUEST      prompt 23 B, timeout=10m0s
10:37:22  PREPARE      opening the conversation
10:37:28  PREPARE      page ready in 6.447s
10:37:28  COMPOSER     waiting for the editable box
10:37:28  COMPOSER     found
10:37:28  INSERTED     fire-and-forget via trusted input — not verified
10:37:28  SEND         waiting for an enabled Send control
10:37:28  SEND         enabled after 52ms
10:37:28  SEND         clicked
10:37:29  SUBMITTED    turn started (checks=1, 848ms)
10:37:29  STREAM       polling for the answer
10:37:35  STREAM       answer now 446 B (generating=no)
10:37:42  DONE         OK | answer 2 B | took 20.005s
```

ChatGPT writes the same shape without `prepare`/`submitted`, and adds
`stream_delta`. Its adapter-level line is `[openai] session=... stage=...` (for
example `done tool_calls=0 answer_bytes=646`), which carries no flow id — match
it to the turn by timestamp or by the session's `index.log` entry.

## Stage reference

Shared vocabulary; each stage answers one question.

| Stage | Fields | What it proves |
| --- | --- | --- |
| `request` | `session`, `fresh`, `prompt_bytes`, `tools`, `stream` | adapter accepted the turn and computed the prompt |
| `start` | `prompt_bytes`, `new_session`, `timeout` | browser turn began (Gemini: `prepare` follows; ChatGPT: composer stage follows) |
| `prepare` | `result`, `prior_answers`, `duration` | Gemini only: the tab was navigated/settled before typing |
| `composer_wait` | `prompt_bytes` | looking for the editable box |
| `composer_focus` | `result=ok\|missing` | the box exists (or `ErrNoComposer` follows) |
| `composer_insert` | `result=inserted\|reused\|error`, `bytes`, `duration` | the text was written — **never verified** |
| `send_wait` | — | waiting for the Send control to become enabled |
| `send_ready` | `enabled`, `waited`, `last_err` | `enabled=false` means nothing was clicked: a proven pre-send failure |
| `send_click` | `selector`, `clicked`, `err` | the one dispatched click; `err` here is the only uncertain outcome |
| `submitted` | — | Gemini: the click landed |
| `send_observed` | `result=submitted\|unobserved`, `retries`, `waited` | the turn visibly reached the thread (composer cleared / stop button) |
| `stream_wait` | — | polling for the answer |
| `stream_poll` | `answer_bytes`, `generating` (Gemini) / `stop` (ChatGPT) | answer size growth; repeated sizes are collapsed in the readable log |
| `stream_delta` | `bytes` | ChatGPT only: streamed chunk size |
| `answer` | `bytes`, `duration` | extracted answer text is available |
| `protocol_validation` | `result=accepted\|rejected`, `tool_calls`, `images`, `err` | the tool-call protocol was validated before anything reached the client |
| `response` | `stream`, `answer_bytes`, `tool_calls` | the completion was written |
| `finish` | `err`, `answer_bytes`, `images`, `duration` | terminal line; `err=<nil>` means success |

Source of truth for each stage:

- Gemini: `gemini-proxy/internal/gemini/gemini.go` (`askRich`,
  `typeAndSubmitLocked`, `waitForAnswerLocked`) and
  `gemini-proxy/cmd/gemini-openai/main.go` (`request`, `answer`,
  `protocol_validation`, `response`).
- ChatGPT: `chatgpt-proxy/internal/chatgpt/chatgpt.go` (`askLocked`, composer,
  send and stream stages).

## Failure classes and what to do

Read the **last stage a flow id reached**. This is the whole diagnostic.

| Last stage / field | Meaning | Action |
| --- | --- | --- |
| `composer_focus result=missing` | no editable box in the tab | sign in / open the tab; nothing was typed |
| `composer_insert result=error` | typing failed | nothing was sent; retry the turn |
| `send_ready enabled=false` | Send never became enabled within 30 s | nothing was submitted (`ErrNotSubmitted`); a partial paste may sit in the box |
| `send_click err=<nil> clicked=true` then `send_observed result=unobserved` | the click landed but no turn appeared | **outcome unproven** — do not resend automatically; inspect the tab. A 150 KB+ prompt can block the renderer past the 10 s observation window, so a very large turn can report this even when it did start. |
| `send_click err=...` | the click call failed after dispatch | **outcome uncertain** — never resend automatically |
| `finish err=... ErrSubmissionUncertain` | same as above, terminal | read the chat in the dedicated window |
| `err=... completed protocol source could not be verified ... (reason=transport\|auth-http\|conversation-http\|exception)` | the transcript API or the read itself was unavailable; identity checks never ran | transient: retryable, never a reason to accept rendered arguments |
| `err=... completed protocol source could not be verified ... (reason=identity)` | the finished answer's DOM/source identity, user ancestry or thread did not match | deterministic refusal; do not bypass |
| `adoption user-bubble mismatch path=bubble` | the stored answer could not be matched to the pending prompt from a partially rendered bubble | wait for the ID-bound source; a `mapping` mismatch is a real divergence |
| `protocol_validation result=rejected` | an answer arrived but broke the tool protocol; no calls delivered | the turn is spent; do not resend |
| `finish err=... context canceled` | the adapter was stopped mid-turn (deploy, restart) | check the deploy window before blaming the bridge |

Hard rules the logs encode:

- A refusal before `send_click` is **always** safe to retry: nothing was
  dispatched.
- Anything at or after `send_click` is **never** retried automatically, even
  when it looks empty. The turn may already be in the thread.
- `submitted` without `send_observed` is not proof of a turn; the answer wait
  remains the authority.

## Stream capture and stream-confirmed turns

The adapter decodes the browser's own SSE response for the generation request.
This is on by default, because the capture is the only proof of a turn that
does not depend on the page settling. `CHATGPT_BRIDGE_SHADOW_STREAM=0` returns
to the DOM-only path; any other value (including unset) keeps the capture on.

What it reads: one POST to `/backend-api/f/conversation`, answered with
`text/event-stream`. Frames patch a message map (`p`/`o`/`v`, with `p` and `o`
sticky across frames), and `message_stream_complete` states completion. Chrome
withholds that body unless it is streamed, and the response never reports
`loadingFinished`, so EOF is not a completion signal.

One content-free line is logged per turn:

```
[chatgpt-shadow] id=... frames=... bytes=... completed=... correlated=...
  match=... ignored=... shadow_bytes=... answer_bytes=... reason="..."
```

| `reason` | meaning |
| --- | --- |
| `""` | the stream reconstructed an answer bound to this prompt |
| `no explicit completion` | the stream ended without `message_stream_complete`; inconclusive, never an answer |
| `no matching user message` | the streamed user text is not this prompt (a `user_candidates=...` line reports sizes only) |
| `assistant lineage` | no assistant message reaches the prompt through its parent chain |
| `completion conversation mismatch` | the completion names a different conversation than the answer |
| `unsupported patch` | a patch could have changed the answer body or its identity and was not applied |
| `stream aborted`, `stream unavailable`, `listener panic` | transport or listener failure; the DOM path still decides the turn |

`match=true` means the reconstruction equals the delivered answer. A `false`
with equal sizes and a long shared prefix is usually a rendering difference,
not a missing answer; sizes and shared edges are logged, never text.

The adapter accepts a turn as soon as the stream declares completion. The captured message is the server's own record of the turn: its
status, completeness, conversation and parent chain were asserted on it, so the
adapter delivers that text directly and re-applies the bridge-call envelope
rule. It does not read the answer back from the page. Logged as:

```
[chatgpt-flow] id=... stage=stream_confirmed answer_bytes=...
```

A separate read-back by message ID is not possible for these threads. Each one
is created inside a project, and `/backend-api/conversations` (flat list) is
empty while `/backend-api/conversation/<id>` and every project-scoped variant
answer 404 for the same IDs. That path exists because waiting for the DOM to
settle is what produces "A previous reply could not be confirmed". Thinking
turns keep the ordinary path, since reasoning is only available from the page.

## History rewrites: revert, fork, model switch

When a turn is interrupted, the session is dirty and its pending turn is
unknown. The client may then rewrite its own timeline. The adapter classifies
the incoming history against the committed journal and continues on a fresh
thread instead of refusing:

| Classification | Shape | Result |
| --- | --- | --- |
| `rewrite-truncation` | shorter, prefix-equal | reseed |
| `rewrite-divergence` | same length, content differs (fork/regeneration) | reseed |
| `rewrite-compaction` | summarized system prefix | reseed |
| `rewrite-continuation` | committed journal is a canonical prefix, tail is new (revert + new message) | reseed |
| `ambiguous` | dangling/duplicate/unmatched tool results | refuse (409/422) |
| (none) | identical retry of the pending request | refuse, never resubmitted |

An armed reseed always abandons the old thread: it never appends a delta to a
thread that holds an abandoned turn. The pending turn is listed as not carried
in `ReseedEvidence`, and nothing from it is submitted as pending. A completed
answer for that prompt that recovery could not bind is load-bearing and still
refuses. Logged as:

```
[openai] session=... rewrite-reseed continuation: committed journal is a prefix of the client history added=N; uncertain turn abandoned without resubmission
[openai] session=... rewrite-reseed armed classification=rewrite-continuation carried=N not_carried=N; abandoning uncertain thread without adoption
```

A dirty session with no committed journal is not a continuation: there is no
committed state to prove the client's history came from the bridge, so it keeps
the terminal refusal.

## Verify a deployment without touching the browser

Probing `/readyz` launches or attaches to Chrome. Use only:

```sh
curl -s http://127.0.0.1:8788/healthz            # ChatGPT adapter
curl -s http://127.0.0.1:8789/healthz            # Gemini adapter
shasum -a 256 ~/.chatgpt-proxy/openai-sessions.json   # must be unchanged by a swap
shasum -a 256 ~/.gemini-proxy/openai-sessions.json
```

The ChatGPT adapter also serves a read-only journal snapshot (no Chrome):

```sh
curl -s -H 'x-opencode-session-id: deployment-nonexistent-probe' \
  http://127.0.0.1:8788/v1/sessions/state
```

Gemini has no state endpoint; `/healthz` plus the journal hash is the check.
