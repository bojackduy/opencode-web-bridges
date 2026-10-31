# Backlog

## 2026-10-18: loopd patch naming and native-input executor

The reported loopd scope-test failures are separate from browser uncertainty:
current OpenCode's structured tool is named patch (patchText input), while
loopd structuredWritePaths recognized only apply_patch. The loopd repository
now recognizes both names using the SAME patch grammar/path extraction and
claim enforcement; allowed-path, outside-path and malformed-patch cases in
the existing suite pass (11tests), plus typecheck/build. It requires a host
plugin reload and resuming blocked goals; no user goal files were created
manually and no scope restrictions were disabled. Loopd changes uncommitted.

The bridge logs also proved native Input.insertText was called directly on a
context lacking a CDP executor, always logging invalid context and taking the
legacy fallback. It now uses chromedp.Run for small inputs and checks focus
errors. Large catalogs retain the existing path to avoid activating a slow
native paste. Bridge vet/tests/build pass; idle locked deployment preserved
the journal hash and readiness. A real isolated no-tool marker request returned
INPUT-OK HTTP200 in43.02s, native input error=nil, and no tool calls. This proves
one live Send path, not recovery of earlier pending sessions or all Send failures.
The initial project-creation path still fell back in that probe; do not claim
project setup or the full loopd integration has been proved working by the marker.

## 2026-10-17: ACK hydration race after the acknowledgement fallback

**Fixed and deployed locally; uncommitted.** The subsequent ses_ebbd828b
failure19:17:05 reproduced despite the previous ACK fix. An independent owned
target opened the exact stored thread6ad36716… without any feedback card and
observed final assistant161904a6…: empty body initially, complete source exactly
ACK, then visible ACK by the readback recheck. Guard1 rejected nonempty ordinary
text and guard3 rejected the now-nonempty DIL root. Both gates retained an obsolete
empty-render assumption while hydration changed representation, not identity.

Readback now permits this exact ACK transition; source must still be finished
final/all, end_turn/is_complete, exact selected assistant ID and linked to the
latest user. Hidden protocol keeps its stricter diagnostic gates and arbitrary
hidden prose remains refused. Tests in the existing DOM case cover non-DIL and
DIL hydration and wrong-user ancestry. Production guard tracing on the original
thread returned ACK_RECOVERED/3bytes with the exact same source ID. Feedback
controls were neither clicked nor submitted; their presence alone is not the
cause proved by this reproduction. No automatic regeneration or journal reset.

Full vet/tests/race/build passed. Exclusive profile/journal-guarded idle
installation preserved the journal SHA and reports ready=true active0/available2;
installed hashes bc859865… (adapter),949471c6… (MCP). This validates original
answer readback, not completion of all pending import chunks or client delivery.

## 2026-10-17: empty-rendered history ACK and repeated source-fetch latency

**Fixed and deployed locally; uncommitted.** In ses_ebbd828b, a187950-byte
history import (chunk1/7) generated from18:18:57 to18:19:29, but rendered zero
answer bytes and failed missing-answer at18:19:45. The source-bound finished
final was exactly `ACK` (3bytes), not empty or a malformed tool call. Its decoded
full input normalized hash equals sent-log f21d3815…; user/assistant ancestry
and completion/end_turn are verified. The fallback supported protocol source
but not this acknowledgement. It now admits only exact trimmed ACK in addition
to the existing protocol cases, never arbitrary hidden prose; other identities
and unfinished answers remain rejected.

Read-only production ReadThreadTurn on the original conversation returned exact
ACK in29.2s, with the full original input hash verified and journal SHA unchanged.
This verifies readback, NOT completion of the remaining six import chunks or
execution/delivery of the user's final task. The interrupted replay checkpoint
is preserved and continues through the existing replay phase path.

Latency evidence: an earlier finished answer was observed18:18:22 but returned
18:18:35 after two duplicate source-verification polls. responseState now marks
an exact completed-final readback SourceVerified; waitForAnswer returns it once,
without two redundant authenticated fetches. Ordinary renderer-only answers
retain stability checks. Existing DOM suite proves exact ACK recovery, wrong
ancestry refusal, and exactly one source fetch for this verified final.
Full vet/tests/race/build passed. Guarded idle deployment preserved journal hash
and returned ready=true active0/available2. This is not incremental streaming
or a fix for every visual-answer variant, rotation cause or recovery UX case.

## 2026-10-17: parallel sent-log ownership and lost rotation target

**Implemented and deployed locally; uncommitted.** Request-local parallel
handlers did not inherit statePath or forward sent-log operations to the shared
owner. Their logs vanished after each request, leaving no durable sent-log even
though browser submissions occurred. Rotation imports/finals and format repairs
also bypassed submission logging. These paths now log intent/outcome, share the
owner's synchronized ring/file, sync appended records and retain old-session
lookup beyond the previous 300-line tail. Diagnostic absence no longer claims
that no browser action happened. Intent is not proof of a successful Send.

Error paths preserve an observed conversation URL even for cancelled polls or
failed send actions, and rotation no longer replaces a known target with an
empty failed return. Recovery can accept an explicit thread_url only with the
exact pending request, restricted ChatGPT URL and full prompt/source proof;
candidate failure leaves the journal untouched. It cannot override a different
already-stored target or manufacture historical sent-log records.

Read-only inspection of ses_ebbd828b showed four equal-label/equal-target URL
wrappers in declared Markdown source accounting for exactly119 extra characters.
An optional source projection is accepted ONLY when the entire projected prompt
matches; literal argument backslashes and unequal links remain significant.
The installed /recover returned HTTP200 in11.14s, recovered1698B with2 validated
cached calls, Dirty=false,105messages, restored thread6ad30058…, all other
journals semantically unchanged. No model generation or tool execution occurred.
The client has not been proved to consume those calls. Tests/vet/race/build
passed, including durable parallel log/restart, rotation lineage and candidate
target rejection cases, using existing test files/functions.

Initial guarded deployment refused an active turn. A later idle deployment
installed the binaries, but readiness exposed a separately dead Chrome/profile
lock. The owner PID was proved absent and its Unix socket refused connections;
only stale Singleton symlinks were moved to a private diagnostic backup before
relaunching the dedicated ChatGPT browser. Final readiness:9222, logged-in,
composer present, active0/available2. No active browser was killed.

**Still open:** an explicit client-authoritative continuation/abandonment policy
that does not require original-response adoption for a new ordinary user turn.
The current exact pending-response recovery guard still exists; do not claim
all uncertainty errors are eliminated or that history rewrites themselves are
the same as unknown submission outcomes. Ordinary ChatGPT output img extraction
also remains distinct from inline SVG rasterization and Gemini image handling.

## 2026-10-15 ec597: source-bound recovery, not guessed text matching

Direct investigation of `ses_ec5972388ffeVexnQwYcnwkZ1K` traced the current
blocker to the 12:58 rotation followed by protocol-source verification failure
at 13:01. An earlier interrupted history import had already resumed and completed;
it is not the submission currently pending. Small history mismatches separately
triggered rotations and remain a distinct continuity concern.

Read-only inspection bound user `953c1b2c…` and completed final assistant
`f4f0f262…` through their actual DOM IDs and source ancestry. Decoding the user
source's declared Markdown and converting CR/CRLF to LF matched the ENTIRE
97,805-byte pending prompt (identical SHA-256). Both decoded and pending text
contain 1,901 literal backslashes: blanket punctuation unescaping was unnecessary
and unsafe, and the speculative implementation was removed. Lone CR must become
LF, not disappear; meaningful code whitespace and backslashes stay significant.

The actual answer renderer has no DIL root and can be temporarily empty while
hydrating. Source readback now supports its exact assistant-owner ID, retaining
finished-final/channel/recipient/ancestry checks and rejecting ordinary prose
fallback. A traced fetch took about fourteen seconds; the former eight-second
read bound was insufficient. Rechecking visual text equality also falsely
rejected hydration even when user/assistant IDs and final status stayed fixed.
The read now has a bounded twenty-second allowance within its caller deadline,
and rechecks immutable IDs, current user binding, completion and alert state.
Wrong identities, unfinished sources, stale answers and invalid tool arguments
remain rejected. An empty display-only escape envelope is removed only AFTER
its contained calls pass full validation; JSON argument bytes are not rewritten.

Verification extends existing test functions/files: newline/literal-backslash
distinctions, non-DIL empty/visible source, hydration during fetch, changed-user
and changed-assistant rejection, and exact tool arguments. Full vet, tests, race
checks and both executable builds passed. Private source/deployment/recovery
evidence is under `~/.chatgpt-proxy/diagnostics/`; no original task or tool was
executed by these diagnostics. This does not complete the broader recovery UI
or establish that every history-mismatch/rotation cause has been fixed.

Installed read-only recovery subsequently returned HTTP200 in36.377s:
`cached_tool_calls=1`, `response_bytes=11316`, Dirty=false, no pending phase,
380 messages (378 previously completed + pending input + validated answer),
same original thread URL. Every other session is semantically unchanged. No
model submission, historical replay, fresh thread or tool execution occurred.
The client still needs to consume the recovered cached response; consumption
was not inferred from successful recovery. Idle deployment used both the
exclusive profile guard and journal owner lock, preserved the journal hash
across restart and retained private backups. Changes remain uncommitted.

## 2026-10-14 post-tool genuinely empty final

**Status:** strict completed-empty classification implemented, live verified and
installed; the original session is durably rejected read-only. This does not
complete the larger recovery UX below or guarantee ChatGPT never emits an empty
answer.

For `ses_f1c7ba8b8ffe4ZYAp4NhTjq49D`, the 10:33:36 response delivered one valid
tool call. The next 3,364-byte submission was a tool-result-only delta, not the
earlier composer failure or a child-header misattribution. Owned read-only
inspection proves the latest DOM assistant `92bf6299…` is complete and its exact
final/all backend text source is genuinely zero bytes, finished_successfully,
end_turn/is_complete true. Its declared parent is a reasoning recap and then
the latest DOM user `cbf59217…`; the entire normalized pending/source prompt
matches at 3,363 bytes (only trailing whitespace differs). Null DIL is present,
but there is no hidden nonempty wrapper or invalid-tag recovery diagnostic.
The prior null-DIL fallback correctly refuses this case; there is no answer to
extract and no evidence that the user's previously delivered tool failed.

Recovery now distinguishes this exact authoritative completed-empty proof from
missing extraction/unknown outcomes. After the existing normal polling grace,
read-only settlement can durably reject it through the existing invalid-output
gate: terminal422, zero new delivered calls, no original resubmission, retained
completed history/pending tool result, and a distinct new ordinary user boundary.
Exact retries cannot regenerate the empty turn. Wrong source identity, ancestry,
status/channel/recipient, changed DOM/user, active generation or alerts remain
protected. Save failure rolls back rejection. Recovery attaches its owned target
to the initialized live executor with cancellation scoped to that target; the
precise cause of the two original dead-tab reconnects is not established.

Existing DOM/error/adoption tests were extended, without new bridge test files
or top-level test functions. Full vet/test/build and both binaries pass. Actual
original-thread readback returns completed-empty in 16.93 seconds, without a
journal mutation. Isolated browser-backed two-slot tool-loop verification passed:
two distinct owned targets/threads, one validated inert call each, externally
fabricated results followed by correct finals on tool-result-only deltas, stable
call/final duplicate caches, active=2 readiness true then idle capacity=2. No real
or dummy function was executed. Live cancellation of a three-second read-only
recovery did not kill a peer pool generation; the peer completed its harmless
final and the root remained healthy (`peer.json`).

Both binaries were atomically installed under an exclusive idle profile guard
and journal-owner lock; no active generation was aborted. Final installed hashes
match artifacts, readiness=true, active=0/available=2; deployment left the journal
unchanged (`installation.json`). The first deployment's post-start health wait
timed out and later readiness passed; two initial installed read-only recoveries
returned context canceled around20 seconds, retaining all uncertainty. Their exact
cause is not established. Stage-specific recovery errors were added rather than
discarding the journal or repeating the original completion. Final installed
POST /v1/sessions/recover with the exact saved request returned422 completed-empty
in18.81 seconds. Only `ses_f1c7ba8…` changed: Dirty=false/Phase=invalid, zero cached
calls, all106 completed messages and the exact pending request/prompt/thread
retained, other journals unchanged (`recovery.json`). This is known rejection,
not a recovered successful answer; no original task or tool was executed and
client consumption is unknown. Send a distinct new ordinary user message in that
same OpenCode session to cross the existing safe continuation boundary; retrying
the original failed step remains terminal. Genuinely unverifiable sessions remain
protected, and the larger recovery UX below is unfinished.

Evidence: goal `3cb60bfe…` (`inspection.json`, `readback.json`,
`stage-verification.log`, `live.json`, `live-adapter.log`, `peer.json`,
`installation.json`, `recovery.json`).

## Incomplete uncertainty journals (2026-10-15)

**Implemented; actual session resync armed, isolated live verification passed.** The
adapter previously saved `Dirty=true` before phase/request/prompt, leaving a
reproducible crash or save-failure window. This is a possible mechanism, not a
proved attribution of older records. The first write now includes the complete
final binding or explicit inert-replay phase. Load/save classify incomplete
records without rewriting their evidence or treating them as clean.

Completions and `/recover` return terminal422 `error.code=incomplete_journal`
when the original binding is absent or repair binding is inconsistent. Supplying
a guessed request is not recovery evidence. Explicit `/resync` accepts reviewed
completed authoritative history and recorded tool results, retaining Dirty until
a distinct new user boundary; historical calls remain inert imports. It can
reconcile completed later history after the journal's outstanding call results.
Old user requests and system-update-only changes cannot consume resync permission
through the loose compaction/re-anchoring path.
For the reviewed current session, endpoint arming preserved 386 original journal
messages and reconciled 2978 completed-history messages, including 1126 recorded
tool results. Only that journal changed; no original task was generated or
historical tool executed. The armed snapshot survived an idle service restart
with unchanged SHA256. This is **armed, not a recovered/settled actual turn**:
the next distinct ordinary user message must trigger inert import and continuation.
An isolated synthetic journal passed live resync of 35 messages/12,340 bytes in
two inert ACK chunks, harmless final response, a separate-thread switch, and
reopening the same resynced thread. Zero tools were offered/executed; the actual
session hash stayed unchanged. The first synthetic attempt hit a proved
pre-submission composer mismatch (clean saved import progress), then safely
resumed; this is not an incomplete/unknown submitted journal.

Separately, the newly reported `ses_ed3b3e58…` typing deadline at 09:41:53 was
reported before submission (`uncertain=false`, retained completed state). It is
not this session's missing-pending journal. Large-input typing/timeout handling
remains a separate follow-up; this change does not claim to fix it.
The larger integrated user-facing recovery UX below is still unfinished.

## User-facing recovery and safe model-switch fallback

**Status:** continuation repair in progress; recovery UX is still unfinished.
Existing read-only adoption, terminal errors and uncertainty guards are not a
complete user-facing fallback. The October 14 repair adds explicit pre-send
failure evidence so an existing thread URL cannot falsely imply this turn was
submitted, and compares the observed optional URL-defusing spaces symmetrically.
The stage-aware repair was installed at an exclusive idle profile-lock boundary.
The latest `ses_ecb2dd…` final was proved unsent by its composer-mismatch log and
exact pending prompt; only its pending uncertainty was cleared, retaining all 15
completed messages and its thread URL. Other sessions were unchanged. Full Go
checks and the exact recorded request route (fake backend, no task execution)
pass. The additional one-character composer difference was traced to rich-link
widgets adding visual newlines to `innerText`, not changed logical input. The
submit check now reads the current ProseMirror document and preserves internal
spaces, tabs and newlines instead of collapsing them. Exact DOM-ID-bound recovery
also preserves internal whitespace. Installed harmless URL-rich two-turn
verification passed: same thread, remembered first-turn codeword, identical
duplicate-cache response, no tools provided or executed, readiness true afterward.
Both binaries were installed atomically with active=0 before/after; no active
generation was restarted. Final audit confirmed all other preexisting sessions
unchanged. Genuinely ambiguous final submissions remain protected; this is not a
complete recovery UX.

**Problem:** a prompt may reach ChatGPT before cancellation, connection failure
or extraction failure prevents a verified response from reaching OpenCode.
Switching to another model does not settle that pending browser turn. Users
currently encounter internal recovery/resync instructions and blocked followups
instead of a understandable continuation path. Proven pre-submit failures and
verified completed-but-invalid output must not be mistaken for unknown outcomes.

**Required behavior:**
- Before submission: automatically retry/fall back only when no submission is
  proved. After submission: first track/recover the exact original turn read-only;
  return a validated cached response with stable call IDs if it can be bound.
- Completed malformed output with no delivered calls: reject once and support
  a safe new-user boundary. A still-generating turn remains tracked, not duplicated.
- If the outcome is genuinely unverifiable: expose an explicit recovery action
  in the OpenCode integration. Explain what is unknown and offer continuation
  from reviewed recorded completed history in a fresh working chat. Resync is a
  deliberate abandonment/continuation boundary, not proof the old turn succeeded.
- Account for already-delivered/executed calls and results before abandonment;
  never rerun historical tools, execute an abandoned late response or blindly
  clear Dirty. Define late-completion, cancellation and parent/child behavior.
- Keep manual exact-request recovery endpoints for diagnostics, not as the normal
  user workflow. Avoid automatic retry loops and make v1/v2 errors actionable.

**Acceptance:** actual installed v1/v2 flow covers completed and canceled
ChatGPT→other-model→ChatGPT switches, delayed/hidden/malformed responses and
already-executed tools; the user can explicitly recover or safely continue without
crafting HTTP bodies, lost completed history or duplicate tool effects. Tests
must distinguish recoverable, known-rejected and genuinely ambiguous outcomes.
Do not claim seamless switching solely from synthetic-history adapter fixtures.

## 2026-10-12 missing answer: complete null-DIL tool-wrapper rendering

**Fixed and deployed; pending answer recovered read-only.** The ordinary user
correction `I said using loopd goal, not subagent` is in the exact final pending
request for `ses_ed4229edfffeKXMTeqPnY2q5y1`. At local 11:00–11:03 three inert
history imports returned ACK; the final turn stopped generating but extracted
zero bytes. This is distinct from the earlier child/header rejection below.
Owned background inspection under the profile turn lock found the final H4
assistant identity `e267b0ed…` fresh and complete, with an empty DIL response root,
no stop/continue control or page alert. Its exact DOM-ID-bound stored source is a
finished final 3,374-byte literal tool wrapper. DIL metadata reports
`invalid_tag_syntax`/`recovered_parse`, an empty fallbackMarkdown and renderer code
returning null. It is not evidence of an empty model response, stale ACK adoption,
a missing heading selector, quota/refusal, or a prompt that never submitted.

The shared response probe now reads only this observed complete empty null-DIL
wrapper case: exact fresh DOM ID, finished final source, declared ancestry to the
latest DOM user, diagnostic metadata and an unchanged DOM boundary after the
read. No renderer code or tool is executed; ordinary Markdown remains DOM-only.
Existing Markdown/math/card serialization and adapter JSON/schema validation are
unchanged. Recovery now obtains the actual last-user ID from the heading's
adjacent bubble, allowing declared Markdown source decoding to prove the full
original prompt despite UI reflow. Unknown/empty responses remain protected,
never guessed or resubmitted. Missing-answer failures are terminal HTTP422 before
SSE instead of the previous first-failure502; journals remain dirty until proved
recovery. Truly retryable pre-submit infra failures retain their prior behavior.
The actual installed opencode2 v2.0.26 executable classifies408/409/5xx as
ProviderInternal (retryable) and other4xx, including422, as InvalidRequest
(terminal); this was checked in the installed bundle, not inferred from a dev
checkout or the separate Homebrew opencode v1 executable.

Existing DOM/error tests were strengthened (no new test files/functions) for the
captured duplicate IDs and sibling user bubble, byte-identical source JSON,
stale baseline, wrong final/role/status/ancestry, reasoning-only/no assistant,
generation in progress, normal rendered answer priority, terminal422 before SSE,
retained exact journal and zero repeated submissions. Full Go vet/test/build
passed; both binaries were atomically installed at an idle ready boundary under
the profile lock, with ready=true before/after launchd restart and matching hashes.
POST /v1/sessions/recover returned200, validated/cached one call without executing
it, and cleared only this proved pending journal. No original completion request
was replayed. Whether the user's client consumes the cache is unknown; retry the
exact original request to receive it, or deliberately continue on the currently
selected model with reviewed context. Current local host metadata shows a later
model switch/continuation, but does not retain the original trigger/error message;
it is not proof that the recovered cache was consumed. Other dirty sessions are
unrelated and untouched. Evidence/artifacts: loopd goal `6f129324…`.

## Model-switch continuation after a canceled browser turn

**Status (2026-10-12):** cause correlated, safety/durability regression fixed and
deployed; the original canceled turn is still unverified, not automatically
recovered. Completed Muse work does not prove a pending ChatGPT answer finished.

The current `ses_ed4229…` journal is a later turn than the earlier null-DIL
recovery above: 112 saved messages, 113 pending messages, 12 tools, and a
3,099-byte pending browser prompt. Its final message is a delivered tool result,
not the later switch-back user question. Logs establish typing at 15:28:03,
submission at 15:28:04, answer wait at 15:28:08, and client-context cancellation
at 15:33:01. Therefore it cannot use the proved pre-submission retry path.
The last DOM-bound user source readback normalizes to 1,473 bytes versus the
original prompt's 3,096 bytes; original completion/delivery remains unknown.
The exact reason that submission did not yield a matching stored user source
is not established. Do not reinterpret this as a navigation failure or clear it
because the alternative provider completed.

Authenticated local host metadata confirms Muse completion/idle, switch back,
a distinct ten-byte ordinary user message, and `provider.invalid-request` with
actual HTTP 422 (not a retryable completion 409). Diagnostics retain IDs, sizes,
hashes, phases and times, not private prompt/tool contents. Existing adoption
already reads the **original** pending prompt and original schemas, then imports
expanded completed alternative-provider exchanges inertly before the fresh
request with current schemas. No model-specific random identity or tool replay
is needed. The terminal error now explains that an unverifiable browser turn
requires explicit authoritative-transcript resync, not resubmission.

One additional defect was found: automatic adoption mutated its in-memory
journal before saving and, on a failed save, discarded the original pending
binding. It now restores the entire previous journal on that failure. Existing
recovery tests were extended (no new bridge test files/functions) for two failed
save/read-only retries, the actual pending tool-result role, current child/root
headers, untouched parent uncertainty, stable original call IDs, changed current
tool schemas, inert completed Muse calls, one fresh request, and restart-safe
duplicate caching. Different/ambiguous source and rejected/metadata-only guards
remain strict; formatting/media and replay/resync regressions remain covered.

`go vet ./...`, `go test ./...`, `go build ./cmd/...` and both named binaries
passed. Installed at an exclusive idle profile-lock boundary; readiness was true
before and after launchd restart. OpenAI binary SHA256 begins `0f5e13e97887…`;
MCP binary remains `5021c726d41d…`, matching the staged build. Existing running
MCP processes were not interrupted. Evidence: goal `aebcf238…` (`audit.json`,
`verification.log`, `deployment.json`, `live-probe.json`). A harmless browser-backed
installed-adapter HTTP probe succeeded: initial marker, synthetic completed
Muse-shaped call/result history, changed current schema with tool choice none,
correct recalled marker, zero executable calls, identical duplicate cache,
clean eight-message child journal and no root-affinity journal. This is live
adapter catchup coverage, **not** an actual installed-host/Muse provider switch
and not recovery of the original uncertain turn.

**Safe continuation:** preserve the canceled journal. Only an exact, completed,
DOM/source-bound original response can authorize read-only recovery/cache; then
the exact original request obtains stable cached calls without browser replay.
If that response cannot be verified, review the completed client transcript and
tool results and explicitly arm `/v1/sessions/resync` with the same authoritative
session header, followed by a new user message. Do not replay the original task,
execute historical tools, or globally clear Dirty. Automatic recovery of the
reported session is still blocked on that verification/review boundary.

## Shared adapter failures: uncertain turns, rejected continuation and long waits

**Status:** bridge recovery fixes implemented and under verification on
2026-10-11; exact reported UI session attribution and real General/Explore
reproduction remain unresolved. This item is not fully complete.

**2026-10-12 confirmed mobile-subagent root cause:** the actual child
`ses_ed37aa…` has a 1,896-byte user message and eleven delivered call IDs exactly
matching the 15-message rejected request stored under parent `ses_ed4229…`.
Installed v2.0.26 was exercised with an isolated harmless subagent and mock SSE
provider: `x-opencode-session-id` identifies the child, but `X-Session-Id`,
`x-opencode-session` and `x-session-affinity` retain the parent/root. The adapter
previously prioritized `X-Session-Id`, replacing the parent's journal with child
history. A child protocol rejection consequently blocked the parent's legitimate
already-delivered subagent-result continuation. This is not evidence that the
parent needs a new ordinary user message, nor a reason to permit arbitrary tool
results after rejected model calls.

Identity resolution now prioritizes the actual `x-opencode-session-id`; legacy
precedence is unchanged when it is absent. Existing rejected-state tests cover
the real header envelope and child role shape, byte-identical parent isolation,
no browser submission on exact child retry, restart persistence, and a parent's
known-delivered tool continuation. Both binaries were rebuilt and installed
under the profile turn lock with readiness true before/after launchd restart.
Actual installed v2.0.26 browser-backed General subagent completed in an isolated
private host: parent `ses_ed20e71…`, child `ses_ed20db86…`, separate clean journals
and separate ChatGPT threads. The parent's delivered `subagent` call completed
with the child's marker answer and the parent consumed its result and completed
its own marker answer. This proves a real harmless subagent flow, not merely a
parent-user fixture; broader Explore/long-running implementation remains untested.
Historical poisoned journals were not cleared. A corrected child identity now
inherits an old root-key rejection/uncertainty only when its authoritative
non-system transcript exactly extends the old pending transcript, including
call IDs/results (canonical SDK encoding, not user substring similarity).
Tests prove transformed retries, fabricated appended results, persisted aliases,
and uncertain submitted turns cannot resubmit; a genuinely distinct task can
start independently. Old parent history cannot be reconstructed from overwritten
journals, so no automatic ActionBox replay or child reset is attempted. Review
already completed work and send explicit fresh context/new user instruction;
do not blindly retry the rejected task or clear its journal. Read-only source
diagnostics in an owned background target bound one user message exactly to the
stored pending prompt, then inspected every declared final-response ancestry
without selecting a latest branch or adopting a response. Original source answer
`0ed28662…` (809 bytes, created 1791748442.924946) contains one authorized,
schema-valid `shell` call plus 519 bytes of non-wrapper commentary. Its subsequent
final continuation `73662c14…` (307 bytes, created 1791748496.410343, parent is the
first answer) also contains one valid `shell` call plus 17 non-wrapper bytes.
Both precede the child's protocol rejection at 1791748507792. Mixing commentary
with executable bridge calls violates the existing strict protocol; neither
response should execute. Escaped wrapper markers are already supported and are
not the established defect. Existing child isolation test now uses a redacted
schema-valid call plus commentary to reproduce this actual failure shape, and
proves zero delivered child calls. Full raw prompts/arguments/results are not
added to diagnostic logs. Current rejection/uncertainty recovery remains strict;
diagnostic source inspection is not an adoption authority.

The named-project setup failure is shared by v1/v2 clients, not v2-exclusive.
The v1 graphify research request is correlated to `ses_ed41e2cc0ffep0eSn7og7zrC8h`:
the exported 236-byte user message matches both reported GitHub targets, and
adapter log 23:53–23:56 repeats `Project URL not observed` before replay turn 1/2.
Explicit pre-submission project-setup errors now activate the existing eager
and rotation shared-project fallback; no projects are manually created to hide
the bug. A possibly submitted/unknown failure still cannot fall back.

**Reported symptom:** General ("Implement ActionBox backend MVP") and Explore
("Inspect browser ingestion options") both display session ID
`ses_ed530eb94ffe6jsmRTCDKTQa7L` and fail with the uncertain-submitted-turn
409/recover instruction; the parent run shows 17m54s elapsed.

**Known versus unknown:** that exact ID was absent from the inspected local
adapter log and journal. A shared ID in the UI is not proof of identical
outgoing request headers. Queueing, replay, recovery and retries may contribute
to elapsed time; the original submission failure has not been correlated.
A separate session had an incomplete tool wrapper followed by recovery comparing
a 10,154-byte rendered prompt with a 90,499-byte submitted prompt. Do not
attribute that separate failure to these subagents without evidence.

**Proved scope:** the local adapter is launchd `com.chatgpt-proxy.openai` on
127.0.0.1:8788 with state in `~/.chatgpt-proxy/openai-sessions.json` and Chrome
profile `~/.chatgpt-proxy` on CDP 9222. Core v2 supplies `X-Session-Id` and
`x-session-affinity` from its actual session ID; agent names do not separate
main-lane journals. In the correlated `ses_ed62e2…` session, rotation submitted
at 18:57:31 and a completed response failed strict tool-wrapper parsing at
18:58:05. Subsequent retries read the same Dirty final journal, not a new
generation. Recovery's 90-second wall-clock check previously did not bound
individual browser reads; it now supplies an actual context deadline. Queueing
and client retry/backoff can add elapsed time, but the reported 3m/17m54s UI
durations are not fully correlated to individual request phases.

**Implemented safeguards:** completed, strictly rejected protocol output delivers
no partial calls and is durably classified separately from unknown submission.
Exact retries do not resubmit. The original byte-prefix continuation requirement
regressed v2's genuine followup: its journal contains a 16-message rejected
tool-history request while its authoritative session retains failed assistant
placeholders and a distinct new user message. Agent/system/history rebuilds
cannot be required to reproduce that prefix. Continuation now requires a distinct
final ordinary user message (text/string encoding independent); volatile v2
`<system-update>` metadata is not new input, and appended undelivered tool calls
or results cannot be made valid by adding a user message afterward. Tool/schema-only
changes and old user input do not qualify. The authoritative rebuilt transcript
is imported into a fresh thread with full current schemas; the rejected
fingerprint remains blocked even after continuation. Completed protocol failures
and rejected continuation attempts return terminal HTTP 422 with
`invalid_request_error`, not transient backend 502. Installed v2 2.0.26 was
verified with a mock provider: 422 terminates after one request, whereas even
409 `invalid_request_error` retries (seven requests in 45 seconds). The completion
uncertainty guard therefore also returns terminal 422 with its distinct recovery
instruction; journal/adoption protections remain unchanged. Recovery/resync
endpoint conflicts retain 409. A separately correlated new implementation prompt
in `ses_ed4229…` never reached submission: per-session Project URL discovery
failed before `askLocked`, but only navigation errors cleared Dirty. Explicit
project-setup errors are now marked at that pre-submission boundary and allow
safe retries only when no conversation URL was reached. Unknown outcomes
remain protected. Rotation/eager fallback is limited to proved pre-submission
navigation/project-setup errors; a possibly reached turn is not automatically replayed.
Recovery navigates only an owned background target under the profile lock,
expands only the last user bubble's Show more control, and removes UI controls
structurally (never strips real prompt suffixes). For escaped rich user text,
the exact DOM turn identity binds one backend user node; only its declared
CommonMark serialization is parsed. Full normalized equality with the original
pending prompt is still mandatory—no prefix matching or guessed unescaping.
An isolated probe decoded the full live 91,925-byte serialized source to exactly
the original 90,499-byte normalized prompt. Prompt windows and credentials are
not logged. Live adoption/deployment status belongs in the goal evidence, not
an assertion that the uncorrelated reported subagents are fixed.

**Investigation scope:**
- Correlate parent/child session IDs, outgoing identity headers, resolved adapter
  journal keys and the actual adapter instance/state path. Use metadata-only
  request IDs and gate/lock-wait, submission, generation and recovery timings;
  do not log private prompts or credentials.
- Determine whether distinct child sessions accidentally share identity, or
  intentionally reuse one session. Keep parent, child and auxiliary histories
  isolated without guessing identity from agent names.
- Find the first backend/protocol failure before the repeated 409s. Inspect
  collapsed/virtualized user bubbles and recover complete submitted text when
  possible; preserve exact request binding, not prefix-only acceptance.
- Define bounded, actionable error handling for queue/recovery/retry waits.
  Never blindly clear Dirty or resubmit an uncertain tool turn.

**Acceptance:** a real v2 General/Explore reproduction records each actual
session identity and original failure; distinct sessions cannot poison each
other's journal; safe recoverable turns adopt/cache without duplicate execution;
truly ambiguous turns remain protected and produce useful diagnostics; elapsed
time is attributable to explicit phases rather than unexplained retry loops.
This is separate from the deferred concurrent-tab feature below.

## Terminal-friendly rich visual responses and math

**Status:** scoped implementation verified on 2026-10-11; broader visual support
and dual-host TUI validation remain partial. Not deployed in this run.

**Implemented evidence:** read-only inspection in an owned background CDP tab
under the profile turn lock captured the exact reported card. It is HTML/CSS
`data-d-component="row"` / `data-d-direction="row"`, with nested comparison
rows and explicit winner/elimination text, not a raster image. These semantic
leaf-content rows now become compact Markdown list rows; ordinary div
paragraphs, lists, tables and code keep their structure. Unlabeled decorative
row icons are omitted; their meaning remains in the explicit captions and
elimination summary, without guessing from success/danger colors.

Annotated math retains one source expression and emits supported Unicode/plain
math without dollar delimiters. Powers/subscripts keep ASCII notation and
grouping; unsupported commands preserve the entire original TeX in inline code.
Literal prose/code TeX and tool-call JSON are not converted. Images, SVG and
canvas expose accessible text or an explicit no-description fallback. Only
absolute HTTPS source URLs without credentials/query/fragment and outside
local/private/ChatGPT authenticated hosts are linked; unavailable/authenticated
sources are not invented or exposed. No OCR, new LLM calls or dependencies.

Existing `response_test.go` rich case extended (no new test files/functions).
Real captured answer replay in isolated headless Chrome verifies all four rows
and both reported math expressions; `go vet ./...`, `go test ./...`,
`go build ./cmd/...` and both named binary builds passed. Evidence and staged
binaries: `.opencode/loopd/goals/ce31ae43-e8ec-4d2d-9f00-9ba9d4e3c661/`
(`answer-3.html`, `before.md`, `after.md`, `progress.md`).

**Remaining limitations:** this is not a general visual renderer. Image-only
pixels, canvas geometry, SVG geometry and color-only meaning without accessible
semantic text are not interpreted. Other card layouts use normal extraction.
Actual visual verification in both OpenCode hosts remains outstanding; DOM
and Markdown correctness are proved, not host screenshot equivalence. Deployment
deferred: the shared adapter readiness probe returned 503 with an about:blank
tab/no composer, so no successful safe idle/restart boundary was established;
no restart, Dirty-state repair or unrelated browser recovery was attempted.

**Reported symptoms (screenshots):**
- A web visual card has compact rows `t = 0: abac < abad` through
  `t = 3: c < d`, winner captions, color and elimination marks. OpenCode instead
  puts each label, operand and comparison on separate widely spaced lines,
  losing grouping and making the explanation hard to follow.
- Display math appears literally as `$$`, `R(i+t)<R(j+t)\\qquad 0\\le t\\le k`,
  and `j,\\ j+1,\\ \\ldots,\\ j+k` instead of readable symbols and spacing.
- User reports increasingly image/visual-heavy web answers. Screenshots alone
  do not establish whether each answer is an actual image, SVG/canvas or an
  HTML/CSS card; identify the representation before selecting a conversion.

**Proposed scope and decisions:**
- Consider a small deterministic terminal-formatting layer after extraction,
  not a second general rendering engine. Prefer semantic Markdown tables,
  grouped lists or fenced aligned text for structured cards; preserve row
  relationships/captions and express color-only meaning with text labels.
- Inspect actual image/SVG/canvas outputs separately: preserve accessible
  descriptions and expose a usable artifact link/file or supported preview.
  Define an explicit fallback when content has no text equivalent. Do not
  silently drop it or invent an OCR/vision description; whether to add OCR or
  model-based descriptions is an unresolved opt-in design/cost decision.
- For OpenCode without math rendering, translate supported TeX into readable
  Unicode/plain text: `\\le` → `≤`, `\\ge` → `≥`, `\\ldots` → `…`, `\\qquad` →
  spacing; retain inequalities, ranges, powers/subscripts and expression order.
  Example: `R(i+t) < R(j+t), 0 ≤ t ≤ k` and `j, j+1, …, j+k`.
- Keep conversion scoped to identified math nodes, never code blocks, literal
  TeX examples, tool-protocol JSON or unrelated prose. Define safe fallback for
  unsupported fractions, matrices and commands rather than lossy guessing.
  Choose Unicode versus ASCII fallback and source-preservation behavior before
  implementation; retain accessible single-expression extraction.

**Acceptance:** actual source-DOM fixtures for the reported visual card and
math produce compact, readable OpenCode output; each comparison stays on one
row with its caption; ≤/ranges/powers preserve meaning without raw unsupported
math delimiters or duplicates; image-only output is explicitly represented;
bold/code/links/tables and tool-call JSON remain intact. Verify in both supported
OpenCode hosts, not only by inspecting generated Markdown.

## Genuine concurrent generations

**Status:** IMPLEMENTED flag-gated default-serial on 2026-10-13
(`-max-parallel=2` / `CHATGPT_BRIDGE_MAX_PARALLEL=2`; default 1 = legacy
serial behavior). Same-session requests (turns, recover, resync, duplicates)
serialize; distinct sessions generate simultaneously on owned dedicated tabs
(bounded pool of 2, cancellation-aware wait). No global lock is held for a
full generation: per-session locks persist across the long turn, the journal
merges immutable committed snapshots under a short mutex with atomic
file replacement, and only browser init/reconnect/project setup take short
shared locks. MCP uses dedicated owned tabs and never adopts adapter tabs.
Serial mode (`-max-parallel=1`) remains the fallback.

**Gate (passed, account permits overlap):** live two-owned-tab probe on the
same logged-in profile, exact distinct prompts submitted ~0.1s apart —
`gate-verdict.json` (goal artifacts): overlapping live answer-growth interval
2026-10-12T17:52:52.603329Z–17:52:52.756373Z (ALPHA 99→258 chars,
BETA 268→310 chars, both Stop controls present), exact own prompt/answer
identity, two distinct persistent thread URLs. Production `Pool` re-proof
(`pool-verdict.json`): two workers started ~10µs apart, overlap
2026-10-13T01:06:41.031423Z–01:06:41.111107Z (ALPHA 190→543, BETA 350→505
chars), both completed with correct markers. An earlier run showing both
replies with the same marker was a probe artifact (inherited homepage draft
concatenated pre-submit), not a server refusal — rejected as evidence.

**Design as proposed, with two corrections found during implementation:**
ownership probe showed the shared `openai-tab-id` file must not be written
by pool workers (each owns a short-lived target), and transaction routes
must re-acquire the profile guard shared (an exclusive re-acquire
self-deadlocks behind the transaction's own shared hold).

**Verified:** race suite covers backend-barrier overlap, same-session
serial/cached IDs, cancel-A-keeps-B with uncertain journal preserved,
saturated-queue cancel, simultaneous-save restart persistence, setup
serialization, non-adoption/close-only-own, journal single-writer. Live
installed-adapter overlap + cancellation + MCP verification and
rollback notes follow in the rollout record below.

**Rollout record (2026-10-13, service at `-max-parallel=2`):**
- Installed overlap: sessions inst-c/inst-d — interleaved service-log
growth polls on two owned tabs (D 410→2055B while C 110→1809B,
08:13:39–08:13:59), done 08:13:57/08:14:08, distinct thread URLs, clean
journals. Repeated inst-e/inst-f (joint streaming 08:16:10–08:16:19) and
inst-i/inst-j runs.
- Installed cancel: session inst-k aborted mid-generation via client
socket shutdown; journal kept `Dirty`/`Phase=final` with thread URL;
peer inst-l completed with correct marker and clean journal; distinct
thread URLs. Server propagates disconnects; the abort does not disturb
the peer or the shared browser.
- MCP ownership: MCP-config client (dedicated owned tab, shared guard)
does read-only status on its own new target and closes only it;
pre-existing tabs verified intact.
- Rollback: remove `-max-parallel 2` from the launchd plist (or set 1),
unload/load the job; serial behavior is the default and needs no code
change. Never auto-retry ambiguous turns; canceled sessions stay Dirty
for explicit recovery.

**Original proposal (kept for context):** run a live two-tab overlapping-generation probe on the same
account. Record submission, generation-start and completion timestamps, prove
overlap rather than two queued requests, and observe rate limits/interruption.
Do not enable concurrency unless this probe succeeds.

**Proposed ownership and locking:**
- One adapter process owns generation orchestration, with a dedicated owned
  tab per active session and a small configurable tab pool (default size 2).
  Concurrency remains flag-gated/off initially.
- Per-session turn/recovery locks serialize duplicate requests, recovery and
  resync within a session; different sessions may generate independently.
- Hold only short state-persistence and browser initialization/reconnection/
  project-setup locks. Replace the profile-wide whole-generation lock only
  after tab ownership and overlap behavior are proven safe.
- MCP uses its own owned tabs and must never adopt or navigate adapter tabs.

**Acceptance before rollout:** timestamped overlap; prompt/answer and thread/
project isolation; canceling one request does not stop the other; duplicate
and recovery safety within each session; restart-safe persistence without
lost updates; pool exhaustion queues safely; reconnect retains ownership;
MCP asks/status cannot steal adapter tabs. Keep serial mode as fallback.

## Project-per-session (1 OpenCode session → 1 ChatGPT Project)

**Status:** IMPLEMENTED flag-gated default-off on 2026-10-10
(`CHATGPT_BRIDGE_PROJECT_PER_SESSION=1`; unset = byte-identical legacy).
Session record +1 field (`Project`); rotation replaces reset-replay with a
single context-card + last-~6-verbatim turn in the session's
`OpenCode Bridge — <repo>/<short8>` project; any infra failure falls back to
the shared-project path (`rotation_fallback`); steady-state/resync
untouched; existing sessions grandfathered. 2026-10-10 eager start added:
new sessions mint their project on turn one (`decision=fresh-project`,
`eager_fallback` pre-submission only) instead of waiting for rotation —
verified live (fresh session answered from its own new project, `Project`
persisted, session clean). `go vet` + `go test ./...` green,
bins rebuilt, launchd restarted, `/readyz` non-503. Phase-0 probe HIT
(fresh "OpenCode Probe" project, codeword PROBE-88 retrieved verbatim by both
the no-card chat B and the with-card chat C; ~30s answers; see
phase0-report.md) — the gate this design was waiting on.

**Idea:** instead of all sessions sharing one "OpenCode Bridge" project with one
thread each, map each OpenCode session to its own dedicated ChatGPT project
(`OpenCode Bridge — <short-session-id>`). Within a project, keep a working chat
plus retired history chats, and rely on ChatGPT's project-level retrieval
([confirmed by OpenAI docs](https://help.openai.com/en/articles/10169521-using-projects-in-chatgpt):
"Chats can reference other conversations within the same project") instead of
replaying full transcripts.

**What it kills:** reset-replay storms, chunk budgets for history, the
normalization arms race for long sessions.

**What it keeps:** delta sync within a working chat, per-thread tool teaching,
dirty/uncertainty guards, resync.

**Design (from plan-mode analysis):**
- Mapping: session ID → project URL + working thread URL (state file +1 field).
- Steady state unchanged (delta-continue the working thread).
- Mismatch/compaction/length triggers **rotation** (fresh working chat in the
  same project) instead of reset-replay: first message = compact context card
  (goal, key decisions, digested tool results) + last N turns verbatim; older
  chats left for retrieval. No replay pastes, ever.
- Fallback mandatory: any project/chat failure drops back to shared-project
  thread behavior; new machinery must never fail-closed.
- Existing sessions grandfathered on the shared project; new sessions get own
  project (flaggable).
- Project creation reuses the proven `ensureProject` UI-dialog flow,
  generalized per session.

**Must validate first (Phase 0 probe, gates everything):** create a project,
put a unique codeword fact in chat A, open chat B in the same project and ask
for it — with and without a context card. If retrieval misses, stop and report;
do not build further. "Can reference" ≠ guaranteed recall.

**Open decisions for the user:** sidebar noise (N sessions = N projects,
contradicts the original no-noise goal — cap/reuse strategy?); naming
(session-short-id vs repo/task name); if probe is weak, rotation-with-card
only or drop entirely; rollout behind a flag vs default.

**Related constraints:** new projects need default (not project-only) memory
for cross-chat references — verify the creation default; creation adds ~15-30s
first-turn latency; unknown creation rate limits.

**Status 2026-10-14 (stable-thread goal, uncommitted, undeployed):** live
correlation on `ses_ec74b8ec4ffekz0UslWJu0xkCT` proved the first rotation was
an untagged title request (same session ID, tools=0, short system) hitting
`prefix_mismatch index=0`, and every later rotation was a 637-byte compact-DCP
reminder suffix the canonicalizer did not recognize. Narrow fixes: untagged
title shapes go to the title-lane journal (`isolateUntaggedTitle`, exact host
prefix only); compact `@N@` reminder envelopes compare equivalent; completed
DIL protocol falls back to DOM-ID-bound source bytes; fully validated outer
commentary is kept as non-executable text. Live same-thread continuity and
deployment still pending; item stays open until proven.
