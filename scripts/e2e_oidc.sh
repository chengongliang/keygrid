#!/usr/bin/env bash
# E2E: OIDC SSO 登录全流程—— mock IdP + 浏览器跳转语义模拟
#
# 前置：
#   - 网关已带 OIDC env 启动（scripts/run_e2e_oidc.sh 会自动组装）
#   - mock IdP 跑在 http://127.0.0.1:9902（issuer 可用 MOCK_OIDC_ISSUER 覆盖）
#
# 流程：注册本地用户 → /api/auth/oidc/login 拿 302 Location → 模拟 IdP 授权拿 code
#   → callback 302 拿 hash 路由里的 sso_token → /api/auth/me 校验 → OIDC-only 密码登录拒绝
#   → 自助改密（绑定用户可改 / SSO-only 账号 403 禁用）
set -euo pipefail

BASE="${BASE:-http://127.0.0.1:8090}"
IDP="${IDP:-http://127.0.0.1:9902}"
SSO_EMAIL="${SSO_EMAIL:-e2e-oidc-$RANDOM@example.com}"

pass=0; fail=0
ok()  { echo "   ✅ $1"; pass=$((pass+1)); }
bad() { echo "   ❌ $1"; fail=$((fail+1)); }
jqd() { python3 -c "import sys,json;d=json.load(sys.stdin);print(json.dumps(d.get('data',d),ensure_ascii=False))"; }

echo "== 1. 本地注册同邮箱用户（测后续 SSO 绑定路径）=="
REG_PASS='LocalPass!234'
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$SSO_EMAIL\",\"password\":\"$REG_PASS\",\"name\":\"本地老用户\"}" >/dev/null \
  && ok "register $SSO_EMAIL" || bad "register failed"

echo "== 2. GET /api/auth/config → oidc_enabled =="
CFG=$(curl -sf $BASE/api/auth/config)
echo "   $CFG"
[ "$(echo "$CFG" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["oidc_enabled"])')" = "True" ] \
  && ok "oidc_enabled=true" || { bad "oidc not enabled"; exit 1; }

echo "== 3. GET /api/auth/oidc/login → 302 IdP authorize =="
LOGIN_RESP=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/api/auth/oidc/login?next=%23%2Fkeys")
HTTP_CODE=${LOGIN_RESP%% *}
AUTH_URL=${LOGIN_RESP#* }
echo "   $HTTP_CODE $AUTH_URL"
[ "$HTTP_CODE" = "302" ] && ok "login 302 → IdP" || { bad "expected 302, got $HTTP_CODE"; exit 1; }

echo "== 4. 模拟浏览器走 IdP /auth（自动同意）→ 302 callback?code&state =="
CB_URL=$(curl -s -o /dev/null -w '%{redirect_url}' "$AUTH_URL&login_hint=$SSO_EMAIL")
echo "   callback: $CB_URL"
echo "$CB_URL" | grep -q 'code=' && ok "IdP issued code" || { bad "no code in callback"; exit 1; }

echo "== 5. 走 callback → 302 前端 hash 路由 + sso_token =="
FINAL=$(curl -s -o /dev/null -w '%{redirect_url}' "$CB_URL")
echo "   final RAW: $FINAL"
TOKEN=$(python3 - "$FINAL" <<'EOF'
import sys
u = sys.argv[1]
for kv in u.split('?', 1)[1].split('&'):
    if kv.startswith('sso_token='):
        print(kv[len('sso_token='):])
EOF
)
[ -n "$TOKEN" ] && ok "got platform jwt (len ${#TOKEN})" || { bad "no sso_token in redirect"; exit 1; }

echo "== 6. /api/auth/me → SSO 用户身份 =="
ME=$(curl -sf $BASE/api/auth/me -H "Authorization: Bearer $TOKEN")
echo "   $ME"
ME_EMAIL=$(echo "$ME" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["email"])')
[ "$ME_EMAIL" = "$SSO_EMAIL" ] && ok "me.email == $SSO_EMAIL" || bad "me.email=$ME_EMAIL"
# 绑定用户已有本地密码 → has_password=true（前端显示改密表单）
ME_HP=$(echo "$ME" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["has_password"])')
[ "$ME_HP" = "True" ] && ok "me.has_password=true（本地密码存在）" || bad "has_password=$ME_HP"

echo "== 7. 邮箱绑定校验：本地用户被绑定 oidc_sub（不再是 OIDC-only）=="
LOC_LOGIN=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/login \
  -H 'Content-Type: application/json' -d "{\"email\":\"$SSO_EMAIL\",\"password\":\"$REG_PASS\"}")
[ "$LOC_LOGIN" = "200" ] && ok "绑定用户本地密码仍可登录 (200)" || bad "bound user local login → $LOC_LOGIN"

echo "== 8. state 重放防护：再次 callback（state 已消费）→ 502 错误页 =="
REPLAY=$(curl -s -o /dev/null -w '%{http_code}' "$CB_URL")
[ "$REPLAY" = "502" ] && ok "replayed state rejected (502)" || bad "replay → $REPLAY (expect 502)"

echo "== 9. 新邮箱 JIT 建号：换一个 IdP 用户 =="
JIT_EMAIL="jit-$RANDOM@example.com"
AUTH2=$(curl -s -o /dev/null -w '%{redirect_url}' "$BASE/api/auth/oidc/login")
AUTH2="${AUTH2}&login_hint=$JIT_EMAIL"
CB2=$(curl -s -o /dev/null -w '%{redirect_url}' "$AUTH2")
FINAL2=$(curl -s -o /dev/null -w '%{redirect_url}' "$CB2")
TOKEN2=$(python3 - "$FINAL2" <<'EOF'
import sys
u = sys.argv[1]
for kv in u.split('?', 1)[1].split('&'):
    if kv.startswith('sso_token='):
        print(kv[len('sso_token='):])
EOF
)
if [ -n "$TOKEN2" ]; then
  ME2=$(curl -sf $BASE/api/auth/me -H "Authorization: Bearer $TOKEN2")
  echo "   $ME2"
  [ "$(echo "$ME2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["email"])')" = "$JIT_EMAIL" ] \
    && ok "JIT 用户 $JIT_EMAIL 建号成功" || bad "JIT user mismatch"
  # JIT 账号为 OIDC-only → has_password=false（前端隐藏改密表单）
  ME2_HP=$(echo "$ME2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["has_password"])')
  [ "$ME2_HP" = "False" ] && ok "me.has_password=false（SSO-only）" || bad "has_password=$ME2_HP"
else
  bad "JIT flow: no token"
fi

echo "== 10. OIDC-only 用户密码登录被拒 =="
# JIT 用户没有 password_hash → 本地登录明确报错
DENY=$(curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$JIT_EMAIL\",\"password\":\"Whatever!123\"}")
echo "   $DENY"
echo "$DENY" | grep -q 'SSO' && ok "oidc-only 拒绝密码登录（明确文案）" || bad "expected SSO message: $DENY"

echo "== 11. RP-Initiated Logout（IdP 配了 end_session → 302）=="
LO=$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/api/auth/logout")
echo "   $LO"
echo "$LO" | grep -qE '^302 .*(end_session|logout)' && ok "logout 302 → IdP end_session" || bad "logout → $LO"

echo "== 12. 自助改密：绑定本地密码的 SSO 用户可改密 =="
SELF_NEW='RotatedPass!234'
CP=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/change_password \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"old_password\":\"$REG_PASS\",\"new_password\":\"$SELF_NEW\"}")
[ "$CP" = "200" ] && ok "change_password 200（有本地密码）" || bad "change_password → $CP"
# 旧密码已失效 → 再用旧密码改密必须 400
REUSE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/change_password \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"old_password\":\"$REG_PASS\",\"new_password\":\"Another!234\"}")
[ "$REUSE" = "400" ] && ok "旧密码改密被拒 (400)" || bad "old password accepted → $REUSE"
# 新密码可登录（绑定账号的本地登录入口不受影响）
NEW_LOGIN=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/login \
  -H 'Content-Type: application/json' -d "{\"email\":\"$SSO_EMAIL\",\"password\":\"$SELF_NEW\"}")
[ "$NEW_LOGIN" = "200" ] && ok "新密码登录成功 (200)" || bad "new password login → $NEW_LOGIN"

echo "== 13. SSO-only 账号：改密功能禁用（403）=="
CP2=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/change_password \
  -H "Authorization: Bearer $TOKEN2" -H 'Content-Type: application/json' \
  -d '{"old_password":"","new_password":"NewPass!234"}')
[ "$CP2" = "403" ] && ok "SSO-only 改密被拒 (403)" || bad "expected 403, got $CP2"

echo
echo "────────────────────────────────"
echo "RESULT: $pass passed, $fail failed"
[ $fail -eq 0 ]
