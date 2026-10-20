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
import { resolveGemini, registerGeminiV2Provider, registerGeminiV2Mcp } from "./gemini"

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
export function buildV2CommandExecutions(
  state: SharedState,
  login: typeof loginFlow = loginFlow,
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
    // A v2 command carries no arguments, so this surface only ever inspects the
    // invoking chat — the read-only action that is safe without a named chat.
    [COMMAND_RECOVER]: async (call) => (state.binaries ? recoverReport(state.opts, {}, call.sessionID) : pending()),
  }
}

const COMMAND_DESCRIPTIONS: Record<string, string> = {
  [COMMAND_STATUS]: "Show ChatGPT Web bridge status: readiness, login state, binary versions.",
  [COMMAND_LOGIN]: "Launch the profile Chrome window for one-time ChatGPT login, then re-probe readiness.",
  [COMMAND_DOCTOR]:
    "Diagnose the ChatGPT Web bridge: binaries, ports, Chrome CDP, login state — each with repair hints.",
  [COMMAND_RECOVER]:
    "Inspect this chat's unanswered turn: what the bridge knows, what is unknown, what was kept, and which actions are available.",
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
      execute: async (call) => {
        const text = await run(call)
        await session.prompt({ ...call.prompt, sessionID: call.sessionID, text, delivery: call.delivery })
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
    if (gemini) {
      await context.provider.transform(editor => registerGeminiV2Provider(editor, gemini))
      await context.mcp.transform(editor => registerGeminiV2Mcp(editor, gemini))
    }
    return
  }
  // `injected` is test-only: unit tests pass a fake SharedState so no real
  // binary download starts. Production hosts always call setup(context).
  const state = injected ?? ensureShared({ ...(context.options as Record<string, unknown>) })
  const opts = state.opts
  const gemini = resolveGemini(context.options.gemini)
  const adapterBaseURL = `http://${opts.adapterHost}:${opts.adapterPort}/v1`
  if (opts.bridgeSource) {
    await state.ready
    if (!state.binaries) throw new Error(state.binaryError ?? "Source cache unavailable; rerun installer")
  }

  await context.provider.transform((editor) => {
    registerV2Provider(editor, adapterBaseURL)
    if (gemini) registerGeminiV2Provider(editor, gemini)
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

  const executions = buildV2CommandExecutions(state)
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
