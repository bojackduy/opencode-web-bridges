import type { Plugin } from "@opencode-ai/plugin"
import type { Plugin as V2Plugin } from "@opencode/plugin"

const LANE_MAIN = "chatgpt-web"
const LANES = ["title", "summary", "compaction"] as const

/** One lane decision, shared by both hosts: primary agent switches stay "main". */
function laneFor(agent: string): string {
  return (LANES as readonly string[]).includes(agent) ? agent : "main"
}

// ─── v1 (chat.headers) ─────────────────────────────────────────────────────
// Scope this protocol to the local ChatGPT bridge provider; unrelated providers
// and existing headers remain untouched.
const server: Plugin = async () => ({
  "chat.headers": async (input, output) => {
    if (input.model.providerID !== LANE_MAIN) return
    output.headers["X-OpenCode-Request-Lane"] = laneFor(input.agent)
  },
})

// ─── v2 (session "model.request") ──────────────────────────────────────────
// v2 has no chat.headers hook, but its session "model.request" hook carries the
// same agent identity plus a mutable per-request header bag seeded from the
// existing request headers. So lane identity travels per REQUEST and one
// provider entry serves every lane, instead of one visible provider per lane.
// evt.agent is the discriminator core already uses: the title agent for title
// requests, the compaction agent for compaction, and the real session agent
// (which maps to "main") for primary turns.
const ADAPTER_BASE_URL = "http://127.0.0.1:8788/v1"
const V2_PROVIDER_PACKAGE = "@opencode/ai/providers/openai-compatible"

function laneModel(): any {
  return {
    id: "chatgpt-web",
    modelID: "chatgpt-web",
    providerID: LANE_MAIN,
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

function laneProviderEntry(): any {
  return {
    info: {
      id: LANE_MAIN,
      name: "ChatGPT Web (browser tool bridge)",
      activation: "enabled",
      package: V2_PROVIDER_PACKAGE,
      settings: {
        baseURL: ADAPTER_BASE_URL,
        headerTimeout: false,
        chunkTimeout: false,
        timeout: false,
      },
    },
    models: [laneModel()],
  }
}

async function setup(context: V2Plugin.Context) {
  const registrations: Array<{ dispose(): Promise<void> }> = []
  registrations.push(
    await context.session.hook(
      "model.request",
      (input) => {
        if (input.model.providerID !== LANE_MAIN) return
        input.headers["X-OpenCode-Request-Lane"] = laneFor(input.agent)
      },
      { providerID: LANE_MAIN },
    ),
  )
  // Fill-missing-only: the manual opencode.jsonc entry (or the
  // opencode-chatgpt plugin) owns `chatgpt-web` when present.
  registrations.push(
    await context.provider.transform((editor: any) => {
      if (!editor.get(LANE_MAIN)) editor.add(laneProviderEntry())
    }),
  )
  return async () => {
    await Promise.allSettled(registrations.reverse().map((r) => r.dispose()))
  }
}

export default { id: "chatgpt-proxy.session-lanes", server, setup }
