// Content-free flow log for the Web bridges plugin.
//
// One JSONL line per step: agent tool call → plugin execute → adapter HTTP →
// supervisor spawn → palette command. Sizes, counts, durations, statuses and
// reason codes only — never prompt text, tool arguments, bodies, or replies.
//
// Two sinks, both best-effort and never throwing:
//   1. `opencode logs` streaming via console.log with the [flow] prefix.
//   2. A persistent file for post-mortem: tail it while reproducing a bug.
//
//   tail -f ~/.local/share/opencode-web-bridges/logs/plugin-flow.jsonl
//
// Env overrides (tests / debugging):
//   OPENCODE_WEB_BRIDGES_FLOW_LOG     redirect the file sink
//   OPENCODE_WEB_BRIDGES_FLOW_SILENT=1  suppress the console sink

import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"

export type FlowActor =
  | "agent-tool" // an opencode tool call into this plugin
  | "plugin" // plugin execute body (v1 tool or v2 command)
  | "adapter-http" // one bridgeRequest HTTP round-trip
  | "supervisor" // managed process launch / exit / reconcile
  | "palette" // TUI palette command run

/** Content-free fields only. Never put prompt text, args, or bodies here. */
export interface FlowFields {
  session?: string
  tool?: string
  command?: string
  action?: string
  method?: string
  path?: string
  name?: string
  status?: number
  reqBytes?: number
  respBytes?: number
  durationMs?: number
  ok?: boolean
  outcome?: string
  error?: string
  pid?: number
  extra?: string
}

function isTestEnv(): boolean {
  return process.env["NODE_ENV"] === "test" || process.env["BUN_TEST"] === "1"
}

function flowLogFile(): string {
  const override = process.env["OPENCODE_WEB_BRIDGES_FLOW_LOG"]?.trim()
  if (override) return override
  // Keep `bun test` hermetic: never touch the real home log from tests.
  if (isTestEnv()) return path.join(os.tmpdir(), "opencode-web-bridges-flow-test.jsonl")
  return path.join(os.homedir(), ".local", "share", "opencode-web-bridges", "logs", "plugin-flow.jsonl")
}

function silentConsole(): boolean {
  if (isTestEnv()) return true
  return process.env["OPENCODE_WEB_BRIDGES_FLOW_SILENT"] === "1"
}

/** One flow step. Never throws; a broken sink must not break the bridge. */
export function logFlow(actor: FlowActor, event: string, fields: FlowFields = {}): void {
  const line = JSON.stringify({
    ts: new Date().toISOString(),
    pid: process.pid,
    actor,
    event,
    ...fields,
  })
  if (!silentConsole()) {
    try {
      console.log(`[opencode-web-bridges:flow] ${line}`)
    } catch {
      // ignore
    }
  }
  try {
    const file = flowLogFile()
    try {
      fs.mkdirSync(path.dirname(file), { recursive: true })
    } catch {
      // best effort
    }
    fs.appendFileSync(file, line + "\n")
  } catch {
    // ignore
  }
}

/** Truncate a system error to a content-free head for the log. */
export function errorHead(error: unknown, max = 160): string {
  const msg = error instanceof Error ? error.message : String(error)
  return msg.slice(0, max)
}

/** Current time in ms for duration arithmetic. */
export function flowNow(): number {
  return Date.now()
}
