import * as path from "node:path"
import * as os from "node:os"
import type { Model, Provider } from "@opencode/plugin"
import type { V2McpTarget, V2ProviderTarget } from "./v2"

/** Explicit opt-in, local-built MCP binary. Adapter is externally managed;
 * no unpublished release download and no second supervisor for launchd. */
export interface GeminiOptions {
  proxyBinary: string
  adapterBaseURL: string
  profileDir: string
  cdpPort: number
}

export function resolveGemini(raw: unknown): GeminiOptions | undefined {
  if (raw === undefined || raw === false) return undefined
  if (!raw || typeof raw !== "object") throw new Error("gemini must be an options object")
  const o = raw as Record<string, unknown>
  if (typeof o.proxyBinary !== "string" || !path.isAbsolute(o.proxyBinary))
    throw new Error("gemini.proxyBinary must be an absolute local-built binary path (no release assets available)")
  const base = typeof o.adapterBaseURL === "string" ? o.adapterBaseURL : "http://127.0.0.1:8789/v1"
  const url = new URL(base)
  if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || url.pathname !== "/v1" || url.username || url.password || url.search || url.hash)
    throw new Error("gemini.adapterBaseURL must be a localhost HTTP /v1 endpoint")
  const port = o.cdpPort ?? 9223
  if (typeof port !== "number" || !Number.isInteger(port) || port < 1 || port > 65535)
    throw new Error("gemini.cdpPort must be a valid port")
  const profile = typeof o.profileDir === "string" ? o.profileDir : path.join(os.homedir(), ".gemini-proxy")
  if (!path.isAbsolute(profile)) throw new Error("gemini.profileDir must be absolute")
  return { proxyBinary: o.proxyBinary, adapterBaseURL: base, profileDir: profile, cdpPort: port }
}

export function geminiArgs(o: GeminiOptions): string[] {
  return ["-chrome-port", String(o.cdpPort), "-profile", o.profileDir]
}

export function applyGeminiConfig(cfg: Record<string, any>, o: GeminiOptions): void {
  cfg.provider ??= {}
  cfg.provider["gemini-web"] ??= {
    npm: "@ai-sdk/openai-compatible", name: "Gemini Web",
    options: { baseURL: o.adapterBaseURL, headerTimeout: false, chunkTimeout: false, timeout: false },
    models: { "gemini-web": { name: "Gemini Web", tool_call: true, modalities: { input: ["text"], output: ["text"] }, limit: { context: 0, output: 32768 } } },
  }
  cfg.mcp ??= {}
  cfg.mcp.gemini ??= { type: "local", command: [o.proxyBinary, ...geminiArgs(o)], enabled: true }
}

export function registerGeminiV2Provider(editor: V2ProviderTarget, o: GeminiOptions): void {
  if (editor.get("gemini-web")) return
  const info: Provider.Info = { id: "gemini-web" as Provider.ID, name: "Gemini Web", activation: "enabled", package: "@opencode/ai/providers/openai-compatible", settings: { baseURL: o.adapterBaseURL, headerTimeout: false, chunkTimeout: false, timeout: false } }
  const model: Model.Info = { id: "gemini-web" as Model.ID, modelID: "gemini-web" as Model.ID, providerID: "gemini-web" as Provider.ID, name: "Gemini Web", capabilities: { tools: true, input: ["text"], output: ["text"] }, variants: [], time: { released: 0 }, cost: [], status: "active", enabled: true, limit: { context: 0, output: 32768 } }
  editor.add({ info, models: [model] })
}

export function registerGeminiV2Mcp(editor: V2McpTarget, o: GeminiOptions): void {
  if (editor.get("gemini")) return
  editor.set("gemini", { type: "local", command: [o.proxyBinary, ...geminiArgs(o)] })
}
