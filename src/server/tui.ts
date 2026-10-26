// Native TUI command-palette recovery UI for the Web bridges.
//
// This is the human-operated surface: palette commands that run the adapter
// HTTP calls directly in the plugin host and show results in UI dialogs.
// No assistant turn, no LLM call, no chat completion and no session.prompt
// is involved in performing OR displaying the work — so it keeps working
// while the model is failed or out of quota.
//
// Shape notes (verified against the installed 2.0.26
// `node_modules/@opencode/plugin/dist/tui/context.d.ts` and sibling
// ../opencode-voice `lib/v2-compat.js`, which both call
// `context.keymap.layer` at the TOP LEVEL of the setup context):
// - `keymap.layer(() => ({mode: "global", commands}))` is top-level on the
//   setup context — never under `ui`. A fake missing `ui.keymap` must still
//   register, or the suite is hiding the defect again.
// - The layer must run inside a component: it is claimed from an `"app"`
//   slot null-render (`context.ui.slot`), the same mount trick voice uses.
// - Commands use `palette: true` and no keybinds (no collisions) and no
//   slash entry (the prompt-injection slash surface already exists elsewhere;
//   this module is the direct-execution path).
// - Dialogs are promise APIs (`alert`/`confirm`/`prompt`); no JSX needed.
// - `router.current()` resolves the default session: `{type:"session",
//   sessionID}` uses the viewed chat, anything else prompts for a chat id.
// - `data.session.status(id)` gates mutations while a turn is live.

import { resolveGemini } from "./gemini"
import { resolveOptions, type ResolvedOptions } from "./options"
import {
  bridgeRequest,
  recoverReport,
  type AdapterRequest,
  type RecoverArgs,
} from "./tools"

// ---------------------------------------------------------------------------
// Minimal structural mirror of the SDK TUI shapes this module touches.
// Full SDK imports are avoided on purpose: @opencode/theme, @opentui/* and
// solid-js are host-provided peers, absent from this repo's node_modules,
// so importing "@opencode/plugin/tui" types would break `tsc --noEmit`.
// Every field below was copied from the installed 2.0.26 dist .d.ts.
// ---------------------------------------------------------------------------

export interface PaletteDialog {
  alert(options: { title: string; message: string }): Promise<void>
  confirm(options: {
    title: string
    message: string
    label?: { confirm?: string; cancel?: string }
  }): Promise<boolean | undefined>
  prompt(options: {
    title: string
    description?: string
    placeholder?: string
    value?: string
  }): Promise<string | undefined>
}

export interface PaletteToast {
  show(options: {
    title?: string
    message: string
    variant?: "info" | "success" | "warning" | "error"
  }): void
}

export type PaletteRoute =
  | { type: "home" }
  | { type: "session"; sessionID: string }
  | { type: "plugin"; id: string; name: string; data?: Record<string, unknown> }

export interface PaletteUI {
  dialog: PaletteDialog
  toast: PaletteToast
  router: { current(): PaletteRoute }
  slot(claim: {
    append: "app"
    render: () => never
  }): () => void
}

export interface PaletteKeymapLayer {
  mode?: string
  commands?: PaletteCommand[]
}

export interface PaletteKeymap {
  layer(factory: () => PaletteKeymapLayer): void
}

export interface PaletteData {
  session: { status(sessionID: string): "idle" | "running" }
}

export interface PaletteCommand {
  id: string
  title: string
  description?: string
  group?: string
  palette: true
  run: (input?: string) => void | Promise<void>
}

export interface PaletteContext {
  options: Readonly<Record<string, unknown>>
  ui: PaletteUI
  data: PaletteData
  keymap: PaletteKeymap
}

// ---------------------------------------------------------------------------
// Command inventory
// ---------------------------------------------------------------------------

export const RECOVERY_GROUP = "Bridge recovery"

export const CMD_INSPECT_CHATGPT = "bridge-recovery.inspect-chatgpt"
export const CMD_RECOVER_CHATGPT = "bridge-recovery.recover-chatgpt"
export const CMD_ABANDON_CHATGPT = "bridge-recovery.abandon-chatgpt"
export const CMD_INSPECT_GEMINI = "bridge-recovery.inspect-gemini"
export const CMD_ABANDON_GEMINI = "bridge-recovery.abandon-gemini"

export interface RecoveryAdapters {
  chatgpt?: AdapterRequest
  gemini?: AdapterRequest
  opts: ResolvedOptions
}

export interface RecoveryDeps {
  ui: Pick<PaletteUI, "dialog" | "toast" | "router">
  data: PaletteData
  adapters: RecoveryAdapters
}

// ---------------------------------------------------------------------------
// Session resolution + live-turn guard (pure UI logic, no adapter calls)
// ---------------------------------------------------------------------------

export async function resolveTargetSession(
  ui: Pick<PaletteUI, "dialog" | "router">,
): Promise<string | undefined> {
  const route = ui.router.current()
  if (route.type === "session" && route.sessionID) return route.sessionID
  const entered = await ui.dialog.prompt({
    title: "Bridge recovery — chat id",
    description: "Not viewing a chat. Name the chat to inspect (never * / all / any).",
  })
  const id = (entered ?? "").trim()
  return id === "" ? undefined : id
}

export function sessionBusy(data: PaletteData, sessionID: string): boolean {
  try {
    return data.session.status(sessionID) === "running"
  } catch {
    // Unknown status must not wedge recovery: the adapter itself refuses
    // concurrent turns (abandon answers 503 while a turn is live).
    return false
  }
}

// ---------------------------------------------------------------------------
// Adapter reads (GET state only — never a submission, never a replay)
// ---------------------------------------------------------------------------

interface StateView {
  ok: boolean
  status: number
  state: string
  summary: string
  canRecover: boolean
  actions: string[]
}

async function fetchState(
  request: AdapterRequest,
  session: string,
): Promise<StateView> {
  const res = await request("GET", "/v1/sessions/state", session, undefined, 15_000)
  let body: Record<string, unknown> = {}
  try {
    body = JSON.parse(res.text) as Record<string, unknown>
  } catch {
    body = {}
  }
  const str = (v: unknown) => (typeof v === "string" ? v : "")
  const actions = Array.isArray(body.actions)
    ? body.actions.filter((a): a is string => typeof a === "string")
    : []
  return {
    ok: res.status === 200,
    status: res.status,
    state: str(body.state) || "unknown",
    summary: str(body.summary),
    canRecover: body.can_recover === true,
    actions,
  }
}

// ---------------------------------------------------------------------------
// Command implementations (all display via dialog/toast — never session.prompt)
// ---------------------------------------------------------------------------

async function runInspect(
  deps: RecoveryDeps,
  request: AdapterRequest,
  bridgeName: string,
): Promise<void> {
  const { ui } = deps
  const session = await resolveTargetSession(ui)
  if (!session) {
    ui.toast.show({ message: "Recovery cancelled — no chat named.", variant: "info" })
    return
  }
  const report = await recoverReport(deps.adapters.opts, { action: "inspect", session }, session, request)
  await ui.dialog.alert({ title: `Bridge recovery — inspect (${bridgeName})`, message: report })
}

async function runRecoverChatgpt(deps: RecoveryDeps, request: AdapterRequest): Promise<void> {
  const { ui, data } = deps
  const session = await resolveTargetSession(ui)
  if (!session) {
    ui.toast.show({ message: "Recovery cancelled — no chat named.", variant: "info" })
    return
  }
  if (sessionBusy(data, session)) {
    await ui.dialog.alert({
      title: "Bridge recovery — read back",
      message: `Chat ${session} has a live turn. Wait for idle, then run this again. Nothing was read and nothing changed.`,
    })
    return
  }
  const state = await fetchState(request, session)
  if (!state.ok || !state.canRecover) {
    const report = await recoverReport(
      deps.adapters.opts,
      { action: "inspect", session },
      session,
      request,
    )
    await ui.dialog.alert({ title: "Bridge recovery — read back", message: report })
    return
  }
  const ok = await ui.dialog.confirm({
    title: "Bridge recovery — read back",
    message: `Read back the pending reply for ${session} without resending anything? No tools will be re-run.${state.summary ? `\n\n${state.summary}` : ""}`,
    label: { confirm: "Read back", cancel: "Cancel" },
  })
  if (!ok) {
    ui.toast.show({ message: "Read-back cancelled — nothing changed.", variant: "info" })
    return
  }
  const report = await recoverReport(
    deps.adapters.opts,
    { action: "recover", session },
    session,
    request,
  )
  await ui.dialog.alert({ title: "Bridge recovery — read back", message: report })
}

async function runAbandon(
  deps: RecoveryDeps,
  request: AdapterRequest,
  bridgeName: string,
  explicitSession: string | undefined,
  reason: string | undefined,
): Promise<void> {
  const { ui, data } = deps
  const session = explicitSession ?? (await resolveTargetSession(ui))
  if (!session) {
    ui.toast.show({ message: "Abandon cancelled — no chat named.", variant: "info" })
    return
  }
  if (sessionBusy(data, session)) {
    await ui.dialog.alert({
      title: `Bridge recovery — abandon (${bridgeName})`,
      message: `Chat ${session} has a live turn. Abandon refuses while a turn is in flight — wait for idle, then run this again. Nothing changed.`,
    })
    return
  }
  const ok = await ui.dialog.confirm({
    title: `Bridge recovery — abandon (${bridgeName})`,
    message: `Clear the unconfirmed turn for ${session}? Kept: completed transcript, thread link, results and fingerprints. Historical tools will NOT be re-run. This only clears the uncertainty binding.`,
    label: { confirm: "Abandon turn", cancel: "Cancel" },
  })
  if (!ok) {
    // Cancel must perform zero mutation calls: no adapter contact at all.
    ui.toast.show({ message: "Abandon cancelled — nothing changed.", variant: "info" })
    return
  }
  const report = await recoverReport(
    deps.adapters.opts,
    { action: "abandon", session, reason } as RecoverArgs,
    session,
    request,
  )
  await ui.dialog.alert({ title: `Bridge recovery — abandon (${bridgeName})`, message: report })
}

export function buildRecoveryCommands(deps: RecoveryDeps): PaletteCommand[] {
  const { adapters } = deps
  const out: PaletteCommand[] = []
  if (adapters.chatgpt) {
    const request = adapters.chatgpt
    out.push({
      id: CMD_INSPECT_CHATGPT,
      title: "Bridge recovery: inspect ChatGPT chat",
      description: "Read-only: what the bridge knows, what is unknown, what was kept.",
      group: RECOVERY_GROUP,
      palette: true,
      run: () => runInspect(deps, request, "ChatGPT"),
    })
    out.push({
      id: CMD_RECOVER_CHATGPT,
      title: "Bridge recovery: read back ChatGPT reply",
      description: "Read-only adoption of the pending reply. Never resends. Asks first.",
      group: RECOVERY_GROUP,
      palette: true,
      run: () => runRecoverChatgpt(deps, request),
    })
    out.push({
      id: CMD_ABANDON_CHATGPT,
      title: "Bridge recovery: abandon ChatGPT turn",
      description: "Clears only the uncertainty binding. Asks first; never re-runs tools.",
      group: RECOVERY_GROUP,
      palette: true,
      run: () => runAbandon(deps, request, "ChatGPT", undefined, undefined),
    })
  }
  if (adapters.gemini) {
    const request = adapters.gemini
    out.push({
      id: CMD_INSPECT_GEMINI,
      title: "Bridge recovery: inspect Gemini chat",
      description: "Read-only: pending state, counts and available moves.",
      group: RECOVERY_GROUP,
      palette: true,
      run: () => runInspect(deps, request, "Gemini"),
    })
    out.push({
      id: CMD_ABANDON_GEMINI,
      title: "Bridge recovery: abandon Gemini turn",
      description: "Clears only the uncertainty binding. Asks first; never re-runs tools.",
      group: RECOVERY_GROUP,
      palette: true,
      run: () => runAbandon(deps, request, "Gemini", undefined, undefined),
    })
  }
  return out
}

// ---------------------------------------------------------------------------
// TUI entrypoint (wired to ./tui probe file + package export + build script)
// ---------------------------------------------------------------------------

export interface TuiSetup {
  adapters: RecoveryAdapters
  entries: PaletteCommand[]
}

export function buildTuiSetup(
  rawOptions: Record<string, unknown>,
  ui: Pick<PaletteUI, "dialog" | "toast" | "router">,
  data: PaletteData,
): TuiSetup {
  const opts = resolveOptions(rawOptions)
  let geminiBaseURL: string | undefined
  try {
    geminiBaseURL = resolveGemini((rawOptions as { gemini?: unknown }).gemini)?.adapterBaseURL
  } catch {
    geminiBaseURL = undefined
  }
  // chatgpt:false disables supervision (managed binaries/Chrome), NOT
  // recovery: a manually-run adapter still answers at the resolved address,
  // so the ChatGPT entries are always registered. Unreachable adapters
  // surface as plain-English reports, never throws.
  const adapters: RecoveryAdapters = {
    opts,
    chatgpt: bridgeRequest(`http://${opts.adapterHost}:${opts.adapterPort}/v1`),
    gemini: geminiBaseURL ? bridgeRequest(geminiBaseURL) : undefined,
  }
  const entries = buildRecoveryCommands({ ui, data, adapters })
  return { adapters, entries }
}

export async function setupTui(context: {
  options: Readonly<Record<string, unknown>>
  ui: Pick<PaletteUI, "dialog" | "toast" | "router" | "slot">
  data: PaletteData
  keymap: PaletteKeymap
  toastError?: (message: string) => void
}): Promise<(() => void) | void> {
  let built: TuiSetup
  try {
    built = buildTuiSetup(
      (context.options ?? {}) as Record<string, unknown>,
      context.ui,
      context.data,
    )
  } catch (error) {
    context.ui.toast.show({
      title: "Bridge recovery",
      message: `Recovery palette disabled: ${error instanceof Error ? error.message : String(error)}`,
      variant: "error",
    })
    return
  }
  if (built.entries.length === 0) {
    context.ui.toast.show({
      title: "Bridge recovery",
      message: "No bridge adapter configured — recovery palette idle.",
      variant: "warning",
    })
    return
  }
  // The layer factory must run inside a component: claim the always-mounted
  // "app" slot with a null render, register once, unclaim on dispose.
  // keymap lives TOP LEVEL on the setup context (SDK Context.keymap) —
  // never under ui; the suite locks this with a keymap-less ui fake.
  let claimed = false
  const unclaim = context.ui.slot({
    append: "app",
    render: () => {
      if (!claimed) {
        claimed = true
        context.keymap.layer(() => ({ mode: "global", commands: built.entries }))
      }
      return null as never
    },
  })
  return () => {
    try {
      unclaim()
    } catch {
      // Already unmounted: nothing to release.
    }
  }
}

export default {
  id: "opencode-web-bridges",
  setup: setupTui,
}
