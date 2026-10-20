// The user-facing recovery tool: session resolution, the wording of every
// adapter state, and the plain-English translation of every refusal. Everything
// runs against a fake adapter so no browser, journal or network is involved.

import { describe, expect, test } from "bun:test"
import { resolveOptions } from "../src/server/options"
import {
  formatAbandon,
  formatRecoverInspect,
  formatRecovered,
  recoverReport,
  resolveRecoverSession,
  type AdapterRequest,
} from "../src/server/tools"

const OPTS = resolveOptions({})
const CALLING = "ses_calling"

interface Call {
  method: string
  reqPath: string
  session: string
  body: unknown
  timeoutMs: number | undefined
}

/** One canned adapter answer; records what the tool actually sent. */
function fakeAdapter(status: number, body: unknown, calls: Call[] = []): AdapterRequest {
  return async (method, reqPath, session, body2, timeoutMs) => {
    calls.push({ method, reqPath, session, body: body2, timeoutMs })
    return { status, text: typeof body === "string" ? body : JSON.stringify(body) }
  }
}

function problemOf(result: ReturnType<typeof resolveRecoverSession>): string {
  if (!("problem" in result)) throw new Error(`expected a problem, got session ${result.session}`)
  return result.problem
}

// ─── Session resolution ──────────────────────────────────────────────────────

describe("which chat a call acts on", () => {
  test("the calling session is the default and an explicitly named session overrides it", () => {
    expect(resolveRecoverSession("inspect", CALLING, undefined)).toEqual({ session: CALLING })
    expect(resolveRecoverSession("recover", CALLING, undefined)).toEqual({ session: CALLING })
    expect(resolveRecoverSession("inspect", CALLING, "ses_named")).toEqual({ session: "ses_named" })
    expect(resolveRecoverSession("recover", "  ", "ses_named")).toEqual({ session: "ses_named" })
  })

  test("neither a calling session nor an explicit one fails with an actionable message", () => {
    for (const action of ["inspect", "recover"] as const) {
      const message = problemOf(resolveRecoverSession(action, undefined, undefined))
      expect(message).toContain("does not know")
      expect(message).toContain("optional session argument")
    }
    expect(problemOf(resolveRecoverSession("inspect", "   ", ""))).toContain("does not know")
  })

  test("clearing always needs the chat named in the call, never merely the current chat", () => {
    const message = problemOf(resolveRecoverSession("abandon", CALLING, undefined))
    expect(message).toContain("named explicitly")
    expect(message).not.toContain(CALLING)
    expect(resolveRecoverSession("abandon", CALLING, "ses_named")).toEqual({ session: "ses_named" })
  })

  test("wildcards and all/any are refused instead of resolved", () => {
    for (const bad of ["ses_*", "*", "all", "ANY"]) {
      expect(problemOf(resolveRecoverSession("inspect", CALLING, bad))).toContain("exactly")
    }
  })
})

// ─── Inspect wording, one per adapter state ─────────────────────────────────

const INSPECT_CASES = [
  {
    state: "clean",
    summary: "This chat is up to date; the next message will be answered normally.",
    unknown: "Nothing — the last reply was received and checked",
    actions: ["continue"],
    action: "just send your next message",
  },
  {
    state: "pending_verifiable",
    summary: "A previous reply could not be confirmed, but it can still be read back without sending anything again.",
    unknown: "Whether the last message you sent was ever answered",
    actions: ["recover", "resync", "abandon"],
    action: "(action 'recover')",
  },
  {
    state: "pending_unverifiable",
    summary: "A previous reply could not be confirmed and cannot be read back; this chat has to be explicitly cleared before it can be used again.",
    unknown: "cannot be read back",
    actions: ["abandon"],
    action: "(action 'abandon', naming this chat)",
  },
  {
    state: "rejected_output",
    summary: "The model's last reply could not be used as a tool call, and nothing was run; a new message will continue.",
    unknown: "Nothing about delivery",
    actions: ["continue"],
    action: "just send your next message",
  },
  {
    state: "empty_final",
    summary: "The model's last reply was empty and nothing was run; a new message will continue.",
    unknown: "Nothing about delivery",
    actions: ["continue"],
    action: "just send your next message",
  },
  {
    state: "unknown_session",
    summary: "No stored chat exists for this session yet.",
    unknown: "Everything",
    actions: [],
    action: "nothing right now",
  },
] as const

describe("inspect explains every state in plain English", () => {
  for (const c of INSPECT_CASES) {
    test(`${c.state}`, () => {
      const out = formatRecoverInspect("ses_1", {
        state: c.state,
        summary: c.summary,
        actions: [...c.actions],
        completed_messages: 0,
        pending_prompt_bytes: 0,
        outstanding_tool_calls: 0,
        thread_url_present: false,
      })
      expect(out).toContain(`What this means: ${c.summary}`)
      expect(out).toContain("What is unknown: ")
      expect(out).toContain(c.unknown)
      expect(out).toContain("What you can do: ")
      expect(out).toContain(c.action)
      expect(out).toContain("What is kept: nothing has been recorded for this chat yet.")
      // No endpoint names, no raw JSON, no runbook.
      expect(out).not.toContain("/v1/")
      expect(out).not.toContain("{")
      expect(out.toLowerCase()).not.toContain("runbook")
    })
  }

  test("reports what was kept, including calls that were handed out but never answered", () => {
    const out = formatRecoverInspect("ses_1", {
      state: "pending_unverifiable",
      summary: "cannot be read back",
      actions: ["abandon"],
      completed_messages: 12,
      pending_prompt_bytes: 987,
      outstanding_tool_calls: 2,
      thread_url_present: true,
    })
    expect(out).toContain("What is kept: 12 completed messages, the link to its ChatGPT conversation")
    expect(out).toContain("the exact text of the unanswered message (987 bytes)")
    expect(out).toContain("2 tool calls handed out with no recorded result")
    expect(out).toContain("are never re-run")
  })

  test("an action this plugin does not know is reported, not guessed", () => {
    const out = formatRecoverInspect("ses_1", { state: "something_new", actions: ["teleport"] })
    expect(out).toContain("does not recognise")
    expect(out).toContain("an action this plugin does not recognise (teleport)")
  })
})

// ─── Recover ────────────────────────────────────────────────────────────────

describe("recover", () => {
  test("a bound reply is reported as bound, without claiming anything was resent", async () => {
    const calls: Call[] = []
    const out = await recoverReport(
      OPTS,
      { action: "recover" },
      "ses_1",
      fakeAdapter(200, { recovered: true, session: "ses_1", cached_tool_calls: 2, response_bytes: 512 }, calls),
    )
    expect(out).toContain("Recovered: the previous reply was read back")
    expect(out).toContain("Nothing was sent again and no tool was run")
    expect(out).toContain("512 bytes of reply text and 2 tool calls")
    expect(out).toContain("send the same message again, unchanged")
    expect(calls).toEqual([
      { method: "POST", reqPath: "/v1/sessions/recover", session: "ses_1", body: undefined, timeoutMs: 120_000 },
    ])
  })

  test("a reply rebuilt from recorded history says so instead of counting bytes", async () => {
    const out = formatRecovered("ses_1", { recovered: true, cached_tool_calls: 3, auto_resync: "hash-lineage" })
    expect(out).toContain("matched to this chat from the recorded history")
    expect(out).not.toContain("bytes of reply text")
  })

  test("nothing verifiable is said plainly, with no resend and an actionable next step", async () => {
    const out = await recoverReport(
      OPTS,
      { action: "recover" },
      "ses_1",
      fakeAdapter(409, { error: { message: "no uncertain stored thread for this session" } }),
    )
    const [headline, next] = out.split("\n")
    expect(headline).toContain("could not match the unanswered message to a saved reply")
    expect(next).toContain("Nothing was re-sent and no tool was run")
    expect(next).toContain("action 'inspect'")
    expect(headline).not.toContain("409")
    expect(out).toContain("status 409")
  })
})

// ─── Abandon ────────────────────────────────────────────────────────────────

describe("abandon", () => {
  test("refuses to clear anything without a session named in the call", async () => {
    const calls: Call[] = []
    const out = await recoverReport(OPTS, { action: "abandon" }, CALLING, fakeAdapter(200, {}, calls))
    expect(out).toContain("Nothing was cleared")
    expect(out).toContain("named explicitly")
    expect(calls).toEqual([])
  })

  test("names exactly one session and reports what was kept, what was dropped and what is not re-run", async () => {
    const calls: Call[] = []
    const out = await recoverReport(
      OPTS,
      { action: "abandon", session: "ses_named", reason: "operator decision" },
      CALLING,
      fakeAdapter(
        200,
        {
          requested: 1,
          abandoned: 1,
          results: [
            {
              session: "ses_named",
              outcome: "abandoned",
              state_before: "pending_unverifiable",
              delivered_calls: 4,
              recorded_results: 3,
              outstanding_calls: 1,
              abandoned_prompt_bytes: 1215,
            },
          ],
        },
        calls,
      ),
    )
    expect(calls).toEqual([
      {
        method: "POST",
        reqPath: "/v1/sessions/abandon",
        session: "ses_named",
        body: { sessions: ["ses_named"], reason: "operator decision" },
        timeoutMs: 20_000,
      },
    ])
    expect(out).toContain("Cleared the unconfirmed turn for ses_named")
    expect(out).toContain("Dropped: the uncertainty about one unanswered message (1215 bytes of prompt text)")
    expect(out).toContain("Kept: 4 delivered tool calls, 3 recorded results")
    expect(out).toContain("1 tool call had no recorded result and will not be executed again")
    expect(out).toContain("It is not proof the previous reply arrived")
  })

  test("a chat with nothing to clear is reported as untouched", async () => {
    expect(formatAbandon("ses_1", { results: [{ session: "ses_1", outcome: "not_pending" }] })).toContain(
      "had no unconfirmed turn",
    )
    expect(formatAbandon("ses_1", { results: [{ session: "ses_1", outcome: "unknown" }] })).toContain(
      "no stored chat for ses_1",
    )
    expect(formatAbandon("ses_1", { results: [] })).toContain("nothing to confirm was cleared")
  })
})

// ─── Refusal translation ────────────────────────────────────────────────────

const REFUSALS = [
  { status: 403, headline: "looked like it came from a web page", next: "Run it again from OpenCode" },
  { status: 400, headline: "rejected the request before changing anything", next: "Use action 'inspect'" },
  { status: 503, headline: "busy with a live reply", next: "Wait for the current reply to finish" },
  { status: 500, headline: "could not save the change", next: "Try once more" },
  { status: 405, headline: "does not support that action", next: "restart OpenCode" },
] as const

describe("adapter refusals become one plain sentence plus a next step", () => {
  for (const r of REFUSALS) {
    test(`${r.status}`, async () => {
      const out = await recoverReport(
        OPTS,
        { action: "abandon", session: "ses_x" },
        undefined,
        fakeAdapter(r.status, { error: { message: "adapter wording" } }),
      )
      const [headline, next] = out.split("\n")
      expect(headline).toContain(r.headline)
      expect(next).toContain(r.next)
      // The status, method and path are secondary detail only.
      expect(headline).not.toContain(String(r.status))
      expect(headline).not.toMatch(/POST|GET|\/v1\//)
      expect(out).toContain(`status ${r.status}`)
      expect(out).toContain("The bridge said: adapter wording")
    })
  }

  test("a 422 keeps the two classes distinct and never resends", async () => {
    const recoverOut = await recoverReport(OPTS, { action: "recover" }, "s", fakeAdapter(422, { error: { message: "x" } }))
    expect(recoverOut).toContain("The saved reply cannot be used, and nothing was re-sent and no tool was run.")
    expect(recoverOut).toContain("nothing needs clearing")
    const abandonOut = await recoverReport(
      OPTS,
      { action: "abandon", session: "s" },
      undefined,
      fakeAdapter(422, { error: { message: "x" } }),
    )
    expect(abandonOut).toContain("cannot clear this chat as it is, and nothing was changed")
  })

  test("an unreachable bridge is a report, not a thrown error", async () => {
    const out = await recoverReport(OPTS, { action: "inspect" }, "s", async () => {
      throw new Error("connect ECONNREFUSED")
    })
    expect(out).toContain("The ChatGPT bridge is not answering, so nothing was done.")
    expect(out).toContain("chatgpt-status")
    expect(out).toContain("ECONNREFUSED")
  })

  test("an unknown action is refused before any adapter call", async () => {
    const calls: Call[] = []
    const out = await recoverReport(OPTS, { action: "resync" }, "s", fakeAdapter(200, {}, calls))
    expect(out).toContain('"resync" is not a recovery action')
    expect(out).toContain("inspect, recover, abandon")
    expect(calls).toEqual([])
  })
})

// ─── Content safety ─────────────────────────────────────────────────────────

describe("no report ever carries prompt text, tool arguments or call ids", () => {
  const PROMPT_SECRET = "PROMPT-SECRET-9137"
  const ARG_SECRET = "ARG-SECRET-4412"
  const CALL_SECRET = "call_secret_1234"

  test("inspect, recover and abandon outputs are content-free", async () => {
    const outputs = [
      await recoverReport(
        OPTS,
        { action: "inspect" },
        "ses_1",
        fakeAdapter(200, {
          state: "pending_unverifiable",
          summary: "cannot be read back",
          actions: ["abandon"],
          completed_messages: 3,
          pending_prompt_bytes: PROMPT_SECRET.length,
          outstanding_tool_calls: 1,
          thread_url_present: true,
        }),
      ),
      await recoverReport(
        OPTS,
        { action: "recover" },
        "ses_1",
        fakeAdapter(200, { recovered: true, cached_tool_calls: 1, response_bytes: 40 }),
      ),
      await recoverReport(
        OPTS,
        { action: "abandon", session: "ses_1", reason: `${PROMPT_SECRET} ${ARG_SECRET}` },
        undefined,
        fakeAdapter(200, {
          results: [
            {
              session: "ses_1",
              outcome: "abandoned",
              delivered_calls: 1,
              recorded_results: 0,
              outstanding_calls: 1,
              abandoned_prompt_bytes: 10,
            },
          ],
        }),
      ),
    ]
    for (const out of outputs) {
      expect(out).not.toContain(PROMPT_SECRET)
      expect(out).not.toContain(ARG_SECRET)
      expect(out).not.toContain(CALL_SECRET)
    }
  })
})
