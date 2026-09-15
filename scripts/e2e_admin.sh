#!/usr/bin/env bash
# E2E: 管理员功能闭环
# 前置：平台已运行（BASE，默认 http://127.0.0.1:8090），且 env ADMIN_EMAIL 指向的
#       用户已存在（首个 admin）；或库里已有 admin。可用 ADMIN_EMAIL 环境变量覆盖。
# 流程：注册普通用户 → admin 提升 → 用户列表/搜索 → 禁用用户 → 被禁用户登录失败
#       → 重置密码可登录 → 注册策略 closed 拒绝注册 → 维护模式 /v1 503 → 恢复
set -e
BASE="${BASE:-http://127.0.0.1:8090}"
ADMIN_EMAIL="${ADMIN_EMAIL:-david@company.com}"
ADMIN_PASS="${ADMIN_PASS:-}"
RAND=$RANDOM

j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

echo "== 0. healthz =="
curl -sf $BASE/healthz >/dev/null
echo "   ok"

echo "== 1. admin login ($ADMIN_EMAIL) =="
if [ -z "$ADMIN_PASS" ]; then
  echo "   ✗ 需要 ADMIN_PASS 环境变量（admin 账号密码）"; exit 1
fi
ADMIN_TOKEN=$(curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" | j '["data"]["token"]') || { echo "   ✗ admin login failed"; exit 1; }
[ -n "$ADMIN_TOKEN" ] && [ "$ADMIN_TOKEN" != "None" ] || { echo "   ✗ admin login failed"; exit 1; }
AUTH="Authorization: Bearer $ADMIN_TOKEN"
echo "   token ok"

echo "== 2. register victim user =="
VICTIM="m4v-$RAND@example.com"
VPASS='VictimPass!234'
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$VICTIM\",\"password\":\"$VPASS\"}" >/dev/null
echo "   $VICTIM registered"

echo "== 3. promote victim to admin then demote (role roundtrip) =="
VID=$(curl -sf "$BASE/api/admin/users?search=$VICTIM" -H "$AUTH" | j '["data"]["items"][0]["id"]')
echo "   victim id=$VID"
curl -sf -X PATCH $BASE/api/admin/users/$VID -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"role":"admin"}' | grep -q '"role":"admin"' || { echo "   FAIL: promote"; exit 1; }
curl -sf -X PATCH $BASE/api/admin/users/$VID -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"role":"user"}' | grep -q '"role":"user"' || { echo "   FAIL: demote"; exit 1; }
echo "   role user→admin→user ok"

echo "== 4. victim (non-admin) gets 403 on admin API =="
VTOKEN=$(curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$VICTIM\",\"password\":\"$VPASS\"}" | j '["data"]["token"]')
CODE=$(curl -s -o /dev/null -w '%{http_code}' $BASE/api/admin/users -H "Authorization: Bearer $VTOKEN")
[ "$CODE" = "403" ] || { echo "   FAIL: expected 403, got $CODE"; exit 1; }
echo "   403 ok"

echo "== 5. user list + search =="
curl -sf "$BASE/api/admin/users?search=$VICTIM" -H "$AUTH" | grep -q "$VICTIM" || { echo "   FAIL: search"; exit 1; }
echo "   search ok"

echo "== 6. disable victim → keys revoked =="
curl -sf -X PATCH $BASE/api/admin/users/$VID -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"status":"disabled"}' >/dev/null
echo "   disabled"

echo "== 7. disabled victim login fails =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$VICTIM\",\"password\":\"$VPASS\"}")
[ "$CODE" = "401" ] || { echo "   FAIL: expected 401, got $CODE"; exit 1; }
echo "   401 ok"

echo "== 8. reset victim password → can login again =="
NEWPASS=$(curl -sf -X POST $BASE/api/admin/users/$VID/reset_password -H "$AUTH" | j '["data"]["password"]')
[ -n "$NEWPASS" ] || { echo "   FAIL: no new password"; exit 1; }
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$VICTIM\",\"password\":\"$NEWPASS\"}")
[ "$CODE" = "200" ] || { echo "   FAIL: expected 200 after reset, got $CODE"; exit 1; }
echo "   reset+login ok (pass len=${#NEWPASS})"

echo "== 9. registration policy: closed → register 403 =="
curl -sf -X PUT $BASE/api/admin/settings -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"registration_policy":"closed"}' >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"m4x-$RAND@example.com\",\"password\":\"Whatever!123\"}")
[ "$CODE" = "403" ] || { echo "   FAIL: expected 403, got $CODE"; exit 1; }
echo "   closed ok"

echo "== 10. registration policy: invite → no code 403 / bad code 403 =="
curl -sf -X PUT $BASE/api/admin/settings -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"registration_policy":"invite"}' >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"m4y-$RAND@example.com\",\"password\":\"Whatever!123\"}")
[ "$CODE" = "403" ] || { echo "   FAIL: expected 403 without code, got $CODE"; exit 1; }
INV=$(curl -sf -X POST $BASE/api/admin/invitations -H "$AUTH" -H 'Content-Type: application/json' -d '{}' | j '["data"]["code"]')
[ -n "$INV" ] || { echo "   FAIL: no invitation code"; exit 1; }
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"m4y-$RAND@example.com\",\"password\":\"Whatever!123\",\"invite_code\":\"$INV\"}" >/dev/null
# 码已被消费：再次使用必须失败
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"m4z-$RAND@example.com\",\"password\":\"Whatever!123\",\"invite_code\":\"$INV\"}")
[ "$CODE" = "403" ] || { echo "   FAIL: reused invite code accepted ($CODE)"; exit 1; }
echo "   invite ok (code=$INV, single-use verified)"

echo "== 11. settings + usage + providers admin endpoints =="
curl -sf $BASE/api/admin/settings -H "$AUTH" | grep -q maintenance_mode || { echo "   FAIL: settings read"; exit 1; }
curl -sf "$BASE/api/admin/usage?by=day&days=7" -H "$AUTH" >/dev/null
curl -sf "$BASE/api/admin/usage/top?days=7" -H "$AUTH" >/dev/null
curl -sf "$BASE/api/admin/usage/export?by=day&days=7" -H "$AUTH" | head -1 | grep -q 'user_id' || { echo "   FAIL: csv header"; exit 1; }
curl -sf $BASE/api/admin/providers -H "$AUTH" >/dev/null
echo "   admin read endpoints ok"

echo "== 12. maintenance mode → /v1 503 =="
curl -sf -X PUT $BASE/api/admin/settings -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"maintenance_mode":true,"announcement":"M4 e2e maintenance"}' >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/chat/completions \
  -H "Authorization: Bearer sk-invalid-maintenance-probe" -H 'Content-Type: application/json' -d '{}')
# 无效 key：维护模式应先于鉴权返回 503；若网关先鉴权则 401 也算实现选择之一，但设计要求 503
[ "$CODE" = "503" ] || { echo "   FAIL: expected 503, got $CODE"; exit 1; }
# 管理端在维护模式下依然可用
curl -sf $BASE/api/admin/settings -H "$AUTH" | grep -q '"maintenance_mode":true' || { echo "   FAIL: admin api during maintenance"; exit 1; }
echo "   503 + admin usable ok"

echo "== 13. restore: maintenance off + registration open =="
curl -sf -X PUT $BASE/api/admin/settings -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"maintenance_mode":false,"registration_policy":"open"}' >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' $BASE/v1/chat/completions \
  -H "Authorization: Bearer sk-e2e-maintenance-probe" -H 'Content-Type: application/json' -d '{}')
[ "$CODE" != "503" ] || { echo "   FAIL: still 503 after restore"; exit 1; }
echo "   restored ok"

echo "== ALL E2E PASS ✅ =="
