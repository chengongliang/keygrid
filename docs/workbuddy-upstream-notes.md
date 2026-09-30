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
