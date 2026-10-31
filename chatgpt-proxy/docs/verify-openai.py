#!/usr/bin/env python3
"""Opt-in live regression check against the local, logged-in browser adapter."""
import json
import pathlib
import sys
import time
import urllib.request
import urllib.error

BASE = "http://127.0.0.1:8788"


def call(path, body=None, session=None):
    headers = {"Content-Type": "application/json"}
    if session:
        headers["X-Session-Id"] = session
    data = None if body is None else json.dumps(body).encode()
    try:
        response = urllib.request.urlopen(urllib.request.Request(BASE + path, data, headers), timeout=650)
    except urllib.error.HTTPError as error:
        print("HTTP_ERROR", error.code, error.read().decode(), flush=True)
        raise
    with response:
        text = response.read().decode()
        if body and body.get("stream"):
            chunks = [json.loads(line[6:]) for line in text.splitlines() if line.startswith("data: ") and line != "data: [DONE]"]
            assert text.endswith("data: [DONE]\n\n")
            assert "```" not in text and "~~~" not in text and "opencode_bridge_call" not in text
            message = {"role": "assistant", "content": ""}
            calls = []
            for chunk in chunks:
                delta = chunk["choices"][0]["delta"]
                message["content"] += delta.get("content", "")
                for tc in delta.get("tool_calls", []):
                    assert tc.pop("index") == len(calls)
                    calls.append(tc)
            if calls:
                message["tool_calls"] = calls
            return message
        return json.loads(text)


def message(body, session):
    result = call("/v1/chat/completions", body, session)
    return result if body.get("stream") else result["choices"][0]["message"]


if "--resume" in sys.argv:
    states = json.loads((pathlib.Path.home() / ".chatgpt-proxy/openai-sessions.json").read_text())
    session, state = next((key, value) for key, value in reversed(list(states.items())) if key.startswith("live-weather-") and not value["Dirty"])
    history = state["Messages"] + [{"role": "user", "content": "What marker did I ask you to remember? Reply only with the marker."}]
    reply = message({"model": "chatgpt-web", "messages": history, "tool_choice": "none"}, session)
    assert "COBALT-73" in reply["content"], reply
    assert call("/readyz")["status"]["url"] == state["URL"]
    print("RESTART_RESUME_PASSED", session, state["URL"], reply["content"], flush=True)
    sys.exit(0)


for attempt in range(10):
    try:
        ready = call("/readyz")
        break
    except (urllib.error.HTTPError, urllib.error.URLError):
        if attempt == 9:
            raise
        time.sleep(1)
assert ready["ready"], ready
print("READY", ready["status"]["url"], flush=True)
saved = []
for stream in [False, True]:
    session = f"live-weather-{time.time_ns()}"
    req = {
        "model": "chatgpt-web", "stream": stream,
        "messages": [{"role": "user", "content": "Use get_weather for Hanoi. After its result report the supplied temperature, do not fetch weather yourself. Remember the marker COBALT-73 for a later followup."}],
        "tools": [{"type": "function", "function": {
            "name": "get_weather", "description": "Return weather for a city",
            "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"], "additionalProperties": False}}}],
        "tool_choice": {"type": "function", "function": {"name": "get_weather"}},
    }
    first = message(req, session)
    tc = first["tool_calls"][0]
    assert tc["function"]["name"] == "get_weather"
    assert json.loads(tc["function"]["arguments"])["city"].lower() == "hanoi"
    assert message(req, session)["tool_calls"][0]["id"] == tc["id"]
    print("TOOL", "SSE" if stream else "JSON", session, tc, flush=True)
    before = call("/readyz")["status"]["url"]
    req["messages"] += [first, {"role": "tool", "tool_call_id": tc["id"], "content": '{"city":"Hanoi","temperature_c":23,"source":"synthetic-live-check"}'}]
    req["tool_choice"] = "none"
    second = message(req, session)
    assert "23" in second["content"], second
    after = call("/readyz")["status"]["url"]
    assert before == after and "/c/" in after, (before, after)
    req["messages"] += [second, {"role": "user", "content": "What marker did I ask you to remember? Reply only with the marker."}]
    third = message(req, session)
    assert "COBALT-73" in third["content"], third
    assert call("/readyz")["status"]["url"] == after
    req["messages"].append(third)
    saved.append((session, req, after))
    print("ROUNDTRIP/FOLLOWUP", after, second["content"], third["content"], flush=True)
session, req, thread = saved[0]
req["messages"].append({"role": "user", "content": "Repeat the remembered marker once more."})
assert "COBALT-73" in message(req, session)["content"]
assert call("/readyz")["status"]["url"] == thread
assert saved[0][2] != saved[1][2]
print("INTERLEAVED_SESSION_RESTORE", session, thread, flush=True)
print("LIVE_CHECK_PASSED", flush=True)
