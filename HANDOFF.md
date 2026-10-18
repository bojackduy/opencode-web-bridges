# Handoff: opencode-web-bridges

> **Written:** 2026-10-18
> **Scope:** parent plugin + `chatgpt-proxy` + `gemini-proxy` submodules, and the
> `opencode-loopd` interaction that surfaced two of the open items.
> **Supersedes:** `chatgpt-proxy/HANDOFF.md` (dated 2026-08-18). That file predates
> the OpenAI-compatible adapter entirely — it documents an MCP-only `ask_chatgpt`
> tool, points at the old `~/Documents/AI_workspaces/chatgpt-proxy` location, and
> its "next steps" are either done or obsolete. Read this instead; delete that one
> or reduce it to a pointer.

---

## What this project is

An OpenCode plugin (`@bojackduy/opencode-web-bridges`) that registers ChatGPT Web
and Gemini Web as ordinary OpenCode providers. Both are backed by a local
OpenAI-compatible HTTP adapter that drives a dedicated Chrome profile over CDP, so
they use the user's own logged-in browser session instead of an API key.

```
OpenCode ──HTTP /v1/chat/completions──▶ adapter ──CDP──▶ Chrome (chatgpt.com / gemini.google.com)
```

Because OpenCode sends **full history on every request** while the web UI keeps
**one continuous thread**, the adapters maintain a durable per-session journal and
sent-log to delta a request into the existing thread, reseed when history was
rewritten, and fail closed when a submission's outcome cannot be proven.

| Component | Location | Runtime |
| --- | --- | --- |
| Plugin (v1 + v2) | this repo, `src/` | loaded by OpenCode at startup |
| ChatGPT adapter | `chatgpt-proxy/` | `127.0.0.1:8788`, Chrome CDP 9222, `~/.chatgpt-proxy` |
| Gemini adapter | `gemini-proxy/` | `127.0.0.1:8789`, Chrome CDP 9223, `~/.gemini-proxy` |
| Lane plugin | `chatgpt-proxy/docs/opencode-session-lanes.ts` | shim at `~/.config/opencode/plugins/` |
| launchd jobs | `com.chatgpt-proxy.openai`, `com.gemini-proxy.openai` | |

---

## 1. Runtime is down right now — check this first

Neither browser is running and both CDP ports are dead.

- `9222: not listening`, `9223: not listening`, `0` Chrome processes.
- `GET :8788/readyz` → HTTP 503
  `failed to launch chrome: dedicated profile is already locked; inspect its owning Chrome and start its exact configured debugging port; no lock was removed`
- `GET :8789/readyz` → `{"ready":false,"detail":"pending uncertain turn; inspect owned chat"}`

The ChatGPT adapter is refusing to start because of a **stale profile lock**, and
its own error text says no lock was removed. There is no live Chrome owner, so
either a `SingletonLock` symlink in `~/.chatgpt-proxy` is orphaned or the lock
probe is matching something dead.

Compounding it: **`pgrep` reports 18 `bin/chatgpt-proxy` and 18 `bin/gemini-proxy`
processes**, while `launchctl` lists exactly one job each (PID 1001 / 989). Repeated
manual starts and/or launchd `KeepAlive` respawns have accumulated duplicate
adapters. Duplicate adapters each try to own the same Chrome profile and journal
file, which is a plausible contributor to the lock complaint and to whatever wrote
the duplicate dirty journals.

**Do not just delete the lock and move on.** First establish why 18 processes exist
and whether more than one is writing `~/.chatgpt-proxy/openai-sessions.json` —
concurrent writers would corrupt every other item in this document.

Suggested order:
1. `pgrep -fl 'bin/(chatgpt|gemini)-proxy'` and identify parent PIDs.
2. Compare against `launchctl list`; keep only the launchd-owned one.
3. Kill the duplicates, then reconcile the profile lock against a *provably absent*
   Chrome owner (same discipline the project used before: verify the PID is gone
   and the socket refuses connections before moving anything).
4. Confirm one adapter per service, one Chrome per profile, readiness true.

---

## 2. Sessions still marked uncertain

`~/.chatgpt-proxy/openai-sessions.json`: **111 sessions, 27 `Dirty`.**

Three are from the most recent failure and are the ones worth attention:

| Session | Pending | Thread |
| --- | --- | --- |
| `ses_eb59c2f54ffe0BUqioSX3FLJpW` | 1678 B | `…/c/6ad38e68-a3b0-83ec-b901-708a5d90ffb6` |
| `ses_eb59c2f50ffe8pn4PcSSzNpV0T` | 1445 B | `…/c/6ad391bd-14d8-83ec-92c7-9608fb878144` |
| `ses_eb59ee2c3ffekvRdoo1XYX1SnZ` | 1542 B | `…/c/6ad38dca-ec30-83ec-96d5-4451055d4b56` |

The rest are older: a deliberate cancel fixture (`inst-k`, 672 B), early test
artifacts (`live-weather-*`, `navskip-*`, `threadstate-*` with 0 B), and September
history (`ses_ed*`, `ses_f*`, several 130–170 KB prompts).

**These must not be bulk-cleared or resubmitted.** A `Dirty` journal is the only
thing preventing a duplicate browser turn and duplicate tool execution. Handle the
three recent ones with read-only `POST /v1/sessions/recover` using the exact
original request, then decide per session. The historical ones can be archived out
of the live file into `~/.chatgpt-proxy/diagnostics/` as a set, once you have
confirmed none correspond to work you still care about — that is a judgement call,
not a cleanup chore.

Gemini separately reports a pending uncertain turn via `/readyz`; its journal
(`~/.gemini-proxy/openai-sessions.json`, 13 entries) shows 0 dirty, so the two are
reporting different things and that mismatch is worth understanding.

---

## 3. The `send did not submit` bug is not closed

This was the failure that prompted the last round of fixes. What is actually
established:

- Logs repeatedly showed `type settle prompt=1678 trajectory=[0 0 0] inserr=invalid context`.
- `input.InsertText(prompt).Do(insCtx)` was called directly on a context with no CDP
  executor, so **native typing never worked at all** — every prompt silently fell
  back to the legacy `execCommand`/paste path. Fixed in `85c497e` by routing through
  `chromedp.Run`.
- After deploying, an isolated marker request returned `INPUT-OK`, HTTP 200, 43.02 s,
  `inserr=<nil>`, 0 tool calls.

What is **not** established, and is the actual open item:

- Why the Send click did not submit. The error text blamed a possibly-stuck
  attachment upload with no evidence any attachment existed — that hint is
  speculative and should be removed or made evidence-based.
- A confirmed no-op Send currently returns a generic error, which `AskThreadTurn`
  classifies as **uncertain**, even though the composer verifiably still held the
  exact prompt and no submission was proved. That is a false uncertainty: it blocks
  the user for no reason.
- Intended fix: if the composer still matches the expected prompt exactly and no
  user bubble / generation indicator appeared, return `ErrNotSubmitted`, clear
  pre-submit journal state, and let the caller retry — instead of going `Dirty`.
  Retry Send/Enter only within the same unsubmitted turn.

Before writing that, inspect one failed thread read-only and record: composer text
and length, Send button disabled state, attachment chips, generation indicator, the
latest user/assistant DOM IDs, and the current URL. The failure mode is not yet
diagnosed, only the typing precondition was.

Related: the native-input fix is gated to prompts **≤ 4096 bytes**; larger catalogs
still use the legacy paste path on purpose (activating native input for those
introduces an unproven slow-paste path). Large-prompt typing/timeout handling is a
separate known follow-up and is **not** fixed.

---

## 4. Two fixes are committed but not live yet

Both need a restart before they have any effect.

- **`chatgpt-proxy` `ed697a0`** — the v2 model picker showed four "ChatGPT Web"
  entries because the lane plugin registered one provider per auxiliary lane
  (`chatgpt-web`, `-title`, `-summary`, `-compaction`) under the assumption that v2
  cannot inject per-request headers. It can: the `session` `model.request` hook
  carries the agent plus a mutable header bag. One provider entry now serves every
  lane. **Restart OpenCode and confirm the picker shows one entry.**
- **`opencode-loopd` `6158564`** (separate repo, not a submodule) — a scoped goal
  worker was denied writing its own claimed file as "uncontrolled", because
  `structuredWritePaths` read only `args.filePath` and only knew the tool name
  `apply_patch`. Current OpenCode sends `path` and names the tool `patch`. Both are
  now accepted, and `bun run smoke:v2-lifecycle` was added as a real-host release
  gate. **Reload the loopd plugin host, then resume the two blocked
  `parallel-scope-test-*` goals.**

Note the SDK version skew that was checked here: the installed `@opencode/plugin` is
**2.0.11** while the OpenCode checkout is **2.0.29** and the CLI is **v2.0.32**. The
lane fix was typechecked against the *installed* 2.0.11, which does expose
`session.hook("model.request")` with the `providerID` option. If the plugin package
is ever bumped, re-check that hook — it is load-bearing.

---

## 5. Nothing is pushed

All three repos are local-only. This is the single highest-risk item for losing
work, because several commits are the only record of a failure analysis.

| Repo | Unpushed |
| --- | --- |
| `chatgpt-proxy` | 8 commits — `ed697a0`, `85c497e`, `5dbfaa6`, `557717a`, `bbede6a`, `58d3fe4`, `842adb9`, `bfead3f` |
| `gemini-proxy` | 3 commits — `9763de3`, `380cabf`, `8779061` |
| `opencode-web-bridges` | 3 commits — `8da9fa7`, `c8ee3d9`, `1c10f1c` |

Also uncommitted in the parent: a stray untracked `viz/viz-mech-lifecycle-*.png`,
deliberately left alone as unrelated.

---

## 6. The big unfinished feature: user-facing recovery

This is the largest piece of remaining work and it is **not** a bug — it is a
designed-but-unbuilt UX. `chatgpt-proxy/BACKLOG.md` §"User-facing recovery and safe
model-switch fallback" is the authoritative spec, including its acceptance criteria.
In short:

Today, when a prompt may have reached ChatGPT before cancellation or an extraction
failure, OpenCode shows the user an internal instruction to hand-craft an HTTP body
(`POST /v1/sessions/recover`, then `/v1/sessions/resync` with a reviewed transcript).
Users are expected to do protocol work in the middle of a task. That is the gap.

What it needs:

- Pre-submission failures auto-retry or fall back. Post-submission failures first
  attempt read-only recovery of the exact original turn and return a validated
  cached response with stable call IDs.
- Completed-but-invalid output with no delivered calls: reject once, then allow a
  safe new-user boundary. A still-generating turn stays tracked, never duplicated.
- Genuinely unverifiable: surface an explicit recovery action that explains *what* is
  unknown and offers continuation from reviewed completed history in a fresh chat.
  Resync is a deliberate abandonment boundary, never proof the old turn succeeded.
- Account for already-delivered/executed calls and results before abandoning. Never
  rerun historical tools, never execute an abandoned late response, never blindly
  clear `Dirty`. Define late-completion, cancellation and parent/child behaviour.
- Keep the raw endpoints for diagnostics, not as the normal user workflow.

**Do not claim model switching is seamless.** Switching to another model does not
settle a pending browser turn, and that limitation is the entire reason this item
exists.

---

## 7. Smaller open items

Ordered roughly by user impact.

- **Ordinary output images.** ChatGPT answers containing `<img>` still fall back to
  `Image: no accessible description; visual content not extracted`. Inline SVG→PNG
  rasterization (`557717a`) and Gemini image output are handled; this is separate.
  Needs a decision on whether to add OCR / model-based descriptions, which is an
  opt-in cost question, not a formatting fix.
- **Real visual/math rendering.** DOM→Markdown extraction and math normalization are
  implemented and fixture-verified, but visual verification in both OpenCode hosts
  is outstanding. The backlog entry notes deployment was deferred at the time because
  the shared readiness probe returned 503 — which, per item 1, is still the case.
- **General/Explore reproduction.** The original correlated failure (`ses_ed530eb9…`,
  General + Explore, 409/recover loop, 17m54s) was never fully reproduced. The
  parent/child identity precedence bug *was* found and fixed, and a real harmless
  subagent flow was verified live. The reported UI symptom itself remains
  uncorrelated — the shared session ID shown in the UI was absent from the local
  adapter log, which is unexplained.
- **Elapsed-time attribution.** 3-minute and 17-minute UI durations are not fully
  attributable to explicit request phases; queueing, recovery and client
  retry/backoff all contribute and the split was never measured.
- **HTTP transport has no auth** (pre-existing, carried from the old handoff). Safe
  for `stdio`; anything bound to a network needs a token. Currently loopback-only.
- **`BACKLOG.md` is 798 lines** and reads as an append-only decision log where each
  section re-states invariants already proven elsewhere. Several "uncommitted"
  statuses are now wrong — the work landed in `85c497e` and `ed697a0`. Worth
  compressing to current-state-plus-open-questions before it becomes unreadable.

---

## Invariants — do not break these

These are the reason this adapter is safe to point at a browser with your real
account. Every one of them was paid for with a specific incident.

1. **Never blindly clear `Dirty`.** It is the only guard against a duplicate browser
   turn and duplicate tool execution.
2. **Never resubmit an uncertain turn**, and never rerun historical tool calls. A
   consistent sent-log hash means a pending record exists; it does **not** mean the
   response was delivered.
3. **Never let ChatGPT attach to Gemini's CDP port** (or vice versa). Separate Chrome
   processes and profiles are intentional, for account and lock isolation.
4. **Never bypass loopd scope enforcement.** The `patch`/`path` fix made a legitimate
   tool work; it did not widen what a goal may write.
5. **Deploy only at an idle boundary**, under both the profile guard and the journal
   owner lock, and confirm the journal hash is unchanged across the restart. Never
   kill an active generation to install.
6. **Selectors are pinned to the current chatgpt.com DOM.** They break when OpenAI
   ships UI changes; the concentration point is `internal/chatgpt/chatgpt.go`.
7. **No prompt text, tool arguments, call IDs or media in diagnostic logs** — sizes,
   hashes, phases and shapes only.

---

## Method notes for whoever picks this up

- **The evidence bar in this repo is deliberately high**, and the commit messages
  say so explicitly. A green check that cannot falsify anything is worse than no
  check: a real-host attempt at verifying the lane plugin used
  `opencode models --standalone`, which accepted a *deliberately broken* plugin
  without reporting it — so that harness was discarded rather than reported as
  passing. If you cannot make a check able to fail, say so instead of claiming it.
- **No new bridge test files.** Extend existing ones; the project has repeatedly
  noted this constraint in backlog entries.
- **Read the whole `BACKLOG.md` section for an item before working on it.** Each
  records the exact reproduction, what was disproved, and what the fix does *not*
  claim. Several apparent regressions are already-explained earlier findings.
- **Prefer read-only inspection over speculative implementation.** The history here
  contains at least one speculative unescaping implementation that was built and then
  removed after the real evidence showed it unnecessary and unsafe.