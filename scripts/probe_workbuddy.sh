#!/usr/bin/env bash
# WorkBuddy 上游事实探针（P0）
#
# 目的：用真实账号验证三件在代码阅读里无法确认的事，为
# docs/workbuddy-integration-design.md 提供实测依据：
#   1. chat 上游路径到底是 /v2/chat/completions 还是 /v2/v1/chat/completions（G1）
#   2. 最小 header 集是否足够，缺 X-IDE-* / X-Product / X-Domain 是否必然 11128（G2）
#   3. 上游是否强制 stream:true，非流请求会怎样（G3）
# 附带采集：模型目录来源与数量、积分查询、token refresh 轮换行为、上游错误码。
#
# 用法：
#   # 免费检查（默认，不消耗额度）
#   WB_TOKEN='xxx' WB_UID='123456' WB_REALM=intl bash scripts/probe_workbuddy.sh
#   # 带 refresh token（脚本自己换 access token）
#   WB_REFRESH_TOKEN='xxx' WB_UID='123456' WB_REALM=cn bash scripts/probe_workbuddy.sh
#   # 聊天矩阵（★会真实消耗账号额度★，必须显式开启）
#   PROBE_CHAT=1 WB_TOKEN='xxx' WB_UID='123456' bash scripts/probe_workbuddy.sh
#
# 环境变量：
#   WB_REALM       intl | cn                        默认 intl
#   WB_PRODUCT     workbuddy | vscode | cli          默认 workbuddy
#   WB_UPSTREAM    覆盖 chat base（E2E 打桩用）      默认按 realm 取
#   PROBE_CHAT     1 = 跑聊天矩阵（会花额度）        默认 0
#   PROBE_MODEL    聊天矩阵使用的模型               默认取目录里第一个
#   PROBE_MAXTOK   聊天矩阵单次 max_tokens          默认 16
#   OUT            汇总 JSON 落盘路径               默认 /tmp/workbuddy-probe-<时间>.json
#
# 安全约定（硬性）：
#   - 绝不回显 token，绝不打印 prompt 与响应正文；
#   - 只记录 HTTP 状态码、上游 code/msg（截断 120 字符）、响应字节数、content-type；
#   - 上游错误摘要的提取规则与 KeyGrid 一致：只取 error.message/code/type（防上游回显 prompt 片段）。
#
# 退出码：0 = 所有免费检查通过；1 = 有检查失败；2 = 参数/依赖问题。
set -uo pipefail

REALM="${WB_REALM:-intl}"
PRODUCT="${WB_PRODUCT:-workbuddy}"
PROBE_CHAT="${PROBE_CHAT:-0}"
PROBE_MAXTOK="${PROBE_MAXTOK:-16}"
TIMEOUT="${WB_PROBE_TIMEOUT:-30}"

case "$REALM" in
  intl|cn) ;;
  *) echo "❌ WB_REALM 只能是 intl 或 cn（收到 '$REALM'）" >&2; exit 2 ;;
esac

command -v curl >/dev/null || { echo "❌ 需要 curl" >&2; exit 2; }
HAVE_PY=0
if command -v python3 >/dev/null; then HAVE_PY=1
elif command -v python >/dev/null; then HAVE_PY=1
else
  echo "⚠️  未找到 python3：JSON 字段提取降级为 grep，汇总 JSON 里的结构化字段会少一些"
fi

# ---- 区域常量（对齐 wb_identity.py:44-53 与 wb_accounts.py:119-139）----
if [ "$REALM" = "intl" ]; then
  BASE_DEFAULT="https://www.workbuddy.ai"
  BILLING_DEFAULT="https://www.workbuddy.ai"
  DOMAIN_DEFAULT="www.workbuddy.ai"
  UA_DESKTOP="WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
else
  BASE_DEFAULT="https://copilot.tencent.com"
  BILLING_DEFAULT="https://www.codebuddy.cn"
  DOMAIN_DEFAULT="copilot.tencent.com"
  UA_DESKTOP="WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
fi
BASE="${WB_UPSTREAM:-$BASE_DEFAULT}"
BILLING="${WB_BILLING:-$BILLING_DEFAULT}"

case "$PRODUCT" in
  workbuddy|wb|desktop) PRODUCT=workbuddy ;;
  vscode|vsc)           PRODUCT=vscode ;;
  cli|codebuddy)        PRODUCT=cli ;;
  *) echo "❌ WB_PRODUCT 只能是 workbuddy/vscode/cli" >&2; exit 2 ;;
esac

OUT="${OUT:-/tmp/workbuddy-probe-$(date +%Y%m%d-%H%M%S).json}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# 结果累积（TSV：检查项<TAB>用例<TAB>状态码<TAB>上游code<TAB>字节数<TAB>判定<TAB>备注）
RESULTS="$WORK/results.tsv"
: > "$RESULTS"

log()  { printf '%s\n' "$*"; }
ok()   { printf '  ✅ %s\n' "$*"; }
bad()  { printf '  ❌ %s\n' "$*"; }
skip() { printf '  ⏭️  %s\n' "$*"; }

record() { # 检查项 用例 状态码 code 字节数 判定 备注
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" "$5" "$6" "$7" >> "$RESULTS"
}

# 提取 JSON 字段（点路径，如 data.accessToken）。失败返回空串。
json_get() {
  local file="$1" path="$2"
  # 无 python 时的降级提取：正则整体用单引号包住（双引号版本里的 \" 会提前闭合引号，必踩）
  [ "$HAVE_PY" = "1" ] || { grep -o '"[^"]*":[[:space:]]*"[^"]*"' "$file" 2>/dev/null | head -1 | cut -d'"' -f4; return; }
  python3 - "$file" "$path" <<'PY' 2>/dev/null
import json,sys
try:
    d=json.load(open(sys.argv[1],encoding='utf-8'))
except Exception:
    sys.exit(0)
cur=d
for part in sys.argv[2].split('.'):
    if isinstance(cur,list):
        try: cur=cur[int(part)]
        except Exception: sys.exit(0)
    elif isinstance(cur,dict):
        cur=cur.get(part)
    else:
        sys.exit(0)
    if cur is None: sys.exit(0)
print(cur if not isinstance(cur,(dict,list)) else json.dumps(cur,ensure_ascii=False))
PY
}

# 统计 JSON 数组长度（点路径）
json_len() {
  local file="$1" path="$2"
  [ "$HAVE_PY" = "1" ] || { echo ""; return; }
  python3 - "$file" "$path" <<'PY' 2>/dev/null
import json,sys
try:
    d=json.load(open(sys.argv[1],encoding='utf-8'))
except Exception:
    sys.exit(0)
cur=d
for part in sys.argv[2].split('.'):
    if isinstance(cur,list):
        try: cur=cur[int(part)]
        except Exception: sys.exit(0)
    elif isinstance(cur,dict):
        cur=cur.get(part)
    else:
        sys.exit(0)
print(len(cur) if isinstance(cur,(list,dict)) else "")
PY
}

# 上游错误摘要：只取 error.message/code/type 或顶层 code/msg，截断 120 字符（与 KeyGrid reqerr 同规则）
err_summary() {
  local file="$1"
  [ "$HAVE_PY" = "1" ] || { head -c 120 "$file" | tr -d '\n'; return; }
  python3 - "$file" <<'PY' 2>/dev/null
import json,sys
raw=open(sys.argv[1],encoding='utf-8',errors='replace').read()
out=""
try:
    d=json.loads(raw)
    if isinstance(d,dict):
        e=d.get("error")
        if isinstance(e,dict):
            out=" ".join(str(e.get(k,"")) for k in ("message","code","type") if e.get(k))
        if not out:
            out=" ".join(str(d.get(k,"")) for k in ("msg","message","code") if d.get(k) not in (None,"",0))
except Exception:
    out=""
print(out[:120])
PY
}

# 构造 header 参数数组：$1 = min|full -> 结果写入全局 HDR_ARGS
# 注：刻意不用 nameref / 关联数组，兼容 macOS 自带 bash 3.2。
HDR_ARGS=()
header_args() {
  local mode="$1"
  HDR_ARGS=()
  HDR_ARGS+=( -H "Content-Type: application/json" -H "Accept: application/json" )
  HDR_ARGS+=( -H "X-Requested-With: XMLHttpRequest" -H "X-CodeBuddy-Request: 1" )
  HDR_ARGS+=( -H "Origin: https://$DOMAIN_DEFAULT" -H "Referer: https://$DOMAIN_DEFAULT/" )
  HDR_ARGS+=( -H "Accept-Language: zh-CN,zh;q=0.9" )
  if [ "$mode" = "min" ]; then
    return
  fi
  HDR_ARGS+=( -H "X-User-Id: $UID_CLAIM" )
  HDR_ARGS+=( -H "X-IDE-Version: 5.5.6" -H "X-Domain: $DOMAIN_DEFAULT" )
  case "$PRODUCT" in
    workbuddy)
      HDR_ARGS+=( -H "X-Agent-Purpose: conversation" -H "X-IDE-Name: WorkBuddy" -H "X-IDE-Type: WorkBuddy" -H "X-Product: WorkBuddy" )
      HDR_ARGS+=( -H "User-Agent: $UA_DESKTOP" -H "X-Request-ID: $(uuid_hex)" ) ;;
    vscode)
      HDR_ARGS+=( -H "X-Agent-Intent: craft" -H "X-IDE-Name: VSCode" -H "X-IDE-Type: VSCode" -H "X-Product: SaaS" )
      HDR_ARGS+=( -H "X-Env-ID: production" -H "X-Product-Version: 4.9.29177644" -H "X-IDE-Version: 1.119.0" )
      HDR_ARGS+=( -H "User-Agent: VSCode/1.119.0 WorkBuddy/4.9.29177644" )
      HDR_ARGS+=( -H "X-Conversation-ID: $CONV_ID" -H "X-Conversation-Request-ID: $(uuid_hex)" -H "X-Conversation-Message-ID: $(uuid_hex)" -H "X-Request-ID: $(uuid_hex)" ) ;;
    cli)
      HDR_ARGS+=( -H "X-Agent-Intent: craft" -H "X-IDE-Name: CLI" -H "X-IDE-Type: CLI" -H "X-IDE-Version: 2.117.2" -H "X-Product: SaaS" )
      HDR_ARGS+=( -H "User-Agent: CLI/2.117.2 CodeBuddy/2.117.2" )
      HDR_ARGS+=( -H "X-Conversation-ID: $CONV_ID" -H "X-Conversation-Request-ID: $(uuid_hex)" -H "X-Conversation-Message-ID: $(uuid_hex)" -H "X-Request-ID: $(uuid_hex)" ) ;;
  esac
}

uuid_hex() {
  if [ "$HAVE_PY" = "1" ]; then python3 -c 'import uuid;print(uuid.uuid4().hex)'
  else printf '%08x%08x%08x%08x' "$RANDOM" "$RANDOM" "$RANDOM" "$RANDOM"; fi
}

# 发一次请求并回填 RESP_STATUS（先建空文件，避免 wc/json 在无响应时读不到文件）。
# 用法：fetch <输出文件> <curl 参数...>
RESP_STATUS="000"
fetch() {
  local out="$1"; shift
  : > "$out"
  local code
  code="$(curl -sS -o "$out" -w '%{http_code}' --max-time "$TIMEOUT" "$@" 2>/dev/null)"
  case "$code" in
    ''|000) RESP_STATUS="000" ;;
    *)      RESP_STATUS="$code" ;;
  esac
}

# 发一次请求：$1=方法 $2=url $3=body（空串=无 body） $4=模式(min|full) $5=额外 -H ... $6=输出文件
# 回填全局 RESP_STATUS / RESP_CODE
request() {
  local method="$1" url="$2" body="$3" mode="$4" extra="$5" outfile="$6"
  header_args "$mode"
  local args=( -X "$method" --max-time "$TIMEOUT" )
  local h
  for h in $extra; do args+=( -H "$h" ); done
  if [ -n "$body" ]; then args+=( --data "$body" ); fi
  fetch "$outfile" "${args[@]}" "${HDR_ARGS[@]}" -H "Authorization: Bearer $ACCESS_TOKEN" "$url"
  RESP_CODE="$(json_get "$outfile" 'code')"
}

echo "=============================================================="
echo " WorkBuddy 上游探针（P0）"
echo "   区域: $REALM   身分: $PRODUCT    chat base: $BASE"
echo "   时间: $(date '+%Y-%m-%d %H:%M:%S %z')"
echo "   聊天矩阵: $([ "$PROBE_CHAT" = 1 ] && echo '开启（会消耗额度）' || echo '关闭')"
echo "   汇总 JSON: $OUT"
echo "=============================================================="

# ---------------- 凭据准备 ----------------
ACCESS_TOKEN="${WB_TOKEN:-}"
CONV_ID=""
UID_CLAIM="${WB_UID:-}"

if [ -n "${WB_REFRESH_TOKEN:-}" ]; then
  log ""
  log "[0] token refresh（验证轮换行为）"
  RT_FILE="$WORK/refresh.json"
  fetch "$RT_FILE" -X POST "$BASE/v2/plugin/auth/token/refresh" \
    -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "User-Agent: $UA_DESKTOP" -H "X-Domain: $DOMAIN_DEFAULT" -H "X-Product: SaaS" \
    -H "X-Refresh-Token: $WB_REFRESH_TOKEN" -H "X-Auth-Refresh-Source: plugin" \
    --data '{}'
  ST="$RESP_STATUS"
  NEW_TOKEN="$(json_get "$RT_FILE" 'data.accessToken')"
  NEW_RT="$(json_get "$RT_FILE" 'data.refreshToken')"
  if [ "$ST" = "200" ] && [ -n "$NEW_TOKEN" ]; then
    ACCESS_TOKEN="$NEW_TOKEN"
    if [ -n "$NEW_RT" ] && [ "$NEW_RT" != "$WB_REFRESH_TOKEN" ]; then RT_ROT="轮换（refresh_token 单次使用型）"; else RT_ROT="不轮换（可重复使用）"; fi
    ok "refresh 成功，$RT_ROT"
    record "refresh" "X-Refresh-Token" "$ST" "$(json_get "$RT_FILE" 'code')" "$(wc -c < "$RT_FILE")" "PASS" "$RT_ROT"
  else
    bad "refresh 失败（status=$ST）: $(err_summary "$RT_FILE")"
    record "refresh" "X-Refresh-Token" "$ST" "$(json_get "$RT_FILE" 'code')" "$(wc -c < "$RT_FILE")" "FAIL" "$(err_summary "$RT_FILE")"
  fi
fi

if [ -z "$ACCESS_TOKEN" ]; then
  log ""
  log "❌ 缺少 WB_TOKEN 或 WB_REFRESH_TOKEN —— 无法继续"
  log "   获取方式：看板 OAuth 授权后从 accounts/*.json 取 accessToken（不要贴到任何地方）"
  exit 2
fi

# uid：优先 WB_UID；否则尝试从未验证的 JWT claim 里读（仅用于 header，不校验签名）
if [ -z "$UID_CLAIM" ]; then
  UID_CLAIM="$(printf '%s' "$ACCESS_TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null \
    | grep -o '"uid":[[:space:]]*"[^"]*"' | head -1 | cut -d'"' -f4)"
fi
[ -n "$UID_CLAIM" ] || UID_CLAIM="anonymous"
CONV_ID="$(uuid_hex | tr 'a-f' 'A-F')"

# ---------------- [1] 全量目录 /v3/config ----------------
log ""
log "[1] 全量模型目录 GET /v3/config（上游要求桌面 UA，否则 400/12403）"
CFG_FILE="$WORK/config.json"
CFG_HDRS=( -H "Accept: application/json" -H "User-Agent: $UA_DESKTOP" -H "X-Domain: $DOMAIN_DEFAULT" -H "X-Product: WorkBuddy" -H "X-IDE-Type: WorkBuddy" -H "X-IDE-Name: WorkBuddy" -H "X-IDE-Version: 5.5.6" )
fetch "$CFG_FILE" "${CFG_HDRS[@]}" -H "Authorization: Bearer $ACCESS_TOKEN" "$BASE/v3/config"
ST="$RESP_STATUS"
CLI_LEN="$(json_len "$CFG_FILE" 'data.agents.0.models')"
[ -z "$CLI_LEN" ] && CLI_LEN="$(json_len "$CFG_FILE" 'data.agents.0')"
if [ "$ST" = "200" ]; then
  ok "目录可达；agents[0] 条目数=$CLI_LEN"
  record "catalog" "GET /v3/config (桌面 UA)" "$ST" "$(json_get "$CFG_FILE" 'code')" "$(wc -c < "$CFG_FILE")" "PASS" "agents[0]=$CLI_LEN"
else
  bad "目录不可达（status=$ST）: $(err_summary "$CFG_FILE")"
  record "catalog" "GET /v3/config (桌面 UA)" "$ST" "$(json_get "$CFG_FILE" 'code')" "$(wc -c < "$CFG_FILE")" "FAIL" "$(err_summary "$CFG_FILE")"
fi

log ""
log "[1b] 目录回落：窄端点 GET /v2/enterprises/personal/models"
NM_FILE="$WORK/narrow_models.json"
request GET "$BASE/v2/enterprises/personal/models" "" full "" "$NM_FILE"
NM_LEN="$(json_len "$NM_FILE" 'data')"
[ -z "$NM_LEN" ] && NM_LEN="$(json_len "$NM_FILE" 'models')"
if [ "$RESP_STATUS" = "200" ]; then
  ok "窄端点可达；条目数=$NM_LEN"
  record "catalog" "GET /v2/enterprises/personal/models" "$RESP_STATUS" "$RESP_CODE" "$(wc -c < "$NM_FILE")" "PASS" "n=$NM_LEN"
  # 供聊天矩阵挑一个模型
  if [ -z "${PROBE_MODEL:-}" ] && [ "$HAVE_PY" = "1" ]; then
    PROBE_MODEL="$(python3 - "$NM_FILE" <<'PY' 2>/dev/null
import json,sys
try: d=json.load(open(sys.argv[1],encoding='utf-8'))
except Exception: sys.exit(0)
def walk(o):
    if isinstance(o,dict):
        for k in ("modelId","model","id","name"):
            v=o.get(k)
            if isinstance(v,str) and v and not any(x in v.lower() for x in ("auto","default-model","fast-model")):
                return v
        for v in o.values():
            r=walk(v)
            if r: return r
    if isinstance(o,list):
        for v in o:
            r=walk(v)
            if r: return r
    return None
print(walk(d) or "")
PY
)"
  fi
else
  bad "窄端点不可达（status=$RESP_STATUS）: $(err_summary "$NM_FILE")"
  record "catalog" "GET /v2/enterprises/personal/models" "$RESP_STATUS" "$RESP_CODE" "$(wc -c < "$NM_FILE")" "FAIL" "$(err_summary "$NM_FILE")"
fi

# ---------------- [2] 积分查询 ----------------
log ""
log "[2] 积分查询 POST $BILLING/v2/billing/meter/get-user-resource"
CR_FILE="$WORK/credits.json"
CR_BODY='{"PageNumber":1,"PageSize":100,"ProductCode":"p_tcaca","Status":[0,3]}'
fetch "$CR_FILE" -X POST "$BILLING/v2/billing/meter/get-user-resource" \
  -H "Content-Type: application/json" -H "Accept: application/json" -H "User-Agent: $UA_DESKTOP" \
  -H "X-Domain: $DOMAIN_DEFAULT" -H "X-Product: WorkBuddy" -H "X-IDE-Type: WorkBuddy" -H "X-IDE-Name: WorkBuddy" \
  -H "X-IDE-Version: 5.5.6" -H "X-User-Id: $UID_CLAIM" -H "X-CodeBuddy-Request: 1" \
  -H "Authorization: Bearer $ACCESS_TOKEN" --data "$CR_BODY"
ST="$RESP_STATUS"
if [ "$ST" = "200" ]; then
  ok "积分接口可达（余额数值需人工在响应里核对，本脚本不打印）"
  record "credits" "POST get-user-resource" "$ST" "$(json_get "$CR_FILE" 'code')" "$(wc -c < "$CR_FILE")" "PASS" ""
else
  bad "积分接口不可达（status=$ST）: $(err_summary "$CR_FILE")"
  record "credits" "POST get-user-resource" "$ST" "$(json_get "$CR_FILE" 'code')" "$(wc -c < "$CR_FILE")" "FAIL" "$(err_summary "$CR_FILE")"
fi

# ---------------- [3] 签到可用性（CN） ----------------
log ""
log "[3] 签到端点可用性（仅 CN；intl 上游无签到，wb_accounts.py:126 has_checkin=False）"
if [ "$REALM" = "cn" ]; then
  log "  ⏭️  跳过真实签到（会改变账号状态，请在网关上手动执行一次）"
  log "     端点：POST $BILLING/v2/billing/meter/daily-checkin  body {}"
  record "checkin" "POST daily-checkin" "-" "-" "-" "SKIP" "需人工确认，脚本不代签"
else
  skip "intl 无签到端点（预期）"
  record "checkin" "n/a (intl)" "-" "-" "-" "SKIP" "intl has_checkin=False"
fi

# ---------------- [4] 聊天矩阵（★消耗额度★） ----------------
log ""
log "[4] 聊天矩阵（路径 × header 集 × stream）"
if [ "$PROBE_CHAT" != "1" ]; then
  skip "未开启（设 PROBE_CHAT=1 才跑）—— 这是唯一会消耗账号额度的部分"
  record "chat" "matrix" "-" "-" "-" "SKIP" "PROBE_CHAT!=1"
else
  [ -n "${PROBE_MODEL:-}" ] || PROBE_MODEL="deepseek-v4.1-flash"
  log "  模型: $PROBE_MODEL   max_tokens: $PROBE_MAXTOK   prompt: (固定短句，不打印)"
  log "  ⚠️  会真实消耗额度；3 条候选路径 × 2 种 header 集 × 2 种 stream = 12 次"
  CANDIDATES=(
    "$BASE/v2/chat/completions"
    "$BASE/v2/v1/chat/completions"
    "$BASE/v1/chat/completions"
  )
  for url in "${CANDIDATES[@]}"; do
    for mode in min full; do
      for stream in 0 1; do
        BODY="{\"model\":\"$PROBE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Hi\"}],\"max_tokens\":$PROBE_MAXTOK,\"stream\":$([ "$stream" = 1 ] && echo true || echo false)}"
        F="$WORK/chat.json"
        request POST "$url" "$BODY" "$mode" "" "$F"
        CT="$(file -b --mime-type "$F" 2>/dev/null || echo unknown)"
        SSE="no"; head -c 200 "$F" | grep -q '^data:' && SSE="yes"
        CODE="$RESP_CODE"; ES="$(err_summary "$F")"
        # 用例名取 URL 的 path（去掉 scheme://host），否则三条候选会显示成同一个名字
        CASE="${url#*://*/} | hdr=$mode | stream=$stream"
        if [ "$RESP_STATUS" = "200" ] && { [ "$SSE" = "yes" ] || [ "$stream" = "0" ]; }; then
          ok "200  $CASE  (bytes=$(wc -c < "$F"), sse=$SSE)"
          record "chat" "$CASE" "$RESP_STATUS" "$CODE" "$(wc -c < "$F")" "PASS" "sse=$SSE"
        else
          bad "$RESP_STATUS $CASE  code=$CODE  $ES"
          record "chat" "$CASE" "$RESP_STATUS" "$CODE" "$(wc -c < "$F")" "FAIL" "$ES sse=$SSE"
        fi
        sleep 1   # 避免连击触发风控/限流
      done
    done
  done
fi

# ---------------- [5] 归一化验证（默认不跑） ----------------
log ""
log "[5] 参数归一化验证（developer role / tool_choice 对象 / max_tokens）"
if [ "$PROBE_CHAT" != "1" ]; then
  skip "未开启（设 PROBE_CHAT=1）"
  record "normalize" "matrix" "-" "-" "-" "SKIP" "PROBE_CHAT!=1"
else
  # 逐条显式列出（不用关联数组，兼容 bash 3.2）
  run_norm_case() {
    local name="$1" body="$2" expect_note="$3"
    local F="$WORK/norm.json"
    request POST "$BASE/v2/chat/completions" "$body" full "" "$F"
    if [ "$RESP_STATUS" = "200" ]; then
      ok "200  $name（raw 形式可直接通过）"
      record "normalize" "$name" "$RESP_STATUS" "$RESP_CODE" "$(wc -c < "$F")" "PASS" "raw 可通过"
    else
      bad "$RESP_STATUS $name  code=$RESP_CODE  $(err_summary "$F")"
      record "normalize" "$name" "$RESP_STATUS" "$RESP_CODE" "$(wc -c < "$F")" "FAIL" "$(err_summary "$F") —— $expect_note"
    fi
    sleep 1
  }
  run_norm_case "developer-role" \
    "{\"model\":\"$PROBE_MODEL\",\"messages\":[{\"role\":\"developer\",\"content\":\"Hi\"}],\"max_tokens\":$PROBE_MAXTOK}" \
    "需在 relay 侧把 developer 归一化为 system"
  run_norm_case "tool-choice-object" \
    "{\"model\":\"$PROBE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Hi\"}],\"max_tokens\":$PROBE_MAXTOK,\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"noop\",\"parameters\":{\"type\":\"object\"}}}],\"tool_choice\":{\"type\":\"function\",\"function\":{\"name\":\"noop\"}}}" \
    "需在 relay 侧把 tool_choice 对象降级为字符串"
  run_norm_case "max-tokens-legacy" \
    "{\"model\":\"$PROBE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Hi\"}],\"max_tokens\":$PROBE_MAXTOK}" \
    "需在 relay 侧把 max_tokens 转为 max_completion_tokens"
fi

# ---------------- 汇总 ----------------
log ""
log "=============================================================="
log " 汇总"
log "=============================================================="
printf '%-9s %-52s %-7s %-7s %-9s %s\n' "CHECK" "CASE" "STATUS" "CODE" "BYTES" "VERDICT"
while IFS=$'\t' read -r chk case st code bytes verdict note; do
  [ -n "${chk:-}" ] || continue
  printf '%-9s %-52s %-7s %-7s %-9s %s\n' "$chk" "$case" "$st" "$code" "$bytes" "$verdict"
done < "$RESULTS"

if [ "$HAVE_PY" = "1" ]; then
  python3 - "$RESULTS" "$OUT" "$REALM" "$PRODUCT" "$BASE" <<'PY'
import json,sys,csv
res,out,realm,product,base=sys.argv[1:6]
rows=[]
with open(res,encoding='utf-8') as f:
    for line in f:
        parts=line.rstrip('\n').split('\t')
        if len(parts)>=7:
            rows.append(dict(zip(["check","case","status","code","bytes","verdict","note"],parts)))
doc={"schema":"keygrid.workbuddy.probe/v1","realm":realm,"product":product,
     "chat_base":base,"rows":rows,
     "passed":sum(1 for r in rows if r["verdict"]=="PASS"),
     "failed":sum(1 for r in rows if r["verdict"]=="FAIL"),
     "skipped":sum(1 for r in rows if r["verdict"]=="SKIP"),
     "note":"本文件不含 token、prompt 与响应正文；可直接粘贴摘要表进 docs/workbuddy-upstream-notes.md"}
json.dump(doc,open(out,"w",encoding='utf-8'),ensure_ascii=False,indent=2)
print("\n汇总 JSON → %s  (PASS=%d FAIL=%d SKIP=%d)"%(out,doc["passed"],doc["failed"],doc["skipped"]))
PY
else
  cp "$RESULTS" "$OUT"
  log "汇总 TSV → $OUT"
fi

FAILED="$(awk -F'\t' '$6=="FAIL"' "$RESULTS" | wc -l | tr -d ' ')"
log ""
log "下一步：把上面的摘要表与结论填进 docs/workbuddy-upstream-notes.md（模板已就绪）。"
[ "$FAILED" = "0" ] && exit 0 || exit 1
