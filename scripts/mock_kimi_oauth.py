#!/usr/bin/env python3
"""Mock Kimi OAuth server (device_code flow) + mock OpenAI upstream for E2E.

Endpoints:
  POST /api/oauth/device_authorization → {device_code, user_code, ...}
  POST /api/oauth/token                → pending / token (grant: device_code | refresh_token)
  POST /approve?user_code=XX           → 用户"确认授权"（真实流程在 kimi.com 网页完成）
  POST /v1/chat/completions            → OpenAI 兼容 echo（校验 Bearer = 发出的 access_token）
"""
import json
import os
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CLIENT_ID = "17e5f671-d194-4dfb-9706-5516cb48c098"
PORT = int(os.environ.get("MOCK_OAUTH_PORT", "9901"))

_lock = threading.Lock()
_flows = {}      # device_code → {user_code, approved, interval}
_codes = {}      # user_code → device_code
_tokens = {}     # access_token → refresh_token
_refreshed = []  # 记录 refresh 调用次数（验收 token 自动刷新）


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _json(self, obj, code=200):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _form(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n).decode()
        out = {}
        for kv in raw.split("&"):
            if "=" in kv:
                k, v = kv.split("=", 2)
                out[urllib.parse.unquote_plus(k)] = urllib.parse.unquote_plus(v)
        return out

    def do_GET(self):
        self._json({"error": "not found"}, 404)

    def do_POST(self):
        if self.path == "/api/oauth/device_authorization":
            f = self._form()
            if f.get("client_id") != CLIENT_ID:
                self._json({"error": "invalid_client"}, 400)
                return
            dc = "dc-%d" % time.time_ns()
            uc = "MOCK-%04d" % (time.time_ns() % 10000)
            with _lock:
                _flows[dc] = {"user_code": uc, "approved": False, "interval": 1}
                _codes[uc] = dc
            self._json({
                "device_code": dc, "user_code": uc,
                "verification_uri": "http://mock-kimi/approve",
                "expires_in": 300, "interval": 1,
            })
            return

        if self.path == "/api/oauth/token":
            f = self._form()
            grant = f.get("grant_type", "")
            if grant == "urn:ietf:params:oauth:grant-type:device_code":
                with _lock:
                    flow = _flows.get(f.get("device_code", ""))
                if not flow:
                    self._json({"error": "expired_token"}, 400)
                    return
                if not flow["approved"]:
                    # CLIProxyAPI kimi: pending = 200 + error field
                    self._json({"error": "authorization_pending"})
                    return
                at, rt = "at-%s" % flow["user_code"], "rt-%s" % flow["user_code"]
                with _lock:
                    _tokens[at] = rt
                self._json({"access_token": at, "refresh_token": rt, "expires_in": 3600})
                return
            if grant == "refresh_token":
                with _lock:
                    _refreshed.append(f.get("refresh_token"))
                at = "at-refreshed-%d" % time.time_ns()
                with _lock:
                    _tokens[at] = f.get("refresh_token", "")
                self._json({"access_token": at, "refresh_token": "", "expires_in": 3600})
                return
            self._json({"error": "unsupported_grant_type"}, 400)
            return

        if self.path == "/approve":
            f = self._form()
            uc = f.get("user_code", "")
            with _lock:
                dc = _codes.get(uc)
                if dc:
                    _flows[dc]["approved"] = True
            self._json({"ok": bool(dc)})
            return

        if self.path == "/v1/chat/completions":
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n) or b"{}")
            auth = self.headers.get("Authorization", "")
            token = auth.replace("Bearer ", "")
            with _lock:
                ok = token in _tokens
            if not ok:
                self._json({"error": {"message": "bad token"}}, 401)
                return
            last = body.get("messages", [{}])[-1].get("content", "")
            self._json({
                "id": "chatcmpl-mockkimi", "object": "chat.completion",
                "model": body.get("model", "gpt-4o"),
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "kimi-echo: " + last}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11},
            })
            return

        self._json({"error": "not found"}, 404)


if __name__ == "__main__":
    print("mock kimi oauth on :%d" % PORT)
    ThreadingHTTPServer(("0.0.0.0", PORT), H).serve_forever()
