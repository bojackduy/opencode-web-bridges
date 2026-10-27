// Flow log: content-free, never throws, hermetic under `bun test`.
import { describe, expect, test } from "bun:test"
import * as fs from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import { errorHead, logFlow } from "../src/server/flow-log"

function readLines(file: string): Record<string, unknown>[] {
  try {
    const text = fs.readFileSync(file, "utf8")
    if (text.trim() === "") return []
    return text.trim().split("\n").map((line) => JSON.parse(line))
  } catch {
    return []
  }
}

describe("flow log", () => {
  test("writes one JSONL line per step with content-free fields", () => {
    const file = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "flow-")), "flow.jsonl")
    process.env["OPENCODE_WEB_BRIDGES_FLOW_LOG"] = file
    try {
      logFlow("agent-tool", "start", { tool: "chatgpt-recover", session: "ses_1" })
      logFlow("adapter-http", "end", {
        method: "POST",
        path: "/v1/sessions/recover",
        session: "ses_1",
        status: 200,
        reqBytes: 0,
        respBytes: 41,
        durationMs: 7,
        ok: true,
      })
      const lines = readLines(file)
      expect(lines.length).toBe(2)
      expect(lines[0]?.["actor"]).toBe("agent-tool")
      expect(lines[0]?.["event"]).toBe("start")
      expect(lines[1]?.["status"]).toBe(200)
      // No prompt text, args, or bodies anywhere in the line.
      expect(JSON.stringify(lines[1])).not.toContain("opencode_bridge_call")
    } finally {
      delete process.env["OPENCODE_WEB_BRIDGES_FLOW_LOG"]
    }
  })

  test("never throws when the sink is unusable", () => {
    process.env["OPENCODE_WEB_BRIDGES_FLOW_LOG"] = "/nonexistent-dir-xyz/deep/flow.jsonl"
    try {
      // /nonexistent-dir-xyz cannot be created at root in most sandboxes,
      // but even if it can, the call must not throw either way.
      logFlow("supervisor", "spawn", { name: "chatgpt-openai", pid: 1 })
    } finally {
      delete process.env["OPENCODE_WEB_BRIDGES_FLOW_LOG"]
    }
  })

  test("errorHead truncates to a short head", () => {
    expect(errorHead(new Error("x".repeat(500))).length).toBeLessThanOrEqual(160)
    expect(errorHead("plain string")).toBe("plain string")
  })
})
