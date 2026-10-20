// v1↔v2 equivalence: the v2 domain registrations must carry the SAME
// provider/model ids, shapes, modalities, limits, MCP layout, and command
// names as the v1 config hook + tools — verified with fake editors.

import { beforeEach, describe, expect, test } from "bun:test"
import { Model, Provider } from "@opencode/plugin"
import {
  buildMcpEntry,
  buildProviderEntry,
  MCP_ID,
  MODEL_ID,
  PROVIDER_ID,
} from "../src/server/config-shape"
import { resolveOptions } from "../src/server/options"
import plugin from "../src/server/plugin"
import { resetSharedForTests, type SharedState } from "../src/server/shared"
import {
  buildV2CommandExecutions,
  buildV2McpConfig,
  buildV2ModelInfo,
  buildV2ProviderInfo,
  COMMAND_DOCTOR,
  COMMAND_LOGIN,
  COMMAND_NAMES,
  COMMAND_RECOVER,
  COMMAND_STATUS,
  registerV2Commands,
  registerV2Mcp,
  registerV2Provider,
  setup,
  shouldCheckLogin,
  type V2CommandTarget,
  type V2McpTarget,
  type V2ProviderTarget,
  type V2SetupContext,
} from "../src/server/v2"

const BASE_URL = "http://127.0.0.1:8788/v1"
const PROXY = "/cache/chatgpt-proxy-test"

// ─── Fake editors ────────────────────────────────────────────────────────────

function fakeProviderEditor(existing?: Record<string, unknown>): V2ProviderTarget & {
  added: Array<{ info: unknown; models: readonly unknown[] }>
} {
  const store = new Map<string, unknown>(Object.entries(existing ?? {}))
  return {
    added: [],
    get(id: string) {
      return store.get(id)
    },
    add(input: { info: unknown; models: readonly unknown[] }) {
      ;(this as any).added.push(input)
      store.set((input.info as any).id, input)
    },
  }
}

function fakeMcpEditor(existing?: Record<string, unknown>): V2McpTarget & {
  sets: Array<[string, unknown]>
} {
  const store = new Map<string, unknown>(Object.entries(existing ?? {}))
  return {
    sets: [],
    get(name: string) {
      return store.get(name)
    },
    set(name: string, config: unknown) {
      ;(this as any).sets.push([name, config])
      store.set(name, config)
    },
  }
}

function fakeCommandEditor(): V2CommandTarget & {
  defs: Map<string, { name: string; description?: string; execute: (call: any) => Promise<void> }>
} {
  return {
    defs: new Map(),
    add(def: any) {
      ;(this as any).defs.set(def.name, def)
    },
  }
}

function fakeState(): SharedState {
  return {
    opts: resolveOptions({}),
    binaries: undefined,
    binaryError: undefined,
    supervisor: undefined,
    ready: Promise.resolve(),
    firstRunNotified: false,
  }
}

function fakeContext(
  overrides?: Partial<{
    providerExisting: Record<string, unknown>
    mcpExisting: Record<string, unknown>
    events: Array<{ type: string }>
  }>,
): V2SetupContext & {
  providerEditor: ReturnType<typeof fakeProviderEditor>
  mcpEditor: ReturnType<typeof fakeMcpEditor>
  commandEditor: ReturnType<typeof fakeCommandEditor>
  prompts: Array<Record<string, unknown>>
} {
  const providerEditor = fakeProviderEditor(overrides?.providerExisting)
  const mcpEditor = fakeMcpEditor(overrides?.mcpExisting)
  const commandEditor = fakeCommandEditor()
  const prompts: Array<Record<string, unknown>> = []
  const events = overrides?.events ?? []
  return {
    providerEditor,
    mcpEditor,
    commandEditor,
    prompts,
    options: {},
    provider: { transform: async (cb) => void cb(providerEditor) },
    mcp: { transform: async (cb) => void cb(mcpEditor), reload: async () => {} },
    command: { transform: async (cb) => void cb(commandEditor) },
    event: {
      async *subscribe() {
        for (const e of events) yield e
      },
    },
    session: {
      prompt: async (input: Record<string, unknown>) => void prompts.push(input),
    },
  }
}

beforeEach(() => resetSharedForTests())

test("pinned source uses exact cached MCP path and matching profile/CDP args on both hosts", async () => {
  const state = fakeState()
  state.opts = resolveOptions({ bridgeSource: { repo: "owner/bridge", commit: "a".repeat(40) }, cdpPort: 9333, profileDir: "/profile spaces" })
  state.binaries = { openai: "/source/cache/adapter", proxy: "/source/cache/proxy", version: "source:" + "a".repeat(40) }
  const command = [state.binaries.proxy, "-chrome-port", "9333", "-profile", "/profile spaces"]
  const hooks = await plugin.server({} as any, undefined, state)
  const config: any = {}
  await hooks.config!(config)
  expect(config.mcp.chatgpt.command).toEqual(command)
  const context = fakeContext()
  const dispose = await setup(context, state)
  expect((context.mcpEditor.sets[0]![1] as any).command).toEqual(command)
  await dispose?.()
  state.binaries = undefined
  state.binaryError = "Source cache missing; rerun installer"
  await expect(setup(fakeContext(), state)).rejects.toThrow("rerun installer")
})

describe("v1 first-install MCP resolution", () => {
  test("waits for binaries before registering the actual MCP command", async () => {
    const state = fakeState()
    let finish!: () => void
    state.ready = new Promise<void>((resolve) => { finish = resolve })
    const hooks = await plugin.server({} as any, undefined, state)
    const config: any = {}
    const applying = hooks.config!(config)
    expect(config.mcp).toBeUndefined()
    state.binaries = { proxy: PROXY, openai: "/cache/adapter", version: "v0.1.0" }
    finish()
    await applying
    expect(config.mcp.chatgpt.command).toEqual([PROXY])
  })

  test("failed downloads never register a fake command", async () => {
    const state = fakeState()
    state.binaryError = "release missing"
    const hooks = await plugin.server({} as any, undefined, state)
    const config: any = {}
    await expect(hooks.config!(config)).rejects.toThrow("Binaries not ready: release missing")
    expect(config.mcp).toBeUndefined()
  })

  test("existing manual MCP is preserved without waiting for managed binaries", async () => {
    const state = fakeState()
    state.ready = new Promise(() => {})
    const hooks = await plugin.server({} as any, undefined, state)
    const manual = { type: "local", command: ["/manual/proxy"] }
    const config: any = { mcp: { chatgpt: manual } }
    await hooks.config!(config)
    expect(config.mcp.chatgpt).toBe(manual)
    expect(config.provider[PROVIDER_ID]).toBeDefined()
  })
})

// ─── Builder shapes ──────────────────────────────────────────────────────────

describe("v2 builders match the v1 config shapes", () => {
  test("same provider id + baseURL as the v1 provider entry", () => {
    const info = buildV2ProviderInfo(BASE_URL) as any
    const v1 = buildProviderEntry(BASE_URL) as any
    expect(info.id).toBe(PROVIDER_ID)
    expect(v1.models).toHaveProperty(MODEL_ID)
    expect(info.settings.baseURL).toBe(v1.options.baseURL)
  })

  test("same model id, modalities, and limits as the v1 model entry", () => {
    const model = buildV2ModelInfo() as any
    const v1model = (buildProviderEntry(BASE_URL) as any).models[MODEL_ID]
    expect(model.id).toBe(MODEL_ID)
    expect(model.modelID).toBe(MODEL_ID)
    expect(model.providerID).toBe(PROVIDER_ID)
    expect(model.capabilities).toEqual({
      tools: v1model.tool_call,
      input: v1model.modalities.input,
      output: v1model.modalities.output,
    })
    expect(model.limit).toEqual(v1model.limit)
  })

  test("same MCP name/command/binary path as the v1 MCP entry", () => {
    const v1 = buildMcpEntry(PROXY) as any
    const v2 = buildV2McpConfig(PROXY) as any
    expect(MCP_ID).toBe("chatgpt")
    expect(v2.type).toBe("local")
    expect(v2.command).toEqual(v1.command)
    expect(v2.command).toEqual([PROXY])
  })

  test("provider builder equals the real schema empty()+overrides (no guessing)", () => {
    const pid = Provider.ID.make(PROVIDER_ID)
    expect(buildV2ProviderInfo(BASE_URL)).toEqual({
      ...Provider.Info.empty(pid),
      name: "ChatGPT Web (browser tool bridge)",
      activation: "enabled",
      package: "@opencode/ai/providers/openai-compatible",
      settings: { baseURL: BASE_URL, headerTimeout: false, chunkTimeout: false, timeout: false },
    })
  })

  test("model builder equals the real schema default()+overrides (no guessing)", () => {
    const pid = Provider.ID.make(PROVIDER_ID)
    const mid = Model.ID.make(MODEL_ID)
    expect(buildV2ModelInfo()).toEqual({
      ...Model.Info.default(pid, mid),
      name: "ChatGPT Web",
      capabilities: { tools: true, input: ["text", "image", "video"], output: ["text"] },
      limit: { context: 0, output: 32768 },
    })
  })
})

// ─── Fill-missing-only on both hosts ─────────────────────────────────────────

describe("v2 transforms never clobber (like the v1 config hook)", () => {
  test("provider: adds on empty, skips when present", () => {
    const empty = fakeProviderEditor()
    expect(registerV2Provider(empty, BASE_URL)).toBe(true)
    expect(empty.added).toHaveLength(1)

    const mine = { provider: { id: PROVIDER_ID }, models: new Map() }
    const full = fakeProviderEditor({ [PROVIDER_ID]: mine })
    expect(registerV2Provider(full, BASE_URL)).toBe(false)
    expect(full.added).toHaveLength(0)
  })

  test("mcp: adds on empty, skips when present", () => {
    const empty = fakeMcpEditor()
    expect(registerV2Mcp(empty, PROXY)).toBe(true)
    expect(empty.sets).toEqual([[MCP_ID, { type: "local", command: [PROXY] }]])

    const mine = { type: "local", command: ["/custom/proxy"] }
    const full = fakeMcpEditor({ [MCP_ID]: mine })
    expect(registerV2Mcp(full, PROXY)).toBe(false)
    expect(full.sets).toHaveLength(0)
  })
})

// ─── Commands ────────────────────────────────────────────────────────────────

describe("v2 commands mirror the v1 tools", () => {
  test("registered under the same 3 names with human-readable descriptions", () => {
    const editor = fakeCommandEditor()
    registerV2Commands(
      editor,
      { prompt: async () => {} },
      Object.fromEntries(COMMAND_NAMES.map((n) => [n, async () => n])),
    )
    expect([...editor.defs.keys()].sort()).toEqual([...COMMAND_NAMES].sort())
    expect([...editor.defs.keys()].sort()).toEqual(
      [COMMAND_STATUS, COMMAND_LOGIN, COMMAND_DOCTOR, COMMAND_RECOVER].sort(),
    )
    for (const def of editor.defs.values()) {
      expect(typeof def.description).toBe("string")
      expect(def.description!.length).toBeGreaterThan(0)
    }
  })

  test("execute delivers the human-readable report via session.prompt", async () => {
    const editor = fakeCommandEditor()
    const prompts: Array<Record<string, unknown>> = []
    registerV2Commands(
      editor,
      { prompt: async (input) => void prompts.push(input) },
      { [COMMAND_STATUS]: async () => "ready (ok)" },
    )
    await editor.defs.get(COMMAND_STATUS)!.execute({
      sessionID: "ses_1",
      prompt: { text: "run it" },
      delivery: "steer",
    })
    expect(prompts).toEqual([
      { text: "ready (ok)", sessionID: "ses_1", delivery: "steer" },
    ])
  })

  test("command executions share the v1 pending-not-ready wording", async () => {
    const state = fakeState()
    const loginOptions: unknown[] = []
    const exec = buildV2CommandExecutions(state, async (opts) => {
      loginOptions.push(opts)
      return "Chrome login (fake)"
    })
    const call = { sessionID: "s", prompt: { text: "" }, delivery: "steer" as const}
    expect(await exec[COMMAND_STATUS]!(call)).toContain("Binaries still downloading")
    expect(await exec[COMMAND_DOCTOR]!(call)).toContain("Binaries still downloading")
    expect(await exec[COMMAND_RECOVER]!(call)).toContain("Binaries still downloading")
    expect(await exec[COMMAND_LOGIN]!(call)).toContain("Chrome")
    expect(loginOptions).toEqual([state.opts])
  })
})

// ─── Login watcher gate ──────────────────────────────────────────────────────

describe("v2 session-activity login watcher gate", () => {
  test("only session.* events with binaries present and not yet notified", () => {
    const ready = { firstRunNotified: false, binaries: { openai: "a", proxy: "b", version: "v" } }
    expect(shouldCheckLogin("session.idle", ready)).toBe(true)
    expect(shouldCheckLogin("message.updated", ready)).toBe(false)
    expect(shouldCheckLogin("session.idle", { ...ready, firstRunNotified: true })).toBe(false)
    expect(shouldCheckLogin("session.idle", { firstRunNotified: false, binaries: undefined })).toBe(false)
  })
})

// ─── Full setup wiring ───────────────────────────────────────────────────────

describe("v2 setup wires every domain", () => {
  test("registers provider + mcp + 4 commands on an empty host", async () => {
    const ctx = fakeContext()
    const state = fakeState()
    const { setup: setupWithState } = await import("../src/server/v2")
    // Inject state to avoid real binary downloads in unit tests.
    const cleanup = await (setupWithState as any)(ctx, state)
    expect(ctx.providerEditor.added).toHaveLength(1)
    expect((ctx.providerEditor.added[0]!.info as any).id).toBe(PROVIDER_ID)
    expect(ctx.mcpEditor.sets).toHaveLength(1)
    expect(ctx.mcpEditor.sets[0]![0]).toBe(MCP_ID)
    expect([...ctx.commandEditor.defs.keys()].sort()).toEqual([...COMMAND_NAMES].sort())
    expect(typeof cleanup).toBe("function")
    await cleanup()
  })

  test("leaves pre-existing provider + mcp entries untouched", async () => {
    const ctx = fakeContext({
      providerExisting: { [PROVIDER_ID]: { provider: "mine" } },
      mcpExisting: { [MCP_ID]: { type: "local", command: ["/mine"] } },
    })
    const { setup: setupWithState } = await import("../src/server/v2")
    const cleanup = await (setupWithState as any)(ctx, fakeState())
    expect(ctx.providerEditor.added).toHaveLength(0)
    expect(ctx.mcpEditor.sets).toHaveLength(0)
    expect(ctx.commandEditor.defs.size).toBe(4)
    await (cleanup as any)()
  })
})
