// Config-hook merge logic: fill missing provider/MCP entries, never clobber.
// Pure (no I/O) so it is unit-testable with a fake config object.

export const PROVIDER_ID = "chatgpt-web"
export const MCP_ID = "chatgpt"
export const MODEL_ID = "chatgpt-web"

export interface ChatGPTConfigContext {
  /** Managed chatgpt-proxy MCP binary path (spawned by opencode as a local MCP). */
  proxyBinary: string
  proxyArgs?: string[]
  /** Base URL of the managed OpenAI-compatible adapter, e.g. http://127.0.0.1:8788/v1 */
  adapterBaseURL: string
}

export function buildProviderEntry(adapterBaseURL: string): Record<string, unknown> {
  return {
    name: "ChatGPT Web (browser tool bridge)",
    npm: "@ai-sdk/openai-compatible",
    options: { baseURL: adapterBaseURL, headerTimeout: false, chunkTimeout: false, timeout: false },
    models: {
      [MODEL_ID]: {
        name: "ChatGPT Web",
        tool_call: true,
        attachment: true,
        reasoning: false,
        temperature: false,
        modalities: { input: ["text", "image", "video"], output: ["text"] },
        limit: { context: 0, output: 32768 },
      },
    },
  }
}

export function buildMcpEntry(proxyBinary: string, args: string[] = []): Record<string, unknown> {
  return { type: "local", command: [proxyBinary, ...args], enabled: true }
}

/**
 * Inject the chatgpt-web provider and chatgpt MCP entries into an opencode
 * config object. Only fills when the user has no such entries — existing
 * entries are left untouched (never clobber).
 *
 * Returns the list of keys that were added ("provider.chatgpt-web", "mcp.chatgpt").
 */
export function applyChatGPTConfig(
  config: Record<string, any>,
  ctx: ChatGPTConfigContext,
): string[] {
  const added: string[] = []
  const provider = (config["provider"] ?? {}) as Record<string, unknown>
  if (provider[PROVIDER_ID] === undefined) {
    provider[PROVIDER_ID] = buildProviderEntry(ctx.adapterBaseURL)
    added.push(`provider.${PROVIDER_ID}`)
  }
  config["provider"] = provider
  const mcp = (config["mcp"] ?? {}) as Record<string, unknown>
  if (mcp[MCP_ID] === undefined) {
    mcp[MCP_ID] = buildMcpEntry(ctx.proxyBinary, ctx.proxyArgs)
    added.push(`mcp.${MCP_ID}`)
  }
  config["mcp"] = mcp
  return added
}
