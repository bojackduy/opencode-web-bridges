---
description: Resolve this chat's unanswered turn — inspect what the bridge knows, read the previous reply back without resending, or deliberately clear the uncertainty.
---

Resolve this chat's unanswered turn using the `chatgpt-recover` tool.

Default to `action: "inspect"`: report what the state means, what is unknown,
what was kept, and which actions are available. Do not dump raw JSON and do
not mention endpoints, HTTP verbs or status codes in the summary.

If the report offers `recover`, run `action: "recover"` — it only reads the
previous reply back from the saved conversation; nothing is sent to ChatGPT
and no tool is run.

Only run `action: "abandon"` when the user explicitly asks to clear the chat,
and pass the chat id it names. Never pick the id for them, never clear a chat
that was not named, and always state that the cleared turn's historical tools
will not be re-run and that clearing is not proof the old reply arrived.
