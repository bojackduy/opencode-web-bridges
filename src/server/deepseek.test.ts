import { expect, test } from "bun:test"
import { applyDeepseekConfig, resolveDeepseek, registerDeepseekV2Provider, DEEPSEEK_MODELS } from "./deepseek"
import { setup } from "./v2"
import plugin from "./plugin"

test("DeepSeek is opt-in and requires a local binary", () => {
  expect(resolveDeepseek(undefined)).toBeUndefined()
  expect(resolveDeepseek(false)).toBeUndefined()
  expect(() => resolveDeepseek({})).toThrow("absolute")
  expect(() => resolveDeepseek({ proxyBinary: "/local/proxy", adapterBaseURL: "http://remote/v1" })).toThrow()
  expect(resolveDeepseek({ proxyBinary: "/local/proxy" })!.adapterBaseURL).toBe("http://127.0.0.1:8790/v1")
})

test("v1 fill-missing preserves manual entries and is idempotent", () => {
  const o = resolveDeepseek({ proxyBinary: "/local path/proxy" })!
  const cfg: any = { model: "other/model", provider: { "deepseek-web": { manual: true } } }
  applyDeepseekConfig(cfg, o)
  expect(cfg.provider["deepseek-web"].manual).toBe(true)
  delete cfg.provider["deepseek-web"]
  applyDeepseekConfig(cfg, o)
  expect(cfg.provider["deepseek-web"].models["deepseek-web"].tool_call).toBe(true)
  expect(Object.keys(cfg.provider["deepseek-web"].models)).toEqual([...DEEPSEEK_MODELS])
  const saved = JSON.stringify(cfg)
  applyDeepseekConfig(cfg, o)
  expect(JSON.stringify(cfg)).toBe(saved)
  expect(cfg.model).toBe("other/model")
})

test("v2 registration advertises every model and never clobbers", () => {
  const o = resolveDeepseek({ proxyBinary: "/local/proxy" })!
  let added: any
  registerDeepseekV2Provider({ get: () => undefined, add: v => { added = v } }, o)
  expect(added.info.package).toBe("@opencode/ai/providers/openai-compatible")
  expect(added.models.map((m: any) => m.modelID)).toEqual([...DEEPSEEK_MODELS])
  expect(added.models.every((m: any) => m.capabilities.tools === true)).toBe(true)
  registerDeepseekV2Provider({ get: () => true, add: () => { throw new Error("clobbered") } }, o)
})

test("DeepSeek-only entries skip ChatGPT binary management and start no MCP", async () => {
  const options = { chatgpt: false, deepseek: { proxyBinary: "/local/proxy" } }
  const hooks = await plugin.server({} as any, options)
  const cfg: any = {}
  await hooks.config!(cfg)
  expect(Object.keys(cfg.provider)).toEqual(["deepseek-web"])
  expect(cfg.mcp).toBeUndefined()
  const providers: string[] = []
  let mcpCalls = 0
  await setup({ options,
    provider: { transform: async (f: any) => f({ get: () => undefined, add: (v: any) => providers.push(v.info.id) }) },
    mcp: { transform: async (f: any) => { mcpCalls++; f({ get: () => undefined, set: () => {} }) } },
  } as any)
  expect(providers).toEqual(["deepseek-web"])
  expect(mcpCalls).toBe(0)
})
