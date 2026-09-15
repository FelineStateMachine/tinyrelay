#!/usr/bin/env python3
"""Small deterministic OpenAI-compatible model used by the integration lab.

It deliberately implements only the HTTP surface Hermes needs.  Scenario
markers are read from the latest user turn, so setup prompts and older turns
cannot accidentally trigger a lab interaction.
"""

from __future__ import annotations

import json
import re
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

PORT = 8080
MARKER = re.compile(r"\b(LAB_(?:HELLO|QUESTION|CUSTOM|OPEN|MULTI|APPROVAL|MENTION|MEDIA)_[A-Za-z0-9_-]+)\b")
MENTION = re.compile(r"@([0-9a-fA-F]{64})")
ATTACHMENT = re.compile(r"\b(tinyagent-attachment-[A-Za-z0-9._-]+)\b")


def _text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return " ".join(_text(part.get("text", "")) for part in content if isinstance(part, dict))
    return ""


def _scenario(messages: list[dict[str, Any]]) -> tuple[str | None, int | None]:
    """Return the marker and the index of its user turn, if any."""
    for index in range(len(messages) - 1, -1, -1):
        message = messages[index]
        if message.get("role") != "user":
            continue
        # Hermes prepends a quote of the replied-to message, which can contain
        # an older scenario marker. The current user's marker follows that quote.
        found = list(MARKER.finditer(_text(message.get("content"))))
        if found:
            return found[-1].group(1), index
        return None, None
    return None, None


def _latest_user_text(messages: list[dict[str, Any]], start: int) -> str:
    return _text(messages[start].get("content"))


def _tool_names(tools: list[dict[str, Any]]) -> set[str]:
    names: set[str] = set()
    for tool in tools:
        function = tool.get("function") if isinstance(tool, dict) else None
        if isinstance(function, dict) and isinstance(function.get("name"), str):
            names.add(function["name"])
    return names


def _tool_result(messages: list[dict[str, Any]], start: int) -> str | None:
    for message in messages[start + 1 :]:
        if message.get("role") == "tool":
            return _text(message.get("content"))
    return None


def _response(message: dict[str, Any], *, model: str, stream: bool) -> dict[str, Any]:
    response_id = "chatcmpl-" + uuid.uuid4().hex
    choice = {"index": 0, "message": message, "finish_reason": "tool_calls" if message.get("tool_calls") else "stop"}
    return {"id": response_id, "object": "chat.completion", "created": int(time.time()), "model": model,
            "choices": [choice], "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}


def complete(body: dict[str, Any]) -> dict[str, Any]:
    messages = body.get("messages") or []
    tools = _tool_names(body.get("tools") or [])
    marker, start = _scenario(messages)
    model = str(body.get("model") or "tiny-lab")
    if not marker or start is None:
        return _response({"role": "assistant", "content": "LAB_IDLE"}, model=model, stream=False)
    result = _tool_result(messages, start)
    if result is not None:
        media = re.search(r"It is saved at: (\S+)\. Its content", _latest_user_text(messages, start)) if marker.startswith("LAB_MEDIA_") else None
        suffix = f"\nMEDIA:{media.group(1)}" if media else ""
        return _response({"role": "assistant", "content": f"LAB_DONE {marker} {result}{suffix}"}, model=model, stream=False)
    if marker.startswith("LAB_HELLO_"):
        return _response({"role": "assistant", "content": f"LAB_DONE {marker}"}, model=model, stream=False)
    latest_text = _latest_user_text(messages, start)
    if marker.startswith("LAB_MENTION_"):
        mentions = MENTION.findall(latest_text)
        suffix = " @" + mentions[0] if mentions else ""
        return _response({"role": "assistant", "content": f"LAB_DONE {marker}{suffix}"}, model=model, stream=False)
    if marker.startswith("LAB_MEDIA_"):
        attachment = ATTACHMENT.search(latest_text)
        cached = re.search(r"It is saved at: (\S+)\. Its content", latest_text)
        if cached and "read_file" in tools:
            call = {"id": "call_" + marker.lower(), "type": "function", "function": {
                "name": "read_file", "arguments": json.dumps({"path": cached.group(1)})}}
            return _response({"role": "assistant", "content": None, "tool_calls": [call]}, model=model, stream=False)
        suffix = f" MEDIA_RECEIVED {attachment.group(1)}" if attachment else " MEDIA_NOT_FOUND"
        return _response({"role": "assistant", "content": f"LAB_DONE {marker}{suffix}"}, model=model, stream=False)
    if marker.startswith(("LAB_QUESTION_", "LAB_MULTI_", "LAB_CUSTOM_", "LAB_OPEN_")):
        if "clarify" not in tools:
            return _response({"role": "assistant", "content": "LAB_ERROR clarify tool is unavailable"}, model=model, stream=False)
        multi = marker.startswith("LAB_MULTI_")
        choices = [] if marker.startswith("LAB_OPEN_") else ["Continue", "Stop"]
        arguments = {"questions": [{"question": f"Choose for {marker}", "choices": choices, "multi_select": multi}]}
        call = {"id": "call_" + marker.lower(), "type": "function", "function": {"name": "clarify", "arguments": json.dumps(arguments)}}
        return _response({"role": "assistant", "content": None, "tool_calls": [call]}, model=model, stream=False)
    if "execute_code" not in tools:
        return _response({"role": "assistant", "content": "LAB_ERROR execute_code tool is unavailable"}, model=model, stream=False)
    call = {"id": "call_" + marker.lower(), "type": "function", "function": {"name": "execute_code", "arguments": json.dumps({"code": "print('tinyagent-lab')"})}}
    return _response({"role": "assistant", "content": None, "tool_calls": [call]}, model=model, stream=False)


def _sse_payload(result: dict[str, Any]) -> list[str]:
    choice = result["choices"][0]
    message = choice["message"]
    chunks: list[dict[str, Any]] = []
    base = {"id": result["id"], "object": "chat.completion.chunk", "created": result["created"], "model": result["model"]}
    delta: dict[str, Any] = {"role": "assistant"}
    if message.get("content") is not None:
        delta["content"] = message["content"]
    if message.get("tool_calls"):
        delta["tool_calls"] = [{"index": 0, **message["tool_calls"][0]}]
    chunks.append({**base, "choices": [{"index": 0, "delta": delta, "finish_reason": None}]})
    chunks.append({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": choice["finish_reason"]}]})
    return [f"data: {json.dumps(chunk, separators=(',', ':'))}\n\n" for chunk in chunks] + ["data: [DONE]\n\n"]


class Handler(BaseHTTPRequestHandler):
    server_version = "tinyagent-lab/1"

    def log_message(self, _format: str, *_args: Any) -> None:
        return

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._send(200, {"status": "ok"})
        elif self.path == "/v1/models":
            self._send(200, {"object": "list", "data": [{"id": "tiny-lab", "object": "model", "owned_by": "tinyagent-lab"}]})
        else:
            self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})

    def do_POST(self) -> None:  # noqa: N802
        if self.path != "/v1/chat/completions":
            self._send(404, {"error": {"message": "not found", "type": "invalid_request_error"}})
            return
        length = int(self.headers.get("Content-Length", "0"))
        try:
            body = json.loads(self.rfile.read(length))
            result = complete(body)
        except (ValueError, TypeError, json.JSONDecodeError) as exc:
            self._send(400, {"error": {"message": f"invalid JSON: {exc}", "type": "invalid_request_error"}})
            return
        if body.get("stream"):
            payload = "".join(_sse_payload(result)).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
        else:
            self._send(200, result)

    def _send(self, status: int, value: dict[str, Any]) -> None:
        payload = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def serve(port: int = PORT) -> None:
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    serve()
