# Bridge task backlog

Source of truth for outstanding bridge work. Each item becomes a loopd goal
when started. Updated 2026-10-30.

## Active

- [ ] **meta.ai bridge** (goal `meta-proxy-bridge`, running) — same
  Web-bridge pattern for `https://www.meta.ai/`. Profile `~/.meta-proxy`
  (logged in), Chrome via launchd `com.meta-proxy.chrome` on CDP 9224,
  adapter port 8791. Phase 1: transport probe (anonymous `/` serves a JS
  `__rd_verify` challenge; no projects per user, gemini-like thread model).
  Phase 2: build meta-proxy/ module, owner integrates.
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
