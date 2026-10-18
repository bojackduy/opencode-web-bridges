import { describe, expect, test } from "bun:test"
import {
  applyChatGPTConfig,
  buildMcpEntry,
  buildProviderEntry,
  MCP_ID,
  MODEL_ID,
  PROVIDER_ID,
} from "../src/server/config-shape"

const CTX = { proxyBinary: "/cache/chatgpt-proxy-linux-amd64", adapterBaseURL: "http://127.0.0.1:8788/v1" }

function emptyConfig(): Record<string, any> {
  return {}
}

describe("applyChatGPTConfig fills missing entries", () => {
  test("adds provider + mcp on an empty config", () => {
    const cfg = emptyConfig()
    const added = applyChatGPTConfig(cfg, CTX)
    expect(added).toEqual([`provider.${PROVIDER_ID}`, `mcp.${MCP_ID}`])
    expect(cfg["provider"][PROVIDER_ID]).toEqual(buildProviderEntry(CTX.adapterBaseURL))
    expect(cfg["mcp"][MCP_ID]).toEqual(buildMcpEntry(CTX.proxyBinary))
  })

  test("provider entry matches the required shape exactly", () => {
    const entry = buildProviderEntry("http://127.0.0.1:8788/v1") as any
    expect(entry.name).toBe("ChatGPT Web (browser tool bridge)")
    expect(entry.npm).toBe("@ai-sdk/openai-compatible")
    expect(entry.options).toEqual({ baseURL: "http://127.0.0.1:8788/v1", headerTimeout: false, chunkTimeout: false, timeout: false })
    const model = entry.models[MODEL_ID]
    expect(model.name).toBe("ChatGPT Web")
    expect(model.tool_call).toBe(true)
    expect(model.attachment).toBe(true)
    expect(model.reasoning).toBe(false)
    expect(model.temperature).toBe(false)
    expect(model.modalities).toEqual({ input: ["text", "image", "video"], output: ["text"] })
    expect(model.limit).toEqual({ context: 0, output: 32768 })
  })

  test("mcp entry matches the required shape exactly", () => {
    expect(buildMcpEntry("/x/chatgpt-proxy")).toEqual({
      type: "local",
      command: ["/x/chatgpt-proxy"],
      enabled: true,
    })
  })
})

describe("applyChatGPTConfig never clobbers", () => {
  test("leaves an existing provider entry untouched", () => {
    const mine = { name: "mine", custom: true }
    const cfg: Record<string, any> = { provider: { [PROVIDER_ID]: mine }, mcp: {} }
    const added = applyChatGPTConfig(cfg, CTX)
    expect(cfg["provider"][PROVIDER_ID]).toBe(mine)
    expect(added).toEqual([`mcp.${MCP_ID}`])
  })

  test("leaves an existing mcp entry untouched", () => {
    const mine = { type: "local", command: ["/custom/proxy"], enabled: false }
    const cfg: Record<string, any> = { provider: {}, mcp: { [MCP_ID]: mine } }
    const added = applyChatGPTConfig(cfg, CTX)
    expect(cfg["mcp"][MCP_ID]).toBe(mine)
    expect(added).toEqual([`provider.${PROVIDER_ID}`])
  })

  test("fully configured config is a no-op returning []", () => {
    const providerEntry = { name: "mine" }
    const mcpEntry = { type: "local", command: ["/mine"], enabled: true }
    const cfg: Record<string, any> = {
      provider: { [PROVIDER_ID]: providerEntry, other: { name: "other" } },
      mcp: { [MCP_ID]: mcpEntry },
    }
    expect(applyChatGPTConfig(cfg, CTX)).toEqual([])
    expect(cfg["provider"][PROVIDER_ID]).toBe(providerEntry)
    expect(cfg["provider"]["other"]).toEqual({ name: "other" })
    expect(cfg["mcp"][MCP_ID]).toBe(mcpEntry)
  })

  test("preserves unrelated provider/mcp entries", () => {
    const cfg: Record<string, any> = { provider: { openai: { name: "OpenAI" } }, mcp: {} }
    applyChatGPTConfig(cfg, CTX)
    expect(cfg["provider"]["openai"]).toEqual({ name: "OpenAI" })
  })
})
