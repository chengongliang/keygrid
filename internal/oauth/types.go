package oauth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// FlowType 授权流程类型（对齐 9router flowType）。
type FlowType string

const (
	FlowDeviceCode FlowType = "device_code" // kimi
	FlowPKCE       FlowType = "pkce"        // openai(codex) / anthropic：authorization_code + code_challenge
)

var ErrNotFound = errors.New("oauth provider not found")

// TokenSet 凭据明文结构（AES-GCM 加密前序列化到 credentials.enc_data）。
// json tag 即落库字段名，relay/oauth 两侧共用。
type TokenSet struct {
	AccessToken  string            `json:"access_token"`
	RefreshToken string            `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time         `json:"expires_at,omitempty"` // access token 绝对过期时间
	Extra        map[string]string `json:"extra,omitempty"`      // kimi deviceId / openai accountId / anthropic scope ...
}

// Kind 区分 api_key 与 oauth 凭据（同一个 enc_data 字段）。
func (t *TokenSet) Kind() string { return "oauth" }

// BeginResult 发起授权的返回。
type BeginResult struct {
	// device_code 流：展示给用户
	UserCode            string `json:"user_code,omitempty"`
	VerificationURI     string `json:"verification_uri,omitempty"`
	VerificationURIComp string `json:"verification_uri_complete,omitempty"`
	Interval            int    `json:"interval,omitempty"` // 轮询间隔秒
	DeviceCodeExpiresIn int    `json:"device_code_expires_in,omitempty"`
	// pkce 流：浏览器跳转
	AuthorizeURL string `json:"authorize_url,omitempty"`
	// RedirectURI 本次授权使用的 redirect_uri（pkce）。以 http://localhost 开头时
	// 平台收不到回调（如 OpenAI Codex 固定 localhost:1455），前端切换为
	// 「粘贴回调 URL」模式，用户把浏览器最终地址栏的 URL 粘贴回平台。
	RedirectURI string `json:"redirect_uri,omitempty"`
}

// Adapter 每个 OAuth provider 一个实现（对应 9router providers/*.js）。
type Adapter interface {
	Key() string
	Flow() FlowType
	// 发起授权。device_code: 请求 device endpoint；pkce: 生成 state+verifier 并拼 authorize URL。
	BeginAuth(ctx context.Context, cb Callbacks, state string) (*BeginResult, map[string]string, error)
	// temp 为 oauth_states.temp 的内容。device_code: 轮询 token endpoint；
	// pkce: 用 temp["code"]+code_verifier 换 token。成功返回 TokenSet（temp 已无用）。
	Resolve(ctx context.Context, cb Callbacks, temp map[string]string) (*TokenSet, error)
	// 用 refresh_token 换新 token。返回新 TokenSet（refresh_token 可能轮换）。
	Refresh(ctx context.Context, cb Callbacks, tok *TokenSet) (*TokenSet, error)
	// NeedsRefresh 提前量判定（per-provider 可覆盖，如 anthropic lead=4h）。
	NeedsRefresh(tok *TokenSet, lead time.Duration) bool
}

// Callbacks 宿主注入的 HTTP 能力（测试时换成 httptest client）。
type Callbacks struct {
	// HTTPClient 带 timeout 的 client；nil 用默认
	HTTPClient *http.Client
	// Now 当前时间（测试注入）
	Now func() time.Time
	// Hostname 设备名（kimi X-Msh-Device-Name）
	Hostname string
	// PublicBaseURL pkce 回调 redirect_uri 拼接
	PublicBaseURL string
}
