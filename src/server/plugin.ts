// ─── opencode-web-bridges Server Plugin Entry ────────────────────────────────────
// Installs the chatgpt-proxy ChatGPT browser bridge with one line in the
// user's `plugin` array: managed binaries + provider/MCP config injection +
// adapter/Chrome supervision + login UX. Never touches the user's live setup
// files directly — everything flows through the `config` hook (fill-missing
// only, never clobber) and plugin-managed processes.
//
// Dual-host: this module also exports `setup` (see v2.ts) for opencode v2
// hosts, so one build works on v1 AND v2. Both entries share the per-process
// lifecycle in shared.ts — whichever host loads wins, never double-spawn.

import type { Hooks, PluginInput, PluginModule, PluginOptions } from "@opencode-ai/plugin"
import type { Plugin as V2Plugin } from "@opencode/plugin"
import { tool } from "@opencode-ai/plugin/tool"
import { ensureShared, type SharedState } from "./shared"
import { applyChatGPTConfig, MCP_ID } from "./config-shape"
import {
  loginInstructions,
  probeReadiness,
} from "./supervisor"
import { doctorReport, formatDoctor, loginFlow, recoverReport, statusReport } from "./tools"
import { setup as setupV2 } from "./v2"
import { resolveGemini, applyGeminiConfig } from "./gemini"

const PLUGIN_ID = "opencode-web-bridges"

const server = async (
  _input: PluginInput,
  pluginOptions: PluginOptions | undefined,
  injected?: SharedState,
): Promise<Hooks> => {
  // Gemini-only registration can coexist with an existing manual ChatGPT
  // service without starting ChatGPT's binary manager or supervisor.
  if (pluginOptions?.chatgpt === false) {
    const gemini = resolveGemini(pluginOptions.gemini)
    return { config: async config => { if (gemini) applyGeminiConfig(config as unknown as Record<string, any>, gemini) } }
  }
  // Background binary ensure + supervision, started once per process and
  // shared with the v2 setup path. Never blocks plugin setup on download.
  const state = injected ?? ensureShared(pluginOptions)
  const opts = state.opts
  const gemini = resolveGemini(pluginOptions?.gemini)

  const adapterBaseURL = `http://${opts.adapterHost}:${opts.adapterPort}/v1`
  const hooks: Hooks = {
    config: async (config) => {
      const cfg = config as unknown as Record<string, any>
      if (gemini) applyGeminiConfig(cfg, gemini)
      // v1 cannot reload the injected MCP command after a background download.
      // Resolve it before registration; existing manual MCP entries need no wait.
      if (cfg.mcp?.[MCP_ID] === undefined) {
        await state.ready
        if (!state.binaries) throw new Error(`Binaries not ready: ${state.binaryError ?? "download failed"}`)
      }
      const added = applyChatGPTConfig(cfg, {
        proxyBinary: state.binaries?.proxy ?? "",
        proxyArgs: opts.bridgeSource ? ["-chrome-port", String(opts.cdpPort), "-profile", opts.profileDir] : [],
        adapterBaseURL,
      })
      if (added.length > 0) {
        console.log(`[${PLUGIN_ID}] config hook added: ${added.join(", ")}`)
      }
    },
    tool: {
      "chatgpt-status": tool({
        description: "Show ChatGPT Web bridge status: readiness, login state, binary versions.",
        args: {},
        execute: async () => {
          if (!state.binaries) {
            return state.binaryError
              ? `Binaries not ready: ${state.binaryError}`
              : "Binaries still downloading — retry in a few seconds."
          }
          return await statusReport(state.opts, state.binaries)
        },
      }),
      "chatgpt-login": tool({
        description: "Launch the profile Chrome window for one-time ChatGPT login, then re-probe readiness.",
        args: {},
        execute: async () => {
          return await loginFlow(state.opts)
        },
      }),
      "chatgpt-doctor": tool({
        description: "Diagnose the ChatGPT Web bridge: binaries, ports, Chrome CDP, login state — each with repair hints.",
        args: {},
        execute: async () => {
          if (!state.binaries) {
            return state.binaryError
              ? `Binaries not ready: ${state.binaryError}`
              : "Binaries still downloading — retry in a few seconds."
          }
          return formatDoctor(await doctorReport(state.opts, state.binaries))
        },
      }),
      "chatgpt-recover": tool({
        description:
          "Inspect or resolve this chat's unanswered turn: what the bridge knows, read the previous reply back without resending anything, or deliberately clear the uncertainty. Inspects by default; never submits.",
        args: {
          action: tool.schema
            .enum(["inspect", "recover", "abandon"])
            .optional()
            .describe("What to do: inspect the chat (default), recover the previous reply, or abandon the unconfirmed turn."),
          session: tool.schema
            .string()
            .optional()
            .describe("Chat to act on. Defaults to this chat; required when clearing one."),
          reason: tool.schema
            .string()
            .optional()
            .describe("Why the turn is being cleared. Only meaningful with action 'abandon'."),
        },
        execute: async (args, context) => {
          if (!state.binaries) {
            return state.binaryError
              ? `Binaries not ready: ${state.binaryError}`
              : "Binaries still downloading — retry in a few seconds."
          }
          return await recoverReport(state.opts, args, context.sessionID)
        },
      }),
    },
    event: async ({ event }) => {
      // LOUD first-run login instructions: when the adapter reports not
      // logged in on session activity, print once per process lifetime.
      const type = (event as any)?.type as string | undefined
      if (!type?.startsWith("session.") || state.firstRunNotified) return
      if (!state.binaries) return
      const ready = await probeReadiness(opts.adapterHost, opts.adapterPort).catch(() => null)
      if (ready && !ready.ok && /not logged in|401|403/i.test(ready.detail)) {
        state.firstRunNotified = true
        console.error(`\n[${PLUGIN_ID}] ───\n${loginInstructions(opts)}\n───\n`)
      }
    },
    dispose: async () => {
      await state.supervisor?.dispose()
    },
  }
  return hooks
}

export default {
  id: PLUGIN_ID,
  server,
  setup: setupV2,
} satisfies PluginModule & V2Plugin.Plugin
