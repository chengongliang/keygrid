package qoder

import (
	"net/url"
	"os"
	"strings"

	"github.com/chengongliang/keygrid/internal/model"
)

// provider.go Qoder 渠道识别与区域端点解析。
//
// 判定同时覆盖两种渠道形态：
//   - kind=oauth + oauth_provider=qoder / qoder-cn（设备流授权）
//   - kind=api_key + base_url 指向 Qoder 主机（PAT pt-… 直填）
//
// 后者是关键：PAT 渠道没有 oauth_provider，只能靠 base_url 认出来。

// providerIDs 两端 OAuth 适配器注册名。
const (
	ProviderIDIntl = "qoder"
	ProviderIDCN   = "qoder-cn"
)

// bases 一个区域的全部上游端点。
type bases struct {
	Chat    string // 推理与模型目录
	ChatAlt string // job token（jt-…）专用：国际站 jt- 走 api2，api3 会 403 Login expired
	OpenAPI string // 设备流 / userinfo / 配额
	Center  string // refresh_token（设备流走到这里必然 403）
	Login   string // 浏览器授权页
	Website string
}

var regionBases = map[string]bases{
	RegionIntl: {
		Chat:    "https://api3.qoder.sh",
		ChatAlt: "https://api2.qoder.sh",
		OpenAPI: "https://openapi.qoder.sh",
		Center:  "https://center.qoder.sh",
		Login:   "https://qoder.com/device/selectAccounts",
		Website: "https://qoder.com",
	},
	RegionCN: {
		Chat:    "https://gateway.qoder.com.cn",
		ChatAlt: "https://gateway.qoder.com.cn",
		OpenAPI: "https://openapi.qoder.com.cn",
		Center:  "https://gateway.qoder.com.cn",
		Login:   "https://qoder.com.cn/device/selectAccounts",
		Website: "https://qoder.com.cn",
	},
}

// BasesOf 区域端点集（未知区域回落国际站）。
func BasesOf(region string) bases {
	if b, ok := regionBases[region]; ok {
		return b
	}
	return regionBases[RegionIntl]
}

// NormalizeRegion 区域名归一（"cn" → RegionCN，其余 → RegionIntl）。
func NormalizeRegion(region string) string {
	if region == RegionCN {
		return RegionCN
	}
	return RegionIntl
}

// 域名 → 区域（base_url 判定用）。
var hostRegions = map[string]string{
	"api3.qoder.sh":        RegionIntl,
	"api2.qoder.sh":        RegionIntl,
	"openapi.qoder.sh":     RegionIntl,
	"center.qoder.sh":      RegionIntl,
	"qoder.sh":             RegionIntl,
	"qoder.com":            RegionIntl,
	"gateway.qoder.com.cn": RegionCN,
	"openapi.qoder.com.cn": RegionCN,
	"qoder.com.cn":         RegionCN,
}

// hostOf 取 base_url 的主机名（小写、去端口）；解析失败返回空串。
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// IsQoder 判断渠道是否走 Qoder 私有协议（COSY 签名 + /algo 端点）。
func IsQoder(p *model.Provider) bool {
	if p == nil {
		return false
	}
	switch p.OAuthProvider {
	case ProviderIDIntl, ProviderIDCN:
		return true
	}
	_, ok := hostRegions[hostOf(p.BaseURL)]
	return ok
}

// RegionOf 渠道所属区域：oauth_provider=qoder-cn 或 base_url 命中国内站 → cn。
func RegionOf(p *model.Provider) string {
	if p == nil {
		return RegionIntl
	}
	if p.OAuthProvider == ProviderIDCN {
		return RegionCN
	}
	if r, ok := hostRegions[hostOf(p.BaseURL)]; ok {
		return r
	}
	return RegionIntl
}

// IsPAT 判断凭据是否为 PAT（pt-…，需先换 jt- 才能签名）。
func IsPAT(token string) bool { return strings.HasPrefix(token, "pt-") }

// isJobToken 判断是否已是 job token（jt-…）：国际站必须改走 api2。
func isJobToken(token string) bool { return strings.HasPrefix(token, "jt-") }

// InferenceBase 推理/模型目录/图片上传的基地址。
//
// 国际站的 jt-… 流量被 api3 拒绝（403 Login expired），官方 qodercli 改走 api2；
// 设备流 token（dt-…）留在 api3。国内站所有 token 都走单一网关。
func InferenceBase(region, token string) string {
	// env 覆盖（自托管 fork / E2E mock）：QODER_CHAT_BASE / QODER_CN_CHAT_BASE。
	key := "QODER_CHAT_BASE"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_CHAT_BASE"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return strings.TrimRight(v, "/")
	}
	b := BasesOf(region)
	if NormalizeRegion(region) == RegionCN {
		return b.Chat
	}
	if isJobToken(token) && !IsPAT(token) {
		return b.ChatAlt
	}
	return b.Chat
}

// ChatURL 对话完整 URL（含 Encode=1：正文经 WAF 混淆编码）。
func ChatURL(region, token string) string {
	return InferenceBase(region, token) + "/algo" + ChatSigPath +
		"?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
}

// ModelListURL 模型目录完整 URL。
func ModelListURL(region, token string) string {
	return InferenceBase(region, token) + "/algo" + ModelListPath
}

// ImageUploadURL 图片上传完整 URL。
func ImageUploadURL(region, token, requestID string) string {
	return InferenceBase(region, token) + "/algo" + ImageUploadSigPath + "?request_id=" + url.QueryEscape(requestID)
}

// DeviceTokenURL 设备流轮询端点。
// env 可覆盖（自托管 fork / E2E mock）：QODER_DEVICE_TOKEN_URL / QODER_CN_DEVICE_TOKEN_URL。
func DeviceTokenURL(region string) string {
	key := "QODER_DEVICE_TOKEN_URL"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_DEVICE_TOKEN_URL"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return BasesOf(region).OpenAPI + "/api/v1/deviceToken/poll"
}

// UserInfoURL 用户信息端点（COSY 签名需要的 userId 从这里取）。
// env 可覆盖：QODER_USERINFO_URL / QODER_CN_USERINFO_URL。
func UserInfoURL(region string) string {
	key := "QODER_USERINFO_URL"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_USERINFO_URL"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return BasesOf(region).OpenAPI + "/api/v1/userinfo"
}

// JobTokenExchangeURL PAT → job token 兑换端点（普通 JSON POST，不签名）。
func JobTokenExchangeURL(region string) string {
	key := "QODER_JOB_TOKEN_URL"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_JOB_TOKEN_URL"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return BasesOf(region).OpenAPI + "/api/v1/jobToken/exchange"
}

// LoginURL 浏览器授权页（env 可覆盖：QODER_LOGIN_URL / QODER_CN_LOGIN_URL）。
func LoginURL(region string) string {
	key := "QODER_LOGIN_URL"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_LOGIN_URL"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return BasesOf(region).Login
}

// RefreshURL token 刷新端点（设备流走到这里必然 403）。env 可覆盖。
func RefreshURL(region string) string {
	key := "QODER_REFRESH_URL"
	if NormalizeRegion(region) == RegionCN {
		key = "QODER_CN_REFRESH_URL"
	}
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return BasesOf(region).Center + "/algo/api/v3/user/refresh_token"
}

// WebsiteURL 站点首页（预设展示用）。
func WebsiteURL(region string) string { return BasesOf(region).Website }
