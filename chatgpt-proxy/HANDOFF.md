# Handoff: chatgpt-proxy

> **Created:** 2026-08-18
> **Stack:** Go 1.26 · chromedp v0.16.0 · gorilla/websocket (legacy only)
> **Location:** `~/Documents/AI_workspaces/chatgpt-proxy`
> **Status:** ✅ ทำงานจริง end-to-end (live test ผ่าน)
> **Goal:** MCP server ให้ AI agent (Hermes / Claude Desktop / Codebuff ฯลฯ) เรียกใช้ **ChatGPT Web** ผ่านบัญชีผู้ใช้แบบฟรี ไม่ใช้ API key

---

## ทิศทาง (สำคัญ — มีการเปลี่ยนทาง)

ตอนแรกสร้างเป็น **relay + agent outbound tunnel** (ให้ ChatGPT Web ควบคุมเครื่อง) ตามแรงบันดาลใจที่เจอมา แต่**ทิศทางจริงที่ต้องการคือกลับกัน**:

```
AI agent ──MCP──▶ chatgpt-proxy ──CDP──▶ Chrome (chatgpt.com, session ของผู้ใช้)
```

โค้ดเก่า (relay/agent/tools/tunnel) ย้ายไปไว้ที่ `legacy/` เก็บไว้เป็น reference — **ไม่ใช่ product หลักอีกต่อไป**

---

## โครงสร้างโปรเจกต์ (ใหม่)

```
chatgpt-proxy/
├── cmd/proxy/main.go          # entrypoint: -transport stdio|http, -wait-login, -open, -profile
├── internal/
│   ├── mcp/mcp.go             # MCP/JSON-RPC types (hand-rolled, ใช้ร่วมกับ legacy)
│   ├── chatgpt/chatgpt.go     # CDP automation: launch/attach Chrome, login check, Ask, Status
│   └── server/
│       ├── server.go          # MCP dispatch + tools (ask_chatgpt, chatgpt_status)
│       ├── transports.go      # stdio (newline JSON) + Streamable HTTP (POST /mcp)
│       └── server_test.go     # unit tests (fake chatgpt client)
├── legacy/                    # relay/agent/tools เก่า + scripts/start-local.sh (อ้างอิง)
├── bin/                       # build artifacts (git-ignored)
└── README.md · HANDOFF.md · docs/
```

---

## Verified ✅ (หลักฐานจากการ live test จริงวันนี้)

| รายการ | ผล |
|---|---|
| `go build ./...` + `go vet ./...` + `go test ./...` | ✅ clean |
| `chatgpt_status` ผ่าน Chrome จริง | ✅ `logged_in:true, composer:true` |
| `ask_chatgpt("1+1?")` | ✅ ตอบ `2` |
| `ask_chatgpt` เขียน fibonacci (Python) ผ่าน HTTP MCP path | ✅ ได้โค้ดกลับมา |
| Profile session อยู่ข้ามการรัน | ✅ `~/.chatgpt-proxy/Default/Cookies` (61KB) |
| Long Task Code Review (~30KB prompt) | ✅ ได้รีวิวเชิงลึก 28,000+ ตัวอักษร |
| Background mode (`-background`) | ✅ รัน off-screen/minimized โดยไม่บังจอ |

### Bug สำคัญที่เจอระหว่าง dev และแก้แล้ว

1. **Deadlock ใน `tabCtx`** — public methods lock `c.mu` แล้ว `tabCtx` lock ซ้ำ (Go mutex ไม่ reentrant) → ค้างตลอด → แก้: `tabCtx` ไม่ lock (caller ถือ lock อยู่แล้ว)
2. **`attach` ค้างไม่มี timeout** — มี HTTP server แปลกๆ ครอบ port 9222 (ตอบ 404 เปล่า) → `attach` รอค้าง → แก้: timeout 3 วิ + **auto-pick port** (`resolvePort` scan 9222-9231)
3. **`launch` ซ้ำวน loop** — หลัง launch ครั้งแรก port เป็น busy แล้ว resolvePort เลือก port ใหม่ → launch ซ้ำ → Chrome handoff → เปิด tab chatgpt.com เพิ่มเรื่อยๆ → แก้: `launchAttempted`/`launchPort` cache + poll รอ port ขึ้น 20 วิ ไม่ relaunch
4. **browser context ผูกกับ request context** — `NewRemoteAllocator(ctx)` ใช้ ctx ของ request แรก → พอ request จบ connection ตาย → แก้: ใช้ `c.life` (background) เป็น parent
5. **`chromedp` cancel tab context หลัง Run แรก** (ตัวที่ทำให้ `ask_chatgpt` พังเป็นวันๆ) — `Allocate` ผูก teardown กับ context ของ **Run แรก** ซึ่งเป็น `navCtx` (WithTimeout) → พอ Run แรกจบ `defer cancel()` → chromedp เข้าใจว่า "ปิด browser" → ปิด websocket + cancel tab → call ถัดไปได้ `context canceled` → **แก้: no-op `chromedp.Run` บน tab context ทันทีหลัง `NewContext`** เพื่อบังคับ allocation เกิดบน long-lived context
6. **selector "New chat" ไม่ตรง** — DOM จริงคือ `<a href="/">` (ไม่มี aria-label, ไม่อยู่ใน `#sidebar`) → แก้: selector `a[href="/"]` + text fallback `"New chat"`
7. **multi-turn (`new_session:false`) composer ยังไม่ render** หลัง Navigate → `typePrompt` fail ทันที → แก้: `typePrompt` poll รอ composer (30 วิ) + เพิ่ม render wait หลัง Navigate ใน `Ask` → verified 3 รอบต่อเนื่องตอบถูก (จำชื่อ "บอส" ได้)
8. **ProseMirror พิมพ์ช้าค้างเมื่อส่งข้อความยาว (~30KB)** → `execCommand('insertText')` ส่งข้อความใหญ่แล้ว browser ค้าง → แก้: ใช้ synthetic `ClipboardEvent('paste')` ด้วย `DataTransfer` paste ทันที (0ms)
9. **Event Click บนปุ่มส่งไม่ trigger ใน React** → `btn.click()` ธรรมดา React มองข้าม → แก้: ยิง Event ครบลูป (`pointerdown` ➔ `mousedown` ➔ `mouseup` ➔ `click`)
10. **False-positive error ใน Prompt ที่มีคำว่า error** → แก้: scope การตรวจ error ในหน้าเว็บให้อยู่เฉพาะ Alert Banners (`[role="alert"]`, `.text-token-text-error`)
11. **Timeout & Polling สำหรับ Long Task / Reasoning Models** → ปรับ Default Timeout เป็น 10 นาที (600s) และ Polling 10s ระหว่างคิด
12. **Background Flag (`-background`)** → เปิด Chrome แบบ off-screen (`--window-position=-2400,-2400`, `--start-minimized`) ทำงานข้างหลังเงียบๆ ไม่บังจอและไม่ติด Bot Protection ของ Cloudflare

---

## Known limitations (P0 → P3)

| Priority | รายการ | หมายเหตุ |
|---|---|---|
| **P0** | HTTP transport ยังไม่มี auth | `stdio` ปลอดภัย (ไม่ bind network) แต่ `-transport http` เปิดใครก็เรียกได้ — ก่อนใช้ข้ามเครื่องต้องมี token |
| **P1** | selector อิง DOM ปัจจุบันของ chatgpt.com | OpenAI เปลี่ยน layout เมื่อไหร่ต้อง re-tune (จุดรวม selector อยู่ที่ `internal/chatgpt/chatgpt.go`) |
| **P2** | ยังไม่มี streaming | คืน text ทั้งก้อนตอนเสร็จ (รอจนครบ) — พอใช้ แต่ latency สูงสำหรับคำตอบยาว |
| **P2** | `Continue generating` กดได้สูงสุด 3 รอบ | คำตอบยาวมากจะถูกตัด |
| **P2** | ยังรองรับแค่ macOS Chrome path | Windows/Linux path ใน `findChrome()` มีครบแล้วแต่ยังไม่ test |
| **P3** | `chrome_port` ใน status report เป็น port ที่ attach ได้จริงแล้ว (actualPort) | แต่ยัง report ค่า default ถ้ายังไม่เคย connect |

---

## ขั้นต่อไป (ลำดับแนะนำ)

1. **เชื่อม Hermes จริง** — ใส่ command `bin/chatgpt-proxy` (stdio) ลง MCP config ของ Hermes แล้วลองให้ Hermes เรียก `ask_chatgpt`
2. **Auth สำหรับ HTTP mode** — เพิ่ม optional `-token` / env `PROXY_TOKEN` บน `/mcp`
3. **เพิ่ม tools** — e.g. `ask_chatgpt_image` (แนบรูป), `chatgpt_new_thread`, หรือ `chatgpt_history`
4. **re-tune selector** เมื่อ chatgpt.com เปลี่ยน UI (จุดเดียวที่ต้องแก้คือ `internal/chatgpt/chatgpt.go`)

---

## หมายเหตุสำคัญ

1. **ใช้ session ส่วนตัวของคุณ** — อย่าเปิด `-transport http` ให้คนอื่นถ้าไม่เพิ่ม auth; `stdio` mode ปลอดภัยโดยปริยาย
2. **MCP protocol เป็น hand-rolled** (`internal/mcp`) — จงใจไม่ใช้ SDK
3. **โค้ด legacy** (`legacy/`) ยัง build/test ผ่าน แต่ไม่เกี่ยวกับ product หลัก — จะลบทีหลังก็ได้ (git history เก็บไว้แล้ว)
4. **`bin/chatgpt-proxy`** คือ binary ใหม่; `bin/relay`/`bin/agent` เป็นของ legacy
