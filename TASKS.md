# Bridge task backlog

Source of truth for outstanding bridge work. Each item becomes a loopd goal
when started. Updated 2026-10-30.

## Active

- [x] **meta.ai bridge** (HARDENED + REDEPLOYED 2026-10-31) — adapter on
  8791, Chrome on CDP 9224, `meta-web` registered. Streamed turn verified:
  role→content→finish_reason:stop→DONE, single submit, 14.6s. Retry-storm
  root cause (no SSE) fixed along with tool-call wiring, chunk budgets,
  reseed + compaction-append, adopt-first, honest abandon/recover. 2026-10-31
  follow-up: Lexical bulk insert via CDP Input.insertText (SendKeys and
  execCommand both wrong for this editor), -max-chars 500000 mirroring
  gemini (205KB tool catalog exceeds the 200KB default), pasteCap rule
  (fixed teaching head excluded from the budget — proven live with a
  207KB catalog), redeployed. Teaching destacks per-tool ahead of the
  question (never split mid-tool) — proven live with the 207KB catalog.
- [ ] **DeepSeek tool-call crash fix** (goal `deepseek-toolcrash-fix`,
  running) — reproduce on current build, fix at verified root, owner
  integrates (commit/push/deploy).
- [ ] **Gemini batchexecute spike** (goal `gemini-batchexecute-spike`,
  running, read-only) — verdict + scratch PoC in /tmp/gemini-batch-spike,
  no repo edits.

## Proposed (awaiting go)

- [ ] **DeepSeek tool-call crash fix** — malformed wrappers crash the most
  reliable transport. Repair-path work; cheapest win on the board.
- [ ] **Gemini batchexecute direct-transport spike** — gate is open (clean
  405, no challenge), logged-in cookies exist. Transport rewrite +
  maintenance treadmill. Attemptable, not started.
- [ ] **Append cap per thread** — after N compaction-appends force a clean
  reseed so stale layers can't stack. Unapproved.

## Blocked (external)

- [ ] **ChatGPT Cloudflare challenge** — `cf-mitigated: challenge` confirmed
  at the API edge from this network. Needs manual solve + cooldown. No code
  fix available; browser stays.

## Done (recent, for context)

- DeepSeek append budget gate (oversized tool-result appends promote to
  reseed instead of 422→409 wedge) — integrated, deploying.
- Tiered adoption identity (exact/key/window) + `POST /v1/sessions/recover`
  on the Gemini adapter — deployed, verified live.
- Compaction-append (stay on thread at ≥90% prefix) — deployed, firing live.
- Dirty adopt-first (30s bounded adoption before reseed) — deployed.
- Rejection reason codes, paste caps, shadow window correlation (ChatGPT).
