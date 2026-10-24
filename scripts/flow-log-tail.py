#!/usr/bin/env python3
"""Human-readable per-request timelines for the ChatGPT and Gemini web bridges.

Both adapters run a fire-and-forget submit path and log every stage of one
browser turn under a single flow id. This tailer follows their logs and writes:

  /tmp/chatgpt-flows/<flow-id>.log     one readable timeline per ChatGPT turn
  /tmp/chatgpt-flows/index.log         one summary line per finished turn
  /tmp/gemini-flows/<flow-id>.log      one readable timeline per Gemini turn
  /tmp/gemini-flows/index.log          one summary line per finished turn

Prompt and answer text are never written: only sizes, flags, durations and
errors. See docs/BRIDGE_LOGS.md.

Usage:
  python3 scripts/flow-log-tail.py              # both bridges, follows forever
  python3 scripts/flow-log-tail.py gemini       # one bridge
"""
import argparse
import os
import re
import time

BRIDGES = {
    "chatgpt": {
        "source": "/tmp/chatgpt-openai.log",
        "outdir": "/tmp/chatgpt-flows",
        "offset": "/tmp/chatgpt-flow-tail.offset",
        "tag": "chatgpt",
    },
    "gemini": {
        "source": "/tmp/gemini-openai.log",
        "outdir": "/tmp/gemini-flows",
        "offset": "/tmp/gemini-flow-tail.offset",
        "tag": "gemini",
    },
}

TS = re.compile(r"^(?P<ts>\d{4}/\d\d/\d\d \d\d:\d\d:\d\d)")


def kv(rest):
    """Parse key=value pairs, keeping spaces inside err=/last_err= values."""
    out = {}
    for m in re.finditer(r"(\w+)=(.*?)(?=\s+\w+=|$)", rest):
        out[m.group(1)] = m.group(2).strip()
    return out


def ts_of(line):
    m = TS.match(line)
    return m.group("ts")[11:] if m else time.strftime("%H:%M:%S")


class Writer:
    def __init__(self, spec):
        self.spec = spec
        self.flow = re.compile(r"\[%s-flow\] id=(?P<id>\S+) stage=(?P<stage>\S+)(?P<rest>.*)" % spec["tag"])
        self.session_flow = re.compile(r"\[%s-flow\] id=(?P<id>\S+) session=(?P<session>\S+) stage=(?P<stage>\S+)(?P<rest>.*)" % spec["tag"])
        os.makedirs(spec["outdir"], exist_ok=True)
        self.index = os.path.join(spec["outdir"], "index.log")
        self.state = {}

    def emit(self, st, ts, text):
        with open(st["path"], "a") as f:
            f.write("%s  %s\n" % (ts, text))

    def handle(self, line):
        m = self.session_flow.search(line) or self.flow.search(line)
        if not m:
            return
        fid, stage, rest = m.group("id"), m.group("stage"), m.group("rest")
        data, ts = kv(rest), ts_of(line)
        st = self.state.setdefault(fid, {"id": fid, "last_bytes": None,
                                         "path": os.path.join(self.spec["outdir"], fid + ".log")})
        handler = STAGES.get(stage)
        if handler:
            handler(self, st, ts, data)

    # --- stages shared by both bridges -------------------------------------
    def stage_start(self, st, ts, data):
        st["prompt"] = data.get("prompt_bytes", "?")
        self.emit(st, ts, "REQUEST      prompt %s B, timeout=%s" % (st["prompt"], data.get("timeout", "?")))

    def stage_prepare(self, st, ts, data):
        if "result" not in data:
            self.emit(st, ts, "PREPARE      opening the conversation")
        elif data.get("result") == "error":
            self.emit(st, ts, "PREPARE      FAILED after %s: %s" % (data.get("duration", "?"), data.get("err", "?")))
        else:
            self.emit(st, ts, "PREPARE      page ready in %s" % data.get("duration", "?"))

    def stage_composer_wait(self, st, ts, data):
        self.emit(st, ts, "COMPOSER     waiting for the editable box")

    def stage_composer_focus(self, st, ts, data):
        self.emit(st, ts, "COMPOSER     %s" % ("found" if data.get("result") == "ok" else "NOT FOUND"))

    def stage_composer_insert(self, st, ts, data):
        if data.get("result") == "error":
            self.emit(st, ts, "INSERTED     FAILED: %s" % data.get("err", "?"))
            return
        mode = "legacy paste" if data.get("legacy") == "true" else "trusted input"
        note = "" if data.get("native_error") in (None, "<nil>") else " (native error: %s)" % data["native_error"]
        self.emit(st, ts, "INSERTED     fire-and-forget via %s%s — not verified" % (mode, note))

    def stage_send_wait(self, st, ts, data):
        self.emit(st, ts, "SEND         waiting for an enabled Send control")

    def stage_send_ready(self, st, ts, data):
        if data.get("enabled") == "true":
            self.emit(st, ts, "SEND         enabled after %s" % data.get("waited", "?"))
        else:
            self.emit(st, ts, "SEND         NEVER enabled after %s (last read: %s)" % (
                data.get("waited", "?"), data.get("last_err", "none")))

    def stage_send_click(self, st, ts, data):
        if data.get("err") not in (None, "<nil>"):
            self.emit(st, ts, "SEND         outcome UNKNOWN: %s" % data["err"])
        elif data.get("clicked") == "true":
            self.emit(st, ts, "SEND         clicked")
        else:
            self.emit(st, ts, "SEND         not clicked (control disabled)")

    def stage_send_observed(self, st, ts, data):
        waited = ", %s" % data["waited"] if "waited" in data else ""
        if data.get("result") == "submitted":
            self.emit(st, ts, "SUBMITTED    turn started (checks=%s%s)" % (data.get("retries", "?"), waited))
        else:
            self.emit(st, ts, "UNOBSERVED   click landed but no turn appeared after %s" % data.get("waited", "the observation window"))

    def stage_stream_wait(self, st, ts, data):
        self.emit(st, ts, "STREAM       polling for the answer")

    def stage_stream_poll(self, st, ts, data):
        if data.get("page_error") == "true":
            self.emit(st, ts, "STREAM       the page showed an error")
        now = data.get("answer_bytes")
        if now is not None and now != st["last_bytes"]:
            st["last_bytes"] = now
            busy = data.get("generating", data.get("stop", "false")) == "true"
            self.emit(st, ts, "STREAM       answer now %s B (generating=%s)" % (now, "yes" if busy else "no"))

    def stage_stream_delta(self, st, ts, data):
        self.emit(st, ts, "STREAM       chunk %s B" % data.get("bytes", "?"))

    def stage_finish(self, st, ts, data):
        err = data.get("err", "")
        verdict = "OK" if err in ("<nil>", "") else "FAILED: " + err
        self.emit(st, ts, "DONE         %s | answer %s B | took %s" % (
            verdict, data.get("answer_bytes", "?"), data.get("duration", "?")))
        with open(self.index, "a") as idx:
            idx.write("%s  %s  prompt=%sB  %s  answer=%sB  %s\n" % (
                ts, st["id"], st.get("prompt", "?"), verdict,
                data.get("answer_bytes", "?"), data.get("duration", "?")))


STAGES = {name[len("stage_"):]: fn for name, fn in Writer.__dict__.items() if name.startswith("stage_")}


def follow(spec, stop_after=None):
    writer = Writer(spec)
    start = 0
    if os.path.exists(spec["offset"]):
        try:
            start = int(open(spec["offset"]).read().strip() or 0)
        except ValueError:
            start = 0
    source = spec["source"]
    while not os.path.exists(source):
        time.sleep(0.5)
    with open(source, errors="replace") as f:
        f.seek(start)
        while True:
            pos = f.tell()
            line = f.readline()
            if not line:
                time.sleep(0.5)
                continue
            with open(spec["offset"], "w") as off:
                off.write(str(pos))
            writer.handle(line)
            if stop_after is not None and pos > stop_after:
                return


def main():
    ap = argparse.ArgumentParser(description="Readable per-request timelines for the web bridges.")
    ap.add_argument("bridge", nargs="?", default="both", choices=["both", *BRIDGES])
    args = ap.parse_args()
    names = list(BRIDGES) if args.bridge == "both" else [args.bridge]
    if len(names) == 1:
        follow(BRIDGES[names[0]])
        return
    children = []
    for name in names:
        pid = os.fork()
        if pid == 0:
            follow(BRIDGES[name])
            os._exit(0)
        children.append(pid)
    for pid in children:
        os.waitpid(pid, 0)


if __name__ == "__main__":
    main()
