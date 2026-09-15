#!/usr/bin/env python3
"""Mock upstream for E2E: OpenAI / Anthropic / Responses 三协议 echo 上游。

Endpoints:
  POST /v1/chat/completions   OpenAI chat echo（stream + non-stream）
  POST /v1/messages           Anthropic messages echo（stream + non-stream）
  POST /v1/responses          Responses echo（stream + non-stream）
  GET  /v1/models             OpenAI 模型列表

Env switches:
  MOCK_MODE=echo     (default) echo last user message
  MOCK_MODE=fail     always 503 (for failover testing)
  MOCK_MODE=flake    503 on first N requests then success (N=MOCK_FAIL_COUNT)

鉴权：固定 Bearer upstream-secret-key（渠道 api_key 填同值即可）。
"""
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

UPSTREAM_KEY = "upstream-secret-key"
PORT = int(os.environ.get("MOCK_PORT", "9999"))
MODE = os.environ.get("MOCK_MODE", "echo")
FAIL_COUNT = int(os.environ.get("MOCK_FAIL_COUNT", "0"))
_state = {"reqs": 0}


def last_user_text(body):
    """从 openai messages 里取最后一条 user 消息文本。"""
    msgs = body.get("messages", [])
    for m in reversed(msgs):
        if m.get("role") == "user":
            return m.get("content", "")
    return ""


def anthropic_last_text(body):
    """从 anthropic messages 里取最后一条 user 消息文本（content 为 string 或 blocks）。"""
    msgs = body.get("messages", [])
    for m in reversed(msgs):
        if m.get("role") != "user":
            continue
        c = m.get("content")
        if isinstance(c, str):
            return c
        if isinstance(c, list):
            return " ".join(b.get("text", "") for b in c if isinstance(b, dict) and b.get("type") == "text")
    return ""


def responses_last_text(body):
    """从 responses input 里取最后一段文本（string 或 items）。"""
    inp = body.get("input")
    if isinstance(inp, str):
        return inp
    if isinstance(inp, list):
        texts = []
        for it in inp:
            if isinstance(it, dict) and it.get("type") == "message":
                c = it.get("content")
                if isinstance(c, str):
                    texts.append(c)
                elif isinstance(c, list):
                    texts += [p.get("text", "") for p in c if isinstance(p, dict)]
        return " ".join(t for t in texts if t)
    return ""


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _authed(self):
        # openai 上游用 Bearer，anthropic 上游用 x-api-key —— 两种都接受
        if self.headers.get("Authorization", "") == f"Bearer {UPSTREAM_KEY}":
            return True
        return self.headers.get("x-api-key", "") == UPSTREAM_KEY

    def _send_json(self, code, obj):
        data = json.dumps(obj, separators=(",", ":")).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _start_sse(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

    def _emit(self, obj, event=None):
        """写一条 SSE 事件（chunked 编码）。"""
        line = (f"event: {event}\n" if event else "") + f"data: {json.dumps(obj, separators=(",", ":"))}\n\n"
        raw = line.encode()
        self.wfile.write(f"{len(raw):x}\r\n".encode() + raw + b"\r\n")
        self.wfile.flush()

    def _end_sse(self, done=False):
        tail = b"data: [DONE]\n\n" if done else b""
        if tail:
            self.wfile.write(f"{len(tail):x}\r\n".encode() + tail + b"\r\n")
        self.wfile.write(b"0\r\n\r\n")

    def do_GET(self):
        if self.path == "/v1/models" and self._authed():
            self._send_json(200, {
                "object": "list",
                "data": [{"id": "mock-model-a", "object": "model"}, {"id": "mock-model-b", "object": "model"}],
            })
            return
        self._send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        if not self._authed():
            self._send_json(401, {"error": {"message": "bad upstream key"}})
            return

        _state["reqs"] += 1
        if MODE == "fail" or (MODE == "flake" and _state["reqs"] <= FAIL_COUNT):
            self._send_json(503, {"error": {"message": "mock upstream unavailable"}})
            return

        path = self.path.split("?")[0]
        if path == "/v1/chat/completions":
            self._chat_completions(body)
        elif path == "/v1/messages":
            self._messages(body)
        elif path == "/v1/responses":
            self._responses(body)
        else:
            self._send_json(404, {"error": {"message": "not found"}})

    # ---- OpenAI chat/completions echo ----

    def _chat_completions(self, body):
        model = body.get("model", "unknown")
        last = last_user_text(body)
        prompt_tokens, completion_tokens = 9, 7

        if body.get("stream"):
            self._start_sse()
            for piece in ["echo", "[", model, "]: ", last]:
                self._emit({"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
                            "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]})
            self._emit({"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": model,
                        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens,
                                  "total_tokens": prompt_tokens + completion_tokens}})
            self._end_sse(done=True)
            return

        self._send_json(200, {
            "id": "chatcmpl-mock", "object": "chat.completion", "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": f"echo[{model}]: {last}"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens,
                      "total_tokens": prompt_tokens + completion_tokens},
        })

    # ---- Anthropic messages echo ----

    def _messages(self, body):
        model = body.get("model", "unknown")
        last = anthropic_last_text(body)
        input_tokens, output_tokens = 11, 5

        if body.get("stream"):
            self._start_sse()
            self._emit({"type": "message_start", "message": {
                "id": "msg-mock", "type": "message", "role": "assistant", "model": model,
                "content": [], "usage": {"input_tokens": input_tokens, "output_tokens": 0}}}, event="message_start")
            for piece in ["anthropic-echo", "[", model, "]: ", last]:
                self._emit({"type": "content_block_delta", "index": 0,
                            "delta": {"type": "text_delta", "text": piece}}, event="content_block_delta")
            self._emit({"type": "message_delta",
                        "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                        "usage": {"output_tokens": output_tokens}}, event="message_delta")
            self._emit({"type": "message_stop"}, event="message_stop")
            self._end_sse()
            return

        self._send_json(200, {
            "id": "msg-mock", "type": "message", "role": "assistant", "model": model,
            "content": [{"type": "text", "text": f"anthropic-echo[{model}]: {last}"}],
            "stop_reason": "end_turn", "stop_sequence": None,
            "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens},
        })

    # ---- Responses echo ----

    def _responses(self, body):
        model = body.get("model", "unknown")
        last = responses_last_text(body)
        input_tokens, output_tokens = 8, 6
        text = f"responses-echo[{model}]: {last}"

        if body.get("stream"):
            self._start_sse()
            self._emit({"type": "response.created", "response": {
                "id": "resp-mock", "object": "response", "status": "in_progress", "model": model, "output": []}})
            self._emit({"type": "response.output_item.added", "output_index": 0, "item": {
                "type": "message", "id": "msg-mock", "role": "assistant", "status": "in_progress", "content": []}})
            self._emit({"type": "response.output_text.delta", "item_id": "msg-mock",
                        "output_index": 0, "content_index": 0, "delta": text})
            self._emit({"type": "response.output_item.done", "output_index": 0, "item": {
                "type": "message", "id": "msg-mock", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": text}]}})
            self._emit({"type": "response.completed", "response": {
                "id": "resp-mock", "object": "response", "status": "completed", "model": model,
                "output": [], "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens,
                                        "total_tokens": input_tokens + output_tokens}}})
            self._end_sse()
            return

        self._send_json(200, {
            "id": "resp-mock", "object": "response", "created_at": 0, "status": "completed",
            "model": model,
            "output": [{"type": "message", "id": "msg-mock", "role": "assistant", "status": "completed",
                        "content": [{"type": "output_text", "text": text}]}],
            "error": None, "incomplete_details": None,
            "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens,
                      "total_tokens": input_tokens + output_tokens},
        })


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", PORT), H).serve_forever()
