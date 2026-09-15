package providers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// openai.go port of 9router src/lib/oauth/providers/codex.js（OpenAI Codex，ChatGPT 订阅，PKCE）。
//
// Endpoints（9router open-sse/providers/registry codex oauth block）:
//
//	clientId:     app_EMoamEEZ73f0CkXaXp7hrann
//	authorizeUrl: https://auth.openai.com/oauth/authorize
//	tokenUrl:     https://auth.openai.com/oauth/token
//	scope:        openid profile email offline_access, S256
//	fixedPort:    1455, callbackPath: /auth/callback —— OpenAI 对该 client_id 只白名单
//	本机回调，平台域名 redirect 会被拒。因此授权采用「浏览器登录 → 落到
//	localhost:1455（无服务监听，页面打不开属预期）→ 用户粘贴地址栏 URL 回平台」模式，
//	与 9router OAuth 弹窗的手动粘贴分支一致。
//
// 上游转发（relay）：ChatGPT backend Responses API，见 internal/relay/codex.go。
const (
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexScope    = "openid profile email offline_access"

	// codexRedirectURI OpenAI Codex CLI client 白名单的本机回调（9router fixedPort 1455）。
	codexRedirectURI = "http://localhost:1455/auth/callback"
)

var (
	codexAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
)

// OpenAI codex pkce adapter。
type OpenAI struct{}

func (OpenAI) Key() string          { return "openai" }
func (OpenAI) Flow() oauth.FlowType { return oauth.FlowPKCE }

// BeginAuth 生成 state+code_verifier（verifier 由宿主存入 oauth_states.temp）并拼 authorize URL。
func (OpenAI) BeginAuth(ctx context.Context, cb oauth.Callbacks, state string) (*oauth.BeginResult, map[string]string, error) {
	verifier := newCodeVerifier()
	challenge := s256Challenge(verifier)

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", codexClientID)
	params.Set("redirect_uri", codexRedirectURI)
	params.Set("scope", codexScope)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	// 9router codex extraParams
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	params.Set("originator", "codex_cli_rs")
	params.Set("state", state)

	temp := map[string]string{"code_verifier": verifier, "redirect_uri": codexRedirectURI}
	return &oauth.BeginResult{
		AuthorizeURL: codexAuthorizeURL + "?" + params.Encode(),
		RedirectURI:  codexRedirectURI,
	}, temp, nil
}

// Resolve 用回调 code + code_verifier 换 token（form body，9router exchangeToken）。
func (OpenAI) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	code := temp["code"]
	verifier := temp["code_verifier"]
	if code == "" || verifier == "" {
		return nil, fmt.Errorf("openai resolve: code/code_verifier missing")
	}
	redirectURI := temp["redirect_uri"]
	if redirectURI == "" {
		redirectURI = codexRedirectURI
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("token exchange failed: %s", string(body))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("token response: %w", err)
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("token exchange missing access_token: %s", string(body))
	}
	// 9router mapTokens: chatgpt account 信息从 id_token 提取（accountId/plan/email）。
	return newTokenSet(data.AccessToken, data.RefreshToken, data.ExpiresIn, chatgptExtra(data.IDToken), nowFunc(cb)), nil
}

// Refresh JSON body（9router REFRESH 8521.Wm refreshCodexToken：JSON + client_id，
// registry 里的 refresh.encoding=form 元数据未被 codex 专用路径使用）。
func (OpenAI) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("openai refresh: no refresh_token")
	}
	payload := map[string]string{
		"client_id":     codexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": tok.RefreshToken,
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(string(buf)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &data)
	if data.AccessToken == "" {
		return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: data.Error}
	}
	rt := data.RefreshToken
	if rt == "" {
		rt = tok.RefreshToken
	}
	// id_token 可能轮换：拿得到就用新的 account 信息，否则保留原 extra。
	extra := tok.Extra
	if data.IDToken != "" {
		if e := chatgptExtra(data.IDToken); len(e) > 0 {
			extra = mergeExtra(extra, e)
		}
	}
	return newTokenSet(data.AccessToken, rt, data.ExpiresIn, extra, nowFunc(cb)), nil
}

// NeedsRefresh 9router registry: codex refreshLeadMs=432000000 (5 天)。
// 平台侧仍由通用 lead+后台扫描兜底，这里按默认提前量即可。
func (OpenAI) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}

// ---- pkce 工具 ----

// newCodeVerifier 43-128 字符的 pkce code_verifier（RFC 7636）。
func newCodeVerifier() string {
	b := make([]byte, 32)
	if _, err := cryptoRandRead(b); err != nil {
		b = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// s256Challenge base64url(sha256(verifier))。
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// pkceRedirectURI 平台回调地址（OAuth 回调路由）—— anthropic 等平台域名回调的 provider 用。
func pkceRedirectURI(cb oauth.Callbacks) string {
	base := cb.PublicBaseURL
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return strings.TrimRight(base, "/") + "/api/oauth/callback"
}

// chatgptClaims OpenAI id_token (JWT) 里的 ChatGPT 账号信息（不解签名，仅 claims）。
// 9router YR（extractCodexTokenInfo）：账号信息在
// claims["https://api.openai.com/auth"] 下（chatgpt_account_id/chatgpt_plan_type），
// 并回退顶层 account_id。
type chatgptClaims struct {
	AccountID string
	PlanType  string
	Email     string
}

func parseChatGPTClaims(idToken string) chatgptClaims {
	var out chatgptClaims
	if idToken == "" {
		return out
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return out
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 兼容带 padding 的签发方
		if payload2, err2 := base64.StdEncoding.DecodeString(parts[1]); err2 == nil {
			payload = payload2
		} else {
			return out
		}
	}
	var claims struct {
		Email      string         `json:"email"`
		AccountID  string         `json:"account_id"`
		OpenAIAuth map[string]any `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return out
	}
	out.Email = claims.Email
	out.AccountID = claims.AccountID
	if claims.OpenAIAuth != nil {
		if v, ok := claims.OpenAIAuth["chatgpt_account_id"].(string); ok && v != "" {
			out.AccountID = v
		}
		if v, ok := claims.OpenAIAuth["chatgpt_plan_type"].(string); ok {
			out.PlanType = v
		}
	}
	return out
}

// chatgptExtra id_token → TokenSet.Extra（relay 拼 chatgpt-account-id 头用）。
func chatgptExtra(idToken string) map[string]string {
	c := parseChatGPTClaims(idToken)
	extra := map[string]string{}
	if c.AccountID != "" {
		extra["chatgptAccountId"] = c.AccountID
	}
	if c.PlanType != "" {
		extra["chatgptPlanType"] = c.PlanType
	}
	if c.Email != "" {
		extra["email"] = c.Email
	}
	return extra
}

// mergeExtra dst 复制自 src 的键（src 覆盖 dst）。
func mergeExtra(dst, src map[string]string) map[string]string {
	out := make(map[string]string, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		out[k] = v
	}
	return out
}
