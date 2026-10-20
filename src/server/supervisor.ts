// Process supervision: keep the managed adapter listening and Chrome CDP
// answering while the plugin lives. All ports/paths/flags overridable via
// ResolvedOptions. No timers survive dispose().

import * as net from "node:net"
import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import { spawn, type ChildProcess } from "node:child_process"
import type { ResolvedOptions } from "./options"

export interface ProbeResult {
  ok: boolean
  detail: string
}

export function tcpProbe(host: string, port: number, timeoutMs = 2000): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port })
    const done = (value: boolean) => {
      socket.destroy()
      resolve(value)
    }
    socket.setTimeout(timeoutMs)
    socket.once("connect", () => done(true))
    socket.once("timeout", () => done(false))
    socket.once("error", () => done(false))
  })
}

/** GET http://host:port/path with a short timeout; never throws. */
export async function httpGet(
  host: string,
  port: number,
  reqPath: string,
  timeoutMs = 5000,
): Promise<{ status: number; body: string }> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetch(`http://${host}:${port}${reqPath}`, {
      signal: controller.signal,
    })
    return { status: res.status, body: (await res.text()).slice(0, 2000) }
  } finally {
    clearTimeout(timer)
  }
}

/**
 * JSON request to the local adapter. Throws only when the adapter cannot be
 * reached at all — a non-2xx status is returned to the caller, because the
 * recovery endpoints spell their refusals in the body.
 */
export async function httpJSON(
  host: string,
  port: number,
  method: "GET" | "POST",
  reqPath: string,
  headers: Record<string, string>,
  body?: unknown,
  timeoutMs = 10_000,
): Promise<{ status: number; text: string }> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetch(`http://${host}:${port}${reqPath}`, {
      method,
      headers: body === undefined ? headers : { ...headers, "content-type": "application/json" },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      signal: controller.signal,
    })
    return { status: res.status, text: (await res.text()).slice(0, 4000) }
  } finally {
    clearTimeout(timer)
  }
}

/** Readiness via the adapter's /readyz endpoint. */
export async function probeReadiness(
  host: string,
  port: number,
): Promise<ProbeResult> {
  if (!(await tcpProbe(host, port))) {
    return { ok: false, detail: `adapter not listening on ${host}:${port}` }
  }
  try {
    const { status, body } = await httpGet(host, port, "/readyz")
    if (status === 200) return { ok: true, detail: `ready (${body || "ok"})` }
    return { ok: false, detail: `/readyz -> HTTP ${status}: ${body}` }
  } catch (error) {
    return { ok: false, detail: `readyz probe failed: ${describeError(error)}` }
  }
}

/** Check that Chrome CDP answers on the debugging port. */
export async function probeChromeCDP(port: number): Promise<ProbeResult> {
  try {
    const { status, body } = await httpGet("127.0.0.1", port, "/json/version")
    if (status === 200 && body.includes("Browser")) return { ok: true, detail: "CDP answering" }
    return { ok: false, detail: `CDP /json/version -> HTTP ${status}` }
  } catch (error) {
    return { ok: false, detail: `CDP not answering on ${port}: ${describeError(error)}` }
  }
}

/**
 * Detect a Chrome binary per-OS, including macOS app bundles and Windows
 * install locations. `platform`/`env` are injectable so unit tests can cover
 * every OS without a matching box.
 *
 * Lookup order per platform: well-known install paths first, then the
 * CHROME_BIN env override (kept as the universal escape hatch on all
 * platforms — set it when Chrome lives somewhere unusual).
 *
 * Node API notes:
 * - On win32, executability is the `.exe` extension, not a mode bit, so
 *   candidates are checked with F_OK (existence); X_OK is used on posix.
 * - `process.env` on Windows already merges per-user + system env, so
 *   PROGRAMFILES / PROGRAMFILES(X86) / LOCALAPPDATA resolve correctly.
 * - Spawned children use an argv array (no shell), so `C:\Program Files\…`
 *   paths with spaces are safe.
 */
export function findChromeBinary(
  platform: string = process.platform,
  env: NodeJS.ProcessEnv = process.env,
): string | undefined {
  const candidates: string[] = chromeCandidates(platform, env)
  // X_OK is existence-only on win32 and F_OK would ignore a stripped exec
  // bit on posix — pick the meaningful check per OS family.
  const checkMode = platform === "win32" ? fs.constants.F_OK : fs.constants.X_OK
  for (const c of candidates) {
    try {
      fs.accessSync(c, checkMode)
      return c
    } catch {
      // try next
    }
  }
  const override = env["CHROME_BIN"]?.trim()
  return override ? override : undefined
}

/** Well-known Chrome install paths per OS (exported for unit tests). */
export function chromeCandidates(platform: string, env: NodeJS.ProcessEnv): string[] {
  if (platform === "darwin") {
    return [
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
      path.join(os.homedir(), "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
      "/Applications/Chromium.app/Contents/MacOS/Chromium",
    ]
  }
  if (platform === "win32") {
    // Env fallbacks cover atypical drives/layouts (injected env makes this
    // unit-testable); hardcoded C: roots are the last resort.
    const programFiles = env["PROGRAMFILES"]?.trim() || "C:\\Program Files"
    const programFilesX86 = env["PROGRAMFILES(X86)"]?.trim() || "C:\\Program Files (x86)"
    const localAppData = env["LOCALAPPDATA"]?.trim() || path.join(os.homedir(), "AppData", "Local")
    return [
      path.join(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
      path.join(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
      path.join(localAppData, "Google", "Chrome", "Application", "chrome.exe"),
    ]
  }
  // linux + anything else posix-ish: distro packages, /opt bundles, snap,
  // and local builds. Covers Debian/Fedora (google-chrome / google-chrome-stable),
  // Chromium renames (chromium vs chromium-browser), the /opt bundle layout,
  // snap (`/snap/bin/*`), and hand-built installs in /usr/local + ~/.local.
  // XDG note: these are system install locations, not XDG_DATA_HOME — no
  // XDG lookup needed for the browser itself.
  return [
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/opt/google/chrome/google-chrome",
    "/opt/google/chrome/chrome",
    "/usr/local/bin/google-chrome",
    "/usr/local/bin/chromium",
    "/snap/bin/chromium",
    "/snap/bin/google-chrome",
    path.join(os.homedir(), ".local", "bin", "google-chrome"),
    path.join(os.homedir(), ".local", "bin", "chromium"),
  ]
}

/**
 * How dispose() should stop supervised children on this platform.
 *
 * - posix: kill the negative pid (the detached process group) so grandchildren
 *   the adapter/Chrome forked die too; falls back to plain child kill.
 * - win32: plain child kill only — negative pids / process-group signals are
 *   POSIX-only and `process.kill(-pid)` throws EINVAL on Windows.
 */
export function disposeMode(platform: string = process.platform): "process-group" | "plain" {
  return platform === "win32" ? "plain" : "process-group"
}

export interface Supervisor {
  dispose(): Promise<void>
}

interface SupervisedChild {
  proc: ChildProcess
  logFile: string
}

/**
 * Ensure (a) the adapter listens on the configured addr and (b) Chrome CDP
 * answers on cdpPort. Spawns managed processes detached with stdout to a log
 * file; respawns with backoff while the supervisor lives.
 */
export function startSupervisor(opts: ResolvedOptions, binaries: { openai: string }): Supervisor {
  let stopped = false
  let sourceConflictReported = false
  const children: SupervisedChild[] = []
  const logDir = path.join(os.homedir(), ".local", "share", "opencode-web-bridges", "logs")

  const launch = (
    name: string,
    command: string,
    args: string[],
    env: NodeJS.ProcessEnv,
  ): void => {
    if (stopped) return
    try {
      fs.mkdirSync(logDir, { recursive: true })
    } catch {
      // best effort
    }
    const logFile = path.join(logDir, `${name}.log`)
    let out: number | undefined
    try {
      out = fs.openSync(logFile, "a")
    } catch {
      out = undefined
    }
    const proc = spawn(command, args, {
      detached: true,
      stdio: [ "ignore", out ?? "ignore", out ?? "ignore" ],
      env,
    })
    proc.unref()
    if (out !== undefined) {
      try {
        fs.closeSync(out)
      } catch {
        // ignore
      }
    }
    children.push({ proc, logFile })
    proc.once("exit", () => {
      if (stopped) return
      // Respawn with backoff while the plugin lives.
      setTimeout(() => {
        if (!stopped) void ensureOnce().catch(() => {})
      }, 5000).unref?.()
    })
  }

  const ensureOnce = async (): Promise<void> => {
    if (stopped) return
    // (b) Chrome CDP first — the adapter needs a browser to drive.
    // Note: opts.profileDir defaults to ~/.chatgpt-proxy via os.homedir(),
    // which resolves on every platform (on win32: %USERPROFILE%\.chatgpt-proxy)
    // — no per-OS profile handling needed here.
    const cdp = await probeChromeCDP(opts.cdpPort)
    if (!cdp.ok) {
      const chrome = findChromeBinary()
      if (chrome) {
        launch("chrome", chrome, [
          `--remote-debugging-port=${opts.cdpPort}`,
          `--user-data-dir=${opts.profileDir}`,
          "--no-first-run",
          "--no-default-browser-check",
          "about:blank",
        ], process.env)
      }
    }
    // (a) Adapter.
    if (!(await tcpProbe(opts.adapterHost, opts.adapterPort, 1000))) {
      const env: NodeJS.ProcessEnv = {
        ...process.env,
        ...(opts.projectPerSession ? { CHATGPT_BRIDGE_PROJECT_PER_SESSION: "1" } : {}),
      }
      launch("chatgpt-openai", binaries.openai, [
        "-addr",
        `${opts.adapterHost}:${opts.adapterPort}`,
        "-max-chars",
        String(opts.maxChars),
        "-chrome-port",
        String(opts.cdpPort),
        "-profile",
        opts.profileDir,
      ], env)
      if (opts.bridgeSource) console.log(`[opencode-web-bridges] selected source adapter executable: ${binaries.openai}`)
    } else if (opts.bridgeSource && !sourceConflictReported && !children.some(c => c.proc.spawnfile === binaries.openai && c.proc.exitCode === null)) {
      sourceConflictReported = true
      console.error(`[opencode-web-bridges] adapter port already occupied by an independently running process; selected source build is NOT verified as running: ${binaries.openai}. Ask the service owner to stop/reconfigure it, then restart OpenCode. No launchd/service was killed or overwritten.`)
    }
  }

  // Initial ensure + periodic reconciliation (60s). Timer is unref'd and
  // cleared on dispose so nothing survives plugin unload.
  void ensureOnce().catch(() => {})
  const timer = setInterval(() => {
    void ensureOnce().catch(() => {})
  }, 60_000)
  timer.unref?.()

  return {
    async dispose(): Promise<void> {
      stopped = true
      clearInterval(timer)
      const mode = disposeMode()
      for (const c of children) {
        if (mode === "process-group") {
          try {
            // Negative pid targets the detached process group.
            if (c.proc.pid !== undefined) process.kill(-c.proc.pid, "SIGTERM")
            continue
          } catch {
            // Fall through to the plain child kill (group already gone,
            // ESRCH) — same path win32 always takes.
          }
        }
        try {
          c.proc.kill("SIGTERM")
        } catch {
          // already gone
        }
      }
    },
  }
}

export function describeError(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}

export function loginInstructions(opts: ResolvedOptions): string {
  return [
    "ChatGPT Web bridge is NOT logged in.",
    "",
    "A Chrome window using the dedicated profile should now be open.",
    "Log into ChatGPT there ONCE — the session persists in the profile",
    `(${opts.profileDir}) and is reused forever.`,
    "",
    "Then re-run /chatgpt-status. If no window appeared, run /chatgpt-login.",
  ].join("\n")
}
