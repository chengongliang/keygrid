#!/usr/bin/env bash
# E2E: 多协议转发矩阵 —— 三入口（chat/completions、messages、responses）× 两协议渠道
# （openai、anthropic）全组合，含流式、x-api-key 鉴权、usage 记账。
# 前置：mock-upstream 服务已起（compose --profile mock；base_url 用 compose 网络名）。
set -e
BASE="${BASE:-http://127.0.0.1:8090}"
UPSTREAM="${MOCK_UPSTREAM:-http://mock-upstream:9999}"
EMAIL="proto-$RANDOM@example.com"
PASS='Passw0rd!123'
RAND=$RANDOM

j() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)"; }

echo "== 1. register/login =="
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" >/dev/null
TOKEN=$(curl -sf -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | j '["data"]["token"]')
AUTH="Authorization: Bearer $TOKEN"
echo "   token ok"

echo "== 2. create channels (openai + anthropic protocol) =="
curl -sf -X POST $BASE/api/providers -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"proto-openai","kind":"api_key","protocol":"openai","base_url":"'"$UPSTREAM"'",
       "api_key":"upstream-secret-key","model_map":{"gpt-x":"mock-model-a"}}' >/dev/null
curl -sf -X POST $BASE/api/providers -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"proto-anthropic","kind":"api_key","protocol":"anthropic","base_url":"'"$UPSTREAM"'",
       "api_key":"upstream-secret-key","model_map":{"claude-x":"mock-model-a"}}' >/dev/null
AK=$(curl -sf -X POST $BASE/api/keys -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"name":"proto-e2e"}' | j '["data"]["api_key"]')
echo "   channels + api key ok"

echo "== 3. /v1/messages → anthropic channel (protocol match) =="
R=$(curl -sf -X POST $BASE/v1/messages -H "x-api-key: $AK" -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hello-proto"}]}')
echo "   $R" | head -c 200; echo
echo "$R" | grep -q '"type":"message"' || { echo "FAIL: anthropic shape"; exit 1; }
echo "$R" | grep -q 'anthropic-echo' || { echo "FAIL: echo content"; exit 1; }
echo "$R" | grep -q '"input_tokens":11' || { echo "FAIL: usage passthrough"; exit 1; }
echo "   ok (x-api-key auth + raw passthrough)"

echo "== 4. /v1/messages → openai channel (converted to anthropic shape) =="
R=$(curl -sf -X POST $BASE/v1/messages -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-x","max_tokens":100,"messages":[{"role":"user","content":"hello-proto"}]}')
echo "$R" | grep -q '"type":"message"' || { echo "FAIL: shape"; exit 1; }
echo "$R" | grep -q 'echo\[mock-model-a\]' || { echo "FAIL: openai→anthropic convert"; exit 1; }
echo "$R" | grep -q '"input_tokens":9' || { echo "FAIL: usage convert (prompt→input)"; exit 1; }
echo "   ok"

echo "== 5. /v1/chat/completions → anthropic channel (converted to openai shape) =="
R=$(curl -sf -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","messages":[{"role":"user","content":"hello-proto"}]}')
echo "$R" | grep -q '"object":"chat.completion"' || { echo "FAIL: shape"; exit 1; }
echo "$R" | grep -q 'anthropic-echo' || { echo "FAIL: anthropic→openai convert"; exit 1; }
echo "$R" | grep -q '"completion_tokens":5' || { echo "FAIL: usage convert (output→completion)"; exit 1; }
echo "   ok"

echo "== 6. /v1/responses → openai channel (converted to responses shape) =="
R=$(curl -sf -X POST $BASE/v1/responses -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-x","input":"hello-proto"}')
echo "$R" | grep -q '"object":"response"' || { echo "FAIL: shape"; exit 1; }
echo "$R" | grep -q 'echo\[mock-model-a\]' || { echo "FAIL: openai→responses convert"; exit 1; }
echo "$R" | grep -q '"input_tokens":9' || { echo "FAIL: usage convert"; exit 1; }
echo "   ok"

echo "== 7. /v1/responses → anthropic channel (converted to responses shape) =="
R=$(curl -sf -X POST $BASE/v1/responses -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","input":"hello-proto"}')
echo "$R" | grep -q '"object":"response"' || { echo "FAIL: shape"; exit 1; }
echo "$R" | grep -q 'anthropic-echo' || { echo "FAIL: anthropic→responses convert"; exit 1; }
echo "   ok"

echo "== 8. stream: /v1/messages → anthropic channel (raw SSE passthrough) =="
S=$(curl -sf -N -X POST $BASE/v1/messages -H "x-api-key: $AK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello-proto"}]}')
echo "$S" | grep -q '"type":"message_start"' || { echo "FAIL: message_start"; exit 1; }
echo "$S" | grep -q '"type":"message_stop"' || { echo "FAIL: message_stop"; exit 1; }
echo "$S" | grep -q 'anthropic-echo' || { echo "FAIL: stream content"; exit 1; }
echo "   ok"

echo "== 9. stream: /v1/messages → openai channel (bridged to anthropic events) =="
S=$(curl -sf -N -X POST $BASE/v1/messages -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-x","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hello-proto"}]}')
echo "$S" | grep -q '"type":"message_start"' || { echo "FAIL: message_start"; exit 1; }
echo "$S" | grep -q '"type":"message_delta"' || { echo "FAIL: message_delta"; exit 1; }
echo "$S" | grep -q '"type":"message_stop"' || { echo "FAIL: message_stop"; exit 1; }
echo "$S" | grep -q 'hello-proto' || { echo "FAIL: bridged content"; exit 1; }
echo "   ok"

echo "== 10. stream: /v1/responses → openai channel (bridged to responses events) =="
S=$(curl -sf -N -X POST $BASE/v1/responses -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-x","input":"hello-proto","stream":true}')
echo "$S" | grep -q '"type":"response.created"' || { echo "FAIL: response.created"; exit 1; }
echo "$S" | grep -q '"type":"response.output_text.delta"' || { echo "FAIL: output_text.delta"; exit 1; }
echo "$S" | grep -q '"type":"response.completed"' || { echo "FAIL: response.completed"; exit 1; }
echo "$S" | grep -q '"input_tokens":9' || { echo "FAIL: usage in completed"; exit 1; }
echo "   ok"

echo "== 11. stream: /v1/chat/completions → anthropic channel (bridged to chat chunks) =="
S=$(curl -sf -N -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $AK" -H 'Content-Type: application/json' \
  -d '{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hello-proto"}]}')
echo "$S" | grep -q '"content":"anthropic-echo"' || { echo "FAIL: chat chunk content"; exit 1; }
echo "$S" | grep -q 'data: \[DONE\]' || { echo "FAIL: [DONE]"; exit 1; }
echo "   ok"

sleep 3
echo "== 12. usage recorded (model + tokens) =="
U=$(curl -sf "$BASE/api/usage" -H "$AUTH")
echo "$U" | grep -q '"model":"claude-x"' || { echo "FAIL: usage model"; exit 1; }
echo "$U" | grep -q '"model":"gpt-x"' || { echo "FAIL: usage model openai"; exit 1; }
echo "   ok"

echo "== ALL PROTOCOLS E2E PASS ✅ =="
