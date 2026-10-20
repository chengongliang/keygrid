# WorkBuddy 上游事实记录（P0 探针）

> 本文件是 **实测记录**，不是设计文档。设计见 [workbuddy-integration-design.md](./workbuddy-integration-design.md)。
>
> 采集方式：`bash scripts/probe_workbuddy.sh`（见该脚本头部的用法说明）。
> 汇总 JSON 默认落在 `/tmp/workbuddy-probe-<时间>.json`，把摘要表粘贴到下面「一、采集结果」。
>
> **禁止写入本文件的内容**：accessToken / refreshToken / 任何密钥、账号昵称与 UID（可写 UID 前 4 位）、
> prompt 原文、模型响应正文。允许写入：HTTP 状态码、上游 `code`/`msg`（≤120 字符）、字节数、
> content-type、模型 ID 列表、时间戳、限额数值。

---

## 〇、元信息

| 项 | 值 |
|---|---|
| 采集人 | |
| 采集时间 | |
| 账号区域 | `intl` / `cn`（二选一） |
| 出站身分 | `workbuddy` / `vscode` / `cli` |
| chat base | |
| 探测脚本版本（git commit） | |
| KeyGrid 版本 | |
| 是否跑了聊天矩阵（消耗额度） | 是 / 否 |

---

## 一、采集结果（粘贴脚本摘要表）

```
CHECK     CASE                                                 STATUS  CODE    BYTES     VERDICT
（粘贴到这里）
```

---

## 二、待验证假设（来自代码阅读，等待本表逐条打勾）

来源缩写：[WBI] = wb2a `wb_identity.py`，[WBA] = wb2a `wb_accounts.py`，[WBP] = wb2a `wb_proxy.py`，[KG] = KeyGrid 本仓库。

| # | 假设 | 来源 | 预期 | 实测 | 结论 |
|---|---|---|---|---|---|
| A1 | chat 上游路径是 `/v2/chat/completions`（**不是** `/v2/v1/chat/completions`） | [WBP] `wb_proxy.py:141`；[KG] `relay/route.go:84` 恒拼 `/v1/chat/completions` | 前者 200、后者 404/400 | | ☐ 成立 ☐ 不成立 |
| A2 | 缺少 `X-IDE-Type/Name/Version` + `X-Product` + `X-Domain` 会被判 `11128 illegal API invocation from an unapproved channel` | [WBP] `wb_proxy.py:2391-2394`；[WBI] `wb_identity.py:76-145` | `min` header 集失败、`full` 成功 | | ☐ |
| A3 | 上游强制 `stream:true`：非流请求失败或返回非标准体 | [WBP] `wb_proxy.py:3040` | `stream=false` 失败 | | ☐ |
| A4 | `role:"developer"` 会被拒（11128） | [WBP] `wb_proxy.py:2349` | 失败，需归一化为 `system` | | ☐ |
| A5 | `tool_choice` 传对象会被拒（11101） | [WBP] `wb_proxy.py:2664` | 失败，需降级为字符串 | | ☐ |
| A6 | `max_tokens` 需要转成 `max_completion_tokens` | [WBP] `wb_proxy.py:2756` | 失败或忽略 | | ☐ |
| A7 | 全量目录 `/v3/config` 必须带桌面 UA，否则 400 / `12403` | [WBP] `wb_proxy.py:1979,1989-2000` | 无桌面 UA 失败 | | ☐ |
| A8 | 窄端点 `/v2/enterprises/personal/models` 可作目录回落 | [WBP] `wb_proxy.py:142` | 200 且条目数 > 0 | | ☐ |
| A9 | 积分接口 `POST {billing}/v2/billing/meter/get-user-resource` 可用，body 需 `PageNumber/PageSize/ProductCode:"p_tcaca"/Status:[0,3]` | [WBA] `wb_accounts.py:151` | 200 | | ☐ |
| A10 | CN 有签到 `POST {billing}/v2/billing/meter/daily-checkin`（body `{}`），INTL 无 | [WBA] `wb_accounts.py:126,150` | CN 可达 | | ☐ |
| A11 | refresh 走 `POST /v2/plugin/auth/token/refresh`，refresh_token 放在 `X-Refresh-Token` 头（非 body） | [WBA] `wb_accounts.py:149,524-560` | 200 | | ☐ |
| A12 | refresh_token 是单次使用型（会轮换） | [WBA] 账号级锁内换新 | 新旧 refresh_token 不同 | | ☐ |
| A13 | `X-Machine-ID` / `X-Session-ID` 缺失 **不是**硬失败（可不做指纹伪造） | [WBA] `wb_accounts.py:470-501` 只在 billing 头里发 | chat 仍 200 | | ☐ |
| A14 | 401 需刷新/重试；429 只冷却该模型（`Retry-After` 或 60s） | [WBA] `wb_accounts.py:415-446`；[WBP] `wb_proxy.py:3264` | | | ☐ |
| A15 | 响应头/错误体里能看到余额或限额窗口（供被动观察） | 待查 | | | ☐ |

---

## 三、上游错误码字典（本表是 KeyGrid `request_errors` 分类与准入过滤的输入）

| code | 含义 | 触发场景 | 应映射到的行为 | 来源 |
|---|---|---|---|---|
| 0 | 成功 | — | — | [WBA] |
| 11128 | Illegal API invocation from an unapproved channel | 缺身分头；`developer` role；客户端特征 | 记 `errKindUpstream4xx`；**不熔断**（渠道健康，是请求被拒） | [WBP] `wb_proxy.py:2391` |
| 11101 | 参数形态非法 | `tool_choice` 传对象等 | 同上；在 relay 侧归一化后应消失 | [WBP] `wb_proxy.py:2664` |
| 11217 | 授权等待中（OAuth 轮询） | state 轮询未完成 | OAuth 层 `PendingError`，继续轮询 | [WBA] `wb_accounts.py:1225` |
| 6004 | 上游 token 窗口打满（限额） | 大请求、当日额度用尽 | 短期冷却该渠道；准入过滤优先换号 | [WBP] `apply_daily_token_limit` 相关 |
| 12302 | 活动模块下线（banner） | 国际版活跃活动 | **不代表**打卡失败；不据此判定 | wb2a v1.6.9 changelog |
| 12403 | `/v3/config` 未授权的 UA | 非桌面 UA 拉目录 | 退回窄端点/快照，不告警 | [WBP] `wb_proxy.py:1989` |
'
| 11140 | `request illegal` — 账号被风控限制（`displayMsg`: 账号存在异常行为，已被限制使用） | 触发风控的请求 | **HTTP 403**；冷却该渠道并换号（与「内容审核 403」区分：后者不冷却、降级后透传） | [WBA/WBP] 上游 `be4f9f8` (#288) |
| 11148 | `tool_call_sequence_broken` — 工具调用序列形状不被接受 | 并发工具批次被拆成相邻多条 assistant 消息 | 归一化阶段合并相邻 `tool_calls` 后应消失 | [WBP] `merge_adjacent_tool_calls` |
| 待补 | | | | |

---

## 四、模型目录

| 项 | 值 |
|---|---|
| `/v3/config` 可达 | 是 / 否 |
| `agents[cli].models` 条目数 | |
| 过滤后条目数（去档位别名/`auto`/`-sg`/`-x`，同名留 0.00 档） | |
| 窄端点条目数 | |
| 实际用于 E2E 打桩的模型 ID | |
| 与 wb2a 内置快照（intl 17 / cn 14）的差异 | |

模型 ID 清单（只写 ID，不要写别的）：

```
（粘贴）
```

---

## 五、限额与积分观察

| 项 | 观察值 | 说明 |
|---|---|---|
| 单账号积分余额 | | 只记数值，不记账号 |
| 积分接口被调用频率上限 | | 是否 429 |
| 免费档模型倍率 | | 如 `hy4-preview-f` x0.00 |
| 聊天请求的 token 计量在哪（非流/流） | | 决定记账路径 |
| 429 时 `Retry-After` 是否存在 | | 决定模型冷却 TTL 取值 |
| 上游是否返回余额相关响应头 | | 决定能否被动观察（`quota.Syncer` 形态） |

---

## 六、结论：对设计的影响（回填）

1. **端点（A1）**：driver 的 `Endpoint()` 必须覆盖默认拼接吗？ → 是 / 否
2. **header 最小集（A2）**：确定为必发头的清单：
   ```
   （列出实测后仍 200 的最小子集）
   ```
3. **强制流式（A3）**：relay 需要「强制 stream + 非流本地聚合」吗？ → 是 / 否；聚合口径：
4. **归一化清单（A4–A6）**：确认必须做的：`developer→system` / `tool_choice 降级` / `max_tokens 转换` / 其它：
5. **指纹与脱敏（A13）**：
   - `X-Machine-ID` / `X-Session-ID`：必发 / 不必发
   - WAF 脱敏：不做 / shadow / 默认开
6. **多账号**：确认「一账号一渠道」可行（无账号级会话亲和需求）→ 是 / 否
7. **准入过滤阈值来源**：积分接口 / 响应头 / usage 折算
8. **E2E mock 需要模拟的形状**：写出 mock 必须支持的路径与响应形态，供 `scripts/mock_upstream.py` 扩展
9. **新增/修正的缺口编号**：G1 ☐ 确认 ☐ 推翻；G2 ☐ 确认 ☐ 推翻；G3 ☐ 确认 ☐ 推翻；新增 G：

---

## 七、复现步骤（下次重跑照做）

```bash
# 1) 从已授权账号取 token（不要打印、不要提交）
#    网关面板 → 渠道 → 该渠道凭据；或看板 accounts/<uid>.json 的 accessToken
export WB_TOKEN='...'        # 只放在当前 shell，勿写入文件
export WB_UID='...'          # 只填前 4 位到本文档
export WB_REALM=intl         # 或 cn
export WB_PRODUCT=workbuddy

# 2) 免费检查（目录/窄端点/积分/refresh）
bash scripts/probe_workbuddy.sh

# 3) 聊天矩阵（★消耗额度★，6 + 3 次请求）
PROBE_CHAT=1 bash scripts/probe_workbuddy.sh

# 4) 用内网打桩验证脚本本身（不碰真实上游）
WB_UPSTREAM=http://127.0.0.1:9999 WB_BILLING=http://127.0.0.1:9999 bash scripts/probe_workbuddy.sh
```

---

## 八、签字

| 项 | 值 |
|---|---|
| 采集人确认：未写入 token / prompt / 响应正文 | ☐ |
| 复核人 | |
| 结论是否足以支撑实现（门槛：A1–A3 + A7–A9 有结论，错误码字典覆盖 11128/6004） | ☐ 是 ☐ 否，还缺： |

---

## 附录：P2 实测记录（本地烟测，非真实上游）

在隔离库（`keygrid_wb_smoke`，用完即删）里对**假渠道**（无凭据、base_url 指向
`https://www.workbuddy.ai`）做的接线验证 —— 目的是证明「拦截发生在该发生的地方」，
**不代表任何上游协议事实**：

| 场景 | 结果 |
| --- | --- |
| 用户关闭总开关（`settings.enabled=false`） | chat / messages / responses 三入口均被拦在上游调用之前，返回 `channel WB smoke parked: disabled`；`request_errors.kind = channel_parked`（provider_id 正确） |
| 用户打开总开关但无凭据 | 请求继续走到凭据解密，`credential` 分类（证明拦截确实只发生在准入层） |
| 跨用户访问 | 用户 B 的 overview 只含自己的渠道；用自己的 token 访问用户 A 的渠道 ID → 404 |
| 设置校验 | 非法 `product` / 未知任务键 → 400；`jobs_enabled` 局部合并不清空其他键 |

仍待真实账号补齐：`/v3/config` 与积分接口的**真实响应形状**（A9 的 `remaining`
候选键名未命中时，用 `ProbeCredits` 报出的键名清单收敛），以及 A1–A3 / A7–A8。

## 附：授权面（state/token/refresh）实测校正

真实账号首次授权时报 `begin auth failed: workbuddy state request failed (400)`，
比对参考实现（`wb_accounts.py`）后确认是移植时漏了参数与头部，已修：

| 项 | 参考实现（wb_accounts.py） | 修正前 | 修正后 |
|---|---|---|---|
| state URL | `POST {chat}/v2/plugin/auth/state?platform=CLI`（:1203） | 无 `?platform` → **上游 400** | 带 `?platform=CLI` |
| 授权面 UA | `billing_ua` = `WorkBuddy/5.5.2`（:127） | chat 面 UA `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2` | `WorkBuddy/5.5.2` |
| Accept | `application/json, text/plain, */*`（:1208） | `application/json` | 与参考实现一致 |
| token 轮询 | 只有 `?state=`，**不带** X-CodeBuddy-Request（:1239-1247） | 带了 `X-CodeBuddy-Request: 1` | 去掉 |
| refresh | 额外带 `X-User-Id`/`X-Domain`/`X-CodeBuddy-Request`/`Accept-Language`（:544-553） | 只有 `X-Refresh-Token`/`X-Auth-Refresh-Source` | 补齐 |

结论：**授权面（插件登录）与 chat 面是两套头部**。chat 面要 `X-Ide-*`/`X-Domain` 等
身份头（relay 侧 `workbuddy.Headers`），授权面用 `billing_ua` 那组、且 state 必须带
`platform=CLI`。同目录 CN 适配器（`codebuddy.go`）一直是这么做的，intl 移植时漏了。

### 实测证据（真实上游，2026-09-30）

直接对 `https://www.workbuddy.ai` 发起 state 请求，两组对照：

```
# 缺 platform（修正前的 KeyGrid 请求）
POST /v2/plugin/auth/state            → HTTP 400
{"code":10001,"msg":"10001:platform is empty","requestId":"..."}

# 带 platform=CLI + billing UA（修正后）
POST /v2/plugin/auth/state?platform=CLI  → HTTP 200
{"code":0,"msg":"OK","data":{"state":"c961ec8d-...",
 "authUrl":"https://www.workbuddy.ai/login?platform=CLI&state=c961ec8d-..."}}
```

由此**确认**（不再是假设）：
- A-auth-1：state 端点**必须**带 `platform`（值为 `CLI`），否则 400/10001；
- A-auth-2：返回的 `authUrl` 是 `https://www.workbuddy.ai/login?platform=CLI&state=…`，
  即「浏览器登录 WorkBuddy 页面」——没有设备码可填，KeyGrid 侧只能给链接 + 轮询；
- A-auth-3：`code:10001` 属参数校验类错误（区别于 `11217` 的待授权、`11128` 的渠道指纹），
  以后遇到 10001 一律先查参数是否漏传。

## 五、上游同步记录（v1.6.20 → v1.7.1）

本次以参考仓库 `ardeyouxipianyi/workbuddy2api-hub` 的 `0ba3a29`（v1.7.1 之后）为基准，
把上一次同步点 `6c2a663` 之后的**协议与任务语义**变化重新对齐。366 个提交里绝大多数是
网关面板/用量看板/多账号管理，与本移植无关；下面是逐项结论。

### 已同步（有代码变更）

| 上游变更 | 我们改了什么 | 上游出处 |
|---|---|---|
| 签到已签到时上游返回 **HTTP 400 + code 10001** | `runCheckinJob` 把它按成功记（只认结构化 code/msg，不误判其它 400） | `0ba3a29` fix(checkin) |
| 每日任务**失败不消耗当天机会** | 任务失败时释放当日幂等锁可重试，Meta 记当天失败次数，上限 3 次后保留锁 | `93e9f26` fix(daily_chat) |
| `stop` 传字符串被上游判 400/11101 | 归一化：字符串包成单元素数组，空值/非法类型删字段 | `c3f263d` (#294) |
| 并发工具批次被拆成相邻 assistant → 400/**11148** | 归一化新增「合并相邻 tool_calls」，且必须**先合并再重排**工具结果 | `merge_adjacent_tool_calls` |
| 请求未带 `max_tokens` 时上游用模型默认值 | 归一化按模型目录的 `maxOutputTokens` 补默认（未知模型不补） | `model_default_max_output_tokens` |
| 风控 **403 / code 11140** 必须换号并冷却 | 识别为独立错误类，冷却更久并换号；内容审核 403 保持原语义（不冷却） | `be4f9f8` (#288) |
| 账号级 429 的软退避是 10 分钟（未验证账号 1 分钟） | 无 `Retry-After` 的账号级 429 采用上游的软退避基准，有 `Retry-After` 时仍以它为准 | `SOFT_RATE_BASE`/`SOFT_RATE_UNVERIFIED_BASE` |
| 积分拆「总剩余 / 可用」，资源查询端点扩展 | 准入用的保留积分改看**可用**值；端点/字段按参考对齐 | `51549a3` (#286) + `wb_accounts.py` 资源段 |
| 网页通道（ACP）在认证代理上固定 407 | 五条腿共用带代理的客户端；新增「勾了代理绝不允许回落默认客户端」的框架级不变量 | `a94eb84` (#279) |
| 成长任务引擎：`auto` 任务无需接取且优先执行；进度轮询后再领奖；接取失败回传原因 | 状态机逐条对齐；企业版账号直接跳过；新增不可伪造任务（公众号关注）跳过；`first_buddy` 规格更新 + 猫猫旅行前先确保 Buddy | `wb_tasks.py` 大改（见日志） |

### 有意不移植（属面板/多账号管理，不属本移植范围）

- 用量看板：小时/分钟分层、剩余用量估算与「剩余优先调度」、缓存 token、总花费、延迟单位。
- 账号管理：手动权重/备注、一键切换出站身分、签到记录看板、出口级退避开关。
- 成长看板：连续登录 streak、抽奖次数、补签卡、档位状态（`fetch_growth_summary`/`lottery`/`tiers`）。
- 网页面板：ZCode 客户端配置器、agents CLI 下发、更新检查卡片。
- 提示词改写：`apply_prompt_mode` 与 `build_upstream_body` 里的系统提示注入 —— 与设计文档
  §3.7「KeyGrid 不注入任何系统提示」的既有决策冲突，保持不移植（`PROMPT_DEGRADE` 的
  403 降级窗口同样不移植）。
- 性能优化：`has_fingerprint` 的字面量快路径（纯性能，行为等价，Go 侧不需要）。

### 未对齐的缺口（记录在案）

1. **企业版判定信号不全**：此处为 v1.7.1 同步时的缺口，已在下方 v1.7.16 同步中补齐：
   CN OAuth 将登录响应 `enterpriseId` 持久化为 `TokenSet.Extra["enterprise_id"]`，刷新后保留。
2. **模型目录元数据**：我们只落模型 id，没有 `maxOutputTokens`/`reasoning` 等元数据，
   所以「默认 max_tokens」用的是从参考静态目录抄来的 id→值表。目录带元数据后应替换。
3. **模型快照无需刷新**：`wb_catalog.py` 自 `6c2a663` 起未变，模型清单没有变化。
4. 账号级 429 的「账号是否已验证」在我们的模型里没有直接等价物（参考是
   credit/free-package 查询成功与否），退避基准的映射方式见 `state.go` 注释。

### A9 实测结论（2026-10-12，真实账号）

用渠道已落库的凭据直连 `POST {billing}/v2/billing/meter/get-user-resource`
（请求体同参考实现，含 `ProductCode=p_tcaca` 与有效期区间），两个区域形状如下。

**国内版（有积分包）**：

```json
{"code":0,"msg":"...","requestId":"...",
 "data":{"Response":{"Data":{"Accounts":[
   {"PackageCode":"...","CycleCapacitySize":500,"CycleCapacityRemain":500,
    "CycleCapacityUsed":0,"CapacityRemain":500,"CycleCapacityRemainPrecise":"...",
    "CycleStartTime":"...","CycleEndTime":"...","DeductionEndTime":2042874643000,
    "AccountAttributes":[...],"Status":0}]},
   "ProTrialStatus":0,"RequestId":"..."}}}
```

→ 正确读法是 `data.Response.Data.Accounts[]`，把每个包的 `*Remain*` 求和
（我们实测该账号聚合值 1335.00；字段优先级与参考实现 `wb_accounts.py:1567` 一致，
`remain` 缺失时用 `size - used` 推导）。

**国际版（无积分包）**：

```json
{"code":0,"data":{"Response":{"Data":{"Accounts":null,"TotalCount":0,"TotalDosage":0},
  "ProTrialStatus":0,"RequestId":"..."}},"msg":"...","requestId":"..."}
```

→ `Accounts: null` **不是**「余额 0」，而是「该账号没有积分包维度」（国际版按订阅
计费）。必须与「余额 0」区分：KeyGrid 返回哨兵 `ErrCreditsUnavailable`，保留上一次
观测值（面板显示「—」而不是 0），积分任务如实记 `skipped`，手动刷新返回明确文案。

**踩过的坑（记一笔）**：原实现只读标量键（`remain`/`balance`/…），真实响应里没有
这些键，于是报「积分字段未识别: keys = [Response, code, data, msg, requestId]」；
请求体也漏了 `PackageEndTimeRangeBegin/End`（参考实现始终带「现在 → 2036」区间）。
两者都已修。

### 11128 实测复现（2026-10-12，Claude Code 接入场景）

用户把 KeyGrid 配到 Claude Code 后报：

```
upstream 400: {"code":11128,"msg":"Illegal API invocation from an unapproved channel", ...}
```

用同一账号同一条最小请求体对照实测（`deepseek-v4.1-flash`）：

| 请求体 | 结果 |
|---|---|
| Claude Code 的 system 指纹原样（`You are Claude Code, Anthropic's official CLI for Claude` + `Main branch (...)` + `cc_entrypoint=…` + `x-anthropic-billing-header: …`） | **HTTP 400 / code 11128** |
| 只把上面这几段指纹片段脱敏后（同一账号、同一模型） | **HTTP 200** |

结论：
1. **不是 UA 问题**。UA 侧我们本来就正确：WorkBuddy 渠道在 `ua_mode` 为空时用身分档位
   自带的 UA 覆盖透传的客户端 UA（`relay/handler.go:444-451`）。
2. **是请求体内容**：上游 WAF 扫描 `messages` 里的客户端特征文本。参考实现在
   `build_upstream_body` 里**无条件**调用 `sanitize_messages`（wb_proxy.py:10789-10811），
   我们此前把脱敏放在用户设置后面且默认 `off` —— 这就是分叉点。
3. 因此把默认档位改为 **on**（`WORKBUDDY_SCRUB` 未设置 = on，用户显式写 off 才关），
   与参考实现对齐；`shadow`/`off` 仍保留给"要看命中面/明确拒绝改写"的场景。
4. 参考实现还有第二层保护 `PROMPT_DEGRADE`（收到 403 后把提示词降级为中性最小提示并
   重试，直到当日 00:00 CST），我们**未移植**：它是"已经出事之后的兜底"，而第一层
   （无条件脱敏）才是根因修复。若将来仍遇到未覆盖的指纹，再按需移植。

## 上游同步记录（2026-10-20，v1.7.1 → v1.7.16）

参考基线：[ffdba8c9b4d75cd43d9d256142923a1bb6b704ce](https://github.com/ardeyouxipianyi/workbuddy2api-hub/commit/ffdba8c9b4d75cd43d9d256142923a1bb6b704ce)。
本轮核对该提交下的 `docs/CHANGELOG.md`、`wb_identity.py`、`wb_accounts.py`、
`wb_proxy.py`、`wb_tasks.py`，选取可直接映射到 KeyGrid 的上游协议修复。
以下结论来自源码比对与本地 mock 回归，**不是使用真实账号对上游的实测结果**。

### 已同步的功能

| 上游变化 | KeyGrid 行为 | 代码位置 |
|---|---|---|
| 国内版 `WB_CN_CHAT_UPSTREAM` 改址（#465） | 默认及历史 `copilot.tencent.com` 渠道、模型目录、OAuth state/token/refresh、成长任务统一改址；保留自填的其它渠道域名与独立 OAuth 端点覆盖 | `internal/workbuddy/upstream/config.go`、`realm.go`、`internal/oauth/providers/codebuddy.go` |
| 版本与 UA 收敛（#403，及 v1.7.14 身份对齐） | 国际桌面 5.8.4、内置 CLI 2.161.0；独立 CLI 2.168.2；VSCode 产品 4.12.39997873。认证与转发共用版本事实，国内桌面保持 5.5.6 | `internal/workbuddy/upstream/config.go`、`identity.go` |
| 桌面会话头及请求 ID 关系 | 三档都带会话头；桌面/CLI 会话 ID 为带连字符 UUID，VSCode 为 32hex；请求/消息 ID 使用随机 UUIDv7，按产品档关联 root/request/message ID，避免同 uid 同纳秒重复 | `internal/workbuddy/identity.go` |
| 账号身份不能跟着连接域名改变 | 优先使用授权保存的 `domain` 作为 `X-Domain`；CLI 补齐 purpose/type/runtime，个人/企业账号分别设置 private-data；VSCode 产品名改为 CodeBuddy | `internal/workbuddy/identity.go` |
| 工具参数 Schema 规整（v1.7.14） | 无参工具补 object/properties，递归处理类型别名、required、items、definitions 与组合 Schema；兼容旧版 functions 和扁平工具；保留合法约束、strict 与 null default | `internal/workbuddy/pipeline.go` |
| 历史工具 arguments 为对象导致 11101 | 将对象/数组参数编码为 JSON 字符串，已有字符串不改变，保留 Unicode 与 HTML 字符 | `internal/workbuddy/pipeline.go` |
| 工具结果前插入消息导致 11148（#420） | user/system/developer 插入消息移到结果批次之后；任何新 assistant 消息都终止本批次，不跨轮次搬运工具结果 | `internal/workbuddy/pipeline.go` |
| 11140 默认停止换号（#450） | 真实转发遇到结构化 HTTP 403/code 11140，记录失败和既有账号冷却后结束本次请求，不继续向后续渠道发送；普通内容审核 403 不受影响 | `internal/relay/handler.go`、`workbuddy_runtime.go` |
| 企业版判定补齐 | CN OAuth 保存 realm/domain/uid/enterprise_id，刷新保留；成长任务可用登录元信息跳过企业账号，而非仅依赖 JWT | `internal/oauth/providers/codebuddy.go` |
| 静态模型目录 | 国内版快照补入 `hy3`；国际版新增项均为已有过滤规则剔除的档位别名，不作为真实模型加入。逐项核对默认最大输出 tokens，已有取值无需调整 | `internal/workbuddy/snapshot/models_cn.json`、`catalog_test.go` |

### 国内版连接问题的配置方法

在部署环境中增加：

```dotenv
WB_CN_CHAT_UPSTREAM=https://www.workbuddy.cn
```

重启 KeyGrid 后生效，不需要批量修改已有渠道。空值或非法 URL 回落
`https://copilot.tencent.com`；只接受 http(s) 根基址，不接受凭据、业务路径、查询参数与片段。
国际版、计费/积分/签到域、自填其它域名的渠道不受此项影响；`X-Domain` 仍来自账号身份。
已有 `CODEBUDDY_STATE_URL` / `CODEBUDDY_TOKEN_URL` / `CODEBUDDY_REFRESH_URL` 的显式配置优先。

### 保留原有实现或暂未移植

- 独立面板、部署脚本、定址 `/u/<ref>/` 路由、账号模型黑名单、价表和统计 UI 不直接搬入 KeyGrid；沿用本项目的渠道绑定、模型映射与用户隔离。
- 上游按账号隔离 TCP/TLS 连接、原始头名保真与 gzip/SDK/trace 头全量仿真未移植；Go HTTP 栈和连接池仍按本项目现有实现工作。
- 401 同请求刷新重试未新增；KeyGrid 保留既有提前刷新和请求时凭据刷新机制，需要另行覆盖锁内重读、单次刷新与流已提交的边界。
- 上游 11140 区域级持久闸门未移植；本轮只停止当前请求的换号并保留既有账号级冷却，不新增可能跨用户互相影响的区域状态。
- 实际 credit 回包扣减余额、网页打卡指数重试、免费模型回落、可配置排程和错峰执行暂未移植。
- 不引入后台模型自动改写、积分付费回落、系统提示注入或默认关闭 Messages 入口，保持 KeyGrid 的既有协议契约。

### 验证

回归覆盖：改址合法性/覆盖优先级、OAuth 完整 state→poll→refresh 链路、企业元信息保留、
HTTP 错误拒收、三档身份矩阵、固定时钟下 ID 唯一性、工具 Schema 幂等性与工具结果顺序、
11140 停止条件及普通 403/429 的区分。

```bash
go test ./internal/workbuddy/... ./internal/oauth/providers ./internal/relay
make test
go test -race ./internal/workbuddy/... ./internal/oauth/providers ./internal/relay
go vet ./...
```

本轮没有调用真实账号、执行付费模型请求或运行自动化打卡，也没有修改数据库结构或前端。
