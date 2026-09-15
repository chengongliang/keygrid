package providers

import (
	"context"
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

// iflow.go port of 9router src/lib/oauth/services/iflow.js + registry/iflow.js oauth block.
//
// iFlow (心流) authorization_code + Basic Auth：
//  1. 拼 authorize URL（loginMethod=phone&type=phone&redirect=&state=&client_id=）
//  2. 浏览器回调带 code → 宿主落 temp
//  3. Resolve：POST tokenUrl（form：grant_type/code/redirect_uri/client_id/client_secret，
//     Header: Authorization: Basic base64(clientId:clientSecret)）
//
// registry oauth block:
//
//	clientId:     10009311001
//	clientSecret: 4Z3YjXycVsQvyGF1etiNlIBB4RsqSDtW
//	authorizeUrl: https://iflow.cn/oauth
//	tokenUrl:     https://iflow.cn/oauth/token
//	refreshLeadMs: 86400000 (1 天 —— 提前量由宿主统一 lead 兜底)
//
// client_id/client_secret 为 iFlow 官方客户端的公开固定值（移植自 9router，
// 见 THIRD_PARTY_NOTICES.md），并非本项目机密；需替换时可用环境变量覆盖。
var (
	iflowAuthorizeURL = envOr("IFLOW_AUTHORIZE_URL", "https://iflow.cn/oauth")
	iflowTokenURL     = envOr("IFLOW_TOKEN_URL", "https://iflow.cn/oauth/token")
	iflowClientID     = envOr("IFLOW_CLIENT_ID", "10009311001")
	iflowClientSecret = envOr("IFLOW_CLIENT_SECRET", "4Z3YjXycVsQvyGF1etiNlIBB4RsqSDtW")
)

// IFlow iFlow（心流）authorization_code + Basic Auth 适配器。
type IFlow struct{}

func (IFlow) Key() string          { return "iflow" }
func (IFlow) Flow() oauth.FlowType { return oauth.FlowAuthCode }

// iflowRedirectURI 平台回调地址（与 pkce 相同的回调路由）。
func iflowRedirectURI(cb oauth.Callbacks) string {
	base := cb.PublicBaseURL
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return strings.TrimRight(base, "/") + "/api/oauth/callback"
}

// iflowBasicAuth Basic base64(clientId:clientSecret)。
func iflowBasicAuth() string {
	return base64.StdEncoding.EncodeToString([]byte(iflowClientID + ":" + iflowClientSecret))
}

// BeginAuth 拼手机号登录授权 URL；无 PKCE，verifier 不需要。
func (IFlow) BeginAuth(ctx context.Context, cb oauth.Callbacks, state string) (*oauth.BeginResult, map[string]string, error) {
	params := url.Values{}
	params.Set("loginMethod", "phone")
	params.Set("type", "phone")
	params.Set("redirect", iflowRedirectURI(cb))
	params.Set("state", state)
	params.Set("client_id", iflowClientID)

	temp := map[string]string{"state": state}
	return &oauth.BeginResult{AuthorizeURL: iflowAuthorizeURL + "?" + params.Encode()}, temp, nil
}

// Resolve 用回调 code + Basic Auth 换 token（9router exchangeCode 1:1）。
func (IFlow) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	if err := oauth.ExtractCallbackError(temp); err != nil {
		return nil, err
	}
	code := temp["code"]
	if code == "" {
		return nil, fmt.Errorf("iflow resolve: code missing")
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {iflowRedirectURI(cb)},
		"client_id":     {iflowClientID},
		"client_secret": {iflowClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, iflowTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+iflowBasicAuth())

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("iflow token exchange failed (%d): %s", resp.StatusCode, string(body))
	}
	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("iflow token response: %w", err)
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("iflow token exchange missing access_token: %s", string(body))
	}
	return newTokenSet(data.AccessToken, data.RefreshToken, data.ExpiresIn, nil, nowFunc(cb)), nil
}

// Refresh 表单 body + Basic Auth（9router REFRESH_PROFILES.iflow 1:1）。
func (IFlow) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("iflow refresh: no refresh_token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
		"client_id":     {iflowClientID},
		"client_secret": {iflowClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, iflowTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+iflowBasicAuth())

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var data struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
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
	return newTokenSet(data.AccessToken, rt, data.ExpiresIn, tok.Extra, nowFunc(cb)), nil
}

// NeedsRefresh iFlow 默认提前量。
func (IFlow) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
