package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// trae.go port of 9router src/lib/oauth/providers/trae.js（浏览器 OAuth：GetLoginGuidance →
// 验证 URL → 回调 refreshToken+loginHost → ExchangeToken → GetUserInfo）。
//
// 流程：
//  1. POST GetLoginGuidance {loginTraceID} → {Result.LoginHost}（多域名重试）
//  2. 浏览器打开 {loginHost}/authorization?client_id=...&login_trace_id=...&auth_callback_url={cb}&machine_id=...
//  3. 回调 → ?isRedirect=true&refreshToken=...&loginHost=...（宿主把完整 query 存 temp._rawCallback）
//  4. Resolve：解析回调取 refreshToken → POST ExchangeToken {ClientID, RefreshToken, ClientSecret:"-"}
//     → {Result:{AccessToken, RefreshToken, ExpiresAt}}
//
// 安全：ExchangeToken/GetUserInfo 的 origin 用硬编码 HTTPS 白名单，
// 回调里的 loginHost 故意不采信（SSRF 防护，9router traeApiOrigins 同思路）。
var (
	traeLoginGuidanceURLs = []string{
		"https://api.marscode.com/cloudide/api/v3/trae/GetLoginGuidance",
		"https://api.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
		"https://www.trae.ai/cloudide/api/v3/trae/GetLoginGuidance",
	}
	traeAPIOrigins = []string{
		"https://api.marscode.com",
		"https://api.trae.ai",
		"https://www.trae.ai",
		"https://www.marscode.com",
	}
	traeExchangeTokenPath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeClientID          = "ono9krqynydwx5"
	traeClientSecret      = "-"                                    // 公开客户端无 secret（移植自 9router，见 THIRD_PARTY_NOTICES.md）
	traeUserAgent         = "Trae/1.0.0 antigravity-cockpit-tools" // 上游识别的客户端标识，勿改
	traeDeviceID          = "0"                                    // 9router stable default
	traeAppVersion        = "3.5.54"
	// traeEnv 端点覆盖测试用（BeginAuth/Resolve 劫持）
	traeEnvOverride = ""
)

// Trae Trae（字节跳动 marscode）适配器。
type Trae struct{}

func (Trae) Key() string          { return "trae" }
func (Trae) Flow() oauth.FlowType { return oauth.FlowAuthCode }

// traeDeviceContext 每次登录的设备上下文（9router buildTraeDeviceContext）。
func traeDeviceContext() map[string]string {
	return map[string]string{
		"plugin_version": "local",
		"machine_id":     qoderUUID(), // 复用 uuid v4 工具
		"device_id":      traeDeviceID,
		"x_device_brand": "unknown",
		"x_device_type":  "unknown",
		"x_os_version":   "unknown",
		"x_env":          "",
		"x_app_version":  traeAppVersion,
		"x_app_type":     "stable",
	}
}

// fetchTraeLoginGuidance POST GetLoginGuidance → LoginHost（多域名依次尝试）。
func fetchTraeLoginGuidance(ctx context.Context, cb oauth.Callbacks, loginTraceID string) (string, error) {
	body := `{"loginTraceID":"` + loginTraceID + `","login_trace_id":"` + loginTraceID + `"}`
	urls := traeLoginGuidanceURLs
	if traeEnvOverride != "" {
		urls = []string{traeEnvOverride}
	}
	var lastErr = "no successful response"
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", traeUserAgent)
		resp, err := doHTTP(ctx, cb, req)
		if err != nil {
			lastErr = u + " " + err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Sprintf("%s HTTP %d", u, resp.StatusCode)
			continue
		}
		data := oauth.ParseJSONObject(string(raw))
		host := oauth.JSONPath(data, "Result", "LoginHost")
		if host == "" {
			host = oauth.JSONPath(data, "Result", "loginHost")
		}
		if host == "" {
			host = oauth.JSONPath(data, "Result", "LoginURL")
		}
		if host == "" {
			host = oauth.JSONPath(data, "data", "Result", "LoginHost")
		}
		if host == "" {
			host = oauth.JSONPath(data, "LoginHost")
		}
		if host != "" {
			return host, nil
		}
		lastErr = u + " missing LoginHost"
	}
	return "", fmt.Errorf("trae GetLoginGuidance failed: %s", lastErr)
}

// buildTraeVerificationURL 拼浏览器登录 URL（9router buildTraeVerificationUrl）。
func buildTraeVerificationURL(loginHost, loginTraceID, callbackURL string, ctxDev map[string]string) string {
	host := loginHost
	if !strings.HasPrefix(host, "http") {
		host = "https://" + strings.TrimLeft(host, "/")
	}
	u, err := url.Parse(host)
	if err != nil {
		return ""
	}
	u.Path = "/authorization"
	p := url.Values{}
	p.Set("login_version", "1")
	p.Set("auth_from", "trae")
	p.Set("login_channel", "native_ide")
	p.Set("plugin_version", ctxDev["plugin_version"])
	p.Set("auth_type", "local")
	p.Set("client_id", traeClientID)
	p.Set("redirect", "0")
	p.Set("login_trace_id", loginTraceID)
	p.Set("auth_callback_url", callbackURL)
	p.Set("machine_id", ctxDev["machine_id"])
	p.Set("device_id", ctxDev["device_id"])
	p.Set("x_device_id", ctxDev["device_id"])
	p.Set("x_machine_id", ctxDev["machine_id"])
	p.Set("x_device_brand", ctxDev["x_device_brand"])
	p.Set("x_device_type", ctxDev["x_device_type"])
	p.Set("x_os_version", ctxDev["x_os_version"])
	p.Set("x_env", ctxDev["x_env"])
	p.Set("x_app_version", ctxDev["x_app_version"])
	p.Set("x_app_type", ctxDev["x_app_type"])
	u.RawQuery = p.Encode()
	return u.String()
}

// traeRedirectURI 平台回调地址。
func traeRedirectURI(cb oauth.Callbacks) string {
	base := cb.PublicBaseURL
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return strings.TrimRight(base, "/") + "/api/oauth/callback"
}

// parseTraeCallback 解析回调 query（9router parseTraeCallback 1:1，容错大小写/下划线变体）。
func parseTraeCallback(raw string) (refreshToken, cloudideToken string, err error) {
	q, perr := url.ParseQuery(strings.TrimPrefix(raw, "?"))
	if perr != nil {
		return "", "", fmt.Errorf("trae callback parse: %w", perr)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := q.Get(k); strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	if e := pick("error", "error_code", "errorCode"); e != "" {
		desc := pick("error_description", "error_desc", "message")
		return "", "", &oauth.ResponseAuthError{ErrCode: e, Desc: desc}
	}
	rt := pick("refreshToken", "refresh_token", "RefreshToken")
	if rt == "" {
		return "", "", fmt.Errorf("trae callback missing refreshToken")
	}
	// loginHost 不采信（SSRF guard），仅 trace 用
	_ = pick("loginHost", "login_host", "LoginHost")
	ct := pick("x-cloudide-token", "xCloudideToken", "accessToken", "access_token", "token")
	return rt, ct, nil
}

// BeginAuth 取 LoginHost 并拼验证 URL（9router prepareConfig+buildAuthUrl 合并）。
func (Trae) BeginAuth(ctx context.Context, cb oauth.Callbacks, state string) (*oauth.BeginResult, map[string]string, error) {
	loginTraceID := state
	if loginTraceID == "" {
		loginTraceID = qoderUUID()
	}
	loginHost, err := fetchTraeLoginGuidance(ctx, cb, loginTraceID)
	if err != nil {
		return nil, nil, err
	}
	devCtx := traeDeviceContext()
	verifyURL := buildTraeVerificationURL(loginHost, loginTraceID, traeRedirectURI(cb), devCtx)

	temp := map[string]string{
		"login_trace_id": loginTraceID,
		"login_host":     loginHost,
		"machine_id":     devCtx["machine_id"],
		"device_id":      devCtx["device_id"],
	}
	return &oauth.BeginResult{
		VerificationURI:     verifyURL,
		VerificationURIComp: verifyURL,
	}, temp, nil
}

// fetchTraeExchangeToken POST ExchangeToken → AccessToken（多 origin 白名单重试）。
func fetchTraeExchangeToken(ctx context.Context, cb oauth.Callbacks, refreshToken, cloudideToken string) (*oauth.TokenSet, error) {
	payload, _ := json.Marshal(map[string]string{
		"ClientID":     traeClientID,
		"RefreshToken": refreshToken,
		"ClientSecret": traeClientSecret,
		"UserID":       "",
	})
	var lastErr = "no successful response"
	for _, origin := range traeAPIOrigins {
		u := strings.TrimRight(origin, "/") + traeExchangeTokenPath
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(payload)))
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", traeUserAgent)
		if cloudideToken != "" {
			req.Header.Set("x-cloudide-token", cloudideToken)
		}
		resp, err := doHTTP(ctx, cb, req)
		if err != nil {
			lastErr = u + " " + err.Error()
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Sprintf("%s HTTP %d", u, resp.StatusCode)
			continue
		}
		data := oauth.ParseJSONObject(string(raw))
		at := oauth.JSONPath(data, "Result", "AccessToken")
		if at == "" {
			at = oauth.JSONPath(data, "Result", "accessToken")
		}
		if at == "" {
			at = oauth.JSONPath(data, "accessToken")
		}
		if at == "" {
			msg := oauth.JSONPath(data, "message")
			if msg == "" {
				msg = oauth.JSONPath(data, "Result", "Message")
			}
			lastErr = u + " " + msg
			continue
		}
		rt := oauth.JSONPath(data, "Result", "RefreshToken")
		if rt == "" {
			rt = oauth.JSONPath(data, "refreshToken")
		}
		if rt == "" {
			rt = refreshToken
		}
		tok := &oauth.TokenSet{AccessToken: at, RefreshToken: rt}
		// ExpiresAt 绝对时间（秒或毫秒 epoch 字符串）
		if ea := oauth.JSONPath(data, "Result", "ExpiresAt"); ea != "" {
			tok.ExpiresAt = traeParseExpiresAt(ea)
		}
		return tok, nil
	}
	return nil, fmt.Errorf("trae ExchangeToken failed: %s", lastErr)
}

// traeParseExpiresAt 上游绝对过期时间（秒/毫秒 epoch）→ time.Time；解析失败返回零值。
func traeParseExpiresAt(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return time.Time{}
	}
	if n > 1e12 { // 毫秒 epoch
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

// Resolve 解析回调 rawCallback → ExchangeToken 换 token。
// 粘贴 token 模式（rawCallback 是裸 Cloud-IDE-JWT）也支持（9router paste-token mode）。
func (Trae) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	raw := temp[oauth.CallbackParamRaw]
	if raw == "" {
		return nil, fmt.Errorf("trae resolve: callback missing")
	}
	trimmed := strings.TrimSpace(raw)
	// 看起来像回调 URL（含 refreshToken=）→ 正常流程
	if strings.Contains(trimmed, "refreshToken=") || strings.Contains(trimmed, "refresh_token=") {
		rt, ct, err := parseTraeCallback(trimmed)
		if err != nil {
			return nil, err
		}
		tok, err := fetchTraeExchangeToken(ctx, cb, rt, ct)
		if err != nil {
			return nil, err
		}
		tok.Extra = map[string]string{"authMethod": "oauth"}
		if temp["machine_id"] != "" {
			tok.Extra["machineId"] = temp["machine_id"]
		}
		return tok, nil
	}
	// 粘贴模式：裸 Cloud-IDE-JWT / Bearer 前缀
	clean := trimmed
	for _, prefix := range []string{"Cloud-IDE-JWT ", "Bearer "} {
		if len(clean) > len(prefix) && equalFoldInsensitive(clean[:len(prefix)], prefix) {
			clean = strings.TrimSpace(clean[len(prefix):])
			break
		}
	}
	if clean == "" {
		return nil, fmt.Errorf("trae resolve: empty token")
	}
	return &oauth.TokenSet{
		AccessToken: clean,
		ExpiresAt:   time.Now().Add(14 * 24 * time.Hour), // TRAE tokenLifetimeDays=14
		Extra:       map[string]string{"authMethod": "imported"},
	}, nil
}

func equalFoldInsensitive(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb2 := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb2 && cb2 <= 'Z' {
			cb2 += 32
		}
		if ca != cb2 {
			return false
		}
	}
	return true
}

// Refresh POST ExchangeToken（JSON body，9router refreshTraeToken 1:1）。
// Response: {Result: {AccessToken, RefreshToken, ExpiresAt}}。
func (Trae) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("trae refresh: no refresh_token")
	}
	newTok, err := fetchTraeExchangeToken(ctx, cb, tok.RefreshToken, "")
	if err != nil {
		return nil, &oauth.RefreshError{Body: err.Error(), Status: 0, OAuthError: "trae_exchange_failed"}
	}
	return newTok, nil
}

// NeedsRefresh Trae 默认提前量。
func (Trae) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
