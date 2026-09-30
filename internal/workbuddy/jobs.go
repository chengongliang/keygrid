package workbuddy

// jobs.go P3 运营自动化任务框架（设计 §3.7）。
//
// 职责边界：本文件只做「任务编排」——注册表、per-user 展开、幂等锁、错峰/并发、
// 超时、kill switch、审计落库。具体上游调用在三个 P3 任务里：
//
//	checkin     CN 每日签到（POST {billing}/v2/billing/meter/daily-checkin {}）
//	credits     积分刷新（复用 Runtime.ProbeCredits）
//	daily_chat  INTL 每日活跃打卡（POST {chat}/v2/chat/completions，短 prompt + 低耗模型）
//
// 红线（与 state.go 一致）：
//  1. 不落内容：任何进入 Message / 日志 / 审计的字符串只能是状态、计数、耗时、
//     结构化上游错误码，绝不包含 prompt、响应正文、token 或积分数值。
//  2. 硬隔离：展开渠道一律走 op.ListProviders(userID)，绝不跨用户取样。
//  3. 失败不污染熔断：任务失败不调用 Observe（不写冷却、不累 token）。
//  4. 可缺省：本包之外（relay）不感知任务框架；kill switch 关闭时调度零副作用。
//
// 与 §3.7 的两处刻意取舍（在此写明原因，避免后来者当 bug 修）：
//   - Tick 在 kill switch 关闭时只打一行日志、不写 job_run 行：调度频率是分钟级，
//     若每次 tick 都落一行 skipped，会把执行日志表淹没；开关状态在平台设置里可查。
//   - 幂等锁 TTL 到本地次日 00:00（整日窗口），而不是设计草稿里的「精确到分钟」：
//     P3 三个任务都是「每日一次」语义，日窗口才是真实的幂等边界；锁一旦拿到，
//     除运行态/凭据读取失败外一律保留（失败也不重试，避免把失败上游按 tick 打爆）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chengongliang/keygrid/internal/crypto"
	"github.com/chengongliang/keygrid/internal/httpx"
	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/op"

	"github.com/redis/go-redis/v9"
)

// 任务状态（RunResult.Status / WorkbuddyJobRun.Status）。
const (
	jobStatusOK      = "ok"
	jobStatusSkipped = "skipped"
	jobStatusFailed  = "failed"
	jobStatusParked  = "parked"

	// jobMsgAlreadyRan 幂等锁命中时的固定摘要（Tick 用它区分「真实执行」与「跳过」）。
	jobMsgAlreadyRan = "已在本次窗口执行"

	// defaultJobTimeout 单次任务默认超时（P4 的 ACP 会覆盖为 120s）。
	defaultJobTimeout = 60 * time.Second
	// jobUpstreamTimeout 任务出口 client 的 HTTP 超时；单次执行的真实上限由
	// spec.Timeout（ctx）控制，这里只兜住「连接建立后上游不响应」的极端情况。
	jobUpstreamTimeout = 5 * time.Minute
	// acpJobTimeout 网页通道 ACP 打卡单轮上限：建会话 → 连沙箱 → 跑完一轮，
	// 比普通任务慢得多（设计文档 §3.7 规定 120s）。
	acpJobTimeout = 120 * time.Second
	// maxJobMessageLen Message 长度上限（WorkbuddyJobRun.Message 列宽 512）。
	maxJobMessageLen = 512
	// maxJobBodyBytes 读取上游响应体的上限（P3 响应都很小，防御异常大正文）。
	maxJobBodyBytes = 1 << 20
	// jobConcurrency Tick 内的并发上限。
	jobConcurrency = 4
	// jobJitterMax 同一 Tick 内同名任务多账号之间的错峰抖动上限。
	jobJitterMax = 30 * time.Second
	// jobCreditsErrLen 积分任务失败摘要长度上限。
	jobCreditsErrLen = 200

	// dailyChat 任务的固定参数（最小 OpenAI chat 请求）。
	dailyChatPrompt    = "你好"
	dailyChatMaxTokens = 16
	dailyChatModelEnv  = "WORKBUDDY_DAILY_MODEL"
)

// errJobsDisabled kill switch 命中时 RunJobs 返回的硬错误（手动入口据此回 501/403）。
var errJobsDisabled = errors.New("workbuddy 自动化任务已关闭（平台设置 workbuddy_jobs_enabled 或环境变量 WORKBUDDY_JOBS）")

// jobKeyOrder 已知任务键（顺序即面板顺序）：P3 三个；P4 的 growth/night/cat/
// daily_chat_web 不在此列，展开时自然被忽略。
var jobKeyOrder = []string{
	"checkin", "credits", "daily_chat",
	// P4 高风险任务：默认全关，逐任务开关 + 平台总闸 + 页面二次确认三重把关。
	"growth", "night", "cat", "daily_chat_web",
}

// JobKeys 返回已知任务键（顺序即面板顺序）。返回副本，调用方改动不影响注册表。
func JobKeys() []string { return append([]string(nil), jobKeyOrder...) }

// jobHandler 任务执行体：返回状态（jobStatus*）与状态摘要（只允许元数据）。
type jobHandler func(ctx context.Context, jc *JobContext) (status string, message string)

// jobSpec 任务描述。upModel 供 Admit 做渠道停用判定（checkin/credits 无模型，留空）。
type jobSpec struct {
	Key     string
	Timeout time.Duration
	UpModel func(p *model.Provider) string
	Handler jobHandler
}

// jobRegistry 任务注册表。P4 任务在此追加即可（Tick 只消费 jobKeyOrder）。
var jobRegistry = map[string]jobSpec{
	"checkin": {
		Key:     "checkin",
		Timeout: defaultJobTimeout,
		Handler: runCheckinJob,
	},
	"credits": {
		Key:     "credits",
		Timeout: defaultJobTimeout,
		Handler: runCreditsJob,
	},
	"daily_chat": {
		Key:     "daily_chat",
		Timeout: defaultJobTimeout,
		UpModel: dailyChatModel,
		Handler: runDailyChatJob,
	},
	// P4：CN 成长/夜猫子/猫猫旅行 + INTL 网页通道 ACP 打卡（tasks_cn.go / webagent.go）
	"growth": {
		Key:     "growth",
		Timeout: defaultJobTimeout,
		Handler: runGrowthJob,
	},
	"night": {
		Key:     "night",
		Timeout: defaultJobTimeout,
		Handler: runNightJob,
	},
	"cat": {
		Key:     "cat",
		Timeout: defaultJobTimeout,
		Handler: runCatJob,
	},
	"daily_chat_web": {
		Key: "daily_chat_web",
		// ACP 单轮要建会话 + 连沙箱 + 跑完一轮，60s 不够（设计文档 §3.7 的 120s）
		Timeout: acpJobTimeout,
		Handler: runDailyChatWebJob,
	},
}

// jobSpecFor 按 key 取任务描述。
func jobSpecFor(key string) (jobSpec, bool) {
	spec, ok := jobRegistry[strings.TrimSpace(key)]
	return spec, ok
}

// ---- 测试接缝（package-level，测试可替换；生产等价于直接调用） ----

var (
	// sleep 错峰抖动用的睡眠（测试置空，避免真实等待）。
	sleep = time.Sleep
	// checkinURL 签到端点构造（测试指向 httptest）。
	checkinURL = CheckinEndpoint
	// dailyChatURL 聊天端点构造（测试指向 httptest）。
	dailyChatURL = Endpoint
	// jobTokenDecoder 凭据解密（测试注入固定 TokenSet，避免依赖 crypto 主密钥）。
	jobTokenDecoder = defaultJobTokenDecoder
	// jobTimeoutOverride >0 时覆盖单次任务超时（测试用短超时验证超时路径）。
	jobTimeoutOverride time.Duration
)

// jobNoRedisWarn 保证「无 Redis」告警整个进程只打一次（不随每次 tick 刷屏）。
var jobNoRedisWarn sync.Once

// lockStore 幂等锁的最小接口。
//
// 抽成接口的原因与 counterStore/stateStore 一致：生产是 Redis SETNX + TTL，
// 测试可注入内存假实现，让 Redis 分支与「锁被占用」分支在没有 Redis 时也被覆盖。
type lockStore interface {
	// Acquire 条件写入：true = 抢到锁，false = 已被占用。err 非 nil 视为故障。
	Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Release 删除锁（仅在「未产生真实执行」的路径上调用，见 runForProvider）。
	Release(ctx context.Context, key string)
}

// redisLocker lockStore 的 go-redis 实现（SetNX + TTL）。
type redisLocker struct{ rdb *redis.Client }

func (l redisLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if l.rdb == nil {
		return true, nil
	}
	return l.rdb.SetNX(ctx, key, "1", ttl).Result()
}

func (l redisLocker) Release(ctx context.Context, key string) {
	if l.rdb == nil {
		return
	}
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := l.rdb.Del(delCtx, key).Err(); err != nil {
		log.Printf("[workbuddy] job lock release failed key=%s: %v", key, err)
	}
}

// jobsStore 任务框架需要的持久化能力（*op.Op 满足）。
//
// 与 stateStore 分开定义是有意的：本文件只依赖任务相关方法，编译期断言能立刻
// 暴露 op 层签名漂移。测试通过 Runtime.dbs（stateStore）注入实现两个接口的假 DB。
type jobsStore interface {
	GetWorkbuddySetting(userID int64) (*model.WorkbuddySetting, error)
	ListWorkbuddySettingsWithJobs() ([]model.WorkbuddySetting, error)
	ListProviders(userID int64) ([]model.Provider, error)
	GetWorkbuddyChannelState(userID, providerID int64) (*model.WorkbuddyChannelState, error)
	GetCredentialByProviderID(providerID int64) (*model.Credential, error)
	InsertWorkbuddyJobRun(r *model.WorkbuddyJobRun) error
	GetSetting(key string) (string, error)
	CreateAuditLog(l *model.AuditLog)
	// ProxyURL 平台出口代理（空串 = 未配置）。任务出口必须与转发一致，
	// 否则同一账号会出现「聊天走代理、打卡走直连」两种出口 IP。
	ProxyURL() (string, error)
}

// 编译期断言：op 层签名变化会在这里立刻暴露。
var _ jobsStore = (*op.Op)(nil)

// JobContext 一次任务执行所需的全部依赖（不暴露 Runtime 之外的内部状态，
// 避免任务实现乱用 Runtime 的私有字段）。
type JobContext struct {
	Op       *op.Op
	Client   *http.Client
	Now      time.Time
	Provider *model.Provider
	Setting  *model.WorkbuddySetting
	Tok      *oauth.TokenSet
	State    *model.WorkbuddyChannelState
	Runtime  *Runtime
}

// httpClient 返回任务 HTTP 客户端（缺省用默认客户端）。
func (jc *JobContext) httpClient() *http.Client {
	if jc != nil && jc.Client != nil {
		return jc.Client
	}
	return http.DefaultClient
}

// RunResult 一次执行的结果（回传前端与写审计）。
type RunResult struct {
	// json tag 必须与 web/src/lib/api.ts 的 WorkbuddyRunItemResult 一致
	// （前端按 provider_id/status/message 取值，缺 tag 会写成大写字段名而丢显示）。
	ProviderID int64  `json:"provider_id"`
	Status     string `json:"status"`  // ok | skipped | failed | parked
	Message    string `json:"message"` // 只允许状态摘要（≤512 rune，绝不含正文/凭据）
}

// JobsAllowed 平台级任务总闸。
//
// 优先级：平台设置 workbuddy_jobs_enabled > 环境变量 WORKBUDDY_JOBS（默认关）。
// 任何读取错误一律按关闭处理（保守）——kill switch 失效比误开危险。
// Runtime.Op/测试注入的 dbs 都为 nil 时只跟 env。
func (r *Runtime) JobsAllowed() bool {
	env := JobsEnabled()
	if r == nil {
		return env
	}
	store := r.jobStore()
	if store == nil {
		return env
	}
	raw, err := store.GetSetting(model.SettingWorkbuddyJobs)
	if err != nil {
		log.Printf("[workbuddy] read jobs kill switch failed: %v (treated as disabled)", err)
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return env // 未设置：跟随环境变量
	default:
		return false // 无法识别的取值：保守关闭
	}
}

// jobStore 返回持久化实现（测试注入的假 DB 优先；否则 *op.Op）。
func (r *Runtime) jobStore() jobsStore {
	if r == nil {
		return nil
	}
	if r.dbs != nil {
		if js, ok := r.dbs.(jobsStore); ok {
			return js
		}
	}
	if r.Op == nil {
		return nil
	}
	return r.Op
}

// locks 返回幂等锁实现（测试注入的假锁优先；无 Redis 返回 nil）。
func (r *Runtime) locks() lockStore {
	if r == nil {
		return nil
	}
	if r.cs != nil {
		if ls, ok := r.cs.(lockStore); ok {
			return ls
		}
	}
	if r.RDB != nil {
		return redisLocker{rdb: r.RDB}
	}
	return nil
}

// RunJobs 手动/调度统一入口：providerID==0 表示该用户所有启用的 workbuddy 渠道。
// jobKey 必须属于 JobKeys()。trigger: "manual" | "schedule"。
//
// 返回的 error 只用于「整体不可执行」（未知任务键、kill switch、数据层不可用、
// 指定渠道不属于该用户）；单渠道执行结果一律在 []RunResult 里，不中断同批其余渠道。
func (r *Runtime) RunJobs(ctx context.Context, userID int64, jobKey string, providerID int64, trigger string) ([]RunResult, error) {
	return r.runJobsAt(ctx, userID, jobKey, providerID, trigger, r.now())
}

// runJobsAt RunJobs 的内部实现：now 允许外部（Tick）注入，保证槽位与抖动的时间基准一致。
func (r *Runtime) runJobsAt(ctx context.Context, userID int64, jobKey string, providerID int64, trigger string, now time.Time) ([]RunResult, error) {
	if r == nil {
		return nil, errors.New("workbuddy: runtime is nil")
	}
	spec, ok := jobSpecFor(jobKey)
	if !ok {
		return nil, fmt.Errorf("workbuddy: unknown job key %q", jobKey)
	}
	if !r.JobsAllowed() {
		return nil, errJobsDisabled
	}
	store := r.jobStore()
	if store == nil {
		return nil, errors.New("workbuddy: job store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = r.now()
	}

	setting, err := store.GetWorkbuddySetting(userID)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: read job settings for user %d: %w", userID, err)
	}
	providers, err := r.providersFor(store, userID, providerID)
	if err != nil {
		return nil, err
	}

	results := make([]RunResult, 0, len(providers))
	for i := range providers {
		res, _ := r.runForProvider(ctx, spec, store, setting, &providers[i], trigger, now)
		results = append(results, res)
	}
	return results, nil
}

// Tick 调度一次：枚举「已启用自动化且有 workbuddy 渠道」的用户 × 启用的任务，
// 对每个（用户, 任务, 渠道）执行一次。返回实际产生执行记录的任务数。
//
// kill switch（平台设置 + env）关闭时直接返回 (0, nil)，只打一行日志、不落库。
func (r *Runtime) Tick(ctx context.Context, now time.Time) (int, error) {
	if r == nil {
		return 0, errors.New("workbuddy: runtime is nil")
	}
	if !r.JobsAllowed() {
		// 刻意不写 skipped 行：tick 是分钟级，逐 tick 落库会把执行日志淹没。
		log.Printf("[workbuddy] tick skipped: jobs disabled by kill switch")
		return 0, nil
	}
	store := r.jobStore()
	if store == nil {
		return 0, errors.New("workbuddy: job store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = r.now()
	}

	settings, err := store.ListWorkbuddySettingsWithJobs()
	if err != nil {
		return 0, fmt.Errorf("workbuddy: list users with jobs enabled: %w", err)
	}

	type jobTask struct {
		userID   int64
		jobKey   string
		provider model.Provider
	}

	var tasks []jobTask
	for i := range settings {
		s := settings[i]
		if !s.Enabled {
			continue // 用户级总开关：Tick 只处理已启用的账号（与 §3.7 一致）
		}
		providers, perr := r.providersFor(store, s.UserID, 0)
		if perr != nil {
			log.Printf("[workbuddy] tick list providers failed user=%d: %v", s.UserID, perr)
			continue
		}
		if len(providers) == 0 {
			continue
		}
		for _, key := range jobKeyOrder {
			if !s.JobsEnabled[key] {
				continue // 逐任务开关：P3 默认全关，只在用户显式打开时执行
			}
			for j := range providers {
				tasks = append(tasks, jobTask{
					userID:   s.UserID,
					jobKey:   key,
					provider: providers[j],
				})
			}
		}
	}
	if len(tasks) == 0 {
		return 0, nil
	}

	sem := make(chan struct{}, jobConcurrency)
	jitterSeen := make(map[string]int, len(jobKeyOrder))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		executed int
	)
	for i := range tasks {
		t := tasks[i]
		spec, ok := jobSpecFor(t.jobKey)
		if !ok {
			continue
		}
		// 同一 Tick 内同名任务的第 2 个及以后渠道加 0–30s 抖动（错峰，避免同秒并发）。
		mu.Lock()
		jitterSeen[t.jobKey]++
		delay := time.Duration(0)
		if jitterSeen[t.jobKey] > 1 {
			delay = jobJitter()
		}
		mu.Unlock()

		wg.Add(1)
		go func(t jobTask, spec jobSpec, delay time.Duration) {
			defer wg.Done()
			if delay > 0 {
				sleep(delay)
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			setting, serr := store.GetWorkbuddySetting(t.userID)
			if serr != nil {
				log.Printf("[workbuddy] tick read settings failed user=%d: %v", t.userID, serr)
				return
			}
			res, ran := r.runForProvider(ctx, spec, store, setting, &t.provider, "schedule", now)
			if ran {
				mu.Lock()
				executed++
				mu.Unlock()
			}
			_ = res
		}(t, spec, delay)
	}
	wg.Wait()
	return executed, nil
}

// providersFor 展开执行目标：providerID==0 时返回该用户所有启用的 workbuddy 渠道，
// 否则只返回该用户拥有的那一条（不存在 → 错误，保证不跨用户执行）。
func (r *Runtime) providersFor(store jobsStore, userID, providerID int64) ([]model.Provider, error) {
	all, err := store.ListProviders(userID)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: list providers for user %d: %w", userID, err)
	}
	out := make([]model.Provider, 0, len(all))
	for i := range all {
		p := all[i]
		if !IsWorkBuddy(&p) || !p.Enabled {
			continue
		}
		if providerID != 0 && p.ID != providerID {
			continue
		}
		out = append(out, p)
	}
	if providerID != 0 && len(out) == 0 {
		return nil, fmt.Errorf("workbuddy: provider %d not found among user %d enabled workbuddy channels", providerID, userID)
	}
	return out, nil
}

// runForProvider 单个（任务, 渠道）的执行核心，RunJobs 与 Tick 共用。
//
// 返回 ran = 是否产生了一条真实执行记录（幂等锁命中不算）。
// jobHTTPClient 任务出口客户端：与 relay/clientFor 同一套决策 ——
// 渠道未勾代理走默认 client；勾了代理则必须拿到平台代理地址，否则报错（绝不直连）。
// 这样同一账号的聊天与自动化任务从同一出口 IP 出去，不会互相暴露成两个来源。
func (r *Runtime) jobHTTPClient(store jobsStore, p *model.Provider) (*http.Client, error) {
	if p == nil || !p.UseProxy {
		return httpx.Client(jobUpstreamTimeout, "")
	}
	store = orJobStore(store, r)
	if store == nil {
		return nil, errors.New("channel requires proxy, but proxy source is not configured")
	}
	proxyURL, err := store.ProxyURL()
	if err != nil {
		return nil, fmt.Errorf("read proxy setting: %w", err)
	}
	if strings.TrimSpace(proxyURL) == "" {
		return nil, errors.New("channel requires proxy, but platform proxy is not configured")
	}
	return httpx.Client(jobUpstreamTimeout, proxyURL)
}

// orJobStore 优先用调用方传入的 store（测试注入），否则回落到 Runtime 自身的 op。
func orJobStore(store jobsStore, r *Runtime) jobsStore {
	if store != nil {
		return store
	}
	if r == nil {
		return nil
	}
	return r.jobStore()
}

func (r *Runtime) runForProvider(ctx context.Context, spec jobSpec, store jobsStore, setting *model.WorkbuddySetting, p *model.Provider, trigger string, now time.Time) (RunResult, bool) {
	res := RunResult{ProviderID: p.ID}

	if setting == nil || !setting.Enabled {
		res.Status = jobStatusSkipped
		res.Message = "用户总开关未启用"
		return res, false
	}

	// 幂等锁：同一 (任务, 用户, 渠道, 槽位) 当天只执行一次。手动与调度走同一键（防连点）。
	lockKey := jobLockKey(r.prefix(), spec.Key, p.UserID, p.ID, jobSlot(now))
	acquired, release, err := r.acquireJobLock(ctx, lockKey, jobLockTTL(now))
	if err != nil {
		res.Status = jobStatusFailed
		res.Message = "获取任务锁失败"
		return res, false
	}
	if !acquired {
		res.Status = jobStatusSkipped
		res.Message = jobMsgAlreadyRan
		return res, false
	}

	// 运行态与凭据：这两步失败不产生上游调用，释放锁以便修好后重试（同一 tick 周期内）。
	state, err := store.GetWorkbuddyChannelState(p.UserID, p.ID)
	if err != nil {
		release()
		log.Printf("[workbuddy] job state read failed provider=%d job=%s: %v", p.ID, spec.Key, err)
		res.Status = jobStatusFailed
		res.Message = "读取渠道运行态失败"
		return res, false
	}
	client, cerr := r.jobHTTPClient(store, p)
	if cerr != nil {
		release()
		// 渠道要求代理但平台未配置：与 relay 一致地失败，不静默直连。
		log.Printf("[workbuddy] job client build failed provider=%d job=%s: %v", p.ID, spec.Key, cerr)
		res.Status = jobStatusFailed
		res.Message = "出口代理不可用"
		return res, false
	}
	tok, err := loadJobToken(store, p)
	if err != nil {
		release()
		log.Printf("[workbuddy] job credential load failed provider=%d job=%s: %v", p.ID, spec.Key, err)
		res.Status = jobStatusFailed
		res.Message = "读取渠道凭据失败"
		return res, false
	}

	jc := &JobContext{
		Op:       r.Op,
		Client:   client,
		Now:      now,
		Provider: p,
		Setting:  setting,
		Tok:      tok,
		State:    state,
		Runtime:  r,
	}

	// 渠道停用判定（复用 Admit：只读不写；任务失败不会污染熔断状态）。
	upModel := ""
	if spec.UpModel != nil {
		upModel = spec.UpModel(p)
	}
	if parkErr := r.Admit(p, upModel); parkErr != nil {
		finished := r.now()
		res.Status = jobStatusParked
		res.Message = sanitizeJobMessage(parkErr.Error())
		r.recordJobRun(store, spec.Key, p, trigger, res, now, &finished)
		// 锁保留：停用条件通常当天不会自行解除（日限额），避免逐 tick 落库。
		return res, true
	}

	timeout := spec.Timeout
	if t := jobTimeoutOverride; t > 0 {
		timeout = t
	}
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := r.now()
	status, message := spec.Handler(runCtx, jc)
	finished := r.now()

	res.Status = normalizeJobStatus(status)
	res.Message = sanitizeJobMessage(message)
	r.recordJobRun(store, spec.Key, p, trigger, res, started, &finished)
	return res, true
}

// recordJobRun 双写执行日志与审计（互为佐证）。
func (r *Runtime) recordJobRun(store jobsStore, jobKey string, p *model.Provider, trigger string, res RunResult, started time.Time, finished *time.Time) {
	providerID := p.ID
	run := &model.WorkbuddyJobRun{
		UserID:     p.UserID,
		JobKey:     jobKey,
		ProviderID: &providerID,
		Trigger:    normalizeTrigger(trigger),
		Status:     res.Status,
		Message:    res.Message,
		StartedAt:  started,
		FinishedAt: finished,
	}
	if err := store.InsertWorkbuddyJobRun(run); err != nil {
		log.Printf("[workbuddy] job run insert failed provider=%d job=%s: %v", p.ID, jobKey, err)
	}
	// 审计只记 provider_id + status：不含 message，进一步降低内容泄漏面。
	store.CreateAuditLog(&model.AuditLog{
		UserID:    p.UserID,
		Event:     "workbuddy.job." + jobKey,
		Detail:    fmt.Sprintf("provider_id=%d status=%s", p.ID, res.Status),
		CreatedAt: r.now(),
	})
}

// acquireJobLock 抢幂等锁。返回 (是否可用, 释放函数, error)。
//
// 语义对齐 oauth/refresh.go:65：无 Redis = 单实例部署，直接放行并告警一次；
// Redis 故障 fail-open（锁是优化，不是正确性依赖）。错误只在极端情况返回，
// 当前实现里 Acquire 故障已 fail-open，故 error 恒为 nil（保留签名便于以后收紧）。
func (r *Runtime) acquireJobLock(ctx context.Context, key string, ttl time.Duration) (bool, func(), error) {
	ls := r.locks()
	if ls == nil {
		jobNoRedisWarn.Do(func() {
			log.Printf("[workbuddy] 未配置 Redis：任务幂等锁按单实例假设放行；多实例部署必须配置 Redis")
		})
		return true, func() {}, nil
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	ok, err := ls.Acquire(ctx, key, ttl)
	if err != nil {
		log.Printf("[workbuddy] job lock acquire failed key=%s: %v (fail-open)", key, err)
		return true, func() {}, nil
	}
	if !ok {
		return false, nil, nil
	}
	return true, func() { ls.Release(context.Background(), key) }, nil
}

// loadJobToken 读取并解密渠道 OAuth 凭据。
func loadJobToken(store jobsStore, p *model.Provider) (*oauth.TokenSet, error) {
	cred, err := store.GetCredentialByProviderID(p.ID)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: load credential for provider %d: %w", p.ID, err)
	}
	tok, err := jobTokenDecoder(cred)
	if err != nil {
		return nil, fmt.Errorf("workbuddy: decode credential for provider %d: %w", p.ID, err)
	}
	return tok, nil
}

// defaultJobTokenDecoder AES-GCM 解密凭据为 TokenSet（api_key 凭据无 access token）。
func defaultJobTokenDecoder(cred *model.Credential) (*oauth.TokenSet, error) {
	if cred == nil {
		return nil, errors.New("credential missing")
	}
	plain, err := crypto.Decrypt(cred.EncData)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var tok oauth.TokenSet
	if err := json.Unmarshal(plain, &tok); err != nil {
		return nil, fmt.Errorf("unmarshal token set: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, errors.New("credential has no oauth access token")
	}
	return &tok, nil
}

// ---- 任务实现 ----

// runCheckinJob CN 每日签到。
//
// 上游：POST {billing}/v2/billing/meter/daily-checkin，body `{}`。
// INTL 无此端点 → skipped（不发起任何请求）。2xx = ok；非 2xx = failed（结构化摘要）。
func runCheckinJob(ctx context.Context, jc *JobContext) (string, string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	if realm != RealmCN {
		return jobStatusSkipped, "国际版无签到端点"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}

	hdr, err := Headers(jc.Provider, jc.Tok, HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()})
	if err != nil {
		return jobStatusFailed, "构建签到请求头失败"
	}
	hdr.Set("Authorization", "Bearer "+jc.Tok.AccessToken)
	hdr.Set("Content-Type", "application/json")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinURL(realm), strings.NewReader("{}"))
	if err != nil {
		return jobStatusFailed, "构建签到请求失败"
	}
	req.Header = hdr

	resp, err := jc.httpClient().Do(req)
	if err != nil {
		return jobStatusFailed, jobTransportErrMessage(ctx, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return jobStatusFailed, "读取签到响应失败"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jobStatusFailed, jobUpstreamFailMessage("签到", resp.StatusCode, body)
	}

	// 2xx 一律算成功。上游若用布尔字段表达「今天已签到」，只影响摘要文案；
	// 拿不准时按「签到成功」记（对齐 §3.7：防御式解析，不确定就当成功）。
	code := businessCode(body)
	if checkinAlreadyDone(body) {
		return jobStatusOK, withBusinessCode("今日已签到", code)
	}
	return jobStatusOK, withBusinessCode("签到成功", code)
}

// runCreditsJob 积分刷新（复用 Runtime.ProbeCredits，其错误已是脱敏摘要）。
func runCreditsJob(ctx context.Context, jc *JobContext) (string, string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	// ProbeCredits 需要 Op 写回积分；未接线视为配置错误（不落上游调用）。
	if jc.Runtime.Op == nil {
		return jobStatusFailed, "运行态未接线（缺少 Op）"
	}
	if _, err := jc.Runtime.ProbeCredits(ctx, jc.httpClient(), jc.Provider, jc.Tok); err != nil {
		// ProbeCredits 的错误只含状态码与结构化上游字段，截断后直接落库。
		return jobStatusFailed, truncateRunes(err.Error(), jobCreditsErrLen)
	}
	// 数值刻意不进 Message：面板从 workbuddy_channel_state.credits 展示，日志只留状态。
	return jobStatusOK, "积分已刷新"
}

// runDailyChatJob INTL 每日活跃打卡（最小 OpenAI chat 请求 + 强制流式 + 本地折叠验证）。
//
// CN 的每日活跃/成长任务属 P4 growth → skipped。请求体不经 AdaptRequest（不需要
// pivot），但同样过 NormalizeRequest 以复用同一套上游归一化规则。
func runDailyChatJob(ctx context.Context, jc *JobContext) (string, string) {
	if jc == nil || jc.Provider == nil || jc.Runtime == nil {
		return jobStatusFailed, "任务上下文缺失"
	}
	realm, ok := RealmOf(jc.Provider)
	if !ok {
		return jobStatusFailed, "渠道不属于 WorkBuddy"
	}
	if realm != RealmIntl {
		return jobStatusSkipped, "国内版每日活跃任务见 P4 growth"
	}
	if jc.Tok == nil {
		return jobStatusFailed, "渠道凭据缺失"
	}

	modelName := dailyChatModel(jc.Provider)
	if modelName == "" {
		return jobStatusFailed, "模型目录为空，无法选择日常打卡模型"
	}
	normalized, err := NormalizeRequest(jc.Provider, modelName, buildDailyChatBody(modelName), true)
	if err != nil {
		return jobStatusFailed, "构建聊天请求体失败"
	}

	hdr, err := Headers(jc.Provider, jc.Tok, HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()})
	if err != nil {
		return jobStatusFailed, "构建聊天请求头失败"
	}
	hdr.Set("Authorization", "Bearer "+jc.Tok.AccessToken)
	hdr.Set("Content-Type", "application/json")

	target, err := dailyChatURL(jc.Provider)
	if err != nil {
		return jobStatusFailed, "构造聊天端点失败"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(normalized))
	if err != nil {
		return jobStatusFailed, "构建聊天请求失败"
	}
	req.Header = hdr

	started := time.Now()
	resp, err := jc.httpClient().Do(req)
	if err != nil {
		return jobStatusFailed, jobTransportErrMessage(ctx, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJobBodyBytes))
	if err != nil {
		return jobStatusFailed, "读取聊天响应失败"
	}
	elapsed := time.Since(started)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return jobStatusFailed, jobUpstreamFailMessage("每日打卡", resp.StatusCode, body)
	}
	segments := countSSESegments(body)
	if !dailyChatConfirmed(body) {
		return jobStatusFailed, "响应无可识别输出段"
	}
	// 只记段数与耗时，绝不记输出文本。
	return jobStatusOK, fmt.Sprintf("每日打卡完成 segments=%d elapsed=%s", segments, elapsed.Round(time.Millisecond))
}

// ---- 任务辅助 ----

// buildDailyChatBody 构造最小 chat 请求体（模型由调用方写入）。
func buildDailyChatBody(modelName string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":          modelName,
		"messages":       []map[string]string{{"role": "user", "content": dailyChatPrompt}},
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"max_tokens":     dailyChatMaxTokens,
	})
	return body
}

// premiumModelMarkers 日常打卡要避开的高倍率/高成本模型标记。
var premiumModelMarkers = []string{"premium", "pro", "opus", "ultra", "thinking"}

// dailyChatModel 选择日常打卡模型（P3 偏好规则）：
//  1. 环境变量 WORKBUDDY_DAILY_MODEL（显式指定，最高优先）；
//  2. 该区域内嵌快照里第一个「非 premium」条目（跳过 premium/pro/opus/ultra/thinking
//     标记的高倍率档）；快照保持上游目录顺序，flash 系通常排在最前；
//  3. 都取不到 → 空串（调用方记 failed，不猜测模型名）。
func dailyChatModel(p *model.Provider) string {
	if m := strings.TrimSpace(os.Getenv(dailyChatModelEnv)); m != "" {
		return m
	}
	realm, ok := RealmOf(p)
	if !ok {
		return ""
	}
	for _, id := range SnapshotModels(realm) {
		if isPremiumModel(id) {
			continue
		}
		return id
	}
	return ""
}

// isPremiumModel 是否命中高倍率模型标记（大小写不敏感）。
func isPremiumModel(id string) bool {
	low := strings.ToLower(id)
	for _, marker := range premiumModelMarkers {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// checkinDoneKeys 上游可能用来表达「今日已签到」的布尔字段（大小写不敏感，防御式）。
var checkinDoneKeys = []string{
	"checkedIn", "checked_in", "todayChecked", "today_checked",
	"signed", "hasCheckedIn", "has_checked_in", "alreadyChecked",
}

// checkinAlreadyDone 尽力识别「今天已经签过」的布尔字段；识别不出返回 false。
func checkinAlreadyDone(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	if anyTrueBool(root, checkinDoneKeys) {
		return true
	}
	if d, ok := dataObject(root); ok {
		return anyTrueBool(d, checkinDoneKeys)
	}
	return false
}

// anyTrueBool 在 map 中大小写不敏感地找值为 true 的布尔字段。
func anyTrueBool(m map[string]any, keys []string) bool {
	for _, want := range keys {
		for k, v := range m {
			if strings.EqualFold(k, want) {
				if b, ok := v.(bool); ok && b {
					return true
				}
			}
		}
	}
	return false
}

// businessCode 提取上游业务码（顶层 code → data.code），只回字面值不回显其它字段。
func businessCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	if c := toText(root["code"]); c != "" {
		return c
	}
	if d, ok := dataObject(root); ok {
		if c := toText(d["code"]); c != "" {
			return c
		}
	}
	return ""
}

// withBusinessCode 有业务码时拼接进摘要（≤512 由 sanitizeJobMessage 兜底）。
func withBusinessCode(msg, code string) string {
	if code == "" {
		return msg
	}
	return msg + "（code=" + code + "）"
}

// jobUpstreamFailMessage 非 2xx 的失败摘要：状态码 + 结构化上游字段（复用
// state.go 的 upstreamErrSummary，规则同 relay/reqerr.go，≤200 字，绝不落正文）。
func jobUpstreamFailMessage(prefix string, status int, body []byte) string {
	if summary := upstreamErrSummary(body); summary != "" {
		return fmt.Sprintf("%s失败: 上游返回 %d: %s", prefix, status, summary)
	}
	return fmt.Sprintf("%s失败: 上游返回 %d", prefix, status)
}

// jobTransportErrMessage 传输层错误摘要：超时/取消用固定文案，其余只带错误文本
// （客户端错误不含请求体；仍截断到 200 字）。
func jobTransportErrMessage(ctx context.Context, err error) string {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "任务超时"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "任务已取消"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "任务超时"
	}
	return "上游请求失败: " + truncateRunes(err.Error(), 200)
}

// countSSESegments 统计 SSE 数据段数（排除空行与 [DONE]），只用于摘要计数。
func countSSESegments(body []byte) int {
	n := 0
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		n++
	}
	return n
}

// dailyChatConfirmed 成功判定，刻意宽松（对齐 §3.7「be lenient」）：
//  1. 2xx 且 CollapseSSE 折叠出含 content 或 finish_reason 的 choice → 成功；
//  2. 折叠失败但确实观察到 data: 段（上游可能回非标准 SSE）→ 仍算成功；
//  3. 完全没有输出段 → 失败（无法证明这一轮跑完了）。
func dailyChatConfirmed(body []byte) bool {
	if collapsed, ok := CollapseSSE(body); ok {
		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(collapsed, &parsed) == nil {
			for _, ch := range parsed.Choices {
				if ch.Message.Content != "" || (ch.FinishReason != nil && *ch.FinishReason != "") {
					return true
				}
			}
		}
	}
	return countSSESegments(body) > 0
}

// normalizeJobStatus 收敛状态值；未知值按 failed 记（诚实反映「没成功」）。
func normalizeJobStatus(status string) string {
	switch strings.TrimSpace(status) {
	case jobStatusOK, jobStatusSkipped, jobStatusFailed, jobStatusParked:
		return status
	default:
		return jobStatusFailed
	}
}

// normalizeTrigger 收敛触发来源（面板/审计查询用）。
func normalizeTrigger(trigger string) string {
	if strings.EqualFold(strings.TrimSpace(trigger), "schedule") {
		return "schedule"
	}
	return "manual"
}

// sanitizeJobMessage 归一化摘要：压平空白（防多行）、按 rune 截断到 ≤512。
// 不做任何内容提取，只负责长度与形状；正文本就不该出现在这里。
func sanitizeJobMessage(message string) string {
	s := strings.Join(strings.Fields(message), " ")
	return truncateRunes(s, maxJobMessageLen)
}

// jobSlot 幂等槽位：P3 三个任务都是日粒度。
func jobSlot(now time.Time) string { return localDay(now) }

// jobLockKey 幂等锁键：{prefix}:job:{jobKey}:{userID}:{providerID}:{slot}。
func jobLockKey(prefix, jobKey string, userID, providerID int64, slot string) string {
	return fmt.Sprintf("%s:job:%s:%d:%d:%s", prefix, jobKey, userID, providerID, slot)
}

// jobLockTTL 锁存活到本地次日 00:00（跨天自动失效，无需清理任务）。
func jobLockTTL(now time.Time) time.Duration {
	ttl := nextLocalDayStart(now).Sub(now)
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return ttl
}

// jobJitter 0–jobJitterMax 的均匀抖动。
func jobJitter() time.Duration {
	if jobJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jobJitterMax) + 1))
}
