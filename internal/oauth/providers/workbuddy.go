package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
	"github.com/chengongliang/keygrid/internal/workbuddy/upstream"
)

// workbuddy.go WorkBuddy 国际版（intl）浏览器 OAuth 轮询流，协议与 codebuddy-cn 一致，
// 仅境域/域名不同：
//  1. POST {stateUrl}（body "{}"，特征头齐全）→ {code:0, data:{state, authUrl}}
//  2. 浏览器打开 authUrl
//  3. GET {tokenUrl}?state=...（code 11217 = pending，code 0 + accessToken = 成功）
//
// Refresh：POST {refreshUrl}，refresh_token 放在 X-Refresh-Token 头（不是 form body），
// body "{}"，响应 {code:0, data:{accessToken, refreshToken, expiresIn}}。
//
// 端点用包级 var（envOr 可覆盖），测试里直接改写包级变量指向 httptest。
var (
	workbuddyStateURL   = envOr("WORKBUDDY_STATE_URL", "https://www.workbuddy.ai/v2/plugin/auth/state")
	workbuddyTokenURL   = envOr("WORKBUDDY_TOKEN_URL", "https://www.workbuddy.ai/v2/plugin/auth/token")
	workbuddyRefreshURL = envOr("WORKBUDDY_REFRESH_URL", "https://www.workbuddy.ai/v2/plugin/auth/token/refresh")
	workbuddyAccountURL = envOr("WORKBUDDY_ACCOUNT_URL", "https://www.workbuddy.ai/v2/plugin/login/account")
	// workbuddyUA chat 面 UA（relay 侧使用，wb_accounts.py REALM_CONFIGS.intl.chat_ua）
	workbuddyUA = upstream.DesktopUAIntl
	// workbuddyAuthUA 授权/计费面 UA（本文件所有请求用它，对应 billing_ua）
	workbuddyAuthUA = "WorkBuddy/" + upstream.DesktopVersionIntl
	workbuddyDomain = "www.workbuddy.ai"
)

// workbuddyAuthHeaders 授权面（state/token/account/refresh）的请求头，逐项对齐参考实现
// wb_accounts.py:1205-1212 / 1239-1247 / 538-546：
//
//	Content-Type: application/json
//	Accept: application/json, text/plain, */*
//	X-Requested-With: XMLHttpRequest
//	User-Agent: WorkBuddy/5.8.4        ← 计费/插件面 UA，**不是** chat 面的 CLI UA
//	Origin/Referer: https://www.workbuddy.ai/
//
// 注意这组头里没有 X-Domain / X-Ide-*：那些是 chat 面（relay 侧 workbuddy.Headers）
// 的要求，授权面照搬反而会偏离参考实现。
func workbuddyAuthHeaders(extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("User-Agent", workbuddyAuthUA)
	h.Set("Origin", "https://"+workbuddyDomain)
	h.Set("Referer", "https://"+workbuddyDomain+"/")
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// applyWorkbuddyAuthHeaders 把授权面请求头逐项写入请求（http.Header 的 Set 语义）。
func applyWorkbuddyAuthHeaders(req *http.Request, extra map[string]string) {
	for k, vs := range workbuddyAuthHeaders(extra) {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
}

// WorkBuddyIntl WorkBuddy 国际版适配器（device_code 语义：BeginAuth → 前端轮询）。
type WorkBuddyIntl struct{}

func (WorkBuddyIntl) Key() string          { return "workbuddy-intl" }
func (WorkBuddyIntl) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

// BeginAuth POST state 拿 {state, authUrl}。
func (WorkBuddyIntl) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	// 必须带 ?platform=CLI：上游缺该参数直接 400（参考实现 wb_accounts.py:1203
	// start_login 的 url 拼接；同目录 codebuddy.go 的 CN 适配器也是这么做的）。
	stateURL := workbuddyStateURL + "?" + url.Values{"platform": {"CLI"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stateURL, strings.NewReader("{}"))
	if err != nil {
		return nil, nil, fmt.Errorf("workbuddy state request: %w", err)
	}
	applyWorkbuddyAuthHeaders(req, nil)

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, nil, fmt.Errorf("workbuddy state request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("workbuddy state request failed (%d)", resp.StatusCode)
	}
	// authUrl 兼容 authUrl / url / auth_url 三种命名。
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State    string `json:"state"`
			AuthURL  string `json:"authUrl"`
			URL      string `json:"url"`
			AuthURL2 string `json:"auth_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("workbuddy state response: %w", err)
	}
	authURL := data.Data.AuthURL
	if authURL == "" {
		authURL = data.Data.URL
	}
	if authURL == "" {
		authURL = data.Data.AuthURL2
	}
	if data.Code != 0 || data.Data.State == "" || authURL == "" {
		return nil, nil, fmt.Errorf("workbuddy state error: %s (code=%d)", data.Msg, data.Code)
	}
	res := &oauth.BeginResult{
		VerificationURI:     authURL,
		VerificationURIComp: authURL,
		Interval:            5,
	}
	return res, map[string]string{"device_code": data.Data.State}, nil
}

// Resolve GET tokenUrl?state=...（code 11217 = pending，code 0 = 成功）。
func (WorkBuddyIntl) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	state := temp["device_code"]
	if state == "" {
		return nil, fmt.Errorf("workbuddy resolve: state missing")
	}
	u := workbuddyTokenURL + "?" + url.Values{"state": {state}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("workbuddy token poll request: %w", err)
	}
	applyWorkbuddyAuthHeaders(req, nil)

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy token poll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, &oauth.PendingError{Reason: "upstream_error"} // 5xx 视为可继续轮询
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("workbuddy token poll failed (%d)", resp.StatusCode)
	}

	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			ExpiresAt    any    `json:"expiresAt"`
			Domain       string `json:"domain"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("workbuddy poll response: %w", err)
	}
	// code 11217 = 仍在等待授权；其余非零码为终态错误（只带 code/msg，绝不带 token）。
	if data.Code == 11217 {
		return nil, &oauth.PendingError{Reason: "authorization_pending"}
	}
	if data.Code != 0 || data.Data.AccessToken == "" {
		return nil, fmt.Errorf("workbuddy token poll error: %s (code=%d)", data.Msg, data.Code)
	}

	now := nowFunc(cb)
	domain := data.Data.Domain
	if domain == "" {
		domain = workbuddyDomain
	}
	// 账号信息为 best-effort：任何失败都忽略。
	nickname := workbuddyFetchNickname(ctx, cb, state, data.Data.AccessToken)
	ts := workbuddyTokenSet(data.Data.AccessToken, data.Data.RefreshToken, domain, nickname,
		workbuddyParseEpoch(data.Data.ExpiresAt), data.Data.ExpiresIn, now)
	return ts, nil
}

// Refresh POST refreshUrl：refresh_token 在 X-Refresh-Token 头，响应可能轮换 refreshToken。
func (WorkBuddyIntl) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("workbuddy refresh: no refresh_token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workbuddyRefreshURL, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("workbuddy refresh request: %w", err)
	}
	// 刷新面比 state/token 多几个头（wb_accounts.py:544-553）：X-User-Id / X-Domain /
	// X-CodeBuddy-Request / Accept-Language 都要带，缺 X-Domain 会被上游判非法请求。
	extra := map[string]string{
		"X-Refresh-Token":       tok.RefreshToken,
		"X-Auth-Refresh-Source": "plugin",
		"X-CodeBuddy-Request":   "1",
		"Accept-Language":       "en-US",
	}
	if uid := strings.TrimSpace(tok.Extra["uid"]); uid != "" {
		extra["X-User-Id"] = uid
	}
	reqDomain := ""
	if tok.Extra != nil {
		reqDomain = strings.TrimSpace(tok.Extra["domain"])
	}
	if reqDomain == "" {
		reqDomain = workbuddyDomain
	}
	extra["X-Domain"] = reqDomain
	applyWorkbuddyAuthHeaders(req, extra)

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, fmt.Errorf("workbuddy refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &data)
	if resp.StatusCode >= 400 || data.Code != 0 || data.Data.AccessToken == "" {
		oauthErr := data.Msg
		if oauthErr == "" {
			oauthErr = fmt.Sprintf("code=%d", data.Code)
		}
		return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: oauthErr}
	}
	// refresh_token 可能轮换：响应未带则沿用旧值。
	rt := data.Data.RefreshToken
	if rt == "" {
		rt = tok.RefreshToken
	}
	domain := workbuddyDomain
	nickname := ""
	if tok.Extra != nil {
		if v := tok.Extra["domain"]; v != "" {
			domain = v
		}
		nickname = tok.Extra["nickname"]
	}
	now := nowFunc(cb)
	ts := workbuddyTokenSet(data.Data.AccessToken, rt, domain, nickname, time.Time{}, data.Data.ExpiresIn, now)
	if ts.ExpiresAt.IsZero() {
		ts.ExpiresAt = tok.ExpiresAt // 响应无过期信息时保留旧值
	}
	return ts, nil
}

// NeedsRefresh WorkBuddy 默认提前量（与 codebuddy-cn 一致，由宿主传入 lead）。
func (WorkBuddyIntl) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}

// workbuddyTokenSet 组装 TokenSet：过期时间优先 expiresAt（秒/毫秒 epoch），
// 其次 expiresIn 秒，最后回落 JWT exp claim；Extra 带非敏感展示值。
func workbuddyTokenSet(access, refresh, domain, nickname string, expiresAt time.Time, expiresIn int64, now time.Time) *oauth.TokenSet {
	claims := parseWorkbuddyJWTClaims(access)
	if expiresAt.IsZero() && expiresIn > 0 {
		expiresAt = now.Add(time.Duration(expiresIn) * time.Second)
	}
	if expiresAt.IsZero() {
		expiresAt = claims.Exp
	}
	if nickname == "" && claims.UID != "" {
		nickname = workbuddyNicknameFallback(claims.UID)
	}
	if domain == "" {
		domain = workbuddyDomain
	}
	extra := map[string]string{
		"nickname": nickname,
		"domain":   domain,
		"realm":    "intl",
	}
	if claims.UID != "" {
		extra["uid"] = claims.UID
	}
	ts := newTokenSet(access, refresh, 0, extra, now)
	ts.ExpiresAt = expiresAt
	return ts
}

// workbuddyFetchNickname GET login/account（best-effort，任何错误返回空串）。
func workbuddyFetchNickname(ctx context.Context, cb oauth.Callbacks, state, accessToken string) string {
	u := workbuddyAccountURL + "?" + url.Values{"state": {state}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	applyWorkbuddyAuthHeaders(req, map[string]string{"Authorization": "Bearer " + accessToken})
	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	var data struct {
		Code int `json:"code"`
		Data struct {
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := decodeJSON(resp.Body, &data); err != nil {
		return ""
	}
	if data.Code != 0 {
		return ""
	}
	return strings.TrimSpace(data.Data.Nickname)
}

// workbuddyNicknameFallback uid 缺省昵称：取 uid 前 8 个字符。
func workbuddyNicknameFallback(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// workbuddyJWTClaims 不校验签名的 JWT 载荷摘取结果。
type workbuddyJWTClaims struct {
	UID string
	Exp time.Time
}

// parseWorkbuddyJWTClaims 解码 JWT 载荷（不验签，仅取 uid/exp）。base64url，容忍 padding。
func parseWorkbuddyJWTClaims(token string) workbuddyJWTClaims {
	var out workbuddyJWTClaims
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return out
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 兼容带 padding 的签发方
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return out
		}
	}
	var claims struct {
		UID string  `json:"uid"`
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return out
	}
	out.UID = strings.TrimSpace(claims.UID)
	if claims.Exp > 0 {
		out.Exp = time.Unix(int64(claims.Exp), 0)
	}
	return out
}

// workbuddyParseEpoch 解析 epoch 过期时间（秒或毫秒；> 1e12 视为毫秒）。容忍数字/字符串。
func workbuddyParseEpoch(v any) time.Time {
	switch t := v.(type) {
	case nil:
		return time.Time{}
	case float64:
		return workbuddyEpoch(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return time.Time{}
		}
		return workbuddyEpoch(f)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return time.Time{}
		}
		return workbuddyEpoch(f)
	}
	return time.Time{}
}

// workbuddyEpoch 秒/毫秒 epoch → time.Time（非正数视为无值）。
func workbuddyEpoch(f float64) time.Time {
	if f <= 0 {
		return time.Time{}
	}
	if f > 1e12 {
		return time.UnixMilli(int64(f))
	}
	return time.Unix(int64(f), 0)
}
