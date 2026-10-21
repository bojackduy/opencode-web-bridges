// Plugin options for @bojackduy/opencode-web-bridges.
// Every field is optional; all ports/paths/flags are overridable.

import * as os from "node:os"
import * as path from "node:path"
import { validateSource, type BridgeSource } from "./source"
import type { GeminiOptions } from "./gemini"

export interface ChatGPTPluginOptions {
  /** false: registration-only Gemini setup; preserve externally owned ChatGPT. */
  chatgpt?: boolean
  /** Explicit opt-in Gemini local binary, externally managed adapter. */
  gemini?: Partial<GeminiOptions> & Pick<GeminiOptions, "proxyBinary">
  /** Installer-pinned source build; startup only reads its verified cache. */
  bridgeSource?: BridgeSource
  /** GitHub repo hosting chatgpt-proxy releases. Default "bojackduy/chatgpt-proxy". */
  releasesRepo?: string
  /** Release tag to download, or "latest". Default "latest". */
  version?: string
  /** Chrome profile dir holding the ChatGPT login. Default "~/.chatgpt-proxy". */
  profileDir?: string
  /** Chrome remote-debugging port. Default 9222. */
  cdpPort?: number
  /** Adapter listen address (host:port). Default "127.0.0.1:8788". */
  adapterAddr?: string
  /** Per-turn paste budget passed to the adapter. Default 200000. */
  maxChars?: number
  /** When true, sets CHATGPT_BRIDGE_PROJECT_PER_SESSION=1 for the adapter. Default false. */
  projectPerSession?: boolean
  /**
   * Keep the plugin's 60s process janitor running: it relaunches Chrome and the
   * adapter whenever they are gone. Set false when something else owns their
   * lifecycle (launchd, a service manager, or you), so quitting Chrome stays
   * quit. Default true.
   */
  supervise?: boolean
}

export interface ResolvedOptions {
  bridgeSource?: BridgeSource
  releasesRepo: string
  version: string
  profileDir: string
  cdpPort: number
  adapterHost: string
  adapterPort: number
  maxChars: number
  projectPerSession: boolean
  supervise: boolean
}

export const DEFAULTS = {
  releasesRepo: "bojackduy/chatgpt-proxy",
  version: "latest",
  profileDir: path.join(os.homedir(), ".chatgpt-proxy"),
  cdpPort: 9222,
  adapterAddr: "127.0.0.1:8788",
  maxChars: 200000,
  projectPerSession: false,
} as const

export function expandHome(p: string): string {
  if (p === "~") return os.homedir()
  if (p.startsWith("~/")) return path.join(os.homedir(), p.slice(2))
  return p
}

export function resolveOptions(raw?: Record<string, unknown>): ResolvedOptions {
  const o = (raw ?? {}) as Partial<ChatGPTPluginOptions>
  const adapterAddr =
    typeof o.adapterAddr === "string" && o.adapterAddr.trim() !== ""
      ? o.adapterAddr.trim()
      : DEFAULTS.adapterAddr
  const { adapterHost, adapterPort } = splitHostPort(adapterAddr)
  const cdpPort =
    typeof o.cdpPort === "number" && Number.isInteger(o.cdpPort) && o.cdpPort > 0
      ? o.cdpPort
      : DEFAULTS.cdpPort
  const maxChars =
    typeof o.maxChars === "number" && Number.isFinite(o.maxChars) && o.maxChars > 0
      ? Math.floor(o.maxChars)
      : DEFAULTS.maxChars
  return {
    ...(o.bridgeSource === undefined ? {} : { bridgeSource: validateSource(o.bridgeSource) }),
    releasesRepo:
      typeof o.releasesRepo === "string" && o.releasesRepo.trim() !== ""
        ? o.releasesRepo.trim()
        : DEFAULTS.releasesRepo,
    version:
      typeof o.version === "string" && o.version.trim() !== ""
        ? o.version.trim()
        : DEFAULTS.version,
    profileDir:
      typeof o.profileDir === "string" && o.profileDir.trim() !== ""
        ? expandHome(o.profileDir.trim())
        : DEFAULTS.profileDir,
    cdpPort,
    adapterHost,
    adapterPort,
    maxChars,
    projectPerSession: o.projectPerSession === true,
    supervise: o.supervise !== false,
  }
}

export function splitHostPort(addr: string): { adapterHost: string; adapterPort: number } {
  const idx = addr.lastIndexOf(":")
  if (idx <= 0) return { adapterHost: addr, adapterPort: 8788 }
  const port = Number(addr.slice(idx + 1))
  if (!Number.isInteger(port) || port <= 0) return { adapterHost: addr, adapterPort: 8788 }
  return { adapterHost: addr.slice(0, idx), adapterPort: port }
}
