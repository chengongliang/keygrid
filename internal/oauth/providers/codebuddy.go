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

// codebuddy.go port of 9router src/lib/oauth/providers/codebuddy-cn.js
// + open-sse/services/tokenRefresh/providers.js refreshCodebuddyToken。
//
// CodeBuddy (腾讯) 浏览器 OAuth 轮询流：
//  1. POST {stateUrl}?platform=CLI（body "{}"，特征头齐全）→ {code:0, data:{state, authUrl}}
//  2. 浏览器打开 authUrl
//  3. GET {tokenUrl}?state=...（code 11217 = pending，code 0 + accessToken = 成功）
//
// Refresh：POST /v2/plugin/auth/token/refresh，refresh_token 放在 X-Refresh-Token
// 请求头（不是 form body），body "{}"，响应 {code:0, data:{accessToken, refreshToken, expiresIn}}。
var (
	codebuddyStateURL   = envOr("CODEBUDDY_STATE_URL", "https://copilot.tencent.com/v2/plugin/auth/state")
	codebuddyTokenURL   = envOr("CODEBUDDY_TOKEN_URL", "https://copilot.tencent.com/v2/plugin/auth/token")
	codebuddyRefreshURL = envOr("CODEBUDDY_REFRESH_URL", "https://copilot.tencent.com/v2/plugin/auth/token/refresh")
	codebuddyUA         = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// codebuddyHeaders CodeBuddy 特征请求头（官方 CLI 同款）。
func codebuddyHeaders(extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", codebuddyUA)
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Domain", "copilot.tencent.com")
	h.Set("X-Product", "SaaS")
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// CodeBuddyCN CodeBuddy CN 适配器（device_code 语义：BeginAuth → 前端轮询）。
type CodeBuddyCN struct{}

func (CodeBuddyCN) Key() string          { return "codebuddy-cn" }
func (CodeBuddyCN) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

// BeginAuth POST state 拿 {state, authUrl}（9router requestDeviceCode 1:1）。
func (CodeBuddyCN) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	u := codebuddyStateURL + "?" + url.Values{"platform": {"CLI"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, vs := range codebuddyHeaders(map[string]string{
		"X-No-Authorization": "true",
		"X-No-User-Id":       "true",
	}) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("codebuddy state request failed: %s", string(body))
	}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("codebuddy state response: %w", err)
	}
	if data.Code != 0 || data.Data.State == "" || data.Data.AuthURL == "" {
		return nil, nil, fmt.Errorf("codebuddy state error: %s (code=%d)", data.Msg, data.Code)
	}
	res := &oauth.BeginResult{
		VerificationURI:     data.Data.AuthURL,
		VerificationURIComp: data.Data.AuthURL,
		Interval:            5, // registry pollInterval=5000
	}
	temp := map[string]string{"device_code": data.Data.State}
	return res, temp, nil
}

// Resolve GET tokenUrl?state=...（9router pollToken 1:1：code 11217=pending，0=成功）。
func (CodeBuddyCN) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	state := temp["device_code"]
	if state == "" {
		return nil, fmt.Errorf("codebuddy resolve: state missing")
	}
	u := codebuddyTokenURL + "?" + url.Values{"state": {state}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, vs := range codebuddyHeaders(map[string]string{
		"X-No-Authorization":   "true",
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, &oauth.PendingError{Reason: "upstream_error"} // 5xx 视为可继续轮询
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("codebuddy token poll failed (%d): %s", resp.StatusCode, string(body))
	}

	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			TokenType    string `json:"tokenType"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("codebuddy poll response: %w", err)
	}
	// code 0 = 成功；code 11217 = pending（RetryFetchToken）
	if data.Code == 0 && data.Data.AccessToken != "" {
		expiresIn := data.Data.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 86400 // registry mapTokens 默认 1 天
		}
		return newTokenSet(data.Data.AccessToken, data.Data.RefreshToken, int(expiresIn), nil, nowFunc(cb)), nil
	}
	if data.Code == 11217 {
		return nil, &oauth.PendingError{Reason: "authorization_pending"}
	}
	return nil, fmt.Errorf("codebuddy token poll error: %s (code=%d)", data.Msg, data.Code)
}

// Refresh POST refreshUrl：refresh_token 在 X-Refresh-Token 头（9router refreshCodebuddyToken 1:1）。
// 响应 {code:0, data:{accessToken, refreshToken, expiresIn}}。
func (CodeBuddyCN) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("codebuddy refresh: no refresh_token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codebuddyRefreshURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, vs := range codebuddyHeaders(map[string]string{
		"X-Refresh-Token":       tok.RefreshToken,
		"X-Auth-Refresh-Source": "plugin",
	}) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
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
	if data.Code != 0 || data.Data.AccessToken == "" {
		oauthErr := data.Msg
		if oauthErr == "" {
			oauthErr = fmt.Sprintf("code=%d", data.Code)
		}
		return nil, &oauth.RefreshError{Body: string(body), Status: resp.StatusCode, OAuthError: oauthErr}
	}
	expiresIn := data.Data.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 86400
	}
	rt := data.Data.RefreshToken
	if rt == "" {
		rt = tok.RefreshToken
	}
	return newTokenSet(data.Data.AccessToken, rt, int(expiresIn), tok.Extra, nowFunc(cb)), nil
}

// NeedsRefresh CodeBuddy 默认提前量。
func (CodeBuddyCN) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
