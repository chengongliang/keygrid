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

1. **企业版判定信号不全**：参考用登录返回的 `enterpriseId`，我们的 CN OAuth 未把
   `enterpriseId` 落进 `TokenSet.Extra`，目前只能靠 JWT claim 判定（`tasks_cn.go` 里有
   `TODO(上游 A-企业版)`）。要完整对齐需在 `codebuddy.go` 的 Resolve 里持久化该字段。
2. **模型目录元数据**：我们只落模型 id，没有 `maxOutputTokens`/`reasoning` 等元数据，
   所以「默认 max_tokens」用的是从参考静态目录抄来的 id→值表。目录带元数据后应替换。
3. **模型快照无需刷新**：`wb_catalog.py` 自 `6c2a663` 起未变，模型清单没有变化。
4. 账号级 429 的「账号是否已验证」在我们的模型里没有直接等价物（参考是
   credit/free-package 查询成功与否），退避基准的映射方式见 `state.go` 注释。
