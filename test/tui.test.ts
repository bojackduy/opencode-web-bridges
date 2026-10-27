// Native TUI palette recovery: registration, routing, dialogs, guards.
// No assistant turn, no session.prompt, no LLM anywhere in these paths —
// fakes below intentionally lack session.prompt, so any such call throws.

import { describe, expect, test } from "bun:test"
import { resolveOptions } from "../src/server/options"
import type { AdapterRequest } from "../src/server/tools"
import {
  buildRecoveryCommands,
  CMD_ABANDON_CHATGPT,
  CMD_ABANDON_GEMINI,
  CMD_CORRECT_CHATGPT,
  CMD_INSPECT_CHATGPT,
  CMD_INSPECT_GEMINI,
  CMD_RECHECK_CHATGPT,
  CMD_RECOVER_CHATGPT,
  RECOVERY_GROUP,
  setupTui,
  type PaletteCommand,
  type RecoveryDeps,
} from "../src/server/tui"

function stateBody(over: Record<string, unknown> = {}) {
  return {
    session: "ses_1",
    state: "pending_verifiable",
    can_recover: true,
    can_resync: false,
    thread_url_present: true,
    completed_messages: 3,
    pending_prompt_bytes: 12,
    outstanding_tool_calls: 1,
    summary: "A previous reply could not be confirmed.",
    actions: ["recover", "abandon"],
    ...over,
  }
}

function fakeAdapter(handler: (method: string, path: string, body: unknown) => { status: number; text: string }) {
  const calls: Array<{ method: string; path: string; session: string; body: unknown }> = []
  const request: AdapterRequest = async (method, path, session, body) => {
    calls.push({ method, path, session, body })
    return handler(method, path, body)
  }
  return { calls, request }
}

const okState = (extra: Record<string, unknown> = {}) => ({
  status: 200,
  text: JSON.stringify(stateBody(extra)),
})

function fakeUI(
  route: { type: string; sessionID?: string } = { type: "session", sessionID: "ses_1" },
  over: Partial<{ promptId: string | undefined; confirm: boolean | undefined; status: "idle" | "running" }> = {},
) {
  const alerts: Array<{ title: string; message: string }> = []
  const confirms: string[] = []
  const prompts: string[] = []
  const toasts: string[] = []
  return {
    calls: { alerts, confirms, prompts, toasts },
    ui: {
      dialog: {
        alert: async (o: { title: string; message: string }) => void alerts.push(o),
        confirm: async (o: { title: string }) => {
          confirms.push(o.title)
          return over.confirm ?? true
        },
        prompt: async (o: { title: string }) => {
          prompts.push(o.title)
          return over.promptId ?? undefined
        },
      },
      toast: { show: (o: { message: string }) => void toasts.push(o.message) },
      router: { current: () => route },
    },
    data: {
      session: { status: (_id: string) => (over.status ?? "idle") as "idle" | "running" },
    },
  }
}

function depsFor(
  ui: ReturnType<typeof fakeUI>,
  adapters: { chatgpt?: AdapterRequest; gemini?: AdapterRequest },
): RecoveryDeps {
  return {
    ui: ui.ui as never,
    data: ui.data,
    adapters: { opts: resolveOptions({}), ...adapters },
  }
}

async function runCommand(cmds: PaletteCommand[], id: string) {
  const cmd = cmds.find((c) => c.id === id)
  if (!cmd) throw new Error(`missing command ${id}`)
  await cmd.run()
}

describe("palette registration", () => {
  test("all entries are palette commands in the recovery group", () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() => ({ status: 200, text: "{}" }))
    const cmds = buildRecoveryCommands(depsFor(fake, { chatgpt: request, gemini: request }))
    expect(cmds.map((c) => c.id).sort()).toEqual(
      [
        CMD_INSPECT_CHATGPT,
        CMD_RECOVER_CHATGPT,
        CMD_RECHECK_CHATGPT,
        CMD_CORRECT_CHATGPT,
        CMD_ABANDON_CHATGPT,
        CMD_INSPECT_GEMINI,
        CMD_ABANDON_GEMINI,
      ].sort(),
    )
    for (const c of cmds) {
      expect(c.palette).toBe(true)
      expect(c.group).toBe(RECOVERY_GROUP)
      expect(typeof c.title).toBe("string")
    }
  })

  test("without a chatgpt adapter only gemini entries exist", () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() => ({ status: 200, text: "{}" }))
    const cmds = buildRecoveryCommands(depsFor(fake, { gemini: request }))
    expect(cmds.map((c) => c.id).sort()).toEqual([CMD_ABANDON_GEMINI, CMD_INSPECT_GEMINI].sort())
  })
})

describe("route and session resolution", () => {
  test("session route uses the viewed chat without prompting", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_9" })
    const { calls, request } = fakeAdapter(() => okState())
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_INSPECT_CHATGPT)
    expect(calls).toHaveLength(1)
    expect(calls[0]?.session).toBe("ses_9")
    expect(fake.calls.prompts).toHaveLength(0)
    expect(fake.calls.alerts).toHaveLength(1)
    expect(fake.calls.alerts[0]?.message).toContain("could not be confirmed")
  })

  test("home route prompts for a chat id", async () => {
    const fake = fakeUI({ type: "home" }, { promptId: "ses_x" })
    const { calls, request } = fakeAdapter(() => okState())
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_INSPECT_CHATGPT)
    expect(fake.calls.prompts).toHaveLength(1)
    expect(calls[0]?.session).toBe("ses_x")
  })

  test("empty answer aborts with zero adapter calls", async () => {
    const fake = fakeUI({ type: "home" }, { promptId: "  " })
    const { calls, request } = fakeAdapter(() => okState())
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_INSPECT_CHATGPT)
    expect(calls).toHaveLength(0)
    expect(fake.calls.toasts.length).toBeGreaterThan(0)
  })
})

describe("guards and confirmations", () => {
  test("live turn refuses mutations with zero adapter calls", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { status: "running" })
    const { calls, request } = fakeAdapter(() => okState())
    const cmds = buildRecoveryCommands(depsFor(fake, { chatgpt: request }))
    await runCommand(cmds, CMD_RECOVER_CHATGPT)
    await runCommand(cmds, CMD_ABANDON_CHATGPT)
    expect(calls).toHaveLength(0)
    expect(fake.calls.alerts).toHaveLength(2)
  })

  test("cancelled abandon confirmation makes zero mutation calls", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: false })
    const { calls, request } = fakeAdapter((method) => {
      if (method === "POST") return { status: 200, text: "{}" }
      return okState()
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_ABANDON_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
    expect(fake.calls.toasts.join(" ")).toContain("nothing changed")
  })

  test("cancelled recover confirmation makes zero mutation calls", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: false })
    const { calls, request } = fakeAdapter((method) => {
      if (method === "POST") return { status: 200, text: "{}" }
      return okState()
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_RECOVER_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
  })

  test("confirmed abandon posts once and shows the outcome", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: true })
    const { calls, request } = fakeAdapter(() => ({
      status: 200,
      text: JSON.stringify({
        results: [
          {
            session: "ses_1",
            outcome: "abandoned",
            state_before: "pending_verifiable",
            delivered_calls: 1,
            recorded_results: 1,
            outstanding_calls: 0,
            abandoned_prompt_bytes: 12,
          },
        ],
      }),
    }))
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_ABANDON_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(1)
    expect(fake.calls.alerts.map((a) => a.message).join("\n")).toContain("Cleared the unconfirmed turn")
  })
})

describe("rejected-reply actions", () => {
  const rejectedState = (extra: Record<string, unknown> = {}) =>
    okState({
      state: "rejected_output",
      can_recover: false,
      can_recheck: true,
      can_correct_format: true,
      summary: "The model's last reply could not be used.",
      actions: ["recheck", "correct_format", "continue"],
      ...extra,
    })

  test("recheck explains itself in one line instead of dumping a state report", async () => {
    const fake = fakeUI()
    const { calls, request } = fakeAdapter((method) =>
      method === "GET" ? rejectedState({ can_recheck: false, actions: ["continue"] }) : { status: 200, text: "{}" },
    )
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_RECHECK_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
    expect(fake.calls.alerts).toHaveLength(1)
    const message = fake.calls.alerts[0]?.message ?? ""
    expect(message).toContain("no longer has its exact turn recorded")
    expect(message).not.toContain("What is kept")
    expect(message).not.toContain("completed messages")
  })

  test("an unconfirmed chat is redirected to recover, not shown a state dump", async () => {
    const fake = fakeUI()
    const { calls, request } = fakeAdapter((method) =>
      method === "GET"
        ? rejectedState({
            state: "pending_verifiable",
            can_recover: true,
            can_recheck: false,
            can_correct_format: false,
            actions: ["recover", "resync", "abandon"],
          })
        : { status: 200, text: "{}" },
    )
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_CORRECT_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
    const message = fake.calls.alerts[0]?.message ?? ""
    expect(message).toContain("not rejected")
    expect(message).toContain("recover ChatGPT turn")
    expect(message).not.toContain("What is unknown")
  })

  test("confirmed recheck posts exactly once and reports what is held", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: true })
    const { calls, request } = fakeAdapter((method) => {
      if (method === "GET") return rejectedState()
      return {
        status: 200,
        text: JSON.stringify({ rechecked: true, cached_tool_calls: 1, response_bytes: 64 }),
      }
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_RECHECK_CHATGPT)
    const posts = calls.filter((c) => c.method === "POST")
    expect(posts).toHaveLength(1)
    expect(posts[0]?.path).toBe("/v1/sessions/recheck")
    const message = fake.calls.alerts.map((a) => a.message).join("\n")
    expect(message).toContain("Checked again")
    expect(message).toContain("does not resume the chat by itself")
  })

  test("cancelled recheck makes zero mutation calls", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: false })
    const { calls, request } = fakeAdapter((method) =>
      method === "GET" ? rejectedState() : { status: 200, text: "{}" },
    )
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_RECHECK_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
    expect(fake.calls.toasts.join(" ")).toContain("nothing changed")
  })

  test("recheck refusal names the content-free reason", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: true })
    const { request } = fakeAdapter((method) => {
      if (method === "GET") return rejectedState()
      return {
        status: 422,
        text: JSON.stringify({ error: { message: "still unusable", details: { reason: "schema_violation" } } }),
      }
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_RECHECK_CHATGPT)
    const message = fake.calls.alerts.map((a) => a.message).join("\n")
    expect(message).toContain("schema_violation")
    expect(message).toContain("still unusable")
  })

  test("confirmed correction submits exactly once and promises no task resend", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: true })
    const { calls, request } = fakeAdapter((method) => {
      if (method === "GET") return rejectedState()
      return {
        status: 200,
        text: JSON.stringify({ corrected: true, cached_tool_calls: 1, response_bytes: 80 }),
      }
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_CORRECT_CHATGPT)
    const posts = calls.filter((c) => c.method === "POST")
    expect(posts).toHaveLength(1)
    expect(posts[0]?.path).toBe("/v1/sessions/correct-format")
    const message = fake.calls.alerts.map((a) => a.message).join("\n")
    expect(message).toContain("Correction sent once")
    expect(message).toContain("original task was not resent")
  })

  test("cancelled correction sends nothing", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: false })
    const { calls, request } = fakeAdapter((method) =>
      method === "GET" ? rejectedState() : { status: 200, text: "{}" },
    )
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_CORRECT_CHATGPT)
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0)
    expect(fake.calls.toasts.join(" ")).toContain("nothing was sent")
  })

  test("a spent correction is reported honestly instead of being resent", async () => {
    const fake = fakeUI({ type: "session", sessionID: "ses_1" }, { confirm: true })
    const { request } = fakeAdapter((method) => {
      if (method === "GET") return rejectedState()
      return {
        status: 409,
        text: JSON.stringify({
          error: { message: "already asked", details: { reason: "correction_already_spent" } },
        }),
      }
    })
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_CORRECT_CHATGPT)
    const message = fake.calls.alerts.map((a) => a.message).join("\n")
    expect(message).toContain("already asked")
    expect(message).toContain("will not ask a second time")
  })

  test("gemini never offers the rejected-reply actions", () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() => okState())
    const ids = buildRecoveryCommands(depsFor(fake, { gemini: request })).map((c) => c.id)
    expect(ids).not.toContain(CMD_RECHECK_CHATGPT)
    expect(ids).not.toContain(CMD_CORRECT_CHATGPT)
  })
})

describe("gemini entries", () => {
  test("inspect shows the gemini state report", async () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() =>
      okState({ state: "pending", summary: "uncertain", actions: ["reseed", "abandon"] }),
    )
    await runCommand(buildRecoveryCommands(depsFor(fake, { gemini: request })), CMD_INSPECT_GEMINI)
    expect(fake.calls.alerts).toHaveLength(1)
  })

  test("no recover entry exists for gemini", () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() => okState())
    const ids = buildRecoveryCommands(depsFor(fake, { chatgpt: request, gemini: request })).map((c) => c.id)
    expect(ids).not.toContain("bridge-recovery.recover-gemini")
  })
})

describe("dialogs carry reports and adapter errors", () => {
  test("adapter refusal surfaces in the dialog", async () => {
    const fake = fakeUI()
    const { request } = fakeAdapter(() => ({ status: 409, text: "nope" }))
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_INSPECT_CHATGPT)
    expect(fake.calls.alerts).toHaveLength(1)
    expect(fake.calls.alerts[0]?.message.length).toBeGreaterThan(0)
  })

  test("unreachable adapter surfaces in the dialog, not as a throw", async () => {
    const fake = fakeUI()
    const request: AdapterRequest = async () => {
      throw new Error("connection refused")
    }
    await runCommand(buildRecoveryCommands(depsFor(fake, { chatgpt: request })), CMD_INSPECT_CHATGPT)
    expect(fake.calls.alerts).toHaveLength(1)
  })
})

describe("tui setup wiring", () => {
  function setupHarness(
    options: Record<string, unknown>,
    route: { type: string; sessionID?: string },
  ) {
    const layers: Array<() => { commands: PaletteCommand[] }> = []
    const renderFns: Array<() => never> = []
    const toasts: string[] = []
    const ctx = {
      options,
      ui: {
        dialog: {
          alert: async () => {},
          confirm: async () => true as boolean | undefined,
          prompt: async () => undefined as string | undefined,
        },
        toast: { show: (o: { message: string }) => void toasts.push(o.message) },
        router: { current: () => route },
        slot: (claim: { render: () => never }) => {
          renderFns.push(claim.render)
          return () => {}
        },
        // Deliberately NO ui.keymap: production code must use the top-level
        // context.keymap (real SDK shape). If it ever reads ui.keymap this
        // harness throws TypeError and the suite goes red.
      },
      keymap: { layer: (f: () => { commands: PaletteCommand[] }) => void layers.push(f) },
      data: { session: { status: (_id: string) => "idle" as const } },
    }
    return { ctx, layers, toasts, renderFns }
  }

  test("layer registers palette commands on slot mount; cleanup resolves", async () => {
    const h = setupHarness({}, { type: "session", sessionID: "ses_1" })
    // Regression lock for the real SDK shape: keymap is top-level on the
    // setup context, never under ui. This fake models that exactly.
    expect((h.ctx.ui as Record<string, unknown>).keymap).toBeUndefined()
    expect(typeof (h.ctx as Record<string, unknown>).keymap).toBe("object")
    const cleanup = await setupTui(h.ctx as never)
    // Nothing registered until the slot render mounts (host calls render).
    expect(h.layers).toHaveLength(0)
    for (const r of h.renderFns) r()
    expect(h.layers).toHaveLength(1)
    const ids = h.layers[0]?.().commands.map((c) => c.id) ?? []
    expect(ids).toContain(CMD_INSPECT_CHATGPT)
    expect(new Set(ids).size).toBe(ids.length)
    if (typeof cleanup === "function") cleanup()
  })

  test("chatgpt:false keeps ChatGPT recovery (supervision off, manual adapter on)", async () => {
    const seen: string[] = []
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      async fetch(req) {
        seen.push(`${req.method} ${new URL(req.url).pathname}`)
        return new Response(
          JSON.stringify({
            session: "ses_1",
            state: "clean",
            completed_messages: 2,
            pending_prompt_bytes: 0,
            thread_url_present: true,
            summary: "up to date",
            actions: ["continue"],
          }),
          { headers: { "content-type": "application/json" } },
        )
      },
    })
    try {
      const h = setupHarness(
        {
          chatgpt: false,
          adapterAddr: `127.0.0.1:${server.port}`,
          gemini: { proxyBinary: "/tmp/fake", adapterBaseURL: "http://127.0.0.1:8789/v1" },
        },
        { type: "session", sessionID: "ses_1" },
      )
      await setupTui(h.ctx as never)
      for (const r of h.renderFns) r()
      expect(h.layers).toHaveLength(1)
      const cmds = h.layers[0]?.().commands ?? []
      const ids = cmds.map((c) => c.id)
      // Supervision off must not remove recovery: manual adapters still answer.
      expect(ids).toContain(CMD_INSPECT_CHATGPT)
      expect(ids).toContain(CMD_INSPECT_GEMINI)
      expect(ids).toContain(CMD_ABANDON_GEMINI)
      // And the ChatGPT inspect entry really talks to the manual adapter.
      await cmds.find((c) => c.id === CMD_INSPECT_CHATGPT)!.run()
      expect(seen).toEqual(["GET /v1/sessions/state"])
    } finally {
      server.stop()
    }
  })
})
