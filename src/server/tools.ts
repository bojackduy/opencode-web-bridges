// Tool implementations for the chatgpt-* commands.
// Probe helpers take explicit parameters so they stay testable without a
// live browser.

import * as fs from "node:fs"
import { spawn } from "node:child_process"
import type { ResolvedOptions } from "./options"
import { cacheDir } from "./binaries"
import {
  findChromeBinary,
  httpGet,
  httpJSON,
  probeChromeCDP,
  probeReadiness,
  tcpProbe,
  describeError,
} from "./supervisor"

export interface DoctorLine {
  name: string
  ok: boolean
  detail: string
  hint?: string
}

export async function statusReport(
  opts: ResolvedOptions,
  binaries: { openai: string; proxy: string; version: string },
): Promise<string> {
  const lines: string[] = [
    `version: ${binaries.version} (repo: plugin-managed binaries)`,
    `openai adapter binary: ${binaries.openai}`,
    `proxy MCP binary: ${binaries.proxy}`,
  ]
  const ready = await probeReadiness(opts.adapterHost, opts.adapterPort)
  lines.push(ready.ok ? `ready: ${ready.detail}` : `NOT READY: ${ready.detail}`)
  const cdp = await probeChromeCDP(opts.cdpPort)
  lines.push(cdp.ok ? `chrome CDP (:${opts.cdpPort}): answering` : `chrome CDP (:${opts.cdpPort}): ${cdp.detail}`)
  return lines.join("\n")
}

export async function doctorReport(
  opts: ResolvedOptions,
  binaries: { openai: string; proxy: string; version: string },
): Promise<DoctorLine[]> {
  const lines: DoctorLine[] = []
  // Platform-aware redownload hint (LOCALAPPDATA on win32, ~/.local/share elsewhere).
  const redownloadHint =
    `Delete the versioned cache dir (${cacheDir(binaries.version)}) and restart opencode to re-download.`
  for (const [name, p] of [["chatgpt-openai", binaries.openai], ["chatgpt-proxy", binaries.proxy]] as const) {
    try {
      await fs.promises.access(p, fs.constants.X_OK)
      lines.push({ name: `binary present+executable: ${name}`, ok: true, detail: p })
    } catch {
      lines.push({
        name: `binary present+executable: ${name}`,
        ok: false,
        detail: `missing or not executable: ${p}`,
        hint: redownloadHint,
      })
    }
  }
  const portOpen = await tcpProbe(opts.adapterHost, opts.adapterPort, 1500)
  lines.push({
    name: `adapter port open (${opts.adapterHost}:${opts.adapterPort})`,
    ok: portOpen,
    detail: portOpen ? "listening" : "not listening",
    hint: portOpen ? undefined : "Restart opencode — the plugin supervisor spawns the adapter. Check ~/.local/share/opencode-web-bridges/logs/chatgpt-openai.log.",
  })
  const cdp = await probeChromeCDP(opts.cdpPort)
  lines.push({
    name: `chrome answering (CDP :${opts.cdpPort})`,
    ok: cdp.ok,
    detail: cdp.detail,
    hint: cdp.ok ? undefined : "Run /chatgpt-login to launch the profile Chrome.",
  })
  let adapterVersion = "unknown"
  try {
    const { status, body } = await httpGet(opts.adapterHost, opts.adapterPort, "/healthz", 3000)
    adapterVersion = status === 200 ? (body || "ok") : `HTTP ${status}`
  } catch (error) {
    adapterVersion = `unreachable: ${describeError(error)}`
  }
  const ready = portOpen ? await probeReadiness(opts.adapterHost, opts.adapterPort) : null
  const loggedIn = ready?.ok === true
  lines.push({
    name: "adapter version (/healthz)",
    ok: portOpen,
    detail: adapterVersion,
  })
  lines.push({
    name: "login state (/readyz)",
    ok: loggedIn,
    detail: ready ? ready.detail : "adapter down — cannot determine login state",
    hint: loggedIn
      ? undefined
      : "Log into ChatGPT once in the profile Chrome window (/chatgpt-login opens it), then re-run /chatgpt-status.",
  })
  return lines
}

export function formatDoctor(lines: DoctorLine[]): string {
  return lines
    .map((l) => `${l.ok ? "PASS" : "FAIL"}  ${l.name}: ${l.detail}${l.hint ? `\n      repair: ${l.hint}` : ""}`)
    .join("\n")
}

/** Launch the profile Chrome window so the user can log in, then re-probe. */
export async function loginFlow(opts: ResolvedOptions): Promise<string> {
  const chrome = findChromeBinary()
  if (!chrome) {
    return [
      "Could not find a Chrome binary.",
      "Install Google Chrome, or set CHROME_BIN to its path, then retry /chatgpt-login.",
    ].join("\n")
  }
  const child = spawn(
    chrome,
    [
      `--remote-debugging-port=${opts.cdpPort}`,
      `--user-data-dir=${opts.profileDir}`,
      "--no-first-run",
      "--no-default-browser-check",
      "https://chatgpt.com",
    ],
    { detached: true, stdio: "ignore" },
  )
  child.unref()
  // Give Chrome a moment, then re-probe CDP + readiness.
  await new Promise((r) => setTimeout(r, 2500))
  const cdp = await probeChromeCDP(opts.cdpPort)
  const ready = await probeReadiness(opts.adapterHost, opts.adapterPort)
  return [
    `profile Chrome launched (pid ${child.pid ?? "unknown"}). Log into ChatGPT there once —`,
    `the session persists in ${opts.profileDir}.`,
    `CDP (:${opts.cdpPort}): ${cdp.detail}`,
    ready.ok ? `bridge: ${ready.detail}` : `bridge: ${ready.detail} — re-run /chatgpt-status after logging in.`,
  ].join("\n")
}

// ─── Recovery UX ─────────────────────────────────────────────────────────────
// The user-facing consumer of the adapter's recovery contract:
//
//   GET  /v1/sessions/state    one chat's state, content-free
//   POST /v1/sessions/recover  read-only adoption of an unanswered message
//   POST /v1/sessions/abandon  deliberate clear of the uncertainty binding
//
// Nothing here submits a turn, re-runs a historical tool, or clears anything
// the caller did not name. Every report is enums, counts and sizes — never
// prompt text, tool arguments, tool call ids or media (the adapter is
// content-free for the same reason).

export type RecoverAction = "inspect" | "recover" | "abandon"

export interface RecoverArgs {
  action?: string
  session?: string
  reason?: string
}

/** One adapter call. Injectable so tests never touch the network. */
export type AdapterRequest = (
  method: "GET" | "POST",
  reqPath: string,
  session: string,
  body?: unknown,
  timeoutMs?: number,
) => Promise<{ status: number; text: string }>

function adapterRequest(opts: ResolvedOptions): AdapterRequest {
  return bridgeRequest(`http://${opts.adapterHost}:${opts.adapterPort}/v1`)
}

/**
 * One adapter call against an explicit base URL. The ChatGPT and Gemini
 * adapters share the recovery endpoint shapes, so one helper serves both.
 */
export function bridgeRequest(baseURL: string): AdapterRequest {
  const url = new URL(baseURL)
  if (url.protocol !== "http:") throw new Error("adapter baseURL must be http")
  const prefix = url.pathname.replace(/\/$/, "")
  return (method, reqPath, session, body, timeoutMs) => {
    // Adapter base URLs end in /v1 and request paths start with /v1/;
    // join without doubling the prefix.
    const path =
      prefix !== "" && !reqPath.startsWith(prefix + "/") ? prefix + reqPath : reqPath
    return httpJSON(
      url.hostname,
      Number(url.port) || 80,
      method,
      path,
      { "x-opencode-session-id": session },
      body,
      timeoutMs,
    )
  }
}

const RECOVER_ACTIONS: RecoverAction[] = ["inspect", "recover", "abandon"]

function isRecoverAction(value: string): value is RecoverAction {
  return (RECOVER_ACTIONS as string[]).includes(value)
}

/** Wildcards and "all"/"any" are refused rather than resolved to anything. */
function unusableID(id: string): string {
  if (id.includes("*")) return `${JSON.stringify(id)} contains a wildcard`
  if (/^(all|any)$/i.test(id)) return `${JSON.stringify(id)} is not a chat id`
  return ""
}

/**
 * Which chat one call acts on. The calling session (the id the client already
 * sends to the adapter) is the default; an explicitly named session overrides
 * it. Clearing is stricter: it always needs a session named in the call, never
 * whatever chat happens to be current.
 */
export function resolveRecoverSession(
  action: RecoverAction,
  calling: string | undefined,
  explicit: string | undefined,
): { session: string } | { problem: string } {
  const named = (explicit ?? "").trim()
  if (action === "abandon" && named === "") {
    return {
      problem: [
        "Nothing was cleared: clearing a chat needs that chat named explicitly in the call.",
        "Use action 'inspect' to see the chat id, then repeat with the optional session set to it.",
      ].join("\n"),
    }
  }
  const id = named !== "" ? named : (calling ?? "").trim()
  const target = action === "abandon" ? "the chat to clear" : "the chat to look at"
  if (id === "") {
    return {
      problem: [
        `This tool does not know ${target}: the call carried no session id and none was named.`,
        "Pass the chat id as the optional session argument and try again.",
      ].join("\n"),
    }
  }
  const why = unusableID(id)
  if (why !== "") return { problem: `Nothing was done: ${why}. One chat has to be named exactly.` }
  return { session: id }
}

// One plain-English sentence plus an actionable next step, keyed by how the
// adapter refused. The status itself is never the headline; it goes in the
// detail line below the report.
function failureAdvice(
  action: RecoverAction,
  status: number,
  adapterMessage: string,
): { sentence: string; next: string } {
  if (status === 403) {
    return {
      sentence: "The bridge refused: this request looked like it came from a web page instead of OpenCode.",
      next: "Run it again from OpenCode — nothing else needs changing.",
    }
  }
  if (status === 503) {
    return {
      sentence: "The bridge is busy with a live reply, so nothing was changed.",
      next: "Wait for the current reply to finish, then try again.",
    }
  }
  if (status === 400) {
    return {
      sentence: "The bridge rejected the request before changing anything.",
      next: "Use action 'inspect' to see the chat id, then repeat with that id named explicitly.",
    }
  }
  if (status === 404 || status === 405) {
    return {
      sentence: "This bridge build does not support that action.",
      next: "Update the browser-bridge plugin and its bridge binary, then restart OpenCode.",
    }
  }
  if (status === 500) {
    return {
      sentence: "The bridge could not save the change, so nothing was changed.",
      next: "Try once more; if it keeps failing, check the bridge log.",
    }
  }
  if (status === 422) {
    return action === "recover"
      ? {
          sentence: "The saved reply cannot be used, and nothing was re-sent and no tool was run.",
          next: "The chat can continue with a new message — nothing needs clearing.",
        }
      : {
          sentence: "The bridge cannot clear this chat as it is, and nothing was changed.",
          next: "Use action 'inspect' to see whether this chat still needs clearing.",
        }
  }
  if (status === 409) {
    if (action === "recover" && adapterMessage.includes("MISMATCH")) {
      // The sent-log proves the thread never received the exact pending
      // message (typically an interrupted multi-chunk submission), so no
      // amount of retrying will make a reply readable for it.
      return {
        sentence:
          "Your message never reached ChatGPT in full: the thread holds something else, so there is no reply that belongs to it.",
        next: "This turn cannot be read back and retrying will not change that. Clear it — start a new chat from the recorded history, or clear the unconfirmed turn — then send a new message; completed messages are kept.",
      }
    }
    if (action === "recover" && adapterMessage.includes("not fully rendered yet")) {
      return {
        sentence: "The saved ChatGPT page had not finished drawing that turn, so it could not be compared yet.",
        next: "Nothing was re-sent and no tool was run. Wait a few seconds, then run recovery again.",
      }
    }
    if (action === "recover") {
      return {
        sentence: "The bridge could not match the unanswered message to a saved reply, so there is nothing it can read back.",
        next: "Nothing was re-sent and no tool was run. Use action 'inspect' to see what is still possible for this chat.",
      }
    }
    return {
      sentence: "The bridge cannot do that for this chat in its current state.",
      next: "Use action 'inspect' to see what is actually possible for this chat.",
    }
  }
  return {
    sentence: "The bridge refused the request.",
    next: "Use action 'inspect' to see the current state of the chat.",
  }
}

function failureReport(action: RecoverAction, status: number, adapterMessage: string): string {
  const advice = failureAdvice(action, status, adapterMessage)
  const said = adapterMessage === "" ? "" : ` The bridge said: ${adapterMessage}`
  return [advice.sentence, advice.next, "", `Details: the bridge answered with status ${status}.${said}`].join("\n")
}

function unreachableReport(action: RecoverAction, error: unknown): string {
  const next =
    action === "inspect"
      ? "Check the bridge with the chatgpt-status tool, then try again."
      : "Nothing was changed. Check the bridge with the chatgpt-status tool, then try again."
  return [
    "The ChatGPT bridge is not answering, so nothing was done.",
    next,
    "",
    `Details: the bridge could not be reached (${describeError(error)}).`,
  ].join("\n")
}

function parseBody(text: string): Record<string, unknown> {
  try {
    const parsed = JSON.parse(text)
    return parsed !== null && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

function numberField(body: Record<string, unknown>, key: string): number {
  const value = body[key]
  return typeof value === "number" && Number.isFinite(value) ? value : 0
}

/** The adapter's own error message, if the body carries one. */
function adapterMessage(body: Record<string, unknown>): string {
  const error = body["error"]
  if (error === null || typeof error !== "object") return ""
  const message = (error as Record<string, unknown>)["message"]
  return typeof message === "string" ? message : ""
}

// One line each, no trailing full stop: the report adds it.
const STATE_UNKNOWN: Record<string, string> = {
  clean: "Nothing — the last reply was received and checked",
  pending_verifiable:
    "Whether the last message you sent was ever answered. If an answer exists, it can still be read back from ChatGPT without sending anything again",
  pending_unverifiable:
    "Whether the last message you sent was ever answered, and what it said. It cannot be read back, so this chat has to be cleared deliberately before it can be used again",
  rejected_output:
    "Nothing about delivery: the model's last reply did arrive, but it could not be used, and nothing was run",
  empty_final: "Nothing about delivery: the model's last reply did arrive, but it was empty, and nothing was run",
  unknown_session: "Everything — the bridge has no stored chat for this session yet",
}

const ACTION_WORDS: Record<string, string> = {
  continue: "just send your next message — this chat works normally",
  recover: "read the previous reply back from ChatGPT without sending anything (action 'recover')",
  resync:
    "start a fresh working chat from the recorded history (action 'resync') — a deliberate boundary, not proof the old turn succeeded",
  abandon: "clear the unconfirmed turn so the chat can be used again (action 'abandon', naming this chat)",
}

function count(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`
}

function keptLine(s: Record<string, unknown>): string {
  const parts: string[] = []
  const completed = numberField(s, "completed_messages")
  const pendingBytes = numberField(s, "pending_prompt_bytes")
  const outstanding = numberField(s, "outstanding_tool_calls")
  if (completed > 0) parts.push(count(completed, "completed message"))
  if (s["thread_url_present"] === true) parts.push("the link to its ChatGPT conversation")
  if (pendingBytes > 0) parts.push(`the exact text of the unanswered message (${pendingBytes} bytes)`)
  if (outstanding > 0) parts.push(`${count(outstanding, "tool call")} handed out with no recorded result`)
  if (parts.length === 0) return "nothing has been recorded for this chat yet"
  return parts.join(", ")
}

function actionsLine(s: Record<string, unknown>): string {
  const raw = Array.isArray(s["actions"]) ? (s["actions"] as unknown[]) : []
  const words = raw
    .filter((a): a is string => typeof a === "string")
    .map((a) => ACTION_WORDS[a] ?? `an action this plugin does not recognise (${a})`)
  if (words.length === 0) return "nothing right now — the bridge offered no action for this chat"
  return words.join("; ")
}

export function formatRecoverInspect(session: string, s: Record<string, unknown>): string {
  const state = typeof s["state"] === "string" ? s["state"] : ""
  const summary = typeof s["summary"] === "string" && s["summary"] !== "" ? s["summary"] : "the bridge did not describe this chat"
  const lines = [
    `Chat: ${session}`,
    `What this means: ${summary}`,
    `What is unknown: ${STATE_UNKNOWN[state] ?? "the bridge reported a state this plugin does not recognise, so treat this chat as uncertain"}.`,
    `What is kept: ${keptLine(s)}.`,
    `What you can do: ${actionsLine(s)}.`,
  ]
  if (numberField(s, "outstanding_tool_calls") > 0) {
    lines.push("Note: tool calls that were already handed out are never re-run, whatever you choose here.")
  }
  return lines.join("\n")
}

export function formatRecovered(session: string, s: Record<string, unknown>): string {
  const calls = numberField(s, "cached_tool_calls")
  const bytes = numberField(s, "response_bytes")
  const held = bytes > 0 ? `${bytes} bytes of reply text and ${count(calls, "tool call")}` : count(calls, "tool call")
  const auto = s["auto_resync"] !== undefined
  return [
    `Recovered: the previous reply was read back from the saved ChatGPT conversation for ${session} and checked against the message that was sent.`,
    auto
      ? "The reply was matched to this chat from the recorded history and is now held by the bridge."
      : `Nothing was sent again and no tool was run. The reply is now held by the bridge: ${held}.`,
    "",
    "Next: send the same message again, unchanged — the bridge returns the stored reply instead of asking ChatGPT.",
  ].join("\n")
}

export function formatAbandon(session: string, s: Record<string, unknown>): string {
  const results = Array.isArray(s["results"]) ? (s["results"] as unknown[]) : []
  const rows = results.filter((r): r is Record<string, unknown> => r !== null && typeof r === "object")
  const row = rows.find((r) => r["session"] === session) ?? rows[0]
  if (row === undefined) {
    return `The bridge did not report on ${session}, so there is nothing to confirm was cleared.`
  }
  const outcome = typeof row["outcome"] === "string" ? row["outcome"] : ""
  if (outcome === "not_pending") {
    return [
      `Nothing was cleared: ${session} had no unconfirmed turn.`,
      "Its completed messages, conversation link and every recorded tool result are untouched.",
    ].join("\n")
  }
  if (outcome === "unknown") {
    return [
      `Nothing was cleared: the bridge has no stored chat for ${session}.`,
      "Check the chat id with action 'inspect' and try again.",
    ].join("\n")
  }
  const bytes = numberField(row, "abandoned_prompt_bytes")
  const delivered = numberField(row, "delivered_calls")
  const recorded = numberField(row, "recorded_results")
  const outstanding = numberField(row, "outstanding_calls")
  const lines = [
    `Cleared the unconfirmed turn for ${session}.`,
    `Dropped: the uncertainty about one unanswered message${bytes > 0 ? ` (${bytes} bytes of prompt text)` : ""} and the binding that let the bridge check it.`,
    `Kept: ${count(delivered, "delivered tool call")}, ${count(recorded, "recorded result")}, and all completed messages and the conversation link.`,
  ]
  if (outstanding > 0) {
    lines.push(
      `Not re-run: ${count(outstanding, "tool call")} had no recorded result and will not be executed again — historical tools are never re-run.`,
    )
  } else {
    lines.push("Not re-run: no historical tool was executed by this clear.")
  }
  lines.push("This clears uncertainty only. It is not proof the previous reply arrived.")
  return lines.join("\n")
}

/**
 * One user-facing recovery action against the managed adapter. `callingSession`
 * is the session id OpenCode attached to the call (or the v2 command's
 * session). Resolves to a plain-English report in every case — a missing
 * session, an unreachable adapter and an adapter refusal are all reports, not
 * thrown errors.
 */
export async function recoverReport(
  opts: ResolvedOptions,
  args: RecoverArgs,
  callingSession?: string,
  request: AdapterRequest = adapterRequest(opts),
): Promise<string> {
  const raw = (args.action ?? "inspect").trim().toLowerCase()
  if (!isRecoverAction(raw)) {
    return [
      `"${raw}" is not a recovery action this tool knows.`,
      `Use one of: ${RECOVER_ACTIONS.join(", ")} (the default is inspect).`,
    ].join("\n")
  }
  const resolved = resolveRecoverSession(raw, callingSession, args.session)
  if ("problem" in resolved) return resolved.problem
  const session = resolved.session

  try {
    if (raw === "inspect") {
      const res = await request("GET", "/v1/sessions/state", session, undefined, 15_000)
      const body = parseBody(res.text)
      if (res.status !== 200) return failureReport(raw, res.status, adapterMessage(body))
      return formatRecoverInspect(session, body)
    }
    if (raw === "recover") {
      // The adapter waits up to 90s for a stored answer to finish; give it room.
      const res = await request("POST", "/v1/sessions/recover", session, undefined, 120_000)
      const body = parseBody(res.text)
      if (res.status !== 200) return failureReport(raw, res.status, adapterMessage(body))
      return formatRecovered(session, body)
    }
    const res = await request(
      "POST",
      "/v1/sessions/abandon",
      session,
      { sessions: [session], reason: (args.reason ?? "").trim() },
      20_000,
    )
    const body = parseBody(res.text)
    if (res.status !== 200) return failureReport(raw, res.status, adapterMessage(body))
    return formatAbandon(session, body)
  } catch (error) {
    return unreachableReport(raw, error)
  }
}
