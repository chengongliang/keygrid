#!/usr/bin/env bash
# E2E: OAuth (kimi device_code) 渠道闭环 —— 对 mock OAuth 服务器 + mock 上游
set -e
BASE="${BASE:-http://127.0.0.1:8090}"
EMAIL="m2test-$RANDOM@example.com"
PASS='Passw0rd!123'
echo "== 1. register/login =="
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" >/dev/null
TOKEN=$(curl -sf -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["token"])')
AUTH="Authorization: Bearer $TOKEN"
echo "   token ok"

echo "== 2. create oauth provider (kimi) =="
PROV=$(curl -sf -X POST $BASE/api/providers -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"kimi-e2e","kind":"oauth","oauth_provider":"kimi","protocol":"openai","base_url":"'"$MOCK_OAUTH_UPSTREAM"'"}')
PID=$(echo "$PROV" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["id"])')
echo "   provider id=$PID"

echo "== 3. oauth/start (device_code) =="
START=$(curl -sf -X POST $BASE/api/providers/$PID/oauth/start -H "$AUTH")
echo "   $START"
USER_CODE=$(echo "$START" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["user_code"])')
DEVICE_ID=$(echo "$START" | python3 -c 'import sys,json;d=json.load(sys.stdin)["data"];print(d.get("interval",""))')

echo "== 4. oauth/poll → pending =="
P1=$(curl -sf $BASE/api/providers/$PID/oauth/poll -H "$AUTH")
echo "   poll#1: $P1"
[ "$(echo $P1 | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["status"])')" = "pending" ] || { echo "FAIL: expected pending"; exit 1; }

echo "== 5. approve user_code on mock kimi =="
curl -sf -X POST ${APPROVE_URL:-http://127.0.0.1:9901}/approve -d "user_code=$USER_CODE" >/dev/null

echo "== 6. oauth/poll → ok =="
for i in 1 2 3 4 5; do
  P=$(curl -sf $BASE/api/providers/$PID/oauth/poll -H "$AUTH")
  ST=$(echo $P | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["status"])')
  echo "   poll: $ST"
  [ "$ST" = "ok" ] && break
  sleep 1
done
[ "$ST" = "ok" ] || { echo "FAIL: poll never ok"; exit 1; }

echo "== 7. issue api key + relay through kimi channel =="
AK=$(curl -sf -X POST $BASE/api/keys -H "$AUTH" -H 'Content-Type: application/json' -d '{"name":"e2e"}' | \
  python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["api_key"])')
RELAY=$(curl -sf -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello-oauth"}]}')
echo "   relay: $RELAY"
echo "$RELAY" | grep -q 'hello-oauth' || { echo "FAIL: relay content"; exit 1; }

sleep 3
echo "== 8. usage recorded =="
U=$(curl -sf "$BASE/api/usage" -H "$AUTH")
echo "   $U" | head -c 300; echo
echo "$U" | grep -q 'gpt-4o' || { echo "FAIL: usage model"; exit 1; }

echo "== ALL E2E PASS ✅ =="
