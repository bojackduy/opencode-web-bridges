# Shared core (mono-repo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extract one shared Go core used by all four bridges so a fix lands everywhere in a single atomic commit.

**Architecture:** Absorb the three submodules in-tree (history intact, paths unchanged), add a dependency-free `bridgecore/` Go module adopted via `replace` directives, port pure logic into it package by package, migrate bridges one by one with live smoke gates, delete old copies last.

**Tech Stack:** Go (1.27 toolchain per `go version`), git subtree, launchd services (untouched), chromedp-based browser clients (untouched).

**Spec:** Approved design in chat 2026-10-31 (option B mono-repo): core holds wrapper scan/validate, chunk/budget, reseed classify+carry, recovery predicates, SSE writer, stable IDs, Markdown converter. Browser clients, journals, flags, plists, ports stay per-bridge. Core must never grow a per-provider branch.

## Global Constraints

- No commit/push/deploy/restart by the worker; owner integrates and deploys.
- Logs stay content-free: sizes, counts, flags, durations, reason codes only.
- Never weaken tool-protocol validation; rendered args are never source.
- `git diff --check` clean at every commit; full `go build/vet/test` green per touched module.
- Launchd plist binary paths, CDP ports (9222/9223/9224), adapter ports (8788/8789/8790/8791), journal paths: all unchanged (files never move, only git metadata changes).
- Commits authored `Bojackduy <trinchanduybojack@gmail.com>`; no force-push.
- On semantic disagreement between copies, ChatGPT's version wins unless live evidence says otherwise, and the difference gets a test, not a comment.
- `report_goal_progress({summary, next, evidence})` — all three args required every call.

## Review Focus

- Submodule absorption silently dropping history (`git log --follow` must show it) — Task 1 pins it.
- `replace` pointing at a wrong relative path breaks standalone builds — Tasks 1–4 pin per-module builds.
- Core importing a bridge (cycle) — Task 5 pins the dependency rule.
- Wrapper JSON altered by Markdown conversion — Task 9 pins byte-identity.
- A migrated bridge regressing live behavior the suite can't see — Tasks 10–13 pin live smokes.

---

### Task 1: Absorb chatgpt-proxy (no code change)

**Files:**
- Modify: `.gitmodules`, `chatgpt-proxy` (gitlink → real tree)
- Test: shell history check + `go build/vet/test` in `chatgpt-proxy/`

**Interfaces:**
- Consumes: remote `Duyyy123:bojackduy/chatgpt-proxy.git`, branch `main`
- Produces: in-tree `chatgpt-proxy/` with history, builds identical

- [ ] **Step 1: Record pre-absorption baseline**

Run: `cd chatgpt-proxy && go build ./... && go vet ./... && go test -count=1 ./... 2>&1 | tail -3` and `git log --oneline -1`
Expected: green suite; note the HEAD hash for comparison

- [ ] **Step 2: Remove the submodule binding (files stay on disk)**

Run:
```bash
git submodule deinit -f chatgpt-proxy
git rm --cached chatgpt-proxy
rm -rf .git/modules/chatgpt-proxy
```
Expected: `git status --short` shows `D chatgpt-proxy` (gitlink) + `M .gitmodules`, working files untouched

- [ ] **Step 3: Absorb history via subtree**

Run: `git subtree add --prefix=chatgpt-proxy Duyyy123:bojackduy/chatgpt-proxy.git main --squash` — NO `--squash` if full history is wanted; default (full history) is required here
Expected: `chatgpt-proxy/` populated with history

- [ ] **Step 4: Verify history + builds**

Run: `git log --follow --oneline -3 -- chatgpt-proxy/go.mod` (shows pre-absorption commits) and repeat the Step 1 suite
Expected: history present, suite green identically

- [ ] **Step 5: Commit**

```bash
git add .gitmodules chatgpt-proxy && git diff --check && git -c user.name=Bojackduy -c user.email=trinchanduybojack@gmail.com commit -m "chore: absorb chatgpt-proxy in-tree (history intact, no code change)"
```

### Task 2: Absorb gemini-proxy (no code change)

Same five steps as Task 1 with `s/chatgpt-proxy/gemini-proxy/g` throughout. Baseline suite: `cd gemini-proxy && go build ./... && go vet ./... && go test -count=1 ./...`.

### Task 3: Absorb deepseek-proxy (no code change)

Same five steps as Task 1 with `s/chatgpt-proxy/deepseek-proxy/g` throughout. (`meta-proxy/` is already in-tree; nothing to absorb.)

### Task 4: Create bridgecore module + wire replace directives

**Files:**
- Create: `bridgecore/go.mod` (module `github.com/bojackduy/bridgecore`, go 1.27), `bridgecore/doc.go` (package purpose + the no-provider-branch rule)
- Modify: `chatgpt-proxy/go.mod`, `gemini-proxy/go.mod`, `deepseek-proxy/go.mod`, `meta-proxy/go.mod` (append `require github.com/bojackduy/bridgecore v0.0.0` + `replace github.com/bojackduy/bridgecore => ../bridgecore`)
- Test: per-module builds

**Interfaces:**
- Consumes: nothing (empty module with doc)
- Produces: `github.com/bojackduy/bridgecore` resolvable from all four proxies

- [ ] **Step 1: Create the module**

Run: `mkdir -p bridgecore && (cd bridgecore && go mod init github.com/bojackduy/bridgecore)`
Expected: `bridgecore/go.mod` exists

- [ ] **Step 2: Write doc.go stating the rule**

Content: package doc — shared pure logic only; importing any `*-proxy` module from core is forbidden; per-provider behavior stays per provider.

- [ ] **Step 3: Wire replace directives in all four go.mods, then verify each module still builds standalone**

Run: for each of the four proxy dirs, `go build ./...`
Expected: all green (replace target exists but unused — no behavior change)

- [ ] **Step 4: Commit**

```bash
git add bridgecore chatgpt-proxy/go.mod gemini-proxy/go.mod deepseek-proxy/go.mod meta-proxy/go.mod && git -c user.name=Bojackduy -c user.email=trinchanduybojack@gmail.com commit -m "chore: add bridgecore module, wired via replace (unused)"
```

### Task 5: Port wrapper scan/validate into core

**Files:**
- Create: `bridgecore/protocol.go`, `bridgecore/protocol_test.go`
- Modify: none (bridges switch in Tasks 10–13)

**Interfaces:**
- Consumes: the four existing implementations — enumerate exact names first: `ExtractExecutableCalls` (+ `ParseBridgeCall`, `ValidateBridgeCall`, `StripFences`, `BridgeWrappers`, fence/incomplete regexes) in `meta-proxy/internal/meta/bridge.go`, `deepseek-proxy` tools file, gemini response/parse file, chatgpt `parseAnswer` path
- Produces: `bridgecore.ParseBridgeCall(payload string) (*BridgeCall, error)`, `bridgecore.ExtractExecutableCalls(text string, schemas map[string]*jsonschema.Schema, toolChoice string) ([]*BridgeCall, error)` — exact signatures; `BridgeCall{Name string, Arguments map[string]any}`

- [ ] **Step 1: Enumerate the four copies**

Run: grep for `func ExtractExecutableCalls|func ParseBridgeCall|func ValidateBridgeCall|toolBlock|toolMarker|incompleteBridgeRe` across the four proxies, record file:line per copy
Expected: a 4-row table; any semantic difference noted with the ChatGPT-wins rule applied

- [ ] **Step 2: Write failing-first port tests in `bridgecore/protocol_test.go`**

Cases (verbatim, from live evidence): pure wrapper returns the call; prose with no wrapper returns (nil,nil) on auto; incomplete opener errors; fenced wrapper errors (never strips-into-acceptance — meta's StripFences-as-prose behavior is rejected: fences fail closed); prose-mixed wrapper errors; overlong/empty shapes per existing suites.
Expected: FAIL (package empty)

- [ ] **Step 3: Implement `bridgecore/protocol.go`** (needs `github.com/santhosh-tekuri/jsonschema/v6` — add to bridgecore/go.mod via `go get` inside `bridgecore/`)

- [ ] **Step 4: Run `cd bridgecore && go test -count=1 ./...`** — Expected: PASS. Then cross-check: for each bridge, run its existing protocol tests unmodified — Expected: still green (nothing switched yet)

- [ ] **Step 5: Commit** (`feat(core): wrapper scan/validate`)

### Task 6: Port chunk/budget helpers into core

**Files:**
- Create/extend: `bridgecore/chunk.go`, `bridgecore/chunk_test.go`
- Produces: `bridgecore.SplitText(text string, limit int) []string`, `bridgecore.ChunkTexts(texts []string, limit int) (groups [][]string, ends []int)`, `bridgecore.PasteCap(limit, headBytes int, replay bool) int`

Same TDD shape as Task 5: enumerate (`splitMetaText`/`chunkMetaTexts`/gemini `chunkGeminiTexts`/`pasteCap`/chatgpt equivalents), note the newline-fallback-cut rule (`LastIndex \n`, else hard cut) as required behavior with a test, port, green, commit.

### Task 7: Port reseed classify + carry into core

**Files:**
- Create/extend: `bridgecore/reseed.go`, `bridgecore/reseed_test.go`
- Produces: `bridgecore.FirstMismatch`, `ClassifyHistoryRewrite` (+ the five kind constants), `CarryExecuted`, `EchoBloatSkipped`, `SystemOnlyDivergence`, `CoherentTranscript`, `IsCompactionShape` — message type stays per-bridge; core defines `bridgecore.Message{Role, Content any, Name, ToolCalls []ToolCall, ToolCallID string}` and each bridge converts at its boundary (one small adapter func per bridge, written in Tasks 10–13, NOT here)

Note: the `Message` struct decision is deliberate — sharing classification logic requires a shared shape; converters are trivial and tested per bridge later.

### Task 8: Port SSE writer + IDs + Markdown converter into core

**Files:**
- Create/extend: `bridgecore/sse.go` (`WriteCompletion`, `WriteSSEChunk`, DONE frame — exact wire shapes from gemini `writeChatResponse`), `bridgecore/markdown.go` (meta's converter becomes canonical; gemini adopts it in Task 11) and `bridgecore/ids.go` (stable call-ID builders)
- Recovery *predicates* stay per-bridge (each wraps its own sentinels); the rule "proven-unsent clears pending" is already a Global Constraint, not shared code
- Tests per file, TDD as above

### Task 9: Migrate meta-proxy onto core

**Files:**
- Modify: `meta-proxy/internal/meta/bridge.go` (delete, re-export or remove), `meta-proxy/cmd/meta-openai/handler.go` (import swap + `Message` converter), `meta-proxy/go.mod` (drop jsonschema if now unused directly — verify with `go mod tidy` semantics per module, no Sinne)
- Test: `go build/vet/test` in `meta-proxy/` + bridge unit tests unmodified and green

- [ ] **Step 1: Swap imports file by file** (`meta-proxy/internal/meta` → `bridgecore`), adding the 10-line `Message` converter
- [ ] **Step 2: Delete the old copies only in meta-proxy**
- [ ] **Step 3: Full suite green**
- [ ] **Step 4: Live smoke** (fresh session turn + streamed turn via owner-run curl — worker records the commands, owner executes since deploy is owner-side; gate the task on owner-confirmed output)
- [ ] **Step 5: Commit** (`refactor(meta): run on bridgecore`)

### Task 10: Migrate deepseek-proxy onto core

Same shape as Task 9 (import swap, converter, delete old, suite green, owner live smoke, commit). DeepSeek's direct-HTTP client is untouched — only protocol/chunk/validation move.

### Task 11: Migrate gemini-proxy onto core (adopt canonical Markdown converter here)

Same shape as Task 9, plus: replace gemini's `htmlToMarkdown` family with core's (meta's copy is canonical per plan) — old tests must pass unmodified against the canonical implementation; any divergence resolved per ChatGPT-wins rule with a new test. Owner live smoke + deploy gate.

### Task 12: Migrate chatgpt-proxy onto core

Same shape as Task 9, last for a reason: its copies are the semantic reference. Any port disagreement found here reopens the relevant core file with a regression test — no silent conformity.

### Task 13: Final pass + delete stragglers

- [ ] **Step 1:** grep the four proxies for remaining duplicated symbols (`func ExtractExecutableCalls|func chunkMetaTexts|func htmlToMarkdown|func pasteCap` outside `bridgecore/`) — Expected: zero hits
- [ ] **Step 2:** `go build/vet/test` in all five modules — Expected: green
- [ ] **Step 3:** `git diff --check` clean — then commit (`chore: shared-core migration complete`)
