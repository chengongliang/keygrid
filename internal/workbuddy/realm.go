// Package workbuddy 承载 WorkBuddy（国际版 www.workbuddy.ai）与 CodeBuddy CN
// （国内版 copilot.tencent.com）两个区域的渠道适配与运营能力。
//
// 定位（详见 docs/workbuddy-integration-design.md）：
//   - 本项目**不是**通用插件框架。workbuddy 的全部业务逻辑收在本包，内核
//     （relay / task / router）只暴露少量薄接线点，形状与既有的
//     relay/codex.go、relay/xai.go 特化一致。
//   - 上游事实（端点、路径、UA、身分头）集中在本包，内核不感知。
//
// 边界规则（改这个包的人请遵守）：
//  1. 不写 SQL：只调用 op 层已带 WHERE user_id 的方法；缺什么先在 op/ 加。
//  2. 不碰明文：加解密只走 crypto，凭据载体是 oauth.TokenSet；不把 token、
//     积分、prompt、上游响应体写进日志。
//  3. 不做转发主循环：failover / 熔断 / 记账顺序归 relay；本包只回答
//     「怎么拼这一次请求」「怎么读这一次响应」。
//  4. 失败不阻断内核：任何函数返回 error → 该渠道 failover + 记 request_errors。
//  5. 可缺省：对非 workbuddy 渠道返回零值 / false，内核行为与今天完全一致。
//
// 待实测回填（P0 探针：scripts/probe_workbuddy.sh，记录见
// docs/workbuddy-upstream-notes.md）：
//   - A1 chat 上游路径是否为 /v2/chat/completions（本包已按此实现）
//   - A3 上游是否强制 stream:true（本包 Plan.ForceStream 默认 true）
//   - A2 最小身分头集（本包按 wb2a 的完整集实现）
package workbuddy

import (
	"errors"
	"os"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
	"github.com/chengongliang/keygrid/internal/workbuddy/upstream"
)

// realm.go 区域识别与端点常量 —— 本包唯一的上游事实来源。
//
// 对齐 wb2a：wb_identity.py:44-53（_ENDPOINTS）、wb_accounts.py:119-139
// （REALM_CONFIGS）、wb_proxy.py:141-142（CHAT_PATH / MODELS_PATH）、
// wb_proxy.py:1979（REMOTE_CONFIG_PATH）、wb_accounts.py:146-151。

// Realm 上游区域。
type Realm string

const (
	RealmIntl Realm = "intl" // 国际版 www.workbuddy.ai
	RealmCN   Realm = "cn"   // 国内版 copilot.tencent.com / codebuddy.cn
)

// ProviderKey 两种区域各自对应的 oauth_provider 取值（providers.oauth_provider）。
//
// 国内版复用 KeyGrid 既有的 codebuddy-cn 适配器（internal/oauth/providers/
// codebuddy.go），只补区域语义；国际版由本方案新增 workbuddy-intl 适配器。
const (
	ProviderIntl = "workbuddy-intl"
	ProviderCN   = "codebuddy-cn"
)

// 上游端点与路径常量。
const (
	intlChatBase    = "https://www.workbuddy.ai"
	cnChatBase      = upstream.CNChatBaseDefault
	intlBillingBase = "https://www.workbuddy.ai"
	cnBillingBase   = "https://www.codebuddy.cn"

	// ChatPath 聊天上游路径。★待 P0 实测确认（A1）★
	// 注意：这不是 base_url + "/v1/chat/completions"，relay 的默认拼接
	// （relay/route.go:84 upstreamTarget）对上游是错的 —— 见 G1。
	ChatPath = "/v2/chat/completions"

	// NarrowModelsPath 模型窄端点（目录回落的第二段）。
	NarrowModelsPath = "/v2/enterprises/personal/models"
	// RemoteConfigPath 全量模型目录（需桌面 UA，否则 400/12403）。
	RemoteConfigPath = "/v3/config"

	// AuthStatePath / AuthTokenPath / AuthRefreshPath 授权与续期。
	AuthStatePath   = "/v2/plugin/auth/state"
	AuthTokenPath   = "/v2/plugin/auth/token"
	AuthRefreshPath = "/v2/plugin/auth/token/refresh"

	// BillingPathCredits 积分查询；BillingPathCheckin 每日签到（仅国内版）。
	BillingPathCredits = "/v2/billing/meter/get-user-resource"
	BillingPathCheckin = "/v2/billing/meter/daily-checkin"

	// WebConversationsPath 国际版网页通道（ACP 打卡）会话入口。
	WebConversationsPath = "/console/as/conversations/"
)

// ErrNotWorkBuddy 渠道不属于本包管理的两个供应商。
var ErrNotWorkBuddy = errors.New("not a workbuddy/codebuddy channel")

// IsWorkBuddy 判定渠道是否由本包接管。
//
// 判定顺序：
//  1. oauth_provider 命中两个已知 key（权威）；
//  2. 否则看 base_url 主机是否落在已知域名（兼容用户手填渠道、以及
//     9router 移植过来的历史渠道）；
//  3. api_key 类型也允许命中第 2 条（用户可能用官方 token 直接配 api_key 渠道）。
func IsWorkBuddy(p *model.Provider) bool {
	if p == nil {
		return false
	}
	if _, ok := RealmOf(p); ok {
		return true
	}
	return false
}

// RealmOf 返回渠道所属区域；不属于本包时 ok=false。
func RealmOf(p *model.Provider) (Realm, bool) {
	if p == nil {
		return "", false
	}
	switch strings.TrimSpace(p.OAuthProvider) {
	case ProviderIntl:
		return RealmIntl, true
	case ProviderCN:
		return RealmCN, true
	}
	host := hostOf(p.BaseURL)
	switch {
	case host == "":
		return "", false
	case strings.HasSuffix(host, "workbuddy.ai"), strings.HasSuffix(host, "workbuddy.cn"):
		// workbuddy.cn 是国内版 VSC 身分的域名（wb_identity.py:57），仍属国内版。
		if strings.HasSuffix(host, "workbuddy.cn") {
			return RealmCN, true
		}
		return RealmIntl, true
	case host == "copilot.tencent.com", strings.HasSuffix(host, ".codebuddy.cn"), host == "codebuddy.cn":
		return RealmCN, true
	}
	return "", false
}

// ChatBase 返回区域的 chat 上游基址（不含路径）。
//
// 优先用渠道自填的 base_url（归一化后），未填则用区域默认值。归一化会剥掉
// 用户可能已经写进 base_url 的路径成分（历史预设是 https://copilot.tencent.com/v2，
// 若直接拼路径会得到 /v2/v2/chat/completions），保证幂等。
func ChatBase(p *model.Provider) (string, error) {
	realm, ok := RealmOf(p)
	if !ok {
		return "", ErrNotWorkBuddy
	}
	if base := normalizeBase(p.BaseURL); base != "" {
		if realm == RealmCN && base == cnChatBase {
			return defaultChatBase(realm), nil
		}
		return base, nil
	}
	return defaultChatBase(realm), nil
}

// Endpoint 返回 chat 上游完整 URL。
//
// 这是修 G1 的地方：relay 的默认拼接是 base_url + "/v1/chat/completions"，
// 而 workbuddy 上游是 /v2/chat/completions。
func Endpoint(p *model.Provider) (string, error) {
	base, err := ChatBase(p)
	if err != nil {
		return "", err
	}
	return base + ChatPath, nil
}

// BillingBase 返回计费/积分上游基址（与 chat 基址可能不同域：国内版 chat 在
// copilot.tencent.com、billing 在 codebuddy.cn）。
//
// 与 chat 基址不同，计费域刻意不跟随渠道的 base_url：积分/签到是供应商侧的账号
// 账本，不是可被用户重定向的转发目标（否则一个填错 base_url 的渠道会把账号操作
// 打到任意主机）。仅保留环境变量覆盖，供本地联调与 P0 探针把计费域指向 mock：
// WORKBUDDY_BILLING_BASE_CN / WORKBUDDY_BILLING_BASE_INTL。
func BillingBase(realm Realm) string {
	if realm == RealmCN {
		if v := strings.TrimSpace(os.Getenv("WORKBUDDY_BILLING_BASE_CN")); v != "" {
			return strings.TrimRight(v, "/")
		}
		return cnBillingBase
	}
	if v := strings.TrimSpace(os.Getenv("WORKBUDDY_BILLING_BASE_INTL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return intlBillingBase
}

// CreditsEndpoint 积分查询完整 URL。
func CreditsEndpoint(realm Realm) string { return BillingBase(realm) + BillingPathCredits }

// CheckinEndpoint 每日签到完整 URL（仅国内版有该端点，wb_accounts.py:126）。
func CheckinEndpoint(realm Realm) string { return BillingBase(realm) + BillingPathCheckin }

// ModelsEndpoint 模型窄端点完整 URL。
func ModelsEndpoint(p *model.Provider) (string, error) {
	base, err := ChatBase(p)
	if err != nil {
		return "", err
	}
	return base + NarrowModelsPath, nil
}

// RemoteConfigEndpoint 全量模型目录完整 URL（需桌面 UA）。
func RemoteConfigEndpoint(p *model.Provider) (string, error) {
	base, err := ChatBase(p)
	if err != nil {
		return "", err
	}
	return base + RemoteConfigPath, nil
}

// WebBase 国际版网页通道基址（ACP 打卡用；国内版无此通道）。
//
// 同 BillingBase：不跟随渠道 base_url，仅允许 WORKBUDDY_WEB_BASE 覆盖（联调/探针）。
func WebBase(realm Realm) (string, error) {
	if realm != RealmIntl {
		return "", errors.New("web conversation channel is intl-only")
	}
	if v := strings.TrimSpace(os.Getenv("WORKBUDDY_WEB_BASE")); v != "" {
		return strings.TrimRight(v, "/"), nil
	}
	return intlChatBase, nil
}

// defaultChatBase 区域默认 chat 基址。
func defaultChatBase(realm Realm) string {
	if realm == RealmCN {
		return upstream.CNChatBase()
	}
	return intlChatBase
}

// hostOf 从 base_url 取主机名（小写，去端口）。解析失败返回空串。
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 { // 去掉 userinfo
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "]") {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// normalizeBase 归一化用户填写的 base_url 为「scheme://host」，剥掉路径与尾斜杠。
//
// 需要兼容的历史写法：
//
//	https://copilot.tencent.com/v2                    （旧预设，presets.go:150）
//	https://copilot.tencent.com/v2/chat/completions
//	https://www.workbuddy.ai/v1/chat/completions
func normalizeBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return ""
	}
	trimmed := strings.TrimRight(raw, "/")
	// 依次剥掉可能被写进 base_url 的路径成分（幂等：重复剥不会更短则停）。
	for _, suffix := range []string{
		"/v2/chat/completions", "/v1/chat/completions", "/chat/completions",
		"/v2/chat", "/v1", "/v2",
	} {
		if strings.HasSuffix(trimmed, suffix) {
			trimmed = strings.TrimSuffix(trimmed, suffix)
			break
		}
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if trimmed == "" || !strings.Contains(trimmed, "://") {
		return ""
	}
	// 只剩 host 才算有效（避免把 https://host/foo 之类当基址）
	if hostOf(trimmed) == "" {
		return ""
	}
	if i := strings.Index(trimmed, "://"); i >= 0 {
		if strings.ContainsAny(trimmed[i+3:], "/?#") {
			return ""
		}
	}
	return trimmed
}
