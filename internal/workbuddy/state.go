package workbuddy

// state.go 运行态判定器（M6，设计 §3.6）：准入过滤（Admit）、结果回填（Observe）、
// 积分主动探测（ProbeCredits）与冷却表读取（Cooldowns）。
//
// 三条不可违背的约定：
//  1. fail-open：任何内部错误（设置/运行态读失败、Redis 挂掉、聚合失败）都返回放行，
//     状态存储故障绝不能让渠道整体不可用。
//  2. 零值/nil 安全：`Runtime` 的零值（以及 nil 指针）一律 no-op，relay 可以无条件
//     调用，接线点不需要判空分支。
//  3. 不落内容：日志只写 provider id / 状态码 / 原因，绝不写 token、prompt、
//     上游响应体或积分数值。
//
// 冷却与当日用量都以 Postgres 为权威、Redis 为热路径：Redis 命中优先，
// 缺失（或未接 Redis 的单实例部署）时回落到运行态行 / usage_logs 聚合。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"

	"github.com/redis/go-redis/v9"
)

// 常量：Redis 键前缀（可用 Runtime.KeyPrefix 覆盖）、停用原因、冷却与缓存参数。
const (
	defaultKeyPrefix = "workbuddy"

	// 停用原因（落 WorkbuddyChannelState.ParkedReason / ParkedError.Reason）。
	reasonDisabled       = "disabled"
	reasonDailyLimit     = "daily_limit"
	reasonReserveCredits = "reserve_credits"
	reasonCooldown       = "cooldown"

	// 冷却：上游未给 Retry-After 时默认 60s；防御性上限 1h
	// （上游若回 Retry-After: 86400，不能把一条渠道停一整天）。
	defaultCooldown = 60 * time.Second
	maxCooldown     = time.Hour

	// 积分缓存 15min（设计 §2.4 Redis 键表）；主动探测 30s 超时。
	creditsCacheTTL     = 15 * time.Minute
	creditsProbeTimeout = 30 * time.Second

	// maxProbeBodyBytes 读取上游响应体的上限（探测响应很小，防止异常超大正文占内存）。
	maxProbeBodyBytes = 1 << 20
	// maxProbeErrLen 上游错误摘要长度上限（rune）。
	maxProbeErrLen = 200

	// redisOpTimeout 热路径上单次 Redis 操作的超时（不能拖住转发）。
	redisOpTimeout = 2 * time.Second

	// upstreamCodeDailyLimit 上游业务码：当日限额（设计 §3.7 错误码分类）。
	upstreamCodeDailyLimit = 6004
)

// probeCreditsBody 积分查询请求体（逐个字段照抄参考实现，不可改）。
const probeCreditsBody = `{"PageNumber":1,"PageSize":100,"ProductCode":"p_tcaca","Status":[0,3]}`

// ParkedError 渠道被准入过滤拦下（不是故障：换下一个候选即可）。
type ParkedError struct {
	Reason string    // disabled | daily_limit | reserve_credits | cooldown
	Until  time.Time // 零值 = 需要人工处理（如 disabled）
}

// Error 稳定输出（内核把它拼进 lastErrMsg / 诊断记录）：
//
//	workbuddy channel parked: disabled
//	workbuddy channel parked: cooldown until 2025-03-14T02:30:00Z
//
// Until 统一按 UTC 输出，避免同一时刻在不同进程/时区下字符串不同。
func (e *ParkedError) Error() string {
	if e == nil {
		return "workbuddy channel parked"
	}
	if e.Until.IsZero() {
		return "workbuddy channel parked: " + e.Reason
	}
	return "workbuddy channel parked: " + e.Reason + " until " + e.Until.UTC().Format(time.RFC3339)
}

// Event 一次上游调用的观察结果（由 relay 在记账后回填）。
type Event struct {
	Provider   *model.Provider
	UpModel    string
	Status     int           // HTTP 状态码（0 = 未知/传输层错误）
	UpCode     int           // 上游业务码（如 6004 限额）；0 = 未知
	RetryAfter time.Duration // 上游 Retry-After；0 = 未提供
	Tokens     int64         // 本次 prompt+completion tokens
	Success    bool
}

// counterStore Redis 热路径的最小接口（Get/Set/Delete）。
//
// 抽成接口有两个目的：一是生产用 go-redis 实现，二是测试可注入内存假实现，
// 让 Redis 分支在没有 Redis 服务器时也被真正覆盖。所有方法都不返回 error ——
// Redis 错误一律按「miss / 静默跳过」处理，这正是 Admit 的 fail-open 语义。
type counterStore interface {
	Get(ctx context.Context, key string) (string, bool)
	Set(ctx context.Context, key, value string, ttl time.Duration)
	Delete(ctx context.Context, key string)
}

// stateStore Admit/Observe/ProbeCredits 需要的持久化能力（*op.Op 满足）。
//
// 生产路径始终走 *op.Op（硬隔离查询都在 op 层）；接口的意义是让本文件的判定/
// 回填逻辑可以在没有 Postgres 的情况下被完整测试 —— 仓库约定不做 DB-backed 测试。
type stateStore interface {
	GetWorkbuddySetting(userID int64) (*model.WorkbuddySetting, error)
	GetWorkbuddyChannelState(userID, providerID int64) (*model.WorkbuddyChannelState, error)
	UpsertWorkbuddyChannelState(s *model.WorkbuddyChannelState) error
	AddWorkbuddyDailyTokens(userID, providerID int64, date string, delta int64, now time.Time) error
	SumProviderTokens(userID, providerID int64, since, until time.Time) (int64, error)
	SetWorkbuddyCredits(userID, providerID int64, credits float64, at time.Time) error
}

// 编译期断言：op 层签名变化会在这里立刻暴露。
var _ stateStore = (*op.Op)(nil)

// redisCounter counterStore 的 go-redis 实现。
type redisCounter struct{ rdb *redis.Client }

func (c redisCounter) Get(ctx context.Context, key string) (string, bool) {
	if c.rdb == nil {
		return "", false
	}
	v, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return "", false // miss 或 Redis 故障：都按「没命中」处理（fail-open）
	}
	return v, true
}

func (c redisCounter) Set(ctx context.Context, key, value string, ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		// 只记键名（含 provider id / 模型名，无敏感内容）与错误。
		log.Printf("[workbuddy] redis set failed key=%s: %v", key, err)
	}
}

func (c redisCounter) Delete(ctx context.Context, key string) {
	if c.rdb == nil {
		return
	}
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		log.Printf("[workbuddy] redis del failed key=%s: %v", key, err)
	}
}

// Runtime 运行态判定器。零值/nil 一律安全可用（no-op），
// 这样 relay 可以无条件调用，接线点不需要判空分支。
type Runtime struct {
	Op        *op.Op
	RDB       *redis.Client // nil = 无 Redis（单实例部署）
	Now       func() time.Time
	KeyPrefix string // 默认 "workbuddy"

	// 以下两个字段是测试接缝：生产分别由 Op / RDB 派生，置空即可。
	dbs stateStore   // 非 nil 时覆盖 Op（测试注入假 DB）
	cs  counterStore // 非 nil 时覆盖 RDB（测试注入内存计数器）
	// creditsURL 积分端点构造（测试注入 httptest 地址）；nil 用 CreditsEndpoint。
	creditsURL func(realm Realm) string
}

// NewRuntime 构造运行态判定器。
func NewRuntime(o *op.Op, rdb *redis.Client) *Runtime {
	return &Runtime{Op: o, RDB: rdb}
}

// now 当前时间（可注入）。
func (r *Runtime) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// prefix Redis 键前缀（默认 workbuddy）。
func (r *Runtime) prefix() string {
	if r == nil {
		return defaultKeyPrefix
	}
	if p := strings.TrimSpace(r.KeyPrefix); p != "" {
		return p
	}
	return defaultKeyPrefix
}

// counters 返回 Redis 计数器（测试注入的内存实现优先）。
func (r *Runtime) counters() counterStore {
	if r == nil {
		return nil
	}
	if r.cs != nil {
		return r.cs
	}
	if r.RDB != nil {
		return redisCounter{rdb: r.RDB}
	}
	return nil
}

// db 返回持久化实现（测试注入的假 DB 优先）。
func (r *Runtime) db() stateStore {
	if r == nil {
		return nil
	}
	if r.dbs != nil {
		return r.dbs
	}
	if r.Op == nil {
		return nil
	}
	return r.Op
}

// Admit 准入判定：返回 *ParkedError 表示本次不应使用该渠道。
// 任何内部错误都 fail-open（返回 nil）——状态存储故障不能让渠道整体不可用。
func (r *Runtime) Admit(p *model.Provider, upModel string) error {
	// 1. 非本包渠道 / 未接线：绝不干预。
	if r == nil || r.Op == nil || !IsWorkBuddy(p) {
		return nil
	}
	db := r.db()
	if db == nil {
		return nil
	}
	now := r.now()

	// 2. 用户总开关。读失败 fail-open；读不到（零值）默认关 → 停用。
	settings, err := db.GetWorkbuddySetting(p.UserID)
	if err != nil {
		log.Printf("[workbuddy] admit settings read failed provider=%d: %v (fail-open)", p.ID, err)
		return nil
	}
	if settings == nil || !settings.Enabled {
		return &ParkedError{Reason: reasonDisabled}
	}

	// 3. 渠道运行态。缺行 = 还没观测过任何数据 → 放行。
	state, err := db.GetWorkbuddyChannelState(p.UserID, p.ID)
	if err != nil {
		log.Printf("[workbuddy] admit state read failed provider=%d: %v (fail-open)", p.ID, err)
		return nil
	}
	if state == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	// 4. 模型级冷却：Redis 优先，miss/无 Redis 回落到运行态 map。
	effective := state
	if until, ok := resolveCooldown(ctx, r.counters(), r.prefix(), p.ID, upModel, state); ok {
		effective = withCooldown(state, upModel, until)
	}

	today := localDay(now)
	return admitDecision(settings, effective, upModel, now, func() (int64, bool) {
		// 6. 当日 token：Redis 实时计数 → 运行态当日缓存 → usage_logs 聚合。
		if v, ok := todayTokensFrom(ctx, r.counters(), r.prefix(), p.ID, today); ok {
			return v, true
		}
		if state.TokensDate == today {
			return state.DailyTokens, true
		}
		total, err := db.SumProviderTokens(p.UserID, p.ID, localDayStart(now), now)
		if err != nil {
			// 聚合失败 = 无数据：不阻塞（与「未计数过的天不阻塞」同一语义）。
			return 0, false
		}
		return total, true
	})
}

// admitDecision 纯判定逻辑（不碰 DB/Redis，便于表驱动测试）。
//
// 判定顺序（设计 §3.6）：
//  1. 用户总开关关闭（settings 为 nil 视为关闭）→ disabled
//  2. 运行态缺失 → 放行
//  3. 模型级冷却未到期 → cooldown
//  4. 保留积分命中（仅当配置了保留额且观测过积分）→ reserve_credits
//  5. 当日 token 限额达到（仅当配置了限额且有当日数据）→ daily_limit
//  6. 放行
//
// todayTokens 返回 (当日总量, 是否有数据)；ok=false 表示「没有计数过」，
// 此时一律不阻塞 —— 这是刻意语义，不是 bug：拿不到数据不等于额度用完。
func admitDecision(settings *model.WorkbuddySetting, state *model.WorkbuddyChannelState, upModel string, now time.Time, todayTokens func() (int64, bool)) error {
	if settings == nil || !settings.Enabled {
		return &ParkedError{Reason: reasonDisabled}
	}
	if state == nil {
		return nil
	}

	// 3. 模型级冷却。
	if until, ok := state.Cooldowns[upModel]; ok && until.After(now) {
		return &ParkedError{Reason: reasonCooldown, Until: until}
	}

	// 4. 保留积分：只有配置了保留额、且确实观测过积分时才判定。
	// 从未观测过（CreditsAt 零值）不能当成「积分为 0」——数据缺失不是拦截理由。
	if settings.ReserveCredits > 0 && !state.CreditsAt.IsZero() && state.Credits <= settings.ReserveCredits {
		return &ParkedError{Reason: reasonReserveCredits}
	}

	// 5. 当日 token 限额。未计数过的天不阻塞（有数据才判定）。
	if settings.DailyTokenLimit > 0 {
		total, ok := int64(0), false
		if todayTokens != nil {
			total, ok = todayTokens()
		}
		if ok && total >= settings.DailyTokenLimit {
			return &ParkedError{Reason: reasonDailyLimit, Until: nextLocalDayStart(now)}
		}
	}
	return nil
}

// Observe 回填一次调用结果：成功清冷却+刷新 last_ok+累加当日用量；
// 429/6004 设置模型级冷却。必须异步安全、不阻塞请求（调用方自行决定是否 goroutine）。
//
// Redis 与 Postgres 都写：Redis 是热路径，Postgres 是权威落库；两者都容忍 nil RDB。
func (r *Runtime) Observe(ev Event) {
	if r == nil || r.Op == nil || !IsWorkBuddy(ev.Provider) {
		return
	}
	p := ev.Provider
	now := r.now()
	cs := r.counters()
	switch {
	case ev.Success:
		r.observeSuccess(cs, p, ev, now)
	case ev.Status == http.StatusTooManyRequests || ev.UpCode == upstreamCodeDailyLimit:
		r.observeCooldown(cs, p, ev, now)
	}
}

// observeSuccess 成功分支：清冷却、写 last_ok_at、累加当日 token。
func (r *Runtime) observeSuccess(cs counterStore, p *model.Provider, ev Event, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	prefix := r.prefix()
	today := localDay(now)
	db := r.db()

	// 先读运行态，目的是「读改写」时保留其它字段（Upsert 是全列覆盖）。
	var state *model.WorkbuddyChannelState
	var readErr error
	if db != nil {
		state, readErr = db.GetWorkbuddyChannelState(p.UserID, p.ID)
	}
	if readErr != nil {
		log.Printf("[workbuddy] observe state read failed provider=%d: %v (redis-only)", p.ID, readErr)
	}

	// 冷却清除：Redis 删除 + Postgres map 删除（过期项顺带清理）。
	if cs != nil {
		cs.Delete(ctx, cooldownKey(prefix, p.ID, ev.UpModel))
	}
	if readErr == nil {
		if state == nil {
			state = &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}
		}
		// 成功请求说明停用条件已解除（用户重新开启 / 额度重置 / 补足积分），
		// 顺手清空停用原因 —— 转发侧的 admit 是只读的，这里是唯一的恢复点。
		if state.ParkedReason != "" {
			state.ParkedReason = ""
		}
		deleteCooldown(state, ev.UpModel, now)
		state.LastOKAt = &now
		state.UpdatedAt = now
		if db != nil {
			if err := db.UpsertWorkbuddyChannelState(state); err != nil {
				log.Printf("[workbuddy] observe last_ok write failed provider=%d: %v", p.ID, err)
			}
		}
	}

	if ev.Tokens > 0 && db != nil {
		if err := db.AddWorkbuddyDailyTokens(p.UserID, p.ID, today, ev.Tokens, now); err != nil {
			log.Printf("[workbuddy] observe daily tokens write failed provider=%d: %v", p.ID, err)
		}
		// Redis 当日计数刷新为「新总量」，TTL = 本地当日结束 + 1h 余量。
		refreshTokensCounter(ctx, cs, prefix, p.ID, today, state, ev.Tokens, now)
	}
}

// observeCooldown 429 / 6004 分支：写模型级冷却（Redis TTL + Postgres map）。
func (r *Runtime) observeCooldown(cs counterStore, p *model.Provider, ev Event, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	prefix := r.prefix()

	cooldown := cooldownFor(ev)
	until := now.Add(cooldown)

	// Redis 先写：即使 Postgres 读失败，热路径也应立刻生效。
	if cs != nil {
		cs.Set(ctx, cooldownKey(prefix, p.ID, ev.UpModel),
			until.UTC().Format(time.RFC3339Nano), cooldown)
	}

	db := r.db()
	if db == nil {
		return
	}
	state, err := db.GetWorkbuddyChannelState(p.UserID, p.ID)
	if err != nil {
		log.Printf("[workbuddy] cooldown state read failed provider=%d: %v (redis-only)", p.ID, err)
		return
	}
	if state == nil {
		state = &model.WorkbuddyChannelState{UserID: p.UserID, ProviderID: p.ID}
	}
	// 写前清理过期项，避免 jsonb 列无限增长。
	applyCooldown(state, ev.UpModel, until, now)
	state.UpdatedAt = now
	if err := db.UpsertWorkbuddyChannelState(state); err != nil {
		log.Printf("[workbuddy] cooldown write failed provider=%d: %v", p.ID, err)
	}
}

// cooldownFor 计算冷却时长：Retry-After（>0）否则默认 60s，并防御性封顶 1h。
func cooldownFor(ev Event) time.Duration {
	d := ev.RetryAfter
	if d <= 0 {
		d = defaultCooldown
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	return d
}

// ProbeCredits 主动查询积分余额（手动刷新与目录刷新时调用）。
//
// 响应形状未实测（P0 探针项 A9），因此解析刻意宽松：按优先级在 data 内、
// 然后顶层大小写不敏感地找第一个可解析字段；失败时只报「见过的键名」，
// 便于探针回填键列表。
func (r *Runtime) ProbeCredits(ctx context.Context, client *http.Client, p *model.Provider, tok *oauth.TokenSet) (float64, error) {
	if p == nil || tok == nil {
		return 0, errors.New("workbuddy: probe credits requires provider and token")
	}
	if r == nil || r.Op == nil {
		return 0, errors.New("workbuddy: runtime not configured")
	}
	realm, ok := RealmOf(p)
	if !ok {
		return 0, ErrNotWorkBuddy
	}

	hdr, err := Headers(p, tok, HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()})
	if err != nil {
		return 0, fmt.Errorf("workbuddy: build credits headers: %w", err)
	}
	hdr.Set("Authorization", "Bearer "+tok.AccessToken)

	reqCtx, cancel := context.WithTimeout(ctx, creditsProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.creditsEndpoint(realm), strings.NewReader(probeCreditsBody))
	if err != nil {
		return 0, fmt.Errorf("workbuddy: build credits request: %w", err)
	}
	req.Header = hdr

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("workbuddy: credits request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes))
	if err != nil {
		return 0, fmt.Errorf("workbuddy: read credits response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 只带状态码 + 结构化上游错误字段（≤200 字），绝不回显原始正文/凭据。
		if summary := upstreamErrSummary(body); summary != "" {
			return 0, fmt.Errorf("workbuddy: credits probe status %d: %s", resp.StatusCode, summary)
		}
		return 0, fmt.Errorf("workbuddy: credits probe status %d", resp.StatusCode)
	}

	credits, err := parseCredits(body)
	if err != nil {
		return 0, err
	}

	now := r.now()
	if err := r.db().SetWorkbuddyCredits(p.UserID, p.ID, credits, now); err != nil {
		return credits, fmt.Errorf("workbuddy: persist credits: %w", err)
	}
	// 刷新 15min 缓存（值不放日志）。
	if cs := r.counters(); cs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		cs.Set(ctx, creditsKey(r.prefix(), p.ID), strconv.FormatFloat(credits, 'f', -1, 64), creditsCacheTTL)
	}
	return credits, nil
}

// creditsEndpoint 积分端点（测试可注入本地 httptest 地址）。
func (r *Runtime) creditsEndpoint(realm Realm) string {
	if r != nil && r.creditsURL != nil {
		return r.creditsURL(realm)
	}
	return CreditsEndpoint(realm)
}

// Cooldowns 返回渠道当前的模型级冷却表（面板展示）。只含未过期项；无数据返回 nil。
func (r *Runtime) Cooldowns(p *model.Provider) map[string]time.Time {
	if r == nil || r.Op == nil || !IsWorkBuddy(p) {
		return nil
	}
	db := r.db()
	if db == nil {
		return nil
	}
	state, err := db.GetWorkbuddyChannelState(p.UserID, p.ID)
	if err != nil || state == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	return activeCooldowns(ctx, r.counters(), r.prefix(), p.ID, state, r.now())
}

// ---- 冷却 / 计数辅助 ----

// cooldownKey 模型级冷却键：workbuddy:cd:{providerID}:{model}。
func cooldownKey(prefix string, providerID int64, upModel string) string {
	return fmt.Sprintf("%s:cd:%d:%s", prefix, providerID, upModel)
}

// tokensKey 当日 token 实时计数键：workbuddy:tokens:{providerID}:{YYYY-MM-DD}。
func tokensKey(prefix string, providerID int64, date string) string {
	return fmt.Sprintf("%s:tokens:%d:%s", prefix, providerID, date)
}

// creditsKey 积分缓存键：workbuddy:credits:{providerID}。
func creditsKey(prefix string, providerID int64) string {
	return fmt.Sprintf("%s:credits:%d", prefix, providerID)
}

// resolveCooldown 取模型级冷却截止时间：Redis 命中优先，miss/无 Redis 回落运行态 map。
func resolveCooldown(ctx context.Context, cs counterStore, prefix string, providerID int64, upModel string, state *model.WorkbuddyChannelState) (time.Time, bool) {
	if cs != nil {
		if raw, ok := cs.Get(ctx, cooldownKey(prefix, providerID, upModel)); ok {
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
				return t, true
			}
		}
	}
	if state != nil {
		if t, ok := state.Cooldowns[upModel]; ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// withCooldown 返回运行态副本（不改原对象），把某个模型的冷却解析结果合并进 map。
func withCooldown(state *model.WorkbuddyChannelState, upModel string, until time.Time) *model.WorkbuddyChannelState {
	if state == nil {
		return nil
	}
	cp := *state
	cooldowns := make(map[string]time.Time, len(state.Cooldowns)+1)
	for k, v := range state.Cooldowns {
		cooldowns[k] = v
	}
	cooldowns[upModel] = until
	cp.Cooldowns = cooldowns
	return &cp
}

// pruneCooldowns 删除已过期（<=now）的冷却项。
func pruneCooldowns(state *model.WorkbuddyChannelState, now time.Time) {
	if state == nil || len(state.Cooldowns) == 0 {
		return
	}
	for k, until := range state.Cooldowns {
		if !until.After(now) {
			delete(state.Cooldowns, k)
		}
	}
}

// applyCooldown 写入一条冷却（写前清理过期项，防止 jsonb 无限增长）。
func applyCooldown(state *model.WorkbuddyChannelState, upModel string, until, now time.Time) {
	if state == nil {
		return
	}
	pruneCooldowns(state, now)
	if state.Cooldowns == nil {
		state.Cooldowns = map[string]time.Time{}
	}
	state.Cooldowns[upModel] = until
}

// deleteCooldown 删除一条冷却（先清理过期项）。
func deleteCooldown(state *model.WorkbuddyChannelState, upModel string, now time.Time) {
	if state == nil {
		return
	}
	pruneCooldowns(state, now)
	delete(state.Cooldowns, upModel)
}

// activeCooldowns 合并运行态 map 与 Redis（Redis 更新则覆盖），只保留未过期项。
func activeCooldowns(ctx context.Context, cs counterStore, prefix string, providerID int64, state *model.WorkbuddyChannelState, now time.Time) map[string]time.Time {
	if state == nil {
		return nil
	}
	out := make(map[string]time.Time, len(state.Cooldowns))
	for k, until := range state.Cooldowns {
		if until.After(now) {
			out[k] = until
		}
	}
	if cs != nil {
		for k := range out {
			if raw, ok := cs.Get(ctx, cooldownKey(prefix, providerID, k)); ok {
				if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil && t.After(now) {
					out[k] = t
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// todayTokensFrom 读 Redis 当日 token 计数；miss/解析失败返回 ok=false。
func todayTokensFrom(ctx context.Context, cs counterStore, prefix string, providerID int64, date string) (int64, bool) {
	if cs == nil {
		return 0, false
	}
	raw, ok := cs.Get(ctx, tokensKey(prefix, providerID, date))
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// refreshTokensCounter 把 Redis 当日计数刷新为「基线 + 本次增量」，
// TTL = 本地当日结束 + 1h 余量（跨天自动失效，无需清理任务）。
func refreshTokensCounter(ctx context.Context, cs counterStore, prefix string, providerID int64, today string, state *model.WorkbuddyChannelState, delta int64, now time.Time) {
	if cs == nil || delta <= 0 {
		return
	}
	base := int64(0)
	if v, ok := todayTokensFrom(ctx, cs, prefix, providerID, today); ok {
		base = v
	} else if state != nil && state.TokensDate == today {
		base = state.DailyTokens
	}
	ttl := nextLocalDayStart(now).Add(time.Hour).Sub(now)
	if ttl <= 0 {
		ttl = time.Hour
	}
	cs.Set(ctx, tokensKey(prefix, providerID, today), strconv.FormatInt(base+delta, 10), ttl)
}

// ---- 本地日窗口 ----

// localDay 用户本地时区的日期串（YYYY-MM-DD）。
func localDay(t time.Time) string { return t.Format("2006-01-02") }

// localDayStart 本地当日 00:00:00。
func localDayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// nextLocalDayStart 本地次日 00:00:00（跨月/跨年由 AddDate 自然处理）。
func nextLocalDayStart(t time.Time) time.Time { return localDayStart(t).AddDate(0, 0, 1) }

// ---- 积分响应解析 ----

// creditValueKeys 积分候选字段（优先级从高到低，大小写不敏感）。
var creditValueKeys = []string{
	"remaining", "remain", "balance", "credits", "totalRemaining", "available", "left",
}

// parseCredits 从响应体里提取积分余额。
//
// data 内优先于顶层；字段优先级见 creditValueKeys；接受 JSON number 或数字字符串。
// 失败时只报「见过的键名」（不含值），供 P0 探针回填键列表。
func parseCredits(body []byte) (float64, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return 0, fmt.Errorf("workbuddy: credits response is not a JSON object: %w", err)
	}
	if v, ok := pickCreditValue(root); ok {
		return v, nil
	}
	return 0, fmt.Errorf("积分字段未识别: data keys = [%s]", strings.Join(creditKeyNames(root), ", "))
}

// pickCreditValue 先在 data 对象内找，再在顶层找。
func pickCreditValue(root map[string]any) (float64, bool) {
	if d, ok := dataObject(root); ok {
		if v, ok := pickCreditFields(d); ok {
			return v, true
		}
	}
	return pickCreditFields(root)
}

// pickCreditFields 按 creditValueKeys 的优先级取第一个可解析字段。
func pickCreditFields(m map[string]any) (float64, bool) {
	if m == nil {
		return 0, false
	}
	for _, want := range creditValueKeys {
		for k, v := range m {
			if strings.EqualFold(k, want) {
				if f, ok := toFloat(v); ok {
					return f, true
				}
			}
		}
	}
	return 0, false
}

// dataObject 大小写不敏感地取 root["data"]，且必须是对象。
func dataObject(root map[string]any) (map[string]any, bool) {
	for k, v := range root {
		if strings.EqualFold(k, "data") {
			m, ok := v.(map[string]any)
			return m, ok
		}
	}
	return nil, false
}

// creditKeyNames 收集顶层与 data 对象的键名（去重排序，只有键名没有值）。
func creditKeyNames(root map[string]any) []string {
	set := make(map[string]struct{}, len(root)+4)
	for k := range root {
		set[k] = struct{}{}
	}
	if d, ok := dataObject(root); ok {
		for k := range d {
			set[k] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// toFloat 把 JSON number / 数字字符串转成 float64。
func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

// upstreamErrSummary 从上游错误体里提取结构化摘要（error.message / error.code / msg），
// 截断到 maxProbeErrLen。非 JSON 或无可识别字段时返回空串 —— 绝不回显原始正文。
func upstreamErrSummary(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Error struct {
			Message any    `json:"message"`
			Msg     string `json:"msg"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Code    any    `json:"code"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	msg := toText(probe.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(probe.Error.Msg)
	}
	if msg == "" {
		msg = strings.TrimSpace(probe.Message)
	}
	if msg == "" {
		msg = strings.TrimSpace(probe.Msg)
	}
	code := toText(probe.Error.Code)
	if code == "" {
		code = toText(probe.Code)
	}
	switch {
	case msg == "" && code == "":
		return ""
	case code == "":
		return truncateRunes(msg, maxProbeErrLen)
	case msg == "":
		return truncateRunes("code="+code, maxProbeErrLen)
	default:
		return truncateRunes(msg+" (code="+code+")", maxProbeErrLen)
	}
}

// toText 把 JSON 里的 string/number/bool 转成字符串（对象/数组不取，避免回显）。
func toText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// truncateRunes 按 rune 截断（不切断多字节字符）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
