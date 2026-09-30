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

# =====================================================================================
# WorkBuddy / CodeBuddy 段
#
# 假设网关进程（宿主机 go run）已按如下方式启动（除 E2E_MODE/SSRF 外，WorkBuddy 段需要）：
#   WORKBUDDY_STATE_URL=<mock>/v2/plugin/auth/state
#   WORKBUDDY_TOKEN_URL=<mock>/v2/plugin/auth/token
#   WORKBUDDY_ACCOUNT_URL=<mock>/v2/plugin/login/account
#   WORKBUDDY_BILLING_BASE_INTL=<mock>      # intl 积分端点 → mock
#   WORKBUDDY_BILLING_BASE_CN=<mock>        # CN 签到端点 → mock
#   WORKBUDDY_REFRESH_URL=<mock>/v2/plugin/auth/token/refresh
#   CODEBUDDY_STATE_URL=<mock>/v2/plugin/auth/state
#   CODEBUDDY_TOKEN_URL=<mock>/v2/plugin/auth/token
#   CODEBUDDY_REFRESH_URL=<mock>/v2/plugin/auth/token/refresh
#   CODEBUDDY_ACCOUNT_URL=<mock>/v2/plugin/login/account
# 这些域刻意不跟随渠道 base_url（见 internal/workbuddy/realm.go 注释），只允许环境变量覆盖。
# 缺 CODEBUDDY_* 时 CN 渠道的 OAuth 授权会打到真实上游并失败，脚本会在 15a 前中断。
#
# 另外：任务幂等锁落在 Redis 且按天槽位键控，同一 Redis 上重复跑本脚本时，
# 上一轮已执行过的任务这一轮会变成 skipped —— 本段在 15 之前会清掉任务锁，
# 因此只需要保证「库与 Redis 是新起的或已被清理」，不必每次 FLUSHALL。
# 本段用 /__wb_last、/__stats 调试端点断言上游收到的路径 / 身分头 / 归一化后的请求体。
# =====================================================================================

echo "== 13. WorkBuddy / CodeBuddy 渠道（/v2 上游 + IDE 身分头 + 本地聚合）=="

# 调试端点走宿主机端口：脚本跑在宿主机，无法解析 compose 网络名 mock-upstream。
MOCK_DEBUG="${MOCK_UPSTREAM_DEBUG:-http://127.0.0.1:${MOCK_UPSTREAM_HOST_PORT:-9999}}"
UK='Authorization: Bearer upstream-secret-key'

# WorkBuddy 段断言：不立即退出，按计数汇总后在末尾统一判定（与上面的 set -e 风格共存）。
WBP_OK=0
WBP_NG=0
assert_case() { # assert_case "描述" 命令...  —— 命令退出码 0 记通过
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "   ok   $desc"; WBP_OK=$((WBP_OK + 1))
  else echo "   FAIL $desc"; WBP_NG=$((WBP_NG + 1)); fi
}
refute_case() { # refute_case "描述" 命令...  —— 命令退出码非 0 记通过
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "   FAIL $desc"; WBP_NG=$((WBP_NG + 1))
  else echo "   ok   $desc"; WBP_OK=$((WBP_OK + 1)); fi
}
wb_last() { curl -sf "$MOCK_DEBUG/__wb_last" -H "$UK"; }
wb_last_of() { curl -sf "$MOCK_DEBUG/__wb_last?path=$1" -H "$UK"; }
wb_total_count() { curl -sf "$MOCK_DEBUG/__stats" -H "$UK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["total"])'; }
wb_chat_count() { curl -sf "$MOCK_DEBUG/__stats" -H "$UK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["wb_chat"])'; }
# 直接读 E2E 库校验运行态落库（表名是单数 workbuddy_channel_state）；容器不可用时返回空串。
# 清空任务幂等锁（只删 *:job:* 键，不动 OAuth 刷新锁/缓存）。
wb_flush_job_locks() {
  ( cd "$(dirname "$0")/.." && docker compose exec -T redis redis-cli --scan --pattern '*:job:*' \
    | tr -d '\r' | grep -v '^$' | xargs -r docker compose exec -T redis redis-cli DEL ) >/dev/null 2>&1 || true
}
wb_psql() { ( cd "$(dirname "$0")/.." && docker compose exec -T postgres psql -U keygrid \
  -d "${E2E_DB_NAME:-keygrid_e2e_wb}" -tAc "$1" 2>/dev/null ); }
# 断言最近一次上游请求落在 /v2/chat/completions，且带齐 6 个 IDE 身分头。
wb_assert_upstream_identity() { # $1 = 场景名
  local last; last=$(wb_last)
  assert_case "$1：上游命中 /v2/chat/completions" [ "$(echo "$last" | j '["path"]')" = "/v2/chat/completions" ]
  local h
  for h in X-Ide-Type X-Ide-Name X-Domain X-Product X-IDE-Version X-CodeBuddy-Request; do
    assert_case "$1：身分头 $h 非空" [ -n "$(echo "$last" | j "[\"headers\"][\"$h\"]")" ]
  done
  assert_case "$1：User-Agent 为 WorkBuddy 身分 UA" grep -q '^WorkBuddy/' <<<"$(echo "$last" | j '["headers"]["User-Agent"]')"
}

# 把当前 WorkBuddy 测试用户提升为 admin（PUT /api/admin/settings 需要 admin 角色）。
# 做法与 scripts/e2e_admin.sh 一致：管理端 PATCH /api/admin/users/{id} {"role":"admin"}；
# 没有现成管理员时回退 psql 直改本地 E2E 库（E2E_DB_NAME 默认 keygrid_e2e_wb，生产勿用）。
# 注意：角色取自 JWT claims，改完必须重登拿新 token。
promote_wb_admin() {
  local uid admin_token
  if [ -n "${ADMIN_EMAIL:-}" ] && [ -n "${ADMIN_PASS:-}" ]; then
    admin_token=$(curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
      -d "{\"email\":\"$ADMIN_EMAIL\",\"password\":\"$ADMIN_PASS\"}" | j '["data"]["token"]' 2>/dev/null || true)
    if [ -n "$admin_token" ] && [ "$admin_token" != "None" ]; then
      uid=$(curl -sf "$BASE/api/admin/users?search=$WB_EMAIL" -H "Authorization: Bearer $admin_token" | j '["data"]["items"][0]["id"]')
      curl -sf -X PATCH "$BASE/api/admin/users/$uid" -H "Authorization: Bearer $admin_token" \
        -H 'Content-Type: application/json' -d '{"role":"admin"}' >/dev/null && return 0
    fi
  fi
  ( cd "$(dirname "$0")/.." && docker compose exec -T postgres psql -U keygrid \
      -d "${E2E_DB_NAME:-keygrid_e2e_wb}" -c "UPDATE users SET role='admin' WHERE email='$WB_EMAIL'" ) >/dev/null 2>&1
}

# OAuth（device_code）授权流：start → 轮询 poll 直到 ok（mock 立即返回 token）。
wb_oauth_authorize() { # $1 = provider id
  local pid="$1" st=""
  curl -sf -X POST "$BASE/api/providers/$pid/oauth/start" -H "$WBAUTH" >/dev/null || return 1
  for _ in 1 2 3 4 5; do
    st=$(curl -sf "$BASE/api/providers/$pid/oauth/poll" -H "$WBAUTH" | j '["data"]["status"]')
    [ "$st" = "ok" ] && return 0
    sleep 1
  done
  return 1
}

# ---- 13.1 专用用户（与上文协议矩阵用户隔离，避免准入状态互相影响）----
WB_EMAIL="wb-$RANDOM@example.com"
WB_PASS='Passw0rd!123'
curl -sf -X POST $BASE/api/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$WB_EMAIL\",\"password\":\"$WB_PASS\"}" >/dev/null
WB_TOKEN=$(curl -sf -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$WB_EMAIL\",\"password\":\"$WB_PASS\"}" | j '["data"]["token"]')
WBAUTH="Authorization: Bearer $WB_TOKEN"
assert_case "WorkBuddy 专用用户注册/登录" [ -n "$WB_TOKEN" ]

# ---- 13.2 渠道：api_key(workbuddy-intl) 用于聊天；oauth(workbuddy-intl/codebuddy-cn) 用于任务 ----
UP_KEY='upstream-secret-key'
# 聊天渠道：oauth_provider 决定路由到 WorkBuddy 上游协议（/v2/chat/completions + 身分头）。
WBP_API_ID=$(curl -sf -X POST $BASE/api/providers -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"name":"wb-intl-apikey","kind":"api_key","protocol":"openai","oauth_provider":"workbuddy-intl",
       "base_url":"'"$UPSTREAM"'","api_key":"'"$UP_KEY"'",
       "model_map":{"wb-x":"hy4-preview-f","wb-ds":"deepseek-v4.1-flash","wb-tools":"hy4-preview-f"}}' \
  | j '["data"]["id"]')
# 任务渠道必须是 oauth 凭据：api_key 凭据解密后没有 access token，任务侧无法加载凭据。
WBP_INTL_ID=$(curl -sf -X POST $BASE/api/providers -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"name":"wb-intl-oauth","kind":"oauth","oauth_provider":"workbuddy-intl","protocol":"openai",
       "base_url":"'"$UPSTREAM"'","model_map":{"wb-job":"deepseek-v4.1-flash"}}' | j '["data"]["id"]')
WBP_CN_ID=$(curl -sf -X POST $BASE/api/providers -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"name":"wb-cn-oauth","kind":"oauth","oauth_provider":"codebuddy-cn","protocol":"openai",
       "base_url":"'"$UPSTREAM"'","model_map":{"wb-cn":"hy4-preview-f"}}' | j '["data"]["id"]')
assert_case "创建 api_key(intl) 渠道" [ -n "$WBP_API_ID" ]
assert_case "创建 oauth(intl) 渠道" [ -n "$WBP_INTL_ID" ]
assert_case "创建 oauth(codebuddy-cn) 渠道" [ -n "$WBP_CN_ID" ]
assert_case "workbuddy-intl OAuth 授权完成并落库凭据" wb_oauth_authorize "$WBP_INTL_ID"
assert_case "codebuddy-cn OAuth 授权完成并落库凭据" wb_oauth_authorize "$WBP_CN_ID"

# 准入（P2）：WorkBuddy 渠道默认停用；聊天/任务都要求用户总开关打开。
curl -sf -X PATCH $BASE/api/workbuddy/settings -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"enabled":true}' >/dev/null
assert_case "overview 暴露 settings.enabled=true（准入放行）" \
  grep -q '"enabled":true' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"

WB_AK=$(curl -sf -X POST $BASE/api/keys -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"name":"wb-e2e"}' | j '["data"]["api_key"]')
assert_case "签发 sk- API Key" [ -n "$WB_AK" ]

# ---- 13.3 五条聊天路径 × 上游断言 ----

echo "-- 13.3a 直接验证 mock 的 11128 失败模式（缺身分头 = 未授权渠道）"
NOID=$(curl -s -X POST "$MOCK_DEBUG/v2/chat/completions" -H "$UK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","stream":true}')
assert_case "缺 IDE 身分头时上游回 11128（模拟 WAF）" grep -q '"code":11128' <<<"$NOID"

echo "-- 13.3b /v1/chat/completions 流式"
S=$(curl -sf -N -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","stream":true,"messages":[{"role":"user","content":"wb-hello"}]}')
assert_case "流式返回内容" grep -q 'wb-hello' <<<"$S"
assert_case "流式以 data: [DONE] 收尾" grep -q 'data: \[DONE\]' <<<"$S"
wb_assert_upstream_identity "流式 chat"

echo "-- 13.3c /v1/chat/completions 非流式（上游强制流式 → KeyGrid 本地聚合）"
R=$(curl -sf -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","messages":[{"role":"user","content":"wb-hello"}]}')
assert_case "非流式返回单条 chat.completion JSON" grep -q '"object":"chat.completion"' <<<"$R"
assert_case "非流式带 choices" grep -q '"choices":\[' <<<"$R"
assert_case "非流式含聚合内容" grep -q 'wb-hello' <<<"$R"
refute_case "非流式响应不是裸 SSE" grep -q '^data: ' <<<"$R"
wb_assert_upstream_identity "非流式 chat"
LAST=$(wb_last)
assert_case "上游请求体被强制 stream:true" [ "$(echo "$LAST" | j '["body"]["stream"]')" = "True" ]
assert_case "上游请求体带 stream_options.include_usage" [ "$(echo "$LAST" | j '["body"]["stream_options"]["include_usage"]')" = "True" ]

echo "-- 13.3d /v1/messages 经 WorkBuddy 上游"
R=$(curl -sf -X POST $BASE/v1/messages -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","max_tokens":64,"messages":[{"role":"user","content":"wb-hello"}]}')
assert_case "/v1/messages 转成 anthropic 形状" grep -q '"type":"message"' <<<"$R"
assert_case "/v1/messages 内容来自 WorkBuddy 上游" grep -q 'wb-hello' <<<"$R"
wb_assert_upstream_identity "/v1/messages"

echo "-- 13.3e /v1/responses 经 WorkBuddy 上游"
R=$(curl -sf -X POST $BASE/v1/responses -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","input":"wb-hello"}')
assert_case "/v1/responses 转成 responses 形状" grep -q '"object":"response"' <<<"$R"
assert_case "/v1/responses 内容来自 WorkBuddy 上游" grep -q 'wb-hello' <<<"$R"
wb_assert_upstream_identity "/v1/responses"

echo "-- 13.3f tools + 对象 tool_choice 归一化"
R=$(curl -sf -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-tools","messages":[{"role":"user","content":"weather?"}],
       "tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object","properties":{}}}}],
       "tool_choice":{"type":"function","function":{"name":"get_weather"}}}')
assert_case "带 tools + 对象 tool_choice 的请求返回 200" grep -q '"choices":\[' <<<"$R"
wb_assert_upstream_identity "带 tools 的 chat"
LAST=$(wb_last)
assert_case "tool_choice 对象被降级为字符串" [ "$(echo "$LAST" | j '["body"]["tool_choice"]')" = "get_weather" ]
assert_case "降级后 tools 仍保留" [ "$(echo "$LAST" | j '["body"]["tools"][0]["function"]["name"]')" = "get_weather" ]

echo "-- 13.3g max_completion_tokens → max_tokens"
R=$(curl -sf -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","max_completion_tokens":77,"messages":[{"role":"user","content":"wb-hello"}]}')
assert_case "max_completion_tokens 请求返回 200" grep -q '"choices":\[' <<<"$R"
wb_assert_upstream_identity "max_completion_tokens chat"
LAST=$(wb_last)
assert_case "max_completion_tokens 转成 max_tokens=77" [ "$(echo "$LAST" | j '["body"]["max_tokens"]')" = "77" ]
refute_case "上游请求体已无 max_completion_tokens" grep -q 'max_completion_tokens' <<<"$LAST"

echo "-- 13.3h deepseek 系 thinking 注入 + reasoning_content 回传"
S=$(curl -sf -N -X POST $BASE/v1/chat/completions -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-ds","stream":true,"messages":[{"role":"user","content":"wb-think"}]}')
assert_case "deepseek SSE 含 reasoning_content delta" grep -q 'reasoning_content' <<<"$S"
wb_assert_upstream_identity "deepseek chat"
LAST=$(wb_last)
assert_case "上游模型已映射为 deepseek 系" [ "$(echo "$LAST" | j '["body"]["model"]')" = "deepseek-v4.1-flash" ]
assert_case "注入 thinking.type=enabled" [ "$(echo "$LAST" | j '["body"]["thinking"]["type"]')" = "enabled" ]
assert_case "注入 reasoning_effort" [ -n "$(echo "$LAST" | j '["body"]["reasoning_effort"]')" ]

# ---- 13.3i 手动刷新积分：全新渠道无运行态行也必须落库（op.SetWorkbuddyCredits upsert 回归）----
# 放在开启任务总闸之前，避免后台 Tick 抢先把运行态行建出来，保证「刷新前 0 行」可复现。
echo "-- 13.3i 手动刷新积分（全新渠道刷新前无运行态行）"
STATE_ROWS_BEFORE=$(wb_psql "SELECT count(*) FROM workbuddy_channel_state WHERE provider_id=$WBP_INTL_ID")
assert_case "刷新前该 intl 渠道无运行态行" [ "$STATE_ROWS_BEFORE" = "0" ]
assert_case "手动刷新积分回传 credits=42.5" \
  grep -q '"credits":42.5' <<<"$(curl -sf -X POST "$BASE/api/workbuddy/channels/$WBP_INTL_ID/credits" -H "$WBAUTH")"
assert_case "overview 记录积分 42.5（无运行态行时不再静默丢失）" \
  grep -q '"credits":42.5' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"
assert_case "运行态行已落库 credits=42.5" \
  [ "$(wb_psql "SELECT credits FROM workbuddy_channel_state WHERE provider_id=$WBP_INTL_ID")" = "42.5" ]

# ---- 13.4 准入负向控制：enabled=false → 429 + 零上游调用 ----
echo "== 14. WorkBuddy 准入负向控制（停用 → 429，零上游调用）=="
curl -sf -X PATCH $BASE/api/workbuddy/settings -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"enabled":false}' >/dev/null
assert_case "overview 暴露 settings.enabled=false" \
  grep -q '"enabled":false' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"
BEFORE=$(wb_chat_count)
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/chat/completions \
  -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","messages":[{"role":"user","content":"blocked"}]}')
AFTER=$(wb_chat_count)
assert_case "停用后聊天返回 429（parked）" [ "$CODE" = "429" ]
assert_case "停用后上游零新增选路请求" [ "$BEFORE" = "$AFTER" ]
# 停用原因必须落库：转发侧准入失败点写 workbuddy_channel_state.parked_reason，
# 渠道面板与运维都要读它（Admit 本身只读，所以这一步只能在这里验）。
# 注意断言对象是 wb-x 实际路由到的渠道（api_key 那条），不是 oauth 任务渠道 ——
# 两个渠道都被停用，但只有被真正选路的渠道才会触发准入失败点。
assert_case "停用原因已落库 parked_reason=disabled" \
  [ "$(wb_psql "SELECT parked_reason FROM workbuddy_channel_state WHERE provider_id=$WBP_API_ID")" = "disabled" ]
# 恢复准入，后续任务段需要 settings.enabled=true。
curl -sf -X PATCH $BASE/api/workbuddy/settings -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"enabled":true}' >/dev/null
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST $BASE/v1/chat/completions \
  -H "Authorization: Bearer $WB_AK" -H 'Content-Type: application/json' \
  -d '{"model":"wb-x","messages":[{"role":"user","content":"re-enabled"}]}')
assert_case "重新启用后聊天恢复 200" [ "$CODE" = "200" ]
# 恢复后停用原因应由 observeSuccess 自动清空，否则面板会一直显示旧的停用原因。
assert_case "恢复后 parked_reason 已清空" \
  [ -z "$(wb_psql "SELECT parked_reason FROM workbuddy_channel_state WHERE provider_id=$WBP_API_ID")" ]

# ---- 13.5 平台任务总闸（workbuddy_jobs_enabled）----
echo "== 15. WorkBuddy 任务总闸（平台开关 OFF → ON）=="
# 任务幂等锁在 Redis 里按「任务+用户+渠道+当天」键控：同一 Redis 上重复跑本脚本时，
# 上一轮的手动执行会把这一轮的 run 变成 skipped（message="已在本次窗口执行"），
# 断言随之失真。这里先清掉任务锁键，让脚本可重复执行。
wb_flush_job_locks
# OFF：平台设置缺省 + WORKBUDDY_JOBS 未开 → 三个任务一律被拒，且零上游调用。
T_BEFORE=$(wb_total_count)
for job in checkin credits daily_chat; do
  CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/workbuddy/jobs/$job/run" \
    -H "$WBAUTH" -H 'Content-Type: application/json' -d '{}')
  assert_case "总闸关闭时 $job 被拒绝（403）" [ "$CODE" = "403" ]
done
assert_case "总闸关闭时零上游调用" [ "$T_BEFORE" = "$(wb_total_count)" ]

assert_case "把 WorkBuddy 用户提升为 admin（管理端设置需要）" promote_wb_admin
WB_TOKEN=$(curl -sf -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$WB_EMAIL\",\"password\":\"$WB_PASS\"}" | j '["data"]["token"]')
WBAUTH="Authorization: Bearer $WB_TOKEN"
assert_case "overview 暴露 platform.jobs_allowed=false" \
  grep -q '"jobs_allowed":false' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"
curl -sf -X PUT $BASE/api/admin/settings -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"workbuddy_jobs_enabled":true}' >/dev/null
assert_case "overview 暴露 platform.jobs_allowed=true" \
  grep -q '"jobs_allowed":true' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"

echo "-- 15a checkin（codebuddy-cn → billing 域 /v2/billing/meter/daily-checkin）"
R=$(curl -sf -X POST $BASE/api/workbuddy/jobs/checkin/run -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"provider_id":'"$WBP_CN_ID"'}')
assert_case "checkin 执行成功（status ok）" grep -q '"status":"ok"' <<<"$R"
LAST=$(wb_last_of /v2/billing/meter/daily-checkin)
assert_case "CN checkin 打到 /v2/billing/meter/daily-checkin" [ "$(echo "$LAST" | j '["path"]')" = "/v2/billing/meter/daily-checkin" ]
assert_case "checkin 请求体为 {}" [ "$(echo "$LAST" | j '["body"]')" = "{}" ]
assert_case "checkin 携带 IDE 身分头 X-Ide-Type" [ -n "$(echo "$LAST" | j '["headers"]["X-Ide-Type"]')" ]
assert_case "checkin 携带 X-CodeBuddy-Request" [ -n "$(echo "$LAST" | j '["headers"]["X-CodeBuddy-Request"]')" ]

echo "-- 15b credits（intl → billing 域 /v2/billing/meter/get-user-resource）"
R=$(curl -sf -X POST $BASE/api/workbuddy/jobs/credits/run -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"provider_id":'"$WBP_INTL_ID"'}')
assert_case "credits 执行成功（status ok）" grep -q '"status":"ok"' <<<"$R"
# 任务结果 JSON 字段名契约（RunResult 带 json tag，与 web/src/lib/api.ts 对齐）。
assert_case "任务结果字段为小写 provider_id" grep -q '"provider_id"' <<<"$R"
refute_case "任务结果字段不再是大写 ProviderID" grep -q '"ProviderID"' <<<"$R"
LAST=$(wb_last_of /v2/billing/meter/get-user-resource)
assert_case "credits 打到 /v2/billing/meter/get-user-resource" [ "$(echo "$LAST" | j '["path"]')" = "/v2/billing/meter/get-user-resource" ]
assert_case "credits body 带 ProductCode=p_tcaca" grep -q 'p_tcaca' <<<"$(echo "$LAST" | j '["body"]')"
assert_case "credits 携带 IDE 身分头" [ -n "$(echo "$LAST" | j '["headers"]["X-Ide-Type"]')" ]
# 积分落库/overview 已在 13.3i 断言（那里无后台 Tick 干扰）；这里只验证任务链路本身。

echo "-- 15c daily_chat（intl → chat 域 /v2/chat/completions）"
CBEFORE=$(wb_chat_count)
R=$(curl -sf -X POST $BASE/api/workbuddy/jobs/daily_chat/run -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"provider_id":'"$WBP_INTL_ID"'}')
assert_case "daily_chat 执行成功（status ok）" grep -q '"status":"ok"' <<<"$R"
assert_case "daily_chat 新增 /v2/chat/completions 上游调用" [ "$(wb_chat_count)" -gt "$CBEFORE" ]
LAST=$(wb_last_of /v2/chat/completions)
assert_case "daily_chat 请求体同样强制 stream:true" [ "$(echo "$LAST" | j '["body"]["stream"]')" = "True" ]

# 恢复总闸，避免污染同库后续用例。
curl -sf -X PUT $BASE/api/admin/settings -H "$WBAUTH" -H 'Content-Type: application/json' \
  -d '{"workbuddy_jobs_enabled":false}' >/dev/null
assert_case "任务总闸恢复关闭" \
  grep -q '"jobs_allowed":false' <<<"$(curl -sf $BASE/api/workbuddy/overview -H "$WBAUTH")"

echo "== WorkBuddy 检查汇总: PASS=$WBP_OK FAIL=$WBP_NG =="
if [ "$WBP_NG" -ne 0 ]; then
  echo "== ALL PROTOCOLS E2E FAIL ❌（WorkBuddy 段失败 $WBP_NG 项）=="
  exit 1
fi
echo "== ALL PROTOCOLS E2E PASS ✅ =="
