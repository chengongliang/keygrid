#!/usr/bin/env python3
"""Mock upstream for E2E: OpenAI / Anthropic / Responses / WorkBuddy 四协议 echo 上游。

Endpoints:
  POST /v1/chat/completions   OpenAI chat echo（stream + non-stream）
  POST /v1/messages           Anthropic messages echo（stream + non-stream）
  POST /v1/responses          Responses echo（stream + non-stream）
  GET  /v1/models             OpenAI 模型列表

WorkBuddy / CodeBuddy 仿真（忠实还原「缺身分头 → 11128」这一真实失败模式）:
  POST /v2/chat/completions                  聊天上游（仅接受 stream:true）
  GET  /v3/config                            全量模型目录（含别名与 -f/base 配对）
  POST /v2/enterprises/personal/models       模型窄端点
  POST /v2/billing/meter/daily-checkin       国内版每日签到
  POST /v2/billing/meter/get-user-resource   积分查询（形状供 ProbeCredits 解析）
  POST /v2/plugin/auth/state                 OAuth device_code 引导（不鉴权）
  GET  /v2/plugin/auth/token                 OAuth 轮询换 token（不鉴权）
  GET  /v2/plugin/login/account              OAuth 昵称 best-effort（不鉴权）

调试端点（与业务端点同一 Bearer 鉴权）:
  GET  /__wb_last[?path=/v2/...]  最近一次 WorkBuddy 请求 {path, headers 子集, body}
  GET  /__stats                   请求计数（含 wb_chat）—— 用于「零上游调用」负向断言

Env switches:
  MOCK_MODE=echo     (default) echo last user message
  MOCK_MODE=fail     always 503 (for failover testing)
  MOCK_MODE=flake    503 on first N requests then success (N=MOCK_FAIL_COUNT)

鉴权：固定 Bearer upstream-secret-key（渠道 api_key 填同值即可）。
"""
import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

UPSTREAM_KEY = "upstream-secret-key"
PORT = int(os.environ.get("MOCK_PORT", "9999"))
MODE = os.environ.get("MOCK_MODE", "echo")
FAIL_COUNT = int(os.environ.get("MOCK_FAIL_COUNT", "0"))
_state = {"reqs": 0, "by_path": {}, "wb_chat": 0, "wb_last": None, "wb_by_path": {}}

# ---- WorkBuddy 路径常量（与 internal/workbuddy/realm.go 一一对应）----
WB_PATH_CHAT = "/v2/chat/completions"
WB_PATH_NARROW_MODELS = "/v2/enterprises/personal/models"
WB_PATH_CHECKIN = "/v2/billing/meter/daily-checkin"
WB_PATH_CREDITS = "/v2/billing/meter/get-user-resource"
WB_PATH_CONFIG = "/v3/config"
WB_PATH_AUTH_STATE = "/v2/plugin/auth/state"
WB_PATH_AUTH_TOKEN = "/v2/plugin/auth/token"
WB_PATH_ACCOUNT = "/v2/plugin/login/account"

# /__wb_last 暴露的头字段子集（顺序固定，便于脚本断言；Authorization 也在内，
# 便于验证 Bearer 鉴权本身走通）。上游真实校验的是 X-Ide-Type / X-Ide-Name /
# X-CodeBuddy-Request 三项，缺任一即 11128。
WB_HEADER_PROBE = [
    "X-Ide-Type", "X-Ide-Name", "X-Domain", "X-Product", "X-IDE-Version",
    "X-CodeBuddy-Request", "X-Agent-Purpose", "X-Requested-With",
    "X-User-Id", "User-Agent", "Authorization", "Content-Type",
]
# 上游 WAF 判定「未授权渠道」的最小身分头集；缺一即 11128。
WB_REQUIRED_HEADERS = ["X-Ide-Type", "X-Ide-Name", "X-CodeBuddy-Request"]


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

    def _bearer_ok(self):
        # WorkBuddy 上游只认 Bearer（调试端点同规则，避免把内部状态裸奔在网络上）
        return self.headers.get("Authorization", "") == f"Bearer {UPSTREAM_KEY}"

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

    def _record_wb(self, path, body):
        """记录一次 WorkBuddy 请求，供 /__wb_last 断言归一化结果。"""
        rec = {
            "path": path,
            "headers": {k: self.headers.get(k, "") for k in WB_HEADER_PROBE},
            "body": body,
        }
        _state["wb_last"] = rec
        _state["wb_by_path"][path] = rec

    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path
        query = parse_qs(parsed.query)

        # OAuth 昵称查询：best-effort，上游不鉴权，mock 也不鉴权
        if path == WB_PATH_ACCOUNT:
            self._send_json(200, {"code": 0, "data": {"nickname": "mock-workbuddy"}})
            return
        # OAuth 轮询换 token：device_code 流；code 0 + accessToken 即成功
        if path == WB_PATH_AUTH_TOKEN:
            self._send_json(200, {
                "code": 0,
                "data": {
                    "accessToken": UPSTREAM_KEY,
                    "refreshToken": "mock-wb-refresh",
                    "expiresIn": 3600,
                    "domain": "www.workbuddy.ai",
                    "uid": "mock-wb-uid",
                },
            })
            return

        # 调试端点：与业务端点同一 Bearer 鉴权
        if path == "/__wb_last":
            if not self._bearer_ok():
                self._send_json(401, {"error": {"message": "bad upstream key"}})
                return
            want = (query.get("path") or [""])[0]
            rec = _state["wb_by_path"].get(want) if want else _state["wb_last"]
            self._send_json(200, rec or {})
            return
        if path == "/__stats":
            if not self._bearer_ok():
                self._send_json(401, {"error": {"message": "bad upstream key"}})
                return
            self._send_json(200, {
                "total": _state["reqs"],
                "by_path": _state["by_path"],
                "wb_chat": _state["wb_chat"],
            })
            return

        if path == "/v1/models" and self._authed():
            self._send_json(200, {
                "object": "list",
                "data": [{"id": "mock-model-a", "object": "model"}, {"id": "mock-model-b", "object": "model"}],
            })
            return
        # WorkBuddy 全量模型目录：含别名（hy4-preview-f）与 base 对（hy4-preview），
        # 用于验证目录侧的 -f 后缀过滤。
        if path == WB_PATH_CONFIG and self._authed():
            self._record_wb(path, None)
            self._send_json(200, {"code": 0, "data": {"agents": [{"agentType": "cli", "models": [
                {"id": "deepseek-v4.1-flash"},
                {"id": "hy4-preview-f"},
                {"id": "default-model"},
            ]}]}})
            return
        self._send_json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        path = self.path.split("?")[0]

        # OAuth 引导端点：不鉴权、不参与 MODE 故障注入（否则 failover 测试会带崩授权流）
        if path == WB_PATH_AUTH_STATE:
            self._send_json(200, {"code": 0, "data": {"state": "mock-wb-state",
                                                      "authUrl": "https://example.invalid/mock-authorize"}})
            return

        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n) or b"{}")
        if not self._authed():
            self._send_json(401, {"error": {"message": "bad upstream key"}})
            return

        _state["reqs"] += 1
        _state["by_path"][path] = _state["by_path"].get(path, 0) + 1
        if MODE == "fail" or (MODE == "flake" and _state["reqs"] <= FAIL_COUNT):
            self._send_json(503, {"error": {"message": "mock upstream unavailable"}})
            return

        if path == "/v1/chat/completions":
            self._chat_completions(body)
        elif path == "/v1/messages":
            self._messages(body)
        elif path == "/v1/responses":
            self._responses(body)
        # ---- WorkBuddy / CodeBuddy ----
        elif path == WB_PATH_CHAT:
            self._wb_chat(body)
        elif path == WB_PATH_NARROW_MODELS:
            self._record_wb(path, body)
            self._send_json(200, {"code": 0, "data": {"models": [{"id": "hy3"}, {"id": "glm-5.3"}]}})
        elif path == WB_PATH_CHECKIN:
            # 国内版每日签到：body 固定 {}；成功返回 credits
            self._record_wb(path, body)
            self._send_json(200, {"code": 0, "data": {"credits": 1}})
        elif path == WB_PATH_CREDITS:
            # 积分查询：ProbeCredits 在 data 内找 remaining/balance/credits 等键。
            # ★ 真实上游响应形状尚未实测（docs/workbuddy-upstream-notes.md P0 A9）★
            self._record_wb(path, body)
            self._send_json(200, {"code": 0, "data": {"remaining": 42.5}})
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

    # ---- WorkBuddy /v2/chat/completions（忠实还原上游失败模式）----

    def _wb_chat(self, body):
        # 上游只认 Authorization: Bearer；x-api-key 不算
        if not self._bearer_ok():
            self._send_json(401, {"error": {"message": "unauthorized"}})
            return
        self._record_wb(WB_PATH_CHAT, body)
        _state["wb_chat"] += 1

        # 缺 IDE 身分头 → 11128（这就是集成的命门：relay 必须补出一整套 X-IDE-*）
        missing = [h for h in WB_REQUIRED_HEADERS if not self.headers.get(h)]
        if missing:
            self._send_json(400, {"code": 11128,
                                  "message": "illegal API invocation from an unapproved channel"})
            return
        # 上游只接受 stream:true；非流式由 KeyGrid 本地聚合（CollapseSSE）
        if body.get("stream") is not True:
            self._send_json(400, {"code": 11101, "message": "stream is required"})
            return

        model = body.get("model", "unknown")
        last = last_user_text(body)
        prompt_tokens, completion_tokens = 9, 7
        include_usage = bool((body.get("stream_options") or {}).get("include_usage"))
        # deepseek 系必须回 reasoning_content（thinking 路径的产物）
        is_deepseek = str(model).lower().startswith("deepseek")

        self._start_sse()
        if is_deepseek:
            self._emit({"id": "chatcmpl-wb-mock", "object": "chat.completion.chunk", "model": model,
                        "choices": [{"index": 0, "delta": {"reasoning_content": "mock-reasoning"},
                                     "finish_reason": None}]})
        for piece in ["echo", "[", model, "]: ", last]:
            self._emit({"id": "chatcmpl-wb-mock", "object": "chat.completion.chunk", "model": model,
                        "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]})
        self._emit({"id": "chatcmpl-wb-mock", "object": "chat.completion.chunk", "model": model,
                    "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        if include_usage:
            self._emit({"id": "chatcmpl-wb-mock", "object": "chat.completion.chunk", "model": model,
                        "choices": [],
                        "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens,
                                  "total_tokens": prompt_tokens + completion_tokens}})
        self._end_sse(done=True)

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
    # 必须多线程：relay/OAuth 客户端用 keep-alive，单线程 HTTPServer 会被一条空闲
    # 长连接卡在 readline 上，导致后续请求超时（OAuth start 曾因此 30s 超时）。
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
