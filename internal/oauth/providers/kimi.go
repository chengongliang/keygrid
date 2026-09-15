package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/chengongliang/keygrid/internal/oauth"
)

// kimi.go 1:1 port from 9router src/lib/oauth/providers/kimi.js
// (which itself follows CLIProxyAPI internal/auth/kimi).
//
// Endpoints (9router open-sse/providers/registry/kimi.js oauth block):
//
//	clientId:  17e5f671-d194-4dfb-9706-5516cb48c098
//	deviceCodeUrl: https://auth.kimi.com/api/oauth/device_authorization
//	tokenUrl:      https://auth.kimi.com/api/oauth/token
//	authorizeDeviceUrl: https://www.kimi.com/code/authorize_device
const (
	kimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	// endpoints var（测试可劫持到 httptest mock server；env 覆盖供 e2e/自托管 fork）
	kimiUpstreamAPI = "https://api.kimi.com/coding" // transport baseUrl (openai: /v1/chat/completions)
)

var (
	kimiDeviceCodeURL = envOr("KIMI_DEVICE_CODE_URL", "https://auth.kimi.com/api/oauth/device_authorization")
	kimiTokenURL      = envOr("KIMI_TOKEN_URL", "https://auth.kimi.com/api/oauth/token")
	kimiAuthorizeURL  = envOr("KIMI_AUTHORIZE_DEVICE_URL", "https://www.kimi.com/code/authorize_device")
)

// envOr 端点 env 覆盖（9router KIMI_CODING_OAUTH_CLIENT_ID 同思路）。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Kimi kimi device_code adapter.
type Kimi struct{}

func (Kimi) Key() string          { return "kimi" }
func (Kimi) Flow() oauth.FlowType { return oauth.FlowDeviceCode }

// kimiDeviceID 生成设备指纹 ID（9router: crypto.randomUUID()）。
func kimiDeviceID() string {
	b := make([]byte, 16)
	if _, err := cryptoRandRead(b); err != nil {
		return fmt.Sprintf("kimi-%d", time.Now().UnixNano())
	}
	// RFC 4122 v4
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// buildKimiHeaders port of 9router open-sse/config/appConstants.js buildKimiHeaders.
// deviceId must stay stable per connection for the whole OAuth session.
func buildKimiHeaders(deviceID, hostname, version string) http.Header {
	osName := runtime.GOOS
	archName := runtime.GOARCH
	var deviceModel string
	switch osName {
	case "darwin":
		deviceModel = "macOS " + archName
	case "windows":
		deviceModel = "Windows " + archName
	case "linux":
		deviceModel = "Linux " + archName
	default:
		deviceModel = osName + " " + archName
	}
	if deviceID == "" {
		deviceID = fmt.Sprintf("kimi-%d", time.Now().UnixMilli())
	}
	// X-Msh-* 为 Kimi 客户端协议头；platform 沿用移植来源 9router 的标识
	// （上游 OAuth 端点按客户端类型识别，见 THIRD_PARTY_NOTICES.md）。
	h := http.Header{}
	h.Set("X-Msh-Platform", "9router")
	h.Set("X-Msh-Version", version)
	h.Set("X-Msh-Device-Name", hostname)
	h.Set("X-Msh-Device-Model", deviceModel)
	h.Set("X-Msh-Device-Id", deviceID)
	return h
}

const kimiVersion = "keygrid"

// BeginAuth POST {deviceCodeUrl} (client_id, 设备指纹头) → device_code/user_code。
func (Kimi) BeginAuth(ctx context.Context, cb oauth.Callbacks, _ string) (*oauth.BeginResult, map[string]string, error) {
	deviceID := kimiDeviceID()
	form := url.Values{"client_id": {kimiClientID}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, vs := range buildKimiHeaders(deviceID, cb.Hostname, kimiVersion) {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := doHTTP(ctx, cb, req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("device code request failed: %s", string(body))
	}
	var data struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("device code response: %w", err)
	}
	if data.DeviceCode == "" || data.UserCode == "" {
		return nil, nil, fmt.Errorf("device code response missing fields: %s", string(body))
	}

	uri := data.VerificationURI
	if uri == "" {
		uri = kimiAuthorizeURL
	}
	complete := data.VerificationURIComplete
	if complete == "" {
		complete = fmt.Sprintf("%s?user_code=%s", uri, url.QueryEscape(data.UserCode))
	}
	interval := data.Interval
	if interval <= 0 {
		interval = 5 // 9router: data.interval || 5
	}
	res := &oauth.BeginResult{
		UserCode:            data.UserCode,
		VerificationURI:     uri,
		VerificationURIComp: complete,
		Interval:            interval,
		DeviceCodeExpiresIn: data.ExpiresIn,
	}
	temp := map[string]string{"device_code": data.DeviceCode, "_kimiDeviceId": deviceID}
	return res, temp, nil
}

// Resolve 轮询 token endpoint（CLIProxyAPI: Kimi 对 pending 状态返回 200 + error 字段）。
func (Kimi) Resolve(ctx context.Context, cb oauth.Callbacks, temp map[string]string) (*oauth.TokenSet, error) {
	deviceCode := temp["device_code"]
	deviceID := temp["_kimiDeviceId"]
	if deviceCode == "" {
		return nil, fmt.Errorf("kimi resolve: device_code missing")
	}

	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {kimiClientID},
		"device_code": {deviceCode},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, vs := range buildKimiHeaders(deviceID, cb.Hostname, kimiVersion) {
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
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &data) // CLIProxyAPI: non-json → error field stays empty; treat below

	// 9router pollToken: pending 状态原样返回（外层继续轮询）
	if data.Error == "authorization_pending" || data.Error == "slow_down" {
		return nil, &oauth.PendingError{Reason: data.Error}
	}
	if data.AccessToken == "" {
		return nil, fmt.Errorf("kimi token poll failed (%d): %s", resp.StatusCode, string(body))
	}
	return newTokenSet(data.AccessToken, data.RefreshToken, data.ExpiresIn, map[string]string{"deviceId": deviceID}, nowFunc(cb)), nil
}

// Refresh form body（no client_secret）+ X-Msh-* headers（9router REFRESH_PROFILES.kimi）。
func (Kimi) Refresh(ctx context.Context, cb oauth.Callbacks, tok *oauth.TokenSet) (*oauth.TokenSet, error) {
	if tok == nil || tok.RefreshToken == "" {
		return nil, fmt.Errorf("kimi refresh: no refresh_token")
	}
	deviceID := tok.Extra["deviceId"]

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
		"client_id":     {kimiClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, vs := range buildKimiHeaders(deviceID, cb.Hostname, kimiVersion) {
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
		rt = tok.RefreshToken // 9router: tokens.refresh_token || refreshToken
	}
	return newTokenSet(data.AccessToken, rt, data.ExpiresIn, tok.Extra, nowFunc(cb)), nil
}

// NeedsRefresh 默认 lead 提前量。
func (Kimi) NeedsRefresh(tok *oauth.TokenSet, lead time.Duration) bool {
	return defaultNeedsRefresh(tok, lead)
}
