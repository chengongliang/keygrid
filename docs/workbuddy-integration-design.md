# WorkBuddy 能力集成设计方案

> 目标：把 [workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub)（下称 wb2a）的能力接进 KeyGrid，既拿到它的上游协议知识与运营自动化，又不破坏 KeyGrid 的硬隔离、记账、熔断与单二进制部署。
>
> 关联交付物：
> - P0 探针脚本：[scripts/probe_workbuddy.sh](../scripts/probe_workbuddy.sh)
> - P0 事实记录模板：[workbuddy-upstream-notes.md](./workbuddy-upstream-notes.md)
>
> 本文是设计文档，不是实现。所有 `路径:行号` 锚点均来自对两个仓库的实际阅读。

---

## 0. TL;DR

| 维度 | 决定 |
|---|---|
| 集成范围 | **全量**：渠道接入 + 运行态（限额/积分/冷却）+ 自动化任务（含成长任务与 ACP 网页打卡）。高风险项一律默认关 + 平台级 kill switch（§3.7、§6）。 |
| 代码形态 | **不做通用插件层**。业务逻辑收在 `internal/workbuddy` 独立包；relay / task / router 各加**薄接线**，沿用 `IsCodexProvider` / `IsXAIProvider` 既有模式。 |
| 多账号 | **一账号一渠道（provider）**。不引入 `provider_accounts` 表。 |
| 内核新增 | 3 张 workbuddy 表 + 1 张通用代理表（P5）；relay 6 个插入点、task 1 个字段、router 1 组路由、前端 1 个页面。 |
| 首批动作 | **P0 已完成**（探针脚本 + 事实记录模板），先跑实测再动 Go 代码 —— 因为 G1/G2/G3 三条缺口都需要实测确认，而且它们决定 §3.2/§3.3 的实现形态。 |
| 合规底线 | 默认全关、平台级 kill switch、只对自家授权账号、不落 prompt 内容、审计可追、`THIRD_PARTY_NOTICES.md` 声明、不做公开注册。 |

---

## 1. 背景

### 1.1 wb2a 的能力清单（按 KeyGrid 的层归类）

| 层 | 能力 | 关键锚点（wb2a） | KeyGrid 是否已有 |
|---|---|---|---|
| 上游协议 | 双区 chat 转发 `/v2/chat/completions` | `wb_proxy.py:141`、`wb_identity.py:44-53` | ⚠️ 部分（`codebuddy-cn` 预设存在，但端点拼接有缺口 G1） |
| 上游协议 | 出站身分三态 + `X-IDE-*` / `X-Product` / `X-Domain` 特征头 | `wb_identity.py:76-145`、`wb_accounts.py:449-505` | ❌ |
| 上游协议 | 强制 `stream:true` + 非流本地聚合 | `wb_proxy.py:3040` | ❌ |
| 上游协议 | `developer`→`system`、`tool_choice` 对象→字符串、`max_tokens`→`max_completion_tokens` | `wb_proxy.py:2349,2664,2705,2756` | ❌ |
| 上游协议 | Responses → chat 转换、`previous_response_id` 拒绝 | `wb_proxy.py:4329,4473,6522` | ✅ pivot 架构已覆盖 |
| 上游协议 | WAF 指纹脱敏（11128/11101/11140） | `wb_proxy.py:2366-2468` | ❌ |
| 上游协议 | DeepSeek thinking 注入 + `reasoning_content` 回填 | `wb_proxy.py:2622,2964-3040` | ❌ |
| 上游协议 | 工具调用配对自愈、DSML 解析 | wb2a v1.4.9 changelog | ❌ |
| 认证 | state 轮询式 OAuth（双区同构，state TTL 600s） | `wb_accounts.py:1203-1287` | ✅ 已有 `CodeBuddyCN`（`internal/oauth/providers/codebuddy.go`）；缺 intl 适配器 |
| 认证 | token refresh（账号级锁，`X-Refresh-Token` 头 + `X-Auth-Refresh-Source`） | `wb_accounts.py:149,524-560` | ✅ 已有（`oauth.Scheduler`） |
| 目录 | `/v3/config` 全量 + 桌面缓存回落 + 静态快照 | `wb_catalog.py`、`wb_proxy.py:1803,1979` | ✅ 机制已有（`oauth/modelcatalog.go`），未接该供应商 |
| 调度 | 选号轮询 + 会话亲和 + 账号/模型级冷却 | `wb_accounts.py:415-446,1166-1190` | ⚠️ priority+随机+failover；无模型级冷却 |
| 调度 | 保留积分 / 每日 token 限额 | `wb_accounts.py:381-414`、`wb_proxy.py:3290` | ❌ |
| 调度 | 每账号出口代理槽位 | `wb_proxy.py:5936-5993` | ⚠️ 仅全局 proxy + `UseProxy` 布尔 |
| 运营 | 签到 / 成长任务 / 猫猫旅行 / 夜猫 / 国际版活跃打卡 / ACP 网页通道 | `wb_tasks.py`、`wb_webagent.py`、`wb_scheduler.py` | ❌ |
| 运维 | 面板（用量、账号、任务、日志） | `dashboard.html` | ✅ KeyGrid 有自己的面板 |

### 1.2 KeyGrid 的三个既有特化就是"事实上的插件"

| 特化 | 判定 | 端点 | 头 | 调用 |
|---|---|---|---|---|
| Codex | `relay/codex.go:58` | `NormalizeCodexBaseURL:67` | `codexUpstreamHeaders:1071` | `upstreamCallCodex:1096` |
| xAI | `relay/xai.go:30` | `NormalizeXAIBaseURL:42` | `XAIUpstreamHeaders:61` | `upstreamCallXAI:175` |
| Codex 额度 | `quota/codex.go:48 SupportsQuota` | `CodexUsageURL:55` | — | `FetchCodexUsage:70` |

分派点集中在 `relay/anthropic.go:41 channelProto` 与 `relay/handler.go:284-383` 的两个 `switch`。**workbuddy 就按第 4 个特化的规格做**，形状与 xai 一致。

### 1.3 现状缺口（本方案要解决）

| 编号 | 缺口 | 证据 |
|---|---|---|
| G1 | **端点拼接硬编码**：`upstreamTarget(baseURL)` 恒拼 `/v1/chat/completions`（`relay/route.go:84-86`），而 `codebuddy-cn` 预设 base_url 是 `https://copilot.tencent.com/v2`（`oauth/presets.go:149-155`）→ 实际请求 `/v2/v1/chat/completions`；上游真实路径是 `/v2/chat/completions`（`wb_proxy.py:141`）。**现存渠道很可能转发不通**。 | 两侧对照，待 P0 实测 |
| G2 | OpenAI 转发只带 `Authorization` / `Content-Type` / UA（`relay/handler.go:660-661`），缺 `X-IDE-*` / `X-Product` / `X-Domain` / `X-User-Id` / `X-CodeBuddy-Request` → 上游 WAF `11128 illegal API invocation from an unapproved channel`（`wb_proxy.py:2391-2394`）。 | 两侧对照 |
| G3 | 上游要求 `stream:true`（`wb_proxy.py:3040`），KeyGrid 默认跟随客户端 `stream` → 非流客户端失败。 | wb2a |
| G4 | 无 `tool_choice` 对象降级 → 上游报 11101；`max_completion_tokens` 上游不认（wb2a 反向转换为 `max_tokens`）。**注**：`developer`→`system` 内核已有（`relay/handler.go:567-609 buildUpstreamBody` + `normalizeDeveloperRoles`），不需要再做。 | wb2a `wb_proxy.py:2664,2756`；KeyGrid 已覆盖 developer |
| G5 | 无 provider 级每日 token 限额 / 保留积分 / 模型级冷却（`quota` 包只做 Codex 快照）。 | `quota/codex.go:48` |
| G6 | 无 provider 级出口代理（只有全局 `platform_settings.proxy_url` + `UseProxy bool`，`relay/state.go:294 clientFor`）。 | `model/provider.go:39` |
| G7 | 无通用后台任务框架：`task.Runner.runOnce` 硬编码 4 件事（`internal/task/task.go:56-77`）。 | 直接阅读 |
| G8 | 上游响应可能对 prompt 做**定向改写**（WAF 脱敏）→ 必须可开关、可审计，否则违反"不静默改用户内容"的信任预期。 | `wb_proxy.py:2366-2468` |

### 1.4 为什么不做通用插件层（以及代价）

曾评估三条路线：

| | A. 外挂 sidecar | **B′. 独立包 + 薄接线（选定）** | C. 通用插件注册表 |
|---|---|---|---|
| 做法 | KeyGrid 建一条 `api_key` 渠道指向自建 wb-proxy | `internal/workbuddy` 包 + relay/task/router 各一处接线 | `internal/plugin` 接口层 + Driver 注册表 + 收编 codex/xai |
| 硬隔离 / 记账 / 配额 / 熔断 | ❌ 一台 wb-proxy 一个 Key，全员共用 | ✅ | ✅ |
| 凭据 | ❌ 明文 JSON + 面板默认密码 `admin`（`wb_accounts.py:313-340`） | ✅ AES-GCM | ✅ |
| 改动面 | 0 行代码 | 小（本书 §2.3 的 8 个插入点） | 中（先把 codex/xai 搬家，再加插件） |
| 新供应商的边际成本 | — | 复制一份模式（与 xai 同款） | 加一个注册项 |

**选 B′ 的理由**：当前只有一个新供应商要接。把 codex/xai 从既有位置搬到新接口层，收益是"未来第 4、第 5 个供应商更省"，但它引入的回归面（`handler.go` 主循环重构 + 两个 switch 拆除）在只有一次交付需求时不划算。B′ 的定位是"**把 workbuddy 的全部业务逻辑收在一个包里，内核只暴露 8 个插入点**"。

**已明确接受的代价**（后续如果有人抱怨，看这里，别重新论证）：

1. relay 里出现第 4 段特化分支，模式与 codex/xai 重复；`handler.go` 的候选循环会再长几行。
2. 逻辑与内核的耦合点分散在 8 处（§2.3），没有单一接口把它们收拢；改 `upstreamCall` 签名会同时牵动 anthropic/codex/xai 三条路径。
3. 无法通过配置"卸载"这段能力，只能靠开关软关（用户级 `enabled` + 平台级 kill switch）。编译期里它始终存在。
4. `internal/workbuddy` 直接依赖 `op`/`model`，不能原样复用到别的项目。
5. 若将来要接第 4、第 5 个同类供应商，**需要做一次机械重构**把它抽成注册表 —— 本方案不阻碍这件事，只是推迟；且届时 workbuddy 包的业务逻辑（`pipeline.go`、`upstream.go`、`jobs.go`）几乎可以原样搬过去。

### 1.5 为什么不用 Go 原生 `plugin` / WASM

- 原生 `plugin`：仅 linux/darwin，**本仓库在 Windows 上开发**（`/mnt/win-data`）；必须 CGO，破坏静态单二进制；.so 与主程序所有依赖 ABI/版本必须完全一致（任何 `go.sum` 漂移都会静默 panic）；产物要随镜像分发，与 `web/dist` embed 的部署哲学冲突。
- WASM（wazero）：要把 `op` / `crypto` / `redis` / `http` 全做成 host function，等于手写一遍 ABI。运行时装卸在"团队自用单二进制"场景里没有价值。

---

## 2. 落地架构

### 2.1 形态

```
internal/workbuddy/            ← 新增：全部 workbuddy 业务逻辑（唯一的上游事实来源）
  realm.go         双区端点/路径/UA 常量表；RealmOf / IsWorkBuddy
  identity.go      出站身分三态 + 特征头拼装 + 稳定 ID 派生
  pipeline.go      请求/响应归一化（纯函数，重点单测）
  adapt.go         AdaptOpenAI：给 relay 用的"body + target + plan"入口
  stream.go        强制流式下把 SSE 折成单条 JSON（非流客户端用）
  scrub.go         WAF 脱敏（off/shadow/on 三档）
  catalog.go       /v3/config + 窄端点 + 内嵌快照回落
  state.go         运行态：Admit / Observe / 积分 / 日限额 / 模型冷却
  jobs.go          任务编排 + Tick
  tasks_cn.go      签到 / 成长任务 / 猫猫旅行 / 夜猫
  tasks_intl.go    国际版活跃打卡（桌面端对话）
  webagent.go      ACP 网页通道打卡
  routes.go        /api/workbuddy/* handler
  snapshot/models_{intl,cn}.json
  *_test.go        httptest 打桩，零外网依赖

internal/oauth/providers/workbuddy.go   ← intl 适配器（与 codebuddy.go 并列，遵循既有约定）
```

**边界规则**（写进 `internal/workbuddy/doc.go`）：

1. **不写 SQL**：只调用 `op` 已带 `WHERE user_id` 的方法；缺什么先在 `op/` 加。
2. **不碰明文**：加解密只走 `crypto`，凭据载体是 `oauth.TokenSet`；不把 token/积分/prompt/响应体写日志。
3. **不做转发主循环**：failover、熔断、记账顺序仍归 relay；本包只回答"怎么拼这一次请求、怎么读这一次响应"。
4. **失败不阻断内核**：任何函数返回 error → 该渠道 failover + 记 `request_errors`。
5. **可缺省**：函数对"通用渠道"返回零值/`false`，内核行为与今天完全一致。

### 2.2 包对外 API（内核只调这些）

```go
package workbuddy

type Realm string // "intl" | "cn"

// —— 识别与端点 ——
func IsWorkBuddy(p *model.Provider) bool
func RealmOf(p *model.Provider) (Realm, bool)
// Endpoint 覆盖内核默认拼接（base_url + "/v1/chat/completions"）。这是修 G1 的地方。
func Endpoint(p *model.Provider) (string, error)

// —— 请求加工 ——
type Plan struct {
	ForceStream bool // 上游必须 stream:true（G3）
	Aggregate   bool // 客户端要非流 → 本地聚合
}
// AdaptOpenAI 返回上游 body / target / plan；内核 openai 分支的 default 路径调用。
func AdaptOpenAI(p *model.Provider, pivot, raw []byte, clientStream bool) (body []byte, target string, plan Plan, err error)

// Headers 鉴权之外的上游头（身分/指纹）。内核负责 Authorization/Content-Type/Accept；UA 以本函数为准。
func Headers(p *model.Provider, tok *oauth.TokenSet, opt HeaderOpt) (http.Header, error)

// —— 运行态（G5）——
type Deps interface {
	Op() *op.Op
	Redis() *redis.Client
	Key(parts ...string) string
}
type ParkedError struct{ Reason string; Until time.Time }
func (e *ParkedError) Error() string
// Admit 准入判定：返回 *ParkedError 时内核记 request_error 并继续下一候选。
func Admit(p *model.Provider, upModel string, d Deps) error
// Observe 响应观察：模型冷却、余额被动观察、usage 修正。
func Observe(ev Event, d Deps)

// —— 目录与额度 ——
func FetchModels(ctx context.Context, client *http.Client, p *model.Provider, tok *oauth.TokenSet) ([]string, error)
func ProbeCredits(ctx context.Context, client *http.Client, p *model.Provider, tok *oauth.TokenSet) (float64, error)

// —— 运营任务（P3/P4）——
func Tick(ctx context.Context, o *op.Op, rdb *redis.Client, now time.Time) // 由 task.Runner 每分钟调用
func RunJob(ctx context.Context, jobKey string, userID int64, providerID *int64, d Deps) (Result, error) // 手动触发
```

`Plan` / `Event` / `HeaderOpt` 的具体字段在实现时定；原则是**不把 `model.Provider` 之外的内核内部类型泄漏进来**。

### 2.3 内核接线点（8 处，精确锚点）

| # | 位置 | 改动 | 影响面 |
|---|---|---|---|
| 1 | `relay/handler.go:343-345`（openai 上游 default 分支） | 若 `workbuddy.IsWorkBuddy(p)` → `workbuddy.AdaptOpenAI(...)` 取 `body/target/plan`，否则原逻辑 | 仅新渠道 |
| 2 | `relay/handler.go:349-358`（client / UA 之后） | 取 `extraHeaders := workbuddy.Headers(...)`；**UA 优先级**：插件身分头存在时跳过 `ResolveUserAgent`，但渠道 `ua_mode=custom/forward` 时以渠道配置为准 | 仅新渠道 |
| 3 | `relay/handler.go:368-383`（调用分派） | `upstreamCall` 增加 `extra http.Header` 与 `forceStream bool` 参数；anthropic/codex/xai 三处传 `nil,false` | 签名改动，4 个调用点 |
| 4 | `relay/handler.go:644-661`（`upstreamCall`） | 注入 `extra` 头；实际请求流式开关 = `forceStream \|\| isStream` | 通用 |
| 5 | `relay/handler.go:660` 之后 / 非流聚合路径 | 上游是 SSE 但客户端要非流 → `workbuddy.CollapseSSE(respBytes)` 折成单条 chat JSON，再走既有 `openaiJSONToAgg`/`writeEntryJSON` | 通用（不破坏既有形态） |
| 6 | `relay/handler.go:244`（候选循环开头，熔断检查附近） | `if err := workbuddy.Admit(p, ...); err != nil { noteFailure(...); continue }` | 仅新渠道 |
| 7 | `relay/handler.go:386` 与 `388-427`（`recordUsage` 之后、失败分支） | `workbuddy.Observe(...)`：429 → 模型冷却；成功 → 刷新 `last_ok_at` | 仅新渠道 |
| 8 | `internal/task/task.go:56-77` | `Runner` 增加 `WorkBuddy func(ctx)` 字段，`runOnce` 末尾调用（nil 安全） | 通用 |

另有两处非 relay 接线：

| # | 位置 | 改动 |
|---|---|---|
| 9 | `server/handlers/provider.go:126 presetsForUI` / `server/handlers/providermodels.go` | 拉模型时对 workbuddy 渠道走 `workbuddy.FetchModels`（否则回落到窄端点/快照） |
| 10 | `server/router/router.go:127-180` | 新增 `/api/workbuddy/*` 路由（见 §2.5） |

`channelProto`（`relay/anthropic.go:41`）**不改**：workbuddy 上游就是 openai chat 形状，走默认 `protoOpenAI` 分支。这条很重要 —— responses / messages 两个入口的协议转换由 KeyGrid 既有的 pivot + `stream_bridge.go` 免费获得（wb2a 里那套 `responses_to_chat` 不需要移植）。

### 2.4 数据模型

**不需要给 `providers` 加 `driver` 列**：用既有的 `oauth_provider` 判定，与 codex/xai 同款。

| 值 | 含义 |
|---|---|
| `workbuddy-intl` | 国际版（新增适配器） |
| `codebuddy-cn` | 国内版（**复用现有适配器**，只补 region 语义） |

三张新表（进 `internal/db/db.go:Migrate` 的 `AutoMigrate` 列表，`db.go:58-70`）：

```go
// 1) 用户级设置（隔离：所有查询 where user_id = ?）
type WorkbuddySetting struct {
	ID              int64  `gorm:"primaryKey"`
	UserID          int64  `gorm:"uniqueIndex;not null"`
	Enabled         bool   `gorm:"not null;default:false"` // 默认关
	ReserveCredits  float64 `gorm:"not null;default:0"`    // 0 = 不保留
	DailyTokenLimit int64   `gorm:"not null;default:0"`    // 0 = 不限
	Product         string  `gorm:"size:16;not null;default:'workbuddy'"` // workbuddy|vscode|cli
	ScrubMode       string  `gorm:"size:8;not null;default:'off'"`        // off|shadow|on
	Fingerprint     bool    `gorm:"not null;default:false"`               // 设备指纹（默认关）
	JobsEnabled     map[string]bool `gorm:"serializer:json;type:jsonb"`   // 逐任务开关，全默认 false
	CreatedAt, UpdatedAt time.Time
}

// 2) 渠道运行态（一个 provider = 一个账号）
type WorkbuddyChannelState struct {
	ID           int64     `gorm:"primaryKey"`
	UserID       int64     `gorm:"index;not null"`
	ProviderID   int64     `gorm:"uniqueIndex;not null"`
	Realm        string    `gorm:"size:8"`   // intl | cn
	AccountUID   string    `gorm:"size:64"`  // 用于派生指纹与展示（只存前 8 位 + 长度？见安全说明）
	Nickname     string    `gorm:"size:128"`
	Credits      float64
	CreditsAt    time.Time
	DailyTokens  int64
	TokensDate   string    `gorm:"size:10"`  // YYYY-MM-DD（本地时区）
	ParkedReason string    `gorm:"size:64"`  // "" | daily_limit | reserve_credits | cooldown
	Cooldowns    map[string]time.Time `gorm:"serializer:json;type:jsonb"` // {model: until}
	LastOKAt     *time.Time
	// Meta 存幂等标记与事件去重：lastCheckin / lastDailyChat / lastGrowth / lastNight /
	// GrowthEventIDs（避免重复上报被上游判重）
	Meta      map[string]any `gorm:"serializer:json;type:jsonb"`
	UpdatedAt time.Time
}

// 3) 任务执行日志（前端展示 + 审计；默认保留 30 天，纳入 task.runOnce 清理）
type WorkbuddyJobRun struct {
	ID         int64     `gorm:"primaryKey"`
	UserID     int64     `gorm:"index;not null"`
	JobKey     string    `gorm:"size:48;not null"` // checkin|credits|growth|cat|night|daily_chat|daily_chat_web
	ProviderID *int64    `gorm:"index"`
	Trigger    string    `gorm:"size:16"` // schedule | manual
	Status     string    `gorm:"size:16"` // ok | skipped | failed | parked
	Message    string    `gorm:"size:512"` // 摘要：状态码/错误码/输出段数；禁止落正文
	StartedAt  time.Time `gorm:"index"`
	FinishedAt *time.Time
}
```

P5 追加的通用能力（不属于 workbuddy 包）：`egress_proxies{id,user_id,name,url_enc,enabled,remark,last_test_at,last_test_ok,last_exit_ip}` + `providers.egress_proxy_id`。

**隔离红线**：三张表都带 `user_id`，所有查询必须像 `op` 现有方法一样带 `WHERE user_id = ?`（AGENTS.md 硬性规则 1）。`Admit`/`Observe` 只接收候选循环里已按 `user_id` 收窄的 `cand.provider`。

**Redis 键**（统一 `Deps.Key(...)` 生成前缀，避免冲突）：

| 键 | 用途 | TTL / 语义 |
|---|---|---|
| `workbuddy:job:{job}:{userID}:{providerID}:{YYYYMMDDHHmm}` | 任务幂等锁（SETNX） | Redis 不可用 → 按单实例放行 + 启动 WARN |
| `workbuddy:cd:{providerID}:{model}` | 模型级冷却 | 上游 `Retry-After` 或 60s |
| `workbuddy:tokens:{providerID}:{date}` | 当日 token 实时增量 | 权威值来自 `usage_logs` 聚合 |
| `workbuddy:credits:{providerID}` | 积分缓存 | 15min |

**为什么不做 `provider_accounts`**：`Provider` 无 `(user_id, oauth_provider)` 唯一约束，仅 `credentials.provider_id` 唯一（`model/credential.go:8`），同一用户可建任意多条同 kind 渠道，`buildCandidates`（`relay/route.go:24-76`）已按 priority 分组洗牌成 failover 序列 —— 这**就是**多账号池，粒度恰好一个账号一条渠道。引入内部账号池会让 1:1 凭据契约破裂，牵动 `relay/oauth.go`、`op`、`quota/sync.go`、`provider_health`、前端渠道列表，收益只是"少建几条渠道"。

> **多账号 = 多建几条渠道**；前端提供"连续添加账号"向导（一次授权产出一条 provider）。

### 2.5 HTTP 与前端

```go
// router.go 的 authed group（:127-180）内新增
authed.Get("/workbuddy/overview", wbH.Overview)               // 设置 + 运行态 + 最近执行日志（页面一次拉全）
authed.Patch("/workbuddy/settings", wbH.UpdateSettings)       // 启停/限额/身分档位/脱敏档位/逐任务开关
authed.Post("/workbuddy/channels/{id}/credits", wbH.RefreshCredits)
authed.Post("/workbuddy/jobs/{job}/run", wbH.RunJob)          // 手动触发（前端二次确认）
```

响应形状（**前端以此为准**，实现时不要各写一套）：

```
GET /workbuddy/overview →
{ settings:{enabled,reserve_credits,daily_token_limit,product,scrub_mode,fingerprint,jobs_enabled{}},
  platform:{jobs_allowed},                     // 平台级 kill switch，false 时前端置灰全部任务开关
  channels:[{provider_id,name,realm,account,nickname,credits,credits_at,daily_tokens,
             tokens_date,parked_reason,cooldowns{},last_ok_at}],
  runs:[{job_key,provider_id,trigger,status,message,started_at,finished_at}] }

PATCH /workbuddy/settings        body = settings 的任意子集 → {ok:true}
POST  /workbuddy/channels/{id}/credits              → {credits, credits_at}
POST  /workbuddy/jobs/{job}/run  body={provider_id?} → {results:[{provider_id,status,message}]}
```

- 用单条 `overview` 换取页面一次拉全（设置 + 运行态 + 日志），避免三个端点各自造形状。
- 平台级 kill switch：`platform_settings` 增 `workbuddy_jobs_enabled`（admin 端可一键全停），优先级高于用户配置。
- 插件路由全在 `SessionAuth` 之后，绝不注册 `/v1/*`（转发面只属于 relay）。
- 渠道列表徽章（积分/当日用量/parked）留到 P2 收尾：需要 `Providers.tsx` 也拉一次 overview，先只做独立页，避免两处数据源不一致。

前端（hash-tab 架构，无 react-router）：

1. 新页 `web/src/pages/WorkBuddy.tsx`：① 运行态表 ② 任务开关 + "立即执行" + 二次确认 ③ 执行日志（最近 50 条）④ 限额/保留积分/身分档位/脱敏档位。
2. `App.tsx` 需改 7 处：`Tab` 联合类型 `:22`、`SECTION_MAIN :26`、`TAB_KEY :55`、import、渲染分支 `:336`、导航项 `:186`。
3. 渠道列表（`web/src/pages/Providers.tsx`）追加徽章：积分、当日 token、`parked`（悬停显示原因与恢复时间）、冷却中的模型。
4. i18n：新域 `web/src/i18n/locales/{zh-CN,en}/workbuddy.ts`，**两个 `index.ts` 都要注册**（`locales/zh-CN/index.ts:16` 与 en 同位置）；zh/en key 结构必须完全一致。
5. Logo：`web/public/logos/workbuddy.png` + `lib/providerIcons.ts:15 HOST_HINTS` 加 `workbuddy.ai` / `copilot.tencent.com`；商标声明按 `docs/provider-logos.md` 补一行。
6. 添加渠道向导零改动 —— `handlers/provider.go:96 Meta` 已把 `presets` + `oauth_keys` 返回，注册预设与适配器后自动出现。

### 2.6 配置分层

| 层 | 位置 | 内容 |
|---|---|---|
| 编译期常量 | `internal/workbuddy/realm.go` | 端点、路径、身分版本号、UA、内嵌快照 |
| 环境变量 | `.env` / `.env.example` | `WORKBUDDY_ENABLED=1`（**默认开**：对 codebuddy-cn 渠道旧行为是错的，适配才是修复；置 0 作紧急回滚）、`WORKBUDDY_FORCE_STREAM=1`（A3 的开关）、`WORKBUDDY_JOBS=0`、`WORKBUDDY_FINGERPRINT=0` |
| 平台设置 | `platform_settings` | 是否允许该能力、任务 kill switch、是否允许用户开自动化 |
| 用户设置 | `workbuddy_settings` | 保留积分、日限额、身分档位、脱敏档位、逐任务开关 |

新增 env 必须同步 `.env.example`（AGENTS.md 硬性规则 7）。

---

## 3. 功能设计

### 3.1 M1 渠道驱动：双区端点与身分头

端点表（`realm.go`，对齐 `wb_identity.py:44-53`、`wb_accounts.py:119-139`）：

| | intl | cn |
|---|---|---|
| chat base | `https://www.workbuddy.ai` | `https://copilot.tencent.com` |
| billing base | `https://www.workbuddy.ai` | `https://www.codebuddy.cn` |
| chat 路径 | `/v2/chat/completions` | 同 |
| 模型窄接口 | `/v2/enterprises/personal/models` | 同 |
| 全量目录 | `/v3/config`（需桌面 UA） | 同 |
| auth state/token/refresh | `/v2/plugin/auth/{state,token,token/refresh}` | 同 |
| 签到 | 无 | `/v2/billing/meter/daily-checkin` |
| 积分 | `/v2/billing/meter/get-user-resource` | 同 |
| 成长任务 | 无 | `/v2/activity/growth/tasks` 等 |
| 网页会话（ACP） | `/console/as/conversations/` | 无（CN 走桌面端） |

`Endpoint()` 返回 `base + "/v2/chat/completions"` —— 一条同时修掉 G1。**落地前用 P0 探针实测确认**（A1）。

`Headers()`（对齐 `wb_accounts.py:449-505` + `wb_identity.py:76-145`）：

- 固定：`X-CodeBuddy-Request: 1`、`X-Requested-With: XMLHttpRequest`、`Origin`、`Referer`、`Accept-Language`
- 身分块（默认 `workbuddy` 桌面档）：`X-IDE-Type/Name: WorkBuddy`、`X-IDE-Version: 5.5.6|5.5.2`、`X-Product: WorkBuddy`、`X-Domain`、`X-Agent-Purpose: conversation`
- UA：`WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1`（cn）/ `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`（intl）
- `X-User-Id`：来自 `oauth.TokenSet.Extra["uid"]`（OAuth 时写入）
- VSC/CLI 档额外：`X-Agent-Intent: craft`、`X-Conversation-ID/-Request-ID/-Message-ID`
- `X-Machine-ID`/`X-Session-ID`：**默认不发**，由 `workbuddy_settings.Fingerprint` 显式开启（§3.4）

**鉴权头由内核设置**（`handler.go:660`），本包不重复设置。`Authorization: Bearer <access_token>` 在两条路径上一致。

**UA 冲突规则**（必须显式实现）：`relay/ua.go:32 ResolveUserAgent` 默认透传客户端 UA，而 workbuddy 需要固定身分 UA。约定：本包 `Headers()` 返回了 `User-Agent` → 内核跳过 `ResolveUserAgent`；渠道 `ua_mode=custom/forward` 时以渠道配置为准。

`codebuddy-cn` 的 base_url 处理：预设里把 base_url 改成 host（`https://copilot.tencent.com`），路径交给 `Endpoint()`；存量渠道的 `https://copilot.tencent.com/v2` 由 `Endpoint()` 识别并去掉尾部的 `/v2`（幂等归一化），避免用户手工改渠道。

### 3.2 M2 请求/响应归一化管线（纯函数）

`Pipeline` 按固定顺序执行（顺序敏感，用单测钉住）：

| 序 | 步骤 | 依据 | 触发 |
|---|---|---|---|
| 1 | 模型名映射 | KeyGrid `model_map`（内核做） | 恒 |
| 2 | `developer` → `system` | `wb_proxy.py:2349` | **内核已做**（`handler.go:567-609`）；本包再幂等补一次，防调用方绕过 |
| 3 | `tool_choice` 对象 → 字符串；**保留 `tools` 声明** | `wb_proxy.py:2664-2704` | 恒（否则 11101 / 模型退化输出 DSML 文本） |
| 4 | 顶层 `name` 工具包成 function schema | `wb_proxy.py:2712-2727` | 有该形态时 |
| 5 | **`max_completion_tokens` → `max_tokens`**（方向别搞反：上游只认 `max_tokens`；`max_tokens` 已存在时丢弃别名） | `wb_proxy.py:2756-2765` | 恒 |
| 6 | `stream:true` + `stream_options.include_usage:true` | `wb_proxy.py:3025-3027` | `Plan.ForceStream` |
| 7 | DeepSeek thinking 注入（含缺档补默认档） | `wb_proxy.py:3012-3023` | 仅 deepseek 系；客户端显式 `disabled`/`effort=none` 不覆盖 |
| 8 | 工具调用配对自愈（结果块移回批次、孤儿 id 对称裁剪） | wb2a v1.4.9 | 历史存在不配对 `tool_calls` 时 |
| 9 | `reasoning_content` 回填 + `reasoning` 镜像非空 | `wb_proxy.py:2622-2660` | 与 7 绑定 |
| 10 | WAF 脱敏（默认关） | `wb_proxy.py:2366-2468` | `ScrubMode != off` |

**明确不做**（wb2a 的做法，但会改用户内容，因此拒绝移植）：

- **注入系统提示**：wb2a 在 `messages[0].role != "system"` 时插入 `SYSTEM_PROMPT`（`wb_proxy.py:2996-2998`）。这会改变用户实际发出的话题，且用户不可见；KeyGrid 不注入任何系统提示。
- **删除 `tools` 以示 `tool_choice:"none"`**：wb2a 的注释（`wb_proxy.py:2672-2697`）说明这会让模型拿不到函数签名、把调用降级成 DSML 伪 JSON 文本，Agent 原地空转。这里保留 `tools`、只把 `tool_choice` 降级为字符串。

响应侧：

| 步骤 | 依据 | 说明 |
|---|---|---|
| 忽略全 0 usage 中间帧 | wb2a v1.4.5 | `protocol.go:83-88` 已按 max 归并，只需跳过全 0 帧 |
| 断流 fallback 估算 | 同上 | 有内容无 usage 时按字节估算，入库标记为估算 |
| 上游错误码分类 | `wb_proxy.py:3264`、`wb_accounts.py:415-446` | `6004` 限额 / `11128` WAF / `11101` 参数 / `12302` 活动下线 → 映射到 `relay/reqerr.go` 既有分类，摘要只取 `error.message/code/type`（截断 512），**不落 body** |
| 429 的 `Retry-After` / reset 时间 | `wb_proxy.py:3264` | 供模型冷却 TTL |

### 3.3 M3 强制流式 + 本地聚合（G3 的解法）

上游要求 `stream:true`，而 KeyGrid 有非流客户端。实现要点：

1. 内核把上游请求的 `stream` 设为 `plan.ForceStream || req.Stream`（接线点 4）。
2. `stream_options.include_usage:true` 保证最后一块带完整 usage → 记账用既有 `extractChunkUsage`（`protocol.go:161`）。
3. 客户端要非流时，`workbuddy.CollapseSSE(respBytes)` 把 SSE 折成单条 chat JSON，再交给既有 `openaiJSONToAgg` → `writeEntryJSON`（`handler.go:694+`）分支，三个入口（chat/messages/responses）各自的输出格式都不用改。
4. 流式客户端走既有 `streamPassthrough`（openai 入口）或 `bridgeChatChunksToEntry`（messages/responses 入口），不加新路径。

**实测前提**：A3 若被推翻（非流也能过），`Plan.ForceStream` 直接置 false，本节的聚合逻辑退化成一个仍可保留的选项。这就是 P0 先跑的原因。

### 3.4 M4 可选增强：指纹与 WAF 脱敏

| 增强 | 对谁必要 | 默认 | 风险 |
|---|---|---|---|
| `X-Machine-ID`/`X-Session-ID` 稳定派生（`md5("{salt}:{uid}")` 取 32 位 hex，`wb_fingerprint.py:12-20`，注意该实现注释说 36 位、实际只取 32 位 hex） | 无硬失败证据，属多账号防风控关联 | **关** | 伪装设备身份；上游改算法即失效 |
| WAF 指纹脱敏（清 `x-anthropic-billing-header:*`、`cc_*=*`、改写 `Sisyphus-Junior...`、`11128`→`11-128`） | 只有 Codex CLI / Claude Code 客户端需要 | **关** | **会改写用户 prompt 内容** |
| `reasoning_content` 回填 | DeepSeek 多轮 + thinking | 与步骤 7 绑定 | 低 |

开启脱敏时必须配套（缺一不合并）：① 页面上显著说明"消息内容可能被平台改写"；② 每次改写写 `audit_log`（事件 `workbuddy.scrub`，只记命中规则名与消息条数，**不记内容**）；③ `off/shadow/on` 三档，`shadow` 只统计不改写。

`uid` 落库策略：`workbuddy_channel_state.AccountUID` 只存**前 8 位 + `…`** 用于展示与派生校验；指纹派生所需的完整 uid 从 `TokenSet.Extra["uid"]` 现取（凭据已加密落库），避免给"账号标识"再造一份明文副本。

### 3.5 M5 模型目录

四段回落（对齐 `wb_proxy.py:1803-1835,1979`）：

1. `/v3/config` 的 `agents[cli].models`（**必须桌面 UA**，否则 400/12403）
2. 窄端点 `/v2/enterprises/personal/models`
3. 内嵌 `snapshot/models_{intl,cn}.json`
4. `preset.Models`

过滤规则：去 5 个档位别名（`default-model`/`fast-model`/`balanced-model`/`primary-model`/`deep-model`）与 `auto`、去 `-sg`/`-x` 变体、同名只留 0.00 倍率档。

接线：`handlers/provider.go:126 presetsForUI` 与 `handlers/providermodels.go` 对 workbuddy 渠道改走本包；刷新节奏沿 `oauth/modelcatalog.go:181` 形态（启动立即 + 周期 + 失败保留旧值），周期任务并入 `Tick`。`FetchModels` 顺带刷新积分。

### 3.6 M6 运行态与准入过滤（G5）

接线点 6：候选循环里、取凭据之前。

```go
// relay/handler.go:244 附近
if err := workbuddy.Admit(cand.provider, cand.upModel, wbDeps); err != nil {
    lastErrMsg = "channel " + cand.provider.Name + " parked: " + err.Error()
    h.noteFailure(userID, apiKeyID, cand.provider.ID, req.Model, errKindChannelParked, 0, lastErrMsg, start)
    continue
}
```

`Admit` 判定顺序（对齐 `wb_accounts.py:415-446`）：

1. 渠道 `enabled` / 凭据非 revoked → 内核已拦（`handler.go:36,278`），不重复
2. 用户 `workbuddy_settings.Enabled` 且平台 kill switch 允许 → 否则 park（`reason=disabled`）
3. 模型级冷却命中 → `ParkedError{cooldown, Until}`
4. 保留积分：`credits <= reserve_credits`（`wb_accounts.py:381-406`）
5. 每日 token 限额：当日用量 ≥ 限额（"未计数过的天不阻塞"，`wb_accounts.py:400-414`）
6. 通过

全候选被 park → 内核统一返 `429` + `Retry-After`（对齐 `wb_proxy.py:3435-3450`，但由内核产出口径一致）。

**日 token 计数比 wb2a 简单**：wb2a 要增量 fold `usage.jsonl`；KeyGrid 的 `usage_logs`（`model/usage_log.go` + `op/usage_range.go`）就是权威账本，按 `provider_id` + 当日聚合即可，Redis 只做热路径增量。

**"22:00 集中保活"不移植**：`oauth.Scheduler`（60s 扫描 + 提前 5min + relay 兜底 `relay/oauth.go:47`）已覆盖。

**积分探测**：`ProbeCredits` 经 `quota.Syncer` 形态接入（手动刷新按钮 + 被动观察 + 15min 缓存）；`quota/codex.go:48 SupportsQuota` 的判定按 `oauth_provider` 分支即可，不必抽象。

### 3.7 M7 运营自动化（全量，默认全关）

| 任务 | JobKey | 上游 | 复杂度 | 风控 | 期 |
|---|---|---|---|---|---|
| CN 每日签到 | `checkin` | `POST {billing}/v2/billing/meter/daily-checkin` `{}` | 低 | 低 | P3 |
| 积分查询 | `credits` | `POST {billing}/v2/billing/meter/get-user-resource` | 低 | 低 | P3 |
| INTL 每日活跃打卡（桌面端） | `daily_chat` | `POST {intl}/v2/chat/completions`（低耗模型 + 短 prompt） | 低 | 中 | P3 |
| CN 猫猫旅行 | `cat` | `GET /buddy/travel/status\|config` → `POST /depart {location_id}` → `POST /claim {}` | 中 | 中 | P4 |
| CN 成长/积分任务 | `growth` | 见下 | 高 | **高** | P4 |
| CN 夜猫子 | `night` | 同 growth + 夜间窗口 | 中 | 中–高 | P4 |
| INTL 网页通道 ACP 打卡 | `daily_chat_web` | 见下 | 高 | 中 | P4 |

**成长任务状态机**（`tasks_cn.go`，对齐 `wb_tasks.py:37-101,428-585`）：

```
1. GET  {billing}/v2/activity/growth/tasks                     → 未接任务列表
2. POST {billing}/v2/activity/growth/tasks/accept              → {task_codes:[...]}，每批 ≤20（wb_tasks.py:165）
3. POST /v2/report  [事件对象...]                               → 逐类上报（wb_tasks.py:347）
4. GET  任务进度（summary）                                     → 轮询到可领奖
5. POST {billing}/v2/activity/growth/tasks/{code}/claim         → 次数类；400 时降级到 workbuddy.cn web 域重试
```

14 类行为事件的构造（画布创建、灵感案例、模板使用、模型体验、多轮对话…）在 `wb_tasks.py:278-347`，`build_event` 里带专家/团队事件 id：同一批事件里 `(eventCode, id)` 必须互异，否则被上游判重。移植时：

- 事件 id 池与已用记录存 `WorkbuddyChannelState.Meta`（跨重启去重）；
- 桌面专属任务（`RichMeow_Chat` / `Library_read` / `Buddy_App` / `Buddy_App_QQ`）**跳过** —— wb2a 自己判定它们只认真实点击（`wb_tasks.py` 注释）；
- `gap ≈ 1s` 节奏、单账号每日一次、夜间任务限 23:00–08:00 窗口；
- 每步写 `workbuddy_job_runs`（`status` + 摘要），失败不重试超过 1 次。

**ACP 网页通道打卡**（`webagent.go`，对齐 `wb_webagent.py`，只用标准库）：

```
1. POST {intl}/console/as/conversations/
   {prompt, model, conversationOrigin:"workbuddy-app", plugins:[{name:"weixinpay",...}]}
   → 仅需 Authorization: Bearer + X-User-Id（无 X-IDE-* 指纹）→ 拿 conversation id
2. GET  {intl}/console/as/conversations/{id}/session  → {link, token, sessionId, cwd}
3. GET  {link}  Accept: text/event-stream             → 从响应头取 Acp-Connection-Id
4. POST {link}  header Acp-Connection-Id              → JSON-RPC 顺序：
     initialize(protocolVersion=1, clientCapabilities 全关)
     session/load(sessionId, cwd, mcpServers:[])
     session/prompt(prompt 文本)
5. 后台读 SSE（统计 session/update / agent_message_chunk 段数）
   + 轮询 GET {intl}/console/as/conversations/{id} 的 status，completed 即成功，failed/error 停止
```

要点：建会话只是**排队**，必须接上沙箱并跑完这一轮才算打卡（wb2a 的 v1.6.9 就是踩了这个坑）；单轮超时 120s（`WB_WEB_TURN_TIMEOUT` 可调）；失败时把 conversation id 写进 `Message` 便于核对；只记"状态 + 输出段数 + 耗时"，**不记输出内容**。

**任务框架**（`jobs.go` + `Tick`）：

- **per-user 展开**：wb2a 是单租户全局账号池；KeyGrid 每用户只能操作自己的渠道。`Tick` 枚举已启用者 × 该用户启用该任务的渠道。
- **幂等**：Redis `SETNX workbuddy:job:{job}:{userID}:{providerID}:{slot}`，slot 精确到分钟；手动触发走同一键（防连点）。
- **错峰**：同分钟多账号执行加 0–30s 抖动 + 并发上限 4。
- **超时与取消**：默认 60s（ACP 120s），`ctx` 随进程退出取消。
- **kill switch**：平台级 `workbuddy_jobs_enabled=false` → `Tick` 直接返回并记一条 skipped；优先级高于用户开关。
- **审计**：每次执行写 `audit_log`（`user_id` + event `workbuddy.job.<key>` + provider_id + status），与 `workbuddy_job_runs` 互为佐证。
- **无 Redis**：单实例假设放行 + 启动 WARN（与 `oauth/refresh.go:65` 同取舍），文档写明多实例必须配 Redis。
- **失败可见**：任务失败只记上游错误摘要（沿用 `relay/reqerr.go` 提取规则，绝不落 body），前端日志表可查。

### 3.8 M8 出口代理槽位（G6，P5，通用能力）

现状：全局 `platform_settings.proxy_url` + `Provider.UseProxy bool`，选择逻辑散在 4 处（`relay/state.go:294 clientFor`、`oauth/refresh.go:218`、`quota/sync.go:173`、`httpx` 调用方）。

1. 新表 `egress_proxies` + `providers.egress_proxy_id`。
2. 统一入口 `proxyForProvider(p)`：渠道专属槽位 → 平台默认 → 直连。
3. 代理地址是用户输入 → 必须过 `ssrf.CheckBaseURL`（`ssrf/ssrf.go:172`）；内网代理是常见部署，沿用 `SSRF_ALLOW_INTERNAL` 语义并在 UI 提示。
4. "测试出口 IP"：经代理请求**平台内置常量端点**（不接收用户 URL），避免成为 SSRF 跳板。
5. 账号身份请求（refresh / 积分 / 订阅探测 / 自动化任务）也走同一出口 —— wb2a v1.4.7 专门修过这点（把账号身份与宿主机 IP 解绑），必做。

### 3.9 M9 面板

| 位置 | 内容 |
|---|---|
| 渠道列表 | 徽章：积分、当日 token、`parked`（悬停原因 + 恢复时间）、冷却中的模型 |
| WorkBuddy 页 | ① 运行态表 ② 任务开关 + 立即执行 + 二次确认 ③ 最近 50 条执行日志 ④ 限额/保留积分/身分档位/脱敏档位 |
| 添加渠道向导 | 零改动（presets 自动出现）；多账号 = 连续走 N 次 OAuth |
| 启用前的 Notice | 强制展示 `Notice` 文案（含"账号可能被上游限制""内容可能被改写"）并要求确认 |

### 3.10 M10 不做清单

| 项 | 原因 |
|---|---|
| 桌面客户端本地凭据扫描导入（`wb_accounts.py:1292-1400`） | 上游已改加密存储，导入即 401；读他人机器上的明文凭据文件属明确越界 |
| 本地 `web_search` / `web_fetch` 代跑（`wb_webtools.py`） | wb2a 自己已在 v1.5.3 判定上游无服务端搜索能力并移除；KeyGrid 若要做应做成通用"服务端工具执行器"，不是私有能力 |
| 跨区域同名模型混合轮询 | wb2a 也明确未做（README §1）；与"一渠道一区域一账号"的隔离模型冲突 |
| 面板密码 / 第二套 API Key | KeyGrid 已有用户体系 + `sk-` key |
| `usage.jsonl` 明细流水 | KeyGrid 已有 `usage_logs` + 失败诊断 + 计费，无需并行账本 |

---

## 4. 分期

### P0：基线与事实（脚本已就绪，待跑）

- [x] 探针脚本 `scripts/probe_workbuddy.sh`（免费检查 + 可选聊天矩阵，绝不回显 token/prompt/正文）
- [x] 事实记录模板 `docs/workbuddy-upstream-notes.md`（含 15 条待验证假设 A1–A15 与错误码字典）
- [ ] 跑一次真实账号探针，回填模板；确认 G1/G2/G3 与最小 header 集
- [ ] 用 `WB_UPSTREAM=http://127.0.0.1:9999` 对 mock 跑一遍脚本自身（不碰真实上游）
- **门禁**：`A1–A3 + A7–A9` 有结论；错误码字典覆盖 `11128`/`6004`；模板签字栏填齐

### P1：渠道接入（能用自己的账号跑通三入口）

- [x] `internal/oauth/providers/workbuddy.go`（intl，state 轮询；`TokenSet.Extra` 写 `uid`/`nickname`/`realm`）
- [x] `internal/workbuddy`：`realm.go` / `identity.go` / `pipeline.go` / `adapt.go` / `stream.go`
- [x] relay 接线点 1–5；`upstreamCall` 增加 `extra http.Header` + `forceStream bool`（4 个调用点已同步）
- [x] 预设：新增 `workbuddy-intl`，修正 `codebuddy-cn`（base_url 只留 host）；存量渠道 base_url 幂等归一化
- [x] `.env.example` 增补 `WORKBUDDY_*` 四项
- [x] 内嵌模型快照 + `FetchModels` + 接线点 9（`catalog.go`，四段回落 /v3/config → 窄端点 → 快照）
- [x] relay 侧 httptest 契约测试（断言请求路径 / 身分头 / `stream:true` / 非流聚合）
- [ ] `E2E_MODE` 打桩下的五路径矩阵 + 真实账号手测（P0 门禁未过，暂缺）
- **验收**：chat 流式/非流、`/v1/messages`、`/v1/responses`、工具调用、429 failover 到第二条渠道 —— 五条路径在 `E2E_MODE` 打桩下全绿；真实账号手测一次成功；`request_errors` 无正文

### P2：运行态（限额/积分/冷却）

- [x] `workbuddy_settings` / `workbuddy_channel_state` / `workbuddy_job_runs` 三表 + `op` 方法（全部带 user_id）
- [x] `Admit` + 全 parked 的 429/Retry-After；日 token 聚合；保留积分；模型冷却；`Observe`（69 项单测，无需 PG/Redis）
- [x] `ProbeCredits` + 手动刷新 + 15min 缓存；接线点 7、10（响应形状待 P0 探针确认）
- [x] 前端：WorkBuddy 页（设置 / 运行态 / 任务开关 / 执行日志）+ i18n 双语言 + 图标复用（渠道徽章留待 P2 收尾）
- **验收（已完成的部分）**：parked 渠道被跳过并 fallback 生效（三入口实测）；跨用户读不到运行态（真库隔离实测：用户 B 拿不到用户 A 的渠道，404）；`make test` + `npx tsc --noEmit` 全绿

### P3：低风险自动化

- [x] `jobs.go` + `Tick` + 接线点 8；幂等锁、错峰（0–30s 抖动 + 并发 4）、超时、kill switch（平台设置优先于 env）、双写审计
- [x] `checkin` / `credits` / `daily_chat`（默认全关；INTL 无签到端点、CN 无 daily_chat 一并如实 skipped）
- [x] 前端任务开关 + 立即执行 + 执行日志（P2 已建，P3 起真实生效）+ 管理端「自动化任务总闸」
- **验收**：并发触发只执行一次（Redis 锁 + 锁竞争单测）；kill switch 三层优先级单测；手动路径不抖动；
  任务失败只写运行日志与审计，不触碰熔断状态（任务路径不经 relay 熔断器）

### P4：高风险自动化（全量）

- [ ] `growth` 状态机 + 事件去重 + 领奖降级域
- [ ] `night`（夜间窗口）/ `cat`
- [ ] `webagent.go` 的 ACP 五步链路（120s 超时、SSE 计数、conversation id 回传）
- [ ] 前端：这些任务的开关默认灰置，开启需二次确认 + 风险文案
- **验收**：每个任务在 `httptest` 打桩下走通全流程；ACP 有"已建会话但未跑完"的超时路径测试；`workbuddy_job_runs` 不出现任何正文

### P5：通用增强

- [ ] 出口代理槽位（含 SSRF 与出口 IP 测试）
- [ ] 脱敏 `off/shadow/on` 三档 + 改写审计
- [ ] 设备指纹稳定派生（默认关）

---

## 5. 测试策略

| 层 | 手段 |
|---|---|
| 纯函数 | `pipeline_test.go`：10 步归一化的输入输出表驱动；响应侧 usage 归并、错误码分类、`CollapseSSE`。**零网络**。 |
| 驱动端到端 | `httptest.Server` 冒充上游，断言：请求路径 `/v2/chat/completions`、必需 header 全集、`stream:true`、body 归一化结果；返回 SSE 与 JSON 两种形态。 |
| 回归门禁 | P1 的 `upstreamCall` 签名改动必须让既有 `relay/*_test.go`（`codex_test.go`/`xai_test.go`/`handler_test.go`/`stream_bridge_test.go`）全绿。 |
| 准入/限额 | `Admit` 单测（日限额、保留积分、冷却、未计数天不阻塞、disabled）；日 token 聚合与 `usage_logs` 口径一致。 |
| 任务 | `Tick` 的到期判定（含 `*/3`、月末、闰年）；幂等锁并发测试；每个任务用桩 `Deps` 打桩；ACP 的超时/失败路径。 |
| 隔离 | 三张新表的 `op` 方法各有一条"跨用户读不到"的单测（对齐现有 `op` 测试风格）。 |
| E2E | `scripts/run_e2e_workbuddy.sh`：`E2E_MODE=1` + `WORKBUDDY_UPSTREAM_BASE` 指向 mock，跑 chat/messages/responses × 流式/非流；`scripts/mock_upstream.py` 按 P0 结论扩展形状。 |
| 真实账号 | **不进 CI**，手工冒烟清单；结果回填 `workbuddy-upstream-notes.md`（只记状态码/token 数，不记内容）。 |

---

## 6. 风险与合规

选定全量范围后，风险等级整体上移，逐项缓解：

| 风险 | 等级 | 缓解 |
|---|---|---|
| **服务条款与账号封禁**（伪造官方客户端身分、绕过渠道校验、伪造行为事件） | **高** | 平台级 kill switch 优先于一切；所有会伪造行为的任务默认关 + 二次确认 + 风险文案；只对本人授权账号；不做公开注册；文档写明账号风险由使用者承担、管理员为行为主体 |
| **成长任务/ACP 打卡的性质** | **高** | 单独列出、独立开关、独立审计事件；失败不重试超过 1 次；单账号每日一次；夜间窗口 + 抖动；上线前在 `docs/` 记一份"为什么做/谁批准" |
| **合规声明**（wb2a 为 MIT 但含逆向与风控规避产物） | 中 | `THIRD_PARTY_NOTICES.md` 补 wb2a 段（参照现有 9router 段写法）；保留版权与许可；前端强制展示 `Notice` |
| **内容被改写**（WAF 脱敏） | 中 | 默认关；三档 + 审计 + UI 明示；规则独立成 `scrub.go` |
| **凭据安全** | 高 | 只走 `crypto` + `TokenSet`；`MASTER_KEY` 不落日志；运行态表不存 token；账号标识只存前 8 位；任务日志只记状态摘要 |
| **SSRF**（出口代理、上游 URL 覆盖） | 中 | 用户输入 URL 过 `ssrf.CheckBaseURL`；出口 IP 测试用平台内置常量端点；本包不接受用户传入的任意回显 URL |
| **多实例重复执行任务**（会真花额度） | 中 | Redis 幂等锁；无 Redis 时启动 WARN + 单实例假设；文档写明多实例必须配 Redis |
| **上游漂移**（wb2a 自己每月都在改端点和字段） | 高 | 上游事实集中 `realm.go`；`httptest` 契约测试；`/v3/config` 失败保留旧快照；错误码表独立可改；P0 记录留档便于对比 |
| **规模放大**（用户数 × 渠道数 × 任务数） | 中 | per-user 展开 + 并发上限 + 抖动错峰 + 单任务超时 + 一键全停 + `workbuddy_job_runs` 可观测 |
| **热路径性能**（`Admit`） | 低 | 只读 Redis + 进程内缓存；credits / 日 token 都有 TTL 缓存，miss 才查 DB |

---

## 7. 决策与待确认

| # | 决策 | 状态 |
|---|---|---|
| D1 | 集成范围 | ✅ **全量**（含成长任务与 ACP 网页打卡），高风险项默认关 + kill switch |
| D2 | 代码形态 | ✅ **不做通用插件层**，`internal/workbuddy` 独立包 + 薄接线；代价见 §1.4 |
| D3 | 首批动作 | ✅ **先补 P0 文档与探针**，脚本与模板已产出，待跑实测 |
| D4 | 多账号表达：一账号一渠道 vs 渠道内多账号池 | ⏳ 建议后者不做（§2.4），需确认 |
| D5 | 面板形态：独立页 vs 融入 Providers | ⏳ 建议独立页 + 渠道徽章（§3.9） |
| D6 | 验收账号与隐私边界：谁提供账号、谁能看到该账号的积分/用量 | ⏳ 需明确（关系到 §2.4 的可见性设计） |
| D7 | 脱敏与指纹：默认关是否可接受，还是干脆不做 | ⏳ 建议默认关且 P5 再做 |
| D8 | 成长任务/ACP 的批准人 | ⏳ 上线前需落到 `docs/`，作为合规留痕 |

---

## 附：与 AGENTS.md 硬性约束的对应

| 规则 | 本方案如何满足 |
|---|---|
| 1 数据隔离 | 三张新表全带 `user_id`，只经 `op`；`Admit`/`Observe` 只接收已按 user_id 收窄的 `cand.provider` |
| 2 凭据加密 | 本包不接触 `enc_data`，只用内核解好的 `oauth.TokenSet`；`Extra` 存 uid/nickname（非密） |
| 3 API Key 存储 | 不受影响 |
| 4 日志不落内容 | `workbuddy_job_runs.Message` 与 `request_errors` 同规则；脱敏只记规则名与条数；ACP 只记段数与耗时 |
| 5 SSRF | 出口代理与任何用户输入 URL 过 `ssrf` |
| 6 OAuth 安全 | 复用 `oauth_states`（一次性 + 5min）与 `oauth.Scheduler` 的 Redis 锁与错误分类 |
| 7 `.env.example` | 新增 `WORKBUDDY_*` 同步文档 |

**代码约定**：注释与文档中文；错误 `fmt.Errorf("...: %w")`；响应走 `server/resp`；新增路由先在 `server/router/router.go` 注册；提交信息 Conventional Commits 中文描述（如 `feat(workbuddy): 渠道接入与端点归一化`）。
