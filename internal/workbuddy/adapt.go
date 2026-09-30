package workbuddy

import (
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
