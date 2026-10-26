// ─── opencode-web-bridges v2 host entry (opencode v2, `@opencode/plugin`) ───────
// One build works on v1 AND v2: v1 hosts call `server` (plugin.ts, config
// hook + tools), v2 hosts call `setup` below (domain transforms + commands).
// v2 needs no config hook — providers, MCP servers, and commands are
// first-class domains.
//
// All imports from `@opencode/plugin` are type-only (erased at build), so
// dist stays self-contained: the host provides the runtime context.
//
// Shape parity with the v1 config hook (config-shape.ts) is intentional:
// same provider id `chatgpt-web`, same model id, same input modalities,
// same limits, same MCP name/command layout, same fill-missing-only merge.

import * as path from "node:path"
import type { Mcp, Model, Provider } from "@opencode/plugin"
import { MCP_ID, MODEL_ID, PROVIDER_ID } from "./config-shape"
import { ensureShared, type SharedState } from "./shared"
import { assetName, cacheDir, currentPlatform } from "./binaries"
import { loginInstructions, probeReadiness } from "./supervisor"
import { doctorReport, formatDoctor, loginFlow, recoverReport, statusReport } from "./tools"
import { bridgeRequest } from "./tools"
import type { RecoverArgs } from "./tools"
import { resolveGemini, registerGeminiV2Provider, registerGeminiV2Mcp } from "./gemini"
import { resolveDeepseek, registerDeepseekV2Provider } from "./deepseek"

/** Runtime package speaking the OpenAI request format (v2 equivalent of the v1 `npm: "@ai-sdk/openai-compatible"` entry). */
export const V2_PROVIDER_PACKAGE = "@opencode/ai/providers/openai-compatible"

export const COMMAND_STATUS = "chatgpt-status"
export const COMMAND_LOGIN = "chatgpt-login"
export const COMMAND_DOCTOR = "chatgpt-doctor"
export const COMMAND_RECOVER = "chatgpt-recover"
export const COMMAND_NAMES = [COMMAND_STATUS, COMMAND_LOGIN, COMMAND_DOCTOR, COMMAND_RECOVER] as const

// ─── Pure builders (unit-testable, no I/O) ─────────────────────────────────

/**
 * v2 provider source definition. Mirrors buildProviderEntry() from
 * config-shape.ts: display name + baseURL survive 1:1; `npm` becomes the v2
 * `package` runtime. Explicitly disable all three independent client deadlines:
 * the browser adapter can buffer output while importing history and validating
 * calls. Disabling only total timeout leaves the five-minute header default.
 */
export function buildV2ProviderInfo(adapterBaseURL: string): Provider.Info {
  return {
    id: PROVIDER_ID as Provider.ID,
    name: "ChatGPT Web (browser tool bridge)",
    activation: "enabled",
    package: V2_PROVIDER_PACKAGE,
    settings: { baseURL: adapterBaseURL, headerTimeout: false, chunkTimeout: false, timeout: false },
  }
}

/**
 * v2 model definition. Mirrors the v1 model entry: same id/shape, tool
 * calling on, text+image+video input, text output, same limits
 * (`context: 0` = unknown/unlimited, exactly as the v1 entry declares).
 */
export function buildV2ModelInfo(): Model.Info {
  return {
    id: MODEL_ID as Model.ID,
    modelID: MODEL_ID as Model.ID,
    providerID: PROVIDER_ID as Provider.ID,
    name: "ChatGPT Web",
    capabilities: { tools: true, input: ["text", "image", "video"], output: ["text"] },
    variants: [],
    time: { released: 0 },
    cost: [],
    status: "active",
    enabled: true,
    limit: { context: 0, output: 32768 },
  }
}

/**
 * v2 MCP server config. Mirrors buildMcpEntry(): local server spawning the
 * managed proxy binary. v2 spells "enabled" as the absence of `disabled`.
 */
export function buildV2McpConfig(proxyBinary: string, args: string[] = []): Mcp.ServerConfig {
  return { type: "local", command: [proxyBinary, ...args] }
}

// ─── Minimal structural context (real V2 context + test fakes both fit) ────

export interface V2CommandCall {
  readonly sessionID: string
  readonly prompt: { readonly text: string; readonly [key: string]: unknown }
  readonly delivery: "steer" | "queue"
}

export interface V2ProviderTarget {
  get(providerID: string): unknown
  add(input: { info: Provider.Info; models: readonly Model.Info[] }): void
}

export interface V2McpTarget {
  get(name: string): unknown
  set(name: string, config: Mcp.ServerConfig): void
}

export interface V2CommandTarget {
  add(definition: {
    name: string
    description?: string
    execute: (input: V2CommandCall) => Promise<void>
  }): void
}

export interface V2SetupContext {
  readonly options: Readonly<Record<string, unknown>>
  readonly provider: {
    transform(callback: (editor: V2ProviderTarget) => void): Promise<unknown>
  }
  readonly mcp: {
    transform(callback: (editor: V2McpTarget) => void): Promise<unknown>
    reload(): Promise<void>
  }
  readonly command: {
    transform(callback: (editor: V2CommandTarget) => void): Promise<unknown>
  }
  readonly event: {
    subscribe(options?: { signal?: AbortSignal }): AsyncIterable<{ type: string }>
  }
  readonly session: {
    prompt(input: Record<string, unknown>): Promise<unknown>
  }
}

// ─── Registration (fill-missing-only, never clobber — same as v1) ───────────

export function registerV2Provider(editor: V2ProviderTarget, adapterBaseURL: string): boolean {
  if (editor.get(PROVIDER_ID)) return false
  editor.add({ info: buildV2ProviderInfo(adapterBaseURL), models: [buildV2ModelInfo()] })
  return true
}

export function registerV2Mcp(editor: V2McpTarget, proxyBinary: string): boolean {
  if (editor.get(MCP_ID)) return false
  editor.set(MCP_ID, buildV2McpConfig(proxyBinary))
  return true
}

/** Gate for the LOUD first-run login notice (pure; the probe itself is I/O). */
export function shouldCheckLogin(eventType: unknown, state: Pick<SharedState, "firstRunNotified" | "binaries">): boolean {
  return (
    typeof eventType === "string" &&
    eventType.startsWith("session.") &&
    !state.firstRunNotified &&
    state.binaries !== undefined
  )
}

/** Command executors share the v1 tool logic via tools.ts — no duplication. */
export interface RecoverBridges {
  readonly geminiBaseURL?: string
}

/**
 * Parse what the user typed after `/chatgpt-recover`: an action, a session
 * id, and an optional `-bridge gemini|chatgpt` plus `-reason "..."`. A
 * leading `/chatgpt-recover` token is tolerated. Anything unrecognized is
 * left for the report layer, which answers in plain English instead of
 * throwing usage errors.
 */
export function parseRecoverInvocation(text: string): {
  args: RecoverArgs
  bridge: "chatgpt" | "gemini"
} {
  const tokens = text.match(/"[^"]*"|'[^']*'|\S+/g) ?? []
  const words = tokens.map((t) =>
    t.startsWith("/chatgpt-recover") ? t.slice("/chatgpt-recover".length) : t,
  ).filter((t) => t !== "")
  const args: RecoverArgs = {}
  let bridge: "chatgpt" | "gemini" = "chatgpt"
  const rest: string[] = []
  for (let i = 0; i < words.length; i++) {
    const w = words[i]!
    if (w === "-bridge" && i + 1 < words.length) {
      const b = words[++i]!.toLowerCase()
      if (b === "gemini" || b === "chatgpt") bridge = b
      continue
    }
    if (w === "-reason") {
      const parts: string[] = []
      while (i + 1 < words.length && !words[i + 1]!.startsWith("-")) parts.push(words[++i]!)
      args.reason = parts.join(" ").replace(/^["']|["']$/g, "")
      continue
    }
    if (w.startsWith("-")) continue
    rest.push(w)
  }
  const actions = ["inspect", "recover", "abandon"]
  for (const w of rest) {
    if (!args.action && actions.includes(w.toLowerCase())) {
      args.action = w.toLowerCase()
      continue
    }
    if (!args.session) {
      const id = w.replace(/^["']|["']$/g, "")
      const bad =
        id === "" || id.includes("*") || /^(all|any)$/i.test(id)
          ? w
          : ""
      if (bad === "") {
        args.session = id
        continue
      }
    }
  }
  if (rest.length > 0 && !args.action && !args.session) {
    // A single unrecognized token is still reported, not thrown: keep it as
    // the action so the report layer explains the valid actions.
    args.action = rest[0]
  }
  return { args, bridge }
}

export function buildV2CommandExecutions(
  state: SharedState,
  login: typeof loginFlow = loginFlow,
  bridges: RecoverBridges = {},
): Record<string, (call: V2CommandCall) => Promise<string>> {
  const pending = (): string =>
    state.binaryError
      ? `Binaries not ready: ${state.binaryError}`
      : "Binaries still downloading — retry in a few seconds."
  return {
    [COMMAND_STATUS]: async () =>
      state.binaries ? statusReport(state.opts, state.binaries) : pending(),
    [COMMAND_LOGIN]: async () => login(state.opts),
    [COMMAND_DOCTOR]: async () =>
      state.binaries ? formatDoctor(await doctorReport(state.opts, state.binaries)) : pending(),
    // A v2 command carries typed arguments in the invocation text after the
    // command name; the calling chat is the default session. The recovery
    // HTTP calls run right here in the plugin host — no assistant turn is
    // spent doing the work. Only the human-readable report travels back
    // through session.prompt, and that delivery is best-effort: on a wedged
    // chat the work is already done, so a failed delivery must not throw.
    [COMMAND_RECOVER]: async (call) => {
      if (!state.binaries) return pending()
      const invocation = parseRecoverInvocation(call.prompt?.text ?? "")
      if (invocation.bridge === "gemini") {
        if (!bridges.geminiBaseURL) {
          return "The Gemini bridge is not configured in this plugin, so there is nothing to inspect. Configure it under the plugin's gemini options, then retry."
        }
        const action = (invocation.args.action ?? "inspect").toLowerCase()
        if (action === "recover") {
          return [
            `The Gemini bridge has no read-only adoption endpoint, so "recover" cannot run there.`,
            `Moves: send a distinct request (the adapter reseeds a fresh thread and abandons the old one, never resending it), or run: /chatgpt-recover abandon <session-id> -bridge gemini`,
          ].join("\n")
        }
        return recoverReport(
          state.opts,
          invocation.args,
          call.sessionID,
          bridgeRequest(bridges.geminiBaseURL),
        )
      }
      return recoverReport(state.opts, invocation.args, call.sessionID)
    },
  }
}

const COMMAND_DESCRIPTIONS: Record<string, string> = {
  [COMMAND_STATUS]: "Show ChatGPT Web bridge status: readiness, login state, binary versions.",
  [COMMAND_LOGIN]: "Launch the profile Chrome window for one-time ChatGPT login, then re-probe readiness.",
  [COMMAND_DOCTOR]:
    "Diagnose the ChatGPT Web bridge: binaries, ports, Chrome CDP, login state — each with repair hints.",
  [COMMAND_RECOVER]:
    "Recover an uncertain bridge turn directly, no assistant turn needed: inspect, recover, or abandon by id. Usage: /chatgpt-recover [inspect|recover|abandon] [session-id] [-bridge gemini] [-reason ...]. Defaults to inspecting this chat.",
}

export function registerV2Commands(
  editor: V2CommandTarget,
  session: V2SetupContext["session"],
  executions: Record<string, (call: V2CommandCall) => Promise<string>>,
): void {
  for (const name of COMMAND_NAMES) {
    const run = executions[name]
    if (!run) continue
    editor.add({
      name,
      description: COMMAND_DESCRIPTIONS[name],
      // v2 command delivery: submit the human-readable report back into the
      // invoking session as a prompt, honoring the requested delivery mode.
      // Best-effort: the command already did its work before this point, so
      // a wedged chat that cannot take the report must not turn into a
      // thrown error. The report is also logged for `opencode logs`.
      execute: async (call) => {
        const text = await run(call)
        try {
          await session.prompt({ ...call.prompt, sessionID: call.sessionID, text, delivery: call.delivery })
        } catch (error) {
          console.error(`[${name}] report delivery failed (work already done): ${error}`)
        }
      },
    })
  }
}

// ─── setup ───────────────────────────────────────────────────────────────────

/** Best-known proxy path before the background download finishes: the exact
 * cache slot ensureBinaries() will fill (pure mapping), so the registered
 * MCP entry is usually already correct and reload() just retries connect. */
function pendingProxyPath(version: string): string {
  return path.join(cacheDir(version), assetName("chatgpt-proxy", currentPlatform()))
}

async function consumeV2Events(
  context: V2SetupContext,
  signal: AbortSignal,
  state: SharedState,
): Promise<void> {
  for await (const event of context.event.subscribe({ signal })) {
    if (!shouldCheckLogin(event?.type, state)) continue
    const ready = await probeReadiness(state.opts.adapterHost, state.opts.adapterPort).catch(() => null)
    if (ready && !ready.ok && /not logged in|401|403/i.test(ready.detail)) {
      state.firstRunNotified = true
      console.error(`\n[opencode-web-bridges] ───\n${loginInstructions(state.opts)}\n───\n`)
    }
  }
}

export async function setup(
  context: V2SetupContext,
  injected?: SharedState,
): Promise<(() => Promise<void>) | void> {
  if (context.options.chatgpt === false) {
    const gemini = resolveGemini(context.options.gemini)
    const deepseek = resolveDeepseek(context.options.deepseek)
    if (gemini || deepseek) {
      await context.provider.transform(editor => {
        if (gemini) registerGeminiV2Provider(editor, gemini)
        if (deepseek) registerDeepseekV2Provider(editor, deepseek)
      })
    }
    if (gemini) await context.mcp.transform(editor => registerGeminiV2Mcp(editor, gemini))
    return
  }
  // `injected` is test-only: unit tests pass a fake SharedState so no real
  // binary download starts. Production hosts always call setup(context).
  const state = injected ?? ensureShared({ ...(context.options as Record<string, unknown>) })
  const opts = state.opts
  const gemini = resolveGemini(context.options.gemini)
  const deepseek = resolveDeepseek(context.options.deepseek)
  const adapterBaseURL = `http://${opts.adapterHost}:${opts.adapterPort}/v1`
  if (opts.bridgeSource) {
    await state.ready
    if (!state.binaries) throw new Error(state.binaryError ?? "Source cache unavailable; rerun installer")
  }

  await context.provider.transform((editor) => {
    registerV2Provider(editor, adapterBaseURL)
    if (gemini) registerGeminiV2Provider(editor, gemini)
    if (deepseek) registerDeepseekV2Provider(editor, deepseek)
  })

  // Fill-missing-only over a live cell: the transform replays on every
  // rebuild, so when the background download lands we just update the cell
  // and reload() — a pre-existing user entry is still never clobbered.
  const mcpPath = { current: state.binaries?.proxy ?? pendingProxyPath(opts.version) }
  let mcpAdded = false
  await context.mcp.transform((editor) => {
    if (gemini) registerGeminiV2Mcp(editor, gemini)
    if (opts.bridgeSource && editor.get(MCP_ID) === undefined) {
      editor.set(MCP_ID, buildV2McpConfig(mcpPath.current, ["-chrome-port", String(opts.cdpPort), "-profile", opts.profileDir]))
      mcpAdded = true
    } else if (registerV2Mcp(editor, mcpPath.current)) mcpAdded = true
  })
  void state.ready.then(() => {
    if (!mcpAdded || !state.binaries) return
    mcpPath.current = state.binaries.proxy
    context.mcp.reload().catch(() => {})
  })

  const executions = buildV2CommandExecutions(state, loginFlow, {
    geminiBaseURL: gemini?.adapterBaseURL,
  })
  await context.command.transform((editor) => {
    registerV2Commands(editor, context.session, executions)
  })

  const controller = new AbortController()
  const eventTask = consumeV2Events(context, controller.signal, state).catch(() => {})

  return async () => {
    controller.abort()
    await eventTask
    await state.supervisor?.dispose()
  }
}
