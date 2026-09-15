#!/usr/bin/env python3
"""Mock OIDC IdP (E2E) — 最小标准 OIDC Provider，Keycloak 兼容流程。

Endpoints:
  GET  /.well-known/openid-configuration   → discovery 文档
  GET  /realms/mock/protocol/openid-connect/auth  → 模拟 IdP 登录页：
          ?response_type=code&client_id&redirect_uri&scope&state&nonce&code_challenge...
          302 redirect_uri?code=...&state=...（自动"同意"）
  POST /realms/mock/protocol/openid-connect/token → authorization_code(+PKCE) 换 id_token
  GET  /realms/mock/protocol/openid-connect/certs   → JWKS（RSA 公钥）
  GET  /realms/mock/protocol/openid-connect/userinfo → userinfo（access_token 校验）
  POST /e2e/user                                  → 注册/更新模拟用户（email, name）

issuer 默认 http://127.0.0.1:9902，可用 MOCK_OIDC_ISSUER 覆盖（容器内用 http://mock-oidc:9902）。
"""
import base64
import json
import os
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
import jwt as pyjwt

PORT = int(os.environ.get("MOCK_OIDC_PORT", "9902"))
ISSUER = os.environ.get("MOCK_OIDC_ISSUER", f"http://127.0.0.1:{PORT}").rstrip("/")

_lock = threading.Lock()
_users = {}    # email → {sub, email, name, email_verified}
_codes = {}    # code → {sub, redirect_uri, challenge, expires}
_tokens = {}   # access_token → sub

# ---- RSA 密钥（进程内生成，kid 固定）----
_kid = "mock-oidc-key-1"
_priv = rsa.generate_private_key(public_exponent=65537, key_size=2048)
_pub = _priv.public_key()
_pubnums = _pub.public_numbers()

def _b64u(n: int, length: int) -> str:
    return base64.urlsafe_b64encode(n.to_bytes(length, "big")).rstrip(b"=").decode()

def _issue_id_token(sub: str, nonce: str) -> str:
    u = _users[sub]
    now = int(time.time())
    claims = {
        "iss": ISSUER,
        "sub": sub,
        "aud": "keygrid",
        "exp": now + 300,
        "iat": now,
        "email": u["email"],
        "email_verified": u.get("email_verified", True),
        "name": u.get("name", ""),
    }
    if nonce:
        claims["nonce"] = nonce
    return pyjwt.encode(claims, _priv, algorithm="RS256", headers={"kid": _kid})


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, format, *args):
        pass

    def _json(self, obj, code=200):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _redirect(self, location):
        self.send_response(302)
        self.send_header("Location", location)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _form(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n).decode()
        return dict(urllib.parse.parse_qsi(kv) if False else urllib.parse.parse_qsl(kv, keep_blank_values=True)) if (kv := raw) else {}

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        q = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(self.path).query, keep_blank_values=True))

        if path == "/.well-known/openid-configuration":
            base = ISSUER + "/realms/mock/protocol/openid-connect"
            self._json({
                "issuer": ISSUER,
                "authorization_endpoint": base + "/auth",
                "token_endpoint": base + "/token",
                "userinfo_endpoint": base + "/userinfo",
                "jwks_uri": base + "/certs",
                "end_session_endpoint": base + "/logout",
                "id_token_signing_alg_values_supported": ["RS256"],
            })
            return

        if path.endswith("/certs"):
            self._json({"keys": [{
                "kid": _kid, "kty": "RSA", "alg": "RS256", "use": "sig",
                "n": _b64u(_pubnums.n, (_pubnums.n.bit_length() + 7) // 8),
                "e": _b64u(_pubnums.e, (_pubnums.e.bit_length() + 7) // 8),
            }]})
            return

        if path.endswith("/auth"):
            # 模拟 IdP 登录页：直接"同意"，签发 code
            if q.get("client_id") != "keygrid":
                self._json({"error": "invalid_client"}, 400)
                return
            email = q.get("login_hint", "e2e-oidc@example.com")
            with _lock:
                u = _users.get(email)
                if not u:
                    sub = "sub-" + email.replace("@", "-at-")
                    u = {"sub": sub, "email": email, "name": email.split("@")[0], "email_verified": True}
                    _users[sub] = u
            code = "code-" + str(time.time_ns())
            with _lock:
                _codes[code] = {
                    "sub": u["sub"],
                    "redirect_uri": q.get("redirect_uri", ""),
                    "challenge": q.get("code_challenge", ""),
                    "nonce": q.get("nonce", ""),
                    "expires": time.time() + 120,
                }
            sep = "&" if "?" in q.get("redirect_uri", "") else "?"
            self._redirect(q.get("redirect_uri", "") + sep + urllib.parse.urlencode({
                "code": code, "state": q.get("state", ""),
            }))
            return

        if path.endswith("/userinfo"):
            auth = self.headers.get("Authorization", "")
            tok = auth[7:] if auth.startswith("Bearer ") else ""
            with _lock:
                sub = _tokens.get(tok)
            if not sub:
                self._json({"error": "invalid_token"}, 401)
                return
            with _lock:
                u = dict(_users.get(sub, {}))
            u.pop("sub", None)
            self._json({"sub": sub, **u})
            return

        self._json({"error": "not found"}, 404)

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        n = int(self.headers.get("Content-Length", 0))
        form = dict(urllib.parse.parse_qsl(self.rfile.read(n).decode(), keep_blank_values=True))

        if path == "/e2e/user":
            email = form.get("email", "")
            with _lock:
                sub = form.get("sub") or ("sub-" + email.replace("@", "-at-"))
                _users[sub] = {"sub": sub, "email": email,
                               "name": form.get("name", email.split("@")[0]),
                               "email_verified": True}
            self._json({"ok": True, "sub": sub, "email": email})
            return

        if path.endswith("/token"):
            if form.get("grant_type") != "authorization_code":
                self._json({"error": "unsupported_grant_type"}, 400)
                return
            if form.get("client_id") != "keygrid" or form.get("client_secret") != "mock-secret":
                self._json({"error": "invalid_client"}, 401)
                return
            with _lock:
                c = _codes.get(form.get("code", ""))
                if c:
                    _codes.pop(form["code"], None)  # code 一次性
            if not c or time.time() > c["expires"]:
                self._json({"error": "invalid_grant", "error_description": "code expired or used"}, 400)
                return
            if c["redirect_uri"] != form.get("redirect_uri", ""):
                self._json({"error": "invalid_grant", "error_description": "redirect_uri mismatch"}, 400)
                return
            # PKCE S256 校验
            import hashlib
            calc = base64.urlsafe_b64encode(hashlib.sha256(form.get("code_verifier", "").encode()).digest()).rstrip(b"=").decode()
            if not c["challenge"] or calc != c["challenge"]:
                self._json({"error": "invalid_grant", "error_description": "pkce verification failed"}, 400)
                return
            at = "at-" + str(time.time_ns())
            with _lock:
                _tokens[at] = c["sub"]
            self._json({
                "access_token": at,
                "id_token": _issue_id_token(c["sub"], c.get("nonce", "")),
                "token_type": "Bearer",
                "expires_in": 300,
            })
            return

        self._json({"error": "not found"}, 404)


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), H)
    print(f"mock oidc idp on {ISSUER}")
    srv.serve_forever()
