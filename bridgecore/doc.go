// Package bridgecore holds the pure, provider-agnostic logic shared by every
// web bridge: wrapper scan/validate, chunk/budget helpers, reseed
// classification and carry, SSE writing, stable IDs,
// and Markdown conversion.
//
// Adoption status: protocol (all four bridges), chunk/budget (meta),
// Markdown (meta; gemini keeps its image-aware converter), stable IDs
// (chatgpt, deepseek). Reseed classification and the SSE writer are ported
// and tested but staged — no bridge imports them yet; adopt, don't duplicate.
//
// Rules (non-negotiable):
//   - Core imports NO *-proxy module and NO browser/HTTP client. It stays
//     dependency-free except for the JSON schema validator.
//   - No per-provider branch may ever live here. If a bridge needs
//     behavior the core does not provide, the behavior stays in the bridge.
//   - Rendered args are never source: tool-protocol validation stays strict.
package bridgecore
