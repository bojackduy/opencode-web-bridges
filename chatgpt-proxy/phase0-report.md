# Phase-0 Retrieval Probe Report — "Project-per-session" gate

VERDICT: HIT

Question: can a fresh chat in a ChatGPT project retrieve a fact stored in
another chat of the SAME project? Answer: yes — both the no-card chat (B)
and the with-card chat (C) returned the codeword verbatim.

- Date: 2026-10-10
- Probe project: "OpenCode Probe" (freshly created by the probe, reused=false)
  https://chatgpt.com/g/g-p-6aca33a2f7548191b2eb9ddd310f5109/project
- Codeword: PROBE-88 (fresh; the earlier PROBE-77 contamination in the shared
  "OpenCode Bridge" project was not touched)
- Method: throwaway Go program driving internal/chatgpt.Client directly on a
  dedicated tab (deleted after the run); live :8788 adapter stopped during
  the probe, restarted after. Shared project and all pre-existing threads
  untouched.

## Chat A — store (new chat in probe project)

URL: https://chatgpt.com/g/g-p-6aca33a2f7548191b2eb9ddd310f5109-opencode-probe/c/6aca33ad-7fd4-83ec-ade1-1e1784e14435

Q: `Remember the codeword PROBE-88. It is important. Reply OK.`

A (verbatim): `OK.`

Latency: ~34s (prompt submit to stable answer)

## Chat B — retrieve, NO context card (another new chat, same project)

URL: https://chatgpt.com/g/g-p-6aca33a2f7548191b2eb9ddd310f5109-opencode-probe/c/6aca33cf-ed78-83ec-a775-15fb07b69da0

Q: `What is the codeword I asked you to remember? Reply with just the codeword.`

A (verbatim): `PROBE-88`

Latency: ~34s

## Chat C — retrieve, WITH context card (another new chat, same project)

URL: https://chatgpt.com/g/g-p-6aca33a2f7548191b2eb9ddd310f5109-opencode-probe/c/6aca33f0-cff4-83ec-a8a0-a2dff64903be

Q (verbatim, card contains no codeword):

```
Context card (working notes for an unrelated task):
- Task: plan weekend hike route
- Buy list: socks, batteries
- Meeting moved to Thursday

Question: What is the codeword I asked you to remember? Reply with just the codeword.
```

A (verbatim): `PROBE-88`

Latency: ~29s

## Evidence of HIT

- Chat B returned `PROBE-88` with no context card and no shared history.
- Chat C returned `PROBE-88` despite its own unrelated context card.
- All three thread URLs sit under the same fresh project
  (g-p-6aca33a2f7548191b2eb9ddd310f5109).

## Latency notes

- Project creation via the generalized ensureProject dialog flow: a few
  seconds (navigate ~3s + dialog + poll).
- New-chat-in-project navigation: ~6s each (navigate + composer ready).
- Answers: 29–34s each for these short prompts (well under the 20–90s
  budget). Total probe wall time ~2min for 3 chats.
- Note: the live adapter's Chrome was found dead after bootout (launchd
  killed the process group, port 9222 refused), so the probe launched its
  own Chrome on the same profile — login cookies intact
  (loggedIn=true on first Status check).
