import { describe, expect, test } from "bun:test"
import { resolveOptions, splitHostPort, DEFAULTS } from "../src/server/options"

describe("resolveOptions defaults", () => {
  test("empty options resolve to documented defaults", () => {
    const o = resolveOptions(undefined)
    expect(o.releasesRepo).toBe("bojackduy/chatgpt-proxy")
    expect(o.version).toBe("latest")
    expect(o.profileDir).toBe(DEFAULTS.profileDir)
    expect(o.cdpPort).toBe(9222)
    expect(o.adapterHost).toBe("127.0.0.1")
    expect(o.adapterPort).toBe(8788)
    expect(o.maxChars).toBe(200000)
    expect(o.projectPerSession).toBe(false)
    expect(o.supervise).toBe(true)
  })

  test("supervise is opt-out only: anything but false keeps the janitor", () => {
    expect(resolveOptions({ supervise: false }).supervise).toBe(false)
    expect(resolveOptions({ supervise: true }).supervise).toBe(true)
    expect(resolveOptions({ supervise: "no" }).supervise).toBe(true)
  })

  test("overrides apply; invalid values fall back to defaults", () => {
    const o = resolveOptions({
      releasesRepo: "myorg/proxy",
      version: "v0.2.0",
      cdpPort: 9333,
      adapterAddr: "127.0.0.1:9999",
      maxChars: 100000,
      projectPerSession: true,
    })
    expect(o.releasesRepo).toBe("myorg/proxy")
    expect(o.version).toBe("v0.2.0")
    expect(o.cdpPort).toBe(9333)
    expect(o.adapterPort).toBe(9999)
    expect(o.maxChars).toBe(100000)
    expect(o.projectPerSession).toBe(true)

    const bad = resolveOptions({ cdpPort: -1, maxChars: 0, adapterAddr: "", projectPerSession: "yes" })
    expect(bad.cdpPort).toBe(9222)
    expect(bad.maxChars).toBe(200000)
    expect(bad.adapterPort).toBe(8788)
    expect(bad.projectPerSession).toBe(false)
  })

  test("~/ in profileDir expands to home", () => {
    const o = resolveOptions({ profileDir: "~/.my-profile" })
    expect(o.profileDir.endsWith(".my-profile")).toBe(true)
    expect(o.profileDir.startsWith("~")).toBe(false)
  })
})

describe("splitHostPort", () => {
  test("splits host and port", () => {
    expect(splitHostPort("127.0.0.1:8788")).toEqual({ adapterHost: "127.0.0.1", adapterPort: 8788 })
  })
})
