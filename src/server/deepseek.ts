import * as path from "node:path"
import type { Model, Provider } from "@opencode/plugin"
import type { V2ProviderTarget } from "./v2"

/** Explicit opt-in, externally managed local adapter. DeepSeek talks HTTP+SSE
 * to the web API, so there is no Chrome or CDP: the binary only needs a token
 * saved with `deepseek-openai -login`. */
export interface DeepseekOptions {
  proxyBinary: string
  adapterBaseURL: string
}

export const DEEPSEEK_MODELS = ["deepseek-web", "deepseek-reasoner", "deepseek-expert"] as const

export function resolveDeepseek(raw: unknown): DeepseekOptions | undefined {
  if (raw === undefined || raw === false) return undefined
  if (!raw || typeof raw !== "object") throw new Error("deepseek must be an options object")
  const o = raw as Record<string, unknown>
  if (typeof o.proxyBinary !== "string" || !path.isAbsolute(o.proxyBinary))
    throw new Error("deepseek.proxyBinary must be an absolute local-built binary path")
  const base = typeof o.adapterBaseURL === "string" ? o.adapterBaseURL : "http://127.0.0.1:8790/v1"
  const url = new URL(base)
  if (
    url.protocol !== "http:" ||
    url.hostname !== "127.0.0.1" ||
    url.pathname !== "/v1" ||
    url.username ||
    url.password ||
    url.search ||
    url.hash
  )
    throw new Error("deepseek.adapterBaseURL must be a localhost HTTP /v1 endpoint")
  return { proxyBinary: o.proxyBinary, adapterBaseURL: base }
}

export function applyDeepseekConfig(cfg: Record<string, any>, o: DeepseekOptions): void {
  cfg.provider ??= {}
  cfg.provider["deepseek-web"] ??= {
    npm: "@ai-sdk/openai-compatible",
    name: "DeepSeek Web",
    options: {
      baseURL: o.adapterBaseURL,
      headerTimeout: false,
      chunkTimeout: false,
      timeout: false,
    },
    models: Object.fromEntries(
      DEEPSEEK_MODELS.map(id => [
        id,
        {
          name:
            id === "deepseek-reasoner"
              ? "DeepSeek Web (thinking)"
              : id === "deepseek-expert"
                ? "DeepSeek Web (expert)"
                : "DeepSeek Web",
          tool_call: true,
          modalities: { input: ["text"], output: ["text"] },
          limit: { context: 0, output: 32768 },
        },
      ]),
    ),
  }
}

export function registerDeepseekV2Provider(editor: V2ProviderTarget, o: DeepseekOptions): void {
  if (editor.get("deepseek-web")) return
  const info: Provider.Info = {
    id: "deepseek-web" as Provider.ID,
    name: "DeepSeek Web",
    activation: "enabled",
    package: "@opencode/ai/providers/openai-compatible",
    settings: {
      baseURL: o.adapterBaseURL,
      headerTimeout: false,
      chunkTimeout: false,
      timeout: false,
    },
  }
  const models: Model.Info[] = DEEPSEEK_MODELS.map(id => ({
    id: id as Model.ID,
    modelID: id as Model.ID,
    providerID: "deepseek-web" as Provider.ID,
    name:
      id === "deepseek-reasoner"
        ? "DeepSeek Web (thinking)"
        : id === "deepseek-expert"
          ? "DeepSeek Web (expert)"
          : "DeepSeek Web",
    capabilities: { tools: true, input: ["text"], output: ["text"] },
    variants: [],
    time: { released: 0 },
    cost: [],
    status: "active",
    enabled: true,
    limit: { context: 0, output: 32768 },
  }))
  editor.add({ info, models })
}
