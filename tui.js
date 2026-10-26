// OpenCode v2 TUI entrypoint (file probe, not package exports).
//
// The v2 host probes for a literal `./tui` FILE inside the package directory
// — package.json `exports["./tui"]` alone is not consulted for path-form
// packages. This file only re-exports the built module; the implementation
// lives in `src/server/tui.ts` and ships as `dist/tui.js` (see `build:tui`).
export { default } from "./dist/tui.js";
