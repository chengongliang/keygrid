package workbuddy

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/chengongliang/keygrid/internal/model"
)

// adapt.go 给 relay 用的「一次调用拿到 target + body + 形态」入口。
//
// relay 的 openai 上游 default 分支只做两件事：把 pivot 换成上游模型名
// （内核的 buildUpstreamBody），然后调用本文件的 AdaptRequest。

// Plan 描述本次上游调用与客户端期望之间的差异。
type Plan struct {
	// ForceStream 上游要求 stream:true（G3；wb_proxy.py:3025）。默认 true，
	// 由 P0 探针的 A3 结论确认；可用 WORKBUDDY_FORCE_STREAM=0 或
	// SetForceStream(false) 关掉。
	ForceStream bool
	// Aggregate 客户端要非流式，但上游只能流式 → 需要 CollapseSSE 本地聚合。
	Aggregate bool
}

// 脱敏档位（settings.scrub_mode 的合法取值；实现见 P5）。
//
// off 完全关闭；shadow 只统计"哪些请求会被改写"但不改内容（先观察再启用，
// 避免误伤正常对话）；on 才真正改写并从审计里留痕。
const (
	ScrubOff    = "off"
	ScrubShadow = "shadow"
	ScrubOn     = "on"
)

// 环境开关（读取时机由 sync.Once 收敛；conf 可用 SetXxx 覆盖）。
var (
	envOnce      sync.Once
	forceStream  = true  // WORKBUDDY_FORCE_STREAM，默认 1
	enabled      = true  // WORKBUDDY_ENABLED，默认 1（0 = 紧急回滚：连转发适配也不生效）
	jobsEnabled  = false // WORKBUDDY_JOBS，默认 0（自动化任务总开关，P3/P4 使用）
	fingerprint  = false // WORKBUDDY_FINGERPRINT，默认 0（设备指纹头，P5 使用）
	overrideMu   sync.Mutex
	overrides    = map[string]bool{}
	envLoadedFor = ""
)

func loadEnv() {
	enabled = envBool("WORKBUDDY_ENABLED", true)
	forceStream = envBool("WORKBUDDY_FORCE_STREAM", true)
	jobsEnabled = envBool("WORKBUDDY_JOBS", false)
	fingerprint = envBool("WORKBUDDY_FINGERPRINT", false)
}

func envBool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if v, err := strconv.Atoi(raw); err == nil {
		return v != 0
	}
	return def
}

// Enabled 转发适配总开关（WORKBUDDY_ENABLED，默认开）。
//
// 关掉时 relay 不再走本包的端点/请求归一化/身分头，回落到内核默认行为 ——
// 用于线上紧急回滚。注意：默认开是刻意的 —— 对 codebuddy-cn 渠道来说，
// 内核默认拼的 base_url+"/v1/chat/completions" 是错的（G1），本包的适配
// 才是正确行为，不能让它默认关着。
func Enabled() bool {
	envOnce.Do(loadEnv)
	if v, ok := getOverride("enabled"); ok {
		return v
	}
	return enabled
}

// ForceStream 上游是否强制流式（WORKBUDDY_FORCE_STREAM，默认开）。
func ForceStream() bool {
	envOnce.Do(loadEnv)
	if v, ok := getOverride("force_stream"); ok {
		return v
	}
	return forceStream
}

// JobsEnabled 自动化任务总开关（WORKBUDDY_JOBS，默认关；P3/P4 使用）。
func JobsEnabled() bool {
	envOnce.Do(loadEnv)
	if v, ok := getOverride("jobs"); ok {
		return v
	}
	return jobsEnabled
}

// Fingerprint 是否默认附带设备指纹头（WORKBUDDY_FINGERPRINT，默认关）。
func Fingerprint() bool {
	envOnce.Do(loadEnv)
	if v, ok := getOverride("fingerprint"); ok {
		return v
	}
	return fingerprint
}

// SetForceStream / SetEnabled / SetJobsEnabled / SetFingerprint 供 conf 注入与测试覆盖。
func SetForceStream(v bool) { setOverride("force_stream", v) }
func SetEnabled(v bool)     { setOverride("enabled", v) }
func SetJobsEnabled(v bool) { setOverride("jobs", v) }
func SetFingerprint(v bool) { setOverride("fingerprint", v) }

func setOverride(key string, v bool) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	overrides[key] = v
}

func getOverride(key string) (bool, bool) {
	overrideMu.Lock()
	defer overrideMu.Unlock()
	v, ok := overrides[key]
	return v, ok
}

// AdaptRequest 计算 chat 上游的 target、请求体与调用形态。
//
// pivot 是内核已按上游模型名替换过的请求体（chat/completions 形态）；
// clientStream 是客户端是否要求流式。返回的 body 已按上游要求归一化。
func AdaptRequest(p *model.Provider, pivot []byte, upModel string, clientStream bool) ([]byte, string, Plan, error) {
	target, err := Endpoint(p)
	if err != nil {
		return nil, "", Plan{}, err
	}
	plan := Plan{ForceStream: ForceStream()}
	body, err := NormalizeRequest(p, upModel, pivot, plan.ForceStream)
	if err != nil {
		return nil, "", Plan{}, err
	}
	plan.Aggregate = plan.ForceStream && !clientStream
	return body, target, plan, nil
}

// HeaderOptFor 构造默认身分头选项（P5 起接入用户配置的设备指纹开关）。
func HeaderOptFor() HeaderOpt {
	return HeaderOpt{Product: DefaultProduct(), Fingerprint: Fingerprint()}
}

// scrubLogOnce 用户设置读取失败只告警一次：转发热路径不能因同一故障反复刷日志。
var scrubLogOnce sync.Once

// ScrubModeFor 返回该渠道当前生效的脱敏档位（用户设置优先，未设置跟随 WORKBUDDY_SCRUB，默认 on）。
//
// 判定顺序：用户 settings.scrub_mode（非空）→ WORKBUDDY_SCRUB 环境默认 → **on**。
//
// 为什么默认 on：参考实现（wb_proxy.py 的 build_upstream_body）**无条件**对每次出站
// 请求做 sanitize_messages；它不是"可选增强"，而是 Claude Code / Codex CLI 这类客户端
// 能用的前提。实测（2026-10-12，真实账号）：把 Claude Code 的 system 指纹原样发上去，
// 上游直接 400 `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`，
// 同一条请求体只脱敏指纹片段后立刻 200。写 off 是显式放弃这条保护（用户自担）。
//
// 任何读失败都回落到环境默认（与 Admit 同一 fail-open 原则：状态读不出来不能让转发
// 受影响）。nil runtime / nil provider 一律 off（没有渠道就无从判定）。
func (r *Runtime) ScrubModeFor(p *model.Provider) string {
	if r == nil || r.Op == nil || p == nil {
		return ScrubOff
	}
	envDefault := scrubEnvDefault()
	db := r.db()
	if db == nil {
		return envDefault
	}
	settings, err := db.GetWorkbuddySetting(p.UserID)
	if err != nil {
		scrubLogOnce.Do(func() {
			log.Printf("[workbuddy] scrub settings read failed provider=%d: %v (fallback env=%s)", p.ID, err, envDefault)
		})
		return envDefault
	}
	// 未设置 / 空串都表示"用户没表态"→ 跟随环境默认；未知值由 ScrubModeOf 归一到 off。
	if settings == nil || strings.TrimSpace(settings.ScrubMode) == "" {
		return envDefault
	}
	return ScrubModeOf(settings.ScrubMode)
}

// scrubEnvDefault 环境变量给出的默认脱敏档位；未设置（或取值非法）时为 on。
//
// 单独抽出来是为了让「默认 on」只有一处定义：测试、文档、.env.example 都指向它。
func scrubEnvDefault() string {
	raw := strings.TrimSpace(os.Getenv("WORKBUDDY_SCRUB"))
	if raw == "" {
		return ScrubOn
	}
	return ScrubModeOf(raw)
}
