# Changelog

## 0.1.2 — npm homepage points to landing page

- Set package `homepage` to the deployed landing page so npm links there instead of the repo README.

## Unreleased

- Add Node-compatible `npx @bojackduy/opencode-web-bridges install` launcher with v1/v2 host detection, explicit config/host selection and a no-side-effects dry-run.
- Preserve JSONC comments, options, manual config and disabled entries; prepare checksum-verified bridge binaries before backed-up atomic config edits.
- Check Bun/Chrome prerequisites read-only, leaving browser login and process supervision to the plugin after restart. Public use still requires npm publication and the first bridge GitHub release.

## 0.1.0 — Initial scaffold

- Hermetic login-command tests: no real Chrome launch or adapter probes during prepack.
- v1 first-install MCP registration waits for binary resolution instead of storing a placeholder command.
- CI and tag-driven npm trusted publishing with provenance; first publication remains manual.
- Host-specific install syntax documented for v1 (`plugin`) and v2 (`plugins`).
- One-line install: `plugin` array entry injects the `chatgpt-web` provider + `chatgpt` MCP via the `config` hook (fill-missing only, never clobbers).
- Managed binaries: downloads `chatgpt-openai` + `chatgpt-proxy` from GitHub releases into a versioned cache dir with sha256 verification.
- Supervisor: keeps the adapter listening and Chrome CDP answering while the plugin lives.
- Commands: `/chatgpt-status`, `/chatgpt-login`, `/chatgpt-doctor`.
