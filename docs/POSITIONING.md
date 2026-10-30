# Product Positioning — opencode-web-bridges

> Evidence-based. Assumptions labeled. No fabricated metrics.

## Product

**opencode-web-bridges** — an OpenCode plugin that registers ChatGPT Web, Gemini Web,
and DeepSeek Web as ordinary OpenCode models. Each uses your own logged-in browser
session instead of an API key. ChatGPT and Gemini are driven over Chrome DevTools
Protocol (CDP) against a dedicated profile; DeepSeek speaks the web API directly
(no browser). Ships an installer (`npx @bojackduy/opencode-web-bridges install`) that
detects OpenCode v1/v2, downloads checksum-verified bridge binaries, and writes
config without clobbering existing entries.

- Repo: https://github.com/bojackduy/opencode-web-bridges (public, AGPL-3.0-or-later)
- npm: `@bojackduy/opencode-web-bridges` (not yet published)
- Version: 0.1.0

## Problem

OpenCode models require provider API keys and paid per-token billing. Many
developers already pay for ChatGPT Plus/Pro or Gemini subscriptions and would
rather reuse that allowance inside OpenCode. Existing "chatgpt web" bridges are
usually single-engine, fragile, and lack recovery when a turn is interrupted.

## Primary audience

OpenCode users (developers) who already hold a ChatGPT Plus/Pro or Gemini
subscription and want to use it as a model inside OpenCode without a separate API
bill. Secondary: developers curious about browser-bridged models, and
OpenCode-plugin authors studying the v1/v2 dual-host pattern.

## Pain points

- Paying twice (subscription + API) for the same model family.
- Manual provider/MCP wiring is error-prone.
- Interrupted turns leave uncertain state; re-running re-submits prompts.
- No recovery UX when a browser turn is lost.

## Value proposition

Use the ChatGPT/Gemini/DeepSeek you already pay for, inside OpenCode, with
one-line install, checksum-verified binaries, and fail-closed recovery that never
re-submits a prompt.

## Differentiators

- Three engines, one plugin: ChatGPT (projects per session), Gemini (text-only),
  DeepSeek (no browser, HTTP+SSE).
- Fail-closed recovery: exact/key/window bubble identity, dirty adopt-first,
  hash-lineage auto-resync, compaction-append — never re-submits on retry.
- OpenCode v1 and v2 from one package (server hook vs setup domains).
- Checksum-verified bridge downloads; never clobbers existing config.
- Content-free logs (sizes, counts, reason codes only).

## Competitors (assumption-labeled)

- Direct API providers (OpenAI, Anthropic, Google) — require API keys/billing;
  higher cost, but official and stable. We do not claim parity on reliability.
- Other ChatGPT-web bridges (community) — usually single engine, no recovery UX.
  (Specific projects not enumerated; no fabricated comparisons.)

## Monetization (assumption)

Free/open-source under AGPL-3.0. No paid tier invented. Realistic sustainability
options: GitHub Sponsors, optional hosted/managed tier later (not built). Do not
promise a paid product that does not exist.

## Recommended channels

- GitHub (topics set, README, releases) — primary discovery for devs.
- npm (first publish pending) — install path.
- OpenCode community (Discord/GitHub discussions) — targeted, not spam.
- Reddit: r/opencode (if exists), r/LocalLLaMA, r/ChatGPT — only with genuine
  technical write-ups, not promo.
- Hacker News "Show HN" — after npm publish + first release land.
- X / LinkedIn — launch thread with real demo GIF.

## Assumptions

- Audience is developers already using OpenCode (small but growing).
- No usage numbers, testimonials, or benchmarks exist; none invented.
- npm package not published; chatgpt-proxy has no GitHub release yet (both block
  prebuilt install). These are the top two growth blockers.
