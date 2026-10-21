// Shared per-process lifecycle: managed binaries + supervisor, started once.
// Both the v1 `server()` entry (plugin.ts) and the v2 `setup()` entry (v2.ts)
// go through `ensureShared` — whichever host loads the plugin wins, and the
// supervisor is never double-spawned. Pure background start: never blocks the
// caller on a network download.

import { resolveOptions, type ResolvedOptions } from "./options"
import { ensureBinaries, type EnsuredBinaries } from "./binaries"
import { startSupervisor, type Supervisor } from "./supervisor"
import { sourceBinaries } from "./source"

export interface SharedState {
  opts: ResolvedOptions
  binaries: EnsuredBinaries | undefined
  binaryError: string | undefined
  supervisor: Supervisor | undefined
  /** Resolves when the background binary ensure settles (success or failure). */
  ready: Promise<void>
  /** LOUD first-run login notice: printed once per process lifetime. */
  firstRunNotified: boolean
}

let shared: SharedState | undefined

export function ensureShared(rawOptions: Record<string, unknown> | undefined): SharedState {
  if (shared) return shared
  const opts = resolveOptions(rawOptions)
  const state: SharedState = {
    opts,
    binaries: undefined,
    binaryError: undefined,
    supervisor: undefined,
    ready: Promise.resolve(),
    firstRunNotified: false,
  }
  state.ready = (opts.bridgeSource ? sourceBinaries(opts.bridgeSource) : ensureBinaries({ releasesRepo: opts.releasesRepo, version: opts.version }))
    .then((binaries) => {
      state.binaries = binaries
      // The janitor relaunches Chrome and the adapter whenever they are gone.
      // When something else owns their lifecycle, leaving it on means a user
      // who quits Chrome gets it reopened within 60 seconds.
      if (opts.supervise) state.supervisor = startSupervisor(opts, binaries)
    })
    .catch((error) => {
      state.binaryError = error instanceof Error ? error.message : String(error)
      console.error(`[opencode-web-bridges] managed binary setup failed: ${state.binaryError}`)
    })
    .then(() => {})
  shared = state
  return state
}

/** Test-only: drop the singleton so each test starts fresh. */
export function resetSharedForTests(): void {
  shared = undefined
}
